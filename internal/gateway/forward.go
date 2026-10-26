package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Limits bound one proxied stream. Copies are backpressured by the slower
// side; these add hard stops.
type Limits struct {
	// MaxBytes caps each direction; 0 means unlimited.
	MaxBytes int64
	// IdleTimeout closes the stream when neither side moves data for this
	// long; 0 disables it.
	IdleTimeout time.Duration
}

// ErrLimit reports a stream closed for exceeding Limits.MaxBytes.
var ErrLimit = errors.New("gateway: stream byte limit reached")

// Splice copies a<->b until both directions finish, ctx ends or a limit
// trips, then closes both. Each direction's EOF is propagated as a half-close
// where the conn supports it, so request/response protocols finish cleanly.
func Splice(ctx context.Context, a, b net.Conn, lim Limits) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	act := newActivity(lim.IdleTimeout, cancel)
	defer act.stop()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	pump := func(i int, dst, src net.Conn) {
		defer wg.Done()
		errs[i] = copyHalf(dst, src, lim.MaxBytes, act)
		if errs[i] != nil {
			cancel()
		}
	}
	wg.Add(2)
	go pump(0, b, a)
	go pump(1, a, b)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	a.Close()
	b.Close()
	<-done
	if act.expired() {
		return fmt.Errorf("gateway: stream idle for %v", lim.IdleTimeout)
	}
	return errors.Join(errs[0], errs[1])
}

type halfCloser interface{ CloseWrite() error }

func copyHalf(dst, src net.Conn, max int64, act *activity) error {
	var r io.Reader = src
	if max > 0 {
		r = io.LimitReader(src, max+1)
	}
	n, err := io.Copy(dst, activityReader{r, act})
	if max > 0 && n > max {
		return ErrLimit
	}
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	if hc, ok := dst.(halfCloser); ok {
		hc.CloseWrite()
	} else {
		dst.Close()
	}
	return nil
}

// activity is an idle timer reset by every read.
type activity struct {
	mu      sync.Mutex
	t       *time.Timer
	d       time.Duration
	tripped bool
}

func newActivity(d time.Duration, onIdle func()) *activity {
	a := &activity{d: d}
	if d > 0 {
		a.t = time.AfterFunc(d, func() {
			a.mu.Lock()
			a.tripped = true
			a.mu.Unlock()
			onIdle()
		})
	}
	return a
}

func (a *activity) touch() {
	if a.t != nil {
		a.t.Reset(a.d)
	}
}

func (a *activity) stop() {
	if a.t != nil {
		a.t.Stop()
	}
}

func (a *activity) expired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tripped
}

type activityReader struct {
	r   io.Reader
	act *activity
}

func (ar activityReader) Read(p []byte) (int, error) {
	n, err := ar.r.Read(p)
	if n > 0 {
		ar.act.touch()
	}
	return n, err
}

// Streams authorizes and opens guest TCP streams. It is the primitive behind
// `forward`, the HTTP tunnel endpoints and the SSH paths.
type Streams struct {
	Auth   Authorizer
	Dialer GuestDialer
	Limits Limits
	// Sessions tracks each authorized stream for revocation.
	Sessions *Sessions
}

// Open authorizes who for req and dials the guest port. The returned Target
// is what was authorized; callers must not re-resolve the VM reference.
func (s *Streams) Open(ctx context.Context, who Identity, req Request) (net.Conn, Target, error) {
	if s == nil || s.Auth == nil || s.Dialer == nil {
		return nil, Target{}, errors.New("gateway: streams not configured")
	}
	if req.VMRef == "" || req.Port == 0 {
		return nil, Target{}, ErrInvalidTarget
	}
	t, err := s.Auth.Authorize(ctx, who, req)
	if err != nil {
		return nil, Target{}, err
	}
	if t.VMID == "" {
		return nil, Target{}, ErrDenied
	}
	c, err := s.Dialer.DialGuest(ctx, t, req.Port)
	if err != nil {
		return nil, t, fmt.Errorf("gateway: dial guest %s:%d: %w", t.VMID, req.Port, err)
	}
	return c, t, nil
}

// Tunnel authorizes who for req and splices client to the guest port until
// either side finishes. client is always closed.
func (s *Streams) Tunnel(ctx context.Context, who Identity, req Request, client net.Conn) error {
	g, t, err := s.Open(ctx, who, req)
	if err != nil {
		client.Close()
		return err
	}
	defer s.Sessions.Track(who.UserID, t.VMID, client)()
	return Splice(ctx, client, g, s.Limits)
}

// ServeForward serves an allocated gateway port: every connection accepted
// on ln is identified via w, authorized for (vmRef, guestPort) and spliced to
// the guest. It returns when ln is closed or ctx ends.
func (s *Streams) ServeForward(ctx context.Context, ln net.Listener, w WhoIser, vmRef string, guestPort uint16, logf func(string, ...any)) error {
	return serveConns(ctx, ln, func(ctx context.Context, c net.Conn) {
		id, err := IdentifyConn(ctx, w, c)
		if err != nil {
			logf("forward: reject %v: %v", c.RemoteAddr(), err)
			c.Close()
			return
		}
		if err := s.Tunnel(ctx, id, Request{VMRef: vmRef, Action: ActionForward, Port: guestPort}, c); err != nil && !errors.Is(err, net.ErrClosed) {
			logf("forward: %s -> %s:%d: %v", id, vmRef, guestPort, err)
		}
	})
}

// serveConns accepts until ln closes or ctx ends, handling each conn in its
// own goroutine. On return it cancels the handlers' context and waits for
// them.
func serveConns(ctx context.Context, ln net.Listener, handle func(context.Context, net.Conn)) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	var wg sync.WaitGroup
	for {
		c, err := ln.Accept()
		if err != nil {
			closed := ctx.Err() != nil || errors.Is(err, net.ErrClosed)
			stop()
			cancel()
			wg.Wait()
			if closed {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle(ctx, c)
		}()
	}
}
