package guestclient

import (
	"context"
	"errors"
	"io"
	"sync"

	"davit/internal/guestproto"
)

// ExecResult is how a process ended.
type ExecResult struct {
	ProcessID string
	PID       int
	Status    guestproto.ProcessState
	// ExitCode is -1 when the process did not exit normally.
	ExitCode int
	Signal   string
	TimedOut bool
}

// ExecIO connects a process's streams. Nil writers discard output; a nil
// Stdin means the process reads /dev/null.
type ExecIO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Exec runs req to completion, streaming output into io and returning the
// exit status. Cancelling ctx abandons the stream; the agent kills a
// non-detached process when its session closes, so callers that cancel
// should Close the session or Signal the process.
func (c *Client) Exec(ctx context.Context, req guestproto.Exec, sio ExecIO) (ExecResult, error) {
	req.Stdin = sio.Stdin != nil
	cl, err := c.start(req, true)
	if err != nil {
		return ExecResult{}, err
	}
	defer cl.release()
	f, err := cl.wait(ctx)
	if err != nil {
		return ExecResult{}, err
	}
	started, err := guestproto.Decode[guestproto.ProcessStarted](f)
	if err != nil {
		return ExecResult{}, err
	}
	res := ExecResult{ProcessID: started.ProcessID, PID: started.PID}

	if sio.Stdin != nil {
		go c.pumpStdin(ctx, cl.id, sio.Stdin)
	}
	ex, err := consume(ctx, cl, 0, writers(sio.Stdout, sio.Stderr))
	if err != nil {
		return res, err
	}
	res.Status, res.Signal, res.ExitCode, res.TimedOut = ex.Status, ex.Signal, -1, ex.TimedOut
	if ex.ExitCode != nil {
		res.ExitCode = *ex.ExitCode
	}
	return res, nil
}

// Stdin feeds a process started by StartProcess. Methods are safe for
// one writer at a time.
type Stdin struct {
	c      *Client
	stream uint64
	mu     sync.Mutex
	seq    uint64
	closed bool
}

// StartProcess starts req as given and returns without following output;
// read it with Attach. When req.Stdin is set it returns the process's stdin,
// which is routed through this session only. Closing the session closes
// that stdin, and kills the process unless req.Detached.
func (c *Client) StartProcess(ctx context.Context, req guestproto.Exec) (guestproto.ProcessStarted, *Stdin, error) {
	req.NoFollow = true
	cl, err := c.start(req, false)
	if err != nil {
		return guestproto.ProcessStarted{}, nil, err
	}
	defer cl.release()
	f, err := cl.wait(ctx)
	if err != nil {
		return guestproto.ProcessStarted{}, nil, err
	}
	st, err := guestproto.Decode[guestproto.ProcessStarted](f)
	if err != nil || !req.Stdin {
		return st, nil, err
	}
	return st, &Stdin{c: c, stream: cl.id}, nil
}

// Write sends p as one or more stdin chunks.
func (s *Stdin) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	n := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), 32<<10)]
		if err := s.send(guestproto.Stdin{Data: chunk}); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// Close closes the process's stdin (EOF).
func (s *Stdin) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.send(guestproto.StdinClose{})
}

func (s *Stdin) send(m guestproto.Message) error {
	s.seq++
	f, err := guestproto.EncodeStream(s.stream, s.seq, m)
	if err != nil {
		return err
	}
	f.V = s.c.ack.Version
	return s.c.pc.WriteFrame(f)
}

// Start launches a detached process and returns without waiting; use
// Attach to read its output and Status to poll it.
func (c *Client) Start(ctx context.Context, req guestproto.Exec) (guestproto.ProcessStarted, error) {
	req.Detached, req.Stdin = true, false
	cl, err := c.start(req, false)
	if err != nil {
		return guestproto.ProcessStarted{}, err
	}
	defer cl.release()
	f, err := cl.wait(ctx)
	if err != nil {
		return guestproto.ProcessStarted{}, err
	}
	return guestproto.Decode[guestproto.ProcessStarted](f)
}

// Attach replays a process's retained output after afterSeq and follows it
// until the process exits.
func (c *Client) Attach(ctx context.Context, processID string, afterSeq uint64, stdout, stderr io.Writer) (guestproto.ProcessExited, error) {
	return c.AttachFunc(ctx, processID, afterSeq, writers(stdout, stderr))
}

// OutputFunc receives one output chunk with its stream sequence number.
// Returning an error stops following.
type OutputFunc func(seq uint64, stderr bool, data []byte) error

// AttachFunc is Attach delivering each chunk with its seq, so callers can
// resume from the last one they handled.
func (c *Client) AttachFunc(ctx context.Context, processID string, afterSeq uint64, fn OutputFunc) (guestproto.ProcessExited, error) {
	cl, err := c.start(guestproto.ProcessAttach{ProcessID: processID, AfterSeq: afterSeq}, true)
	if err != nil {
		return guestproto.ProcessExited{}, err
	}
	defer cl.release()
	f, err := cl.wait(ctx)
	if err != nil {
		return guestproto.ProcessExited{}, err
	}
	if _, err := guestproto.Decode[guestproto.Ack](f); err != nil {
		return guestproto.ProcessExited{}, err
	}
	return consume(ctx, cl, afterSeq, fn)
}

func writers(stdout, stderr io.Writer) OutputFunc {
	return func(_ uint64, isErr bool, data []byte) error {
		if isErr {
			return write(stderr, data)
		}
		return write(stdout, data)
	}
}

// consume reads a process stream until ProcessExited.
func consume(ctx context.Context, cl *call, afterSeq uint64, fn OutputFunc) (guestproto.ProcessExited, error) {
	var seqs guestproto.SeqTracker
	seqs.Resume(cl.id, afterSeq)
	for {
		f, err := cl.next(ctx)
		if err != nil {
			return guestproto.ProcessExited{}, err
		}
		if f.Error != nil {
			return guestproto.ProcessExited{}, f.Error
		}
		if dup, err := seqs.Accept(f); err != nil {
			return guestproto.ProcessExited{}, err
		} else if dup {
			continue
		}
		m, err := guestproto.DecodeMessage(f)
		if err != nil {
			return guestproto.ProcessExited{}, err
		}
		switch m := m.(type) {
		case *guestproto.Stdout:
			err = fn(f.Seq, false, m.Data)
		case *guestproto.Stderr:
			err = fn(f.Seq, true, m.Data)
		case *guestproto.ProcessExited:
			return *m, nil
		default:
			err = guestproto.Errorf(guestproto.CodeUnexpectedType, "unexpected %q on process stream", f.Type)
		}
		if err != nil {
			return guestproto.ProcessExited{}, err
		}
	}
}

func write(w io.Writer, p []byte) error {
	if w == nil {
		return nil
	}
	_, err := w.Write(p)
	return err
}

// pumpStdin streams r to the process and closes its stdin at EOF. Errors end
// the pump; the process then sees no further input.
func (c *Client) pumpStdin(ctx context.Context, stream uint64, r io.Reader) {
	buf := make([]byte, 32<<10)
	var seq uint64
	send := func(m guestproto.Message) bool {
		seq++
		f, err := guestproto.EncodeStream(stream, seq, m)
		if err != nil {
			return false
		}
		f.V = c.ack.Version
		return c.pc.WriteFrame(f) == nil
	}
	for ctx.Err() == nil {
		n, err := r.Read(buf)
		if n > 0 && !send(guestproto.Stdin{Data: append([]byte(nil), buf[:n]...)}) {
			return
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				send(guestproto.StdinClose{})
			}
			return
		}
	}
}
