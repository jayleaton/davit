//go:build linux

package guestagent

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"davit/internal/buildinfo"
	"davit/internal/guestproto"
)

// session is one authenticated control connection.
type session struct {
	a      *Agent
	conn   net.Conn
	pc     *guestproto.Conn
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	vmID string
	gen  uint64

	mu     sync.Mutex
	stdin  map[uint64]*proc      // exec stream -> process (stdin routing)
	writes map[uint64]*fileWrite // write stream -> pending write
	seqs   guestproto.SeqTracker // host->guest stream frames (reader goroutine only)

	requests int      // requests dispatched (reader goroutine only)
	tcp      net.Conn // set once DialTCP was acknowledged; the session is then a raw stream
}

// handshakeTimeout bounds how long a connection may sit before Hello.
const handshakeTimeout = 10 * time.Second

func newSession(a *Agent, c net.Conn) *session {
	ctx, cancel := context.WithCancel(context.Background())
	return &session{a: a, conn: c, pc: guestproto.NewConn(c), ctx: ctx, cancel: cancel,
		stdin: map[uint64]*proc{}, writes: map[uint64]*fileWrite{}}
}

// close ends the session: it kills the session's non-detached processes and
// aborts its pending file writes.
func (s *session) close() {
	s.once.Do(func() {
		s.cancel()
		s.conn.Close()
		s.a.procs.sessionClosed(s)
		s.a.closeListeners(s)
		s.mu.Lock()
		for id, w := range s.writes {
			w.abort()
			delete(s.writes, id)
		}
		s.mu.Unlock()
	})
}

func (s *session) run() {
	defer s.close()
	s.a.addSession(s)
	defer s.a.removeSession(s)
	if !s.handshake() {
		return
	}
	for {
		f, err := s.pc.ReadFrame()
		if err != nil {
			return
		}
		if f.V != s.pc.Version() {
			s.reply(guestproto.ErrorFrame(f.ID, guestproto.Errorf(guestproto.CodeMalformedFrame, "frame version %d, negotiated %d", f.V, s.pc.Version())))
			return
		}
		if !s.dispatch(f) {
			if s.tcp != nil {
				s.splice()
			}
			return
		}
	}
}

func (s *session) handshake() bool {
	_ = s.conn.SetDeadline(time.Now().Add(handshakeTimeout))
	f, err := s.pc.ReadFrame()
	if err != nil {
		return false
	}
	hello, err := guestproto.Decode[guestproto.Hello](f)
	if err != nil {
		s.reply(guestproto.ErrorFrame(f.ID, err))
		return false
	}
	v, err := guestproto.NegotiateVersion(hello.Versions, guestproto.SupportedVersions)
	if err != nil {
		s.reply(guestproto.ErrorFrame(f.ID, err))
		return false
	}
	secret, err := s.a.authenticate(hello)
	if err != nil {
		s.a.cfg.Log.Warn("handshake refused", "vm", hello.VMID, "generation", hello.Generation, "err", err)
		s.reply(guestproto.ErrorFrame(f.ID, err))
		return false
	}
	ack := guestproto.HelloAck{Version: v, Features: guestproto.IntersectFeatures(hello.Features, Features),
		AgentVersion: buildinfo.Version, GuestNonce: guestproto.NewChallenge()}
	if ack.Proof, err = guestproto.ComputeProof(secret, guestproto.TranscriptFor(hello, ack)); err != nil {
		s.reply(guestproto.ErrorFrame(f.ID, err))
		return false
	}
	if hello.BootstrapSecret != nil {
		if err := s.a.bind(s, hello, secret); err != nil {
			s.reply(guestproto.ErrorFrame(f.ID, err))
			return false
		}
	}
	if err := s.pc.Send(f.ID, ack); err != nil {
		return false
	}
	_ = s.conn.SetDeadline(time.Time{})
	s.pc.SetVersion(v)
	s.vmID, s.gen = hello.VMID, hello.Generation
	return true
}

// Features is what this agent advertises in HelloAck (intersected with the
// host's offer).
var Features = []string{guestproto.FeatureExec, guestproto.FeatureFiles, guestproto.FeatureIdentity, guestproto.FeatureFilesTar,
	guestproto.FeatureTCP, guestproto.FeatureJobs, guestproto.FeaturePS, guestproto.FeatureListen, guestproto.FeatureWorkspace}

// reply writes a prebuilt frame (error responses) at the negotiated
// version, or as-is before the handshake.
func (s *session) reply(f *guestproto.Frame) {
	if v := s.pc.Version(); v != 0 {
		f.V = v
	}
	_ = s.pc.WriteFrame(f)
}

func (s *session) send(id uint64, m guestproto.Message) {
	if err := s.pc.Send(id, m); err != nil {
		s.close()
	}
}

func (s *session) fail(id uint64, err error) { s.reply(guestproto.ErrorFrame(id, err)) }

// sendStream writes a stream frame; false means the session is gone.
func (s *session) sendStream(f *guestproto.Frame) bool {
	f.V = s.pc.Version()
	if err := s.pc.WriteFrame(f); err != nil {
		s.close()
		return false
	}
	return true
}

// gate refuses workload RPCs until the restore barrier completed for this
// session's generation (defence in depth; the host also gates admission).
func (s *session) gate(id uint64) bool {
	if s.a.barrierDone(s.vmID, s.gen) {
		return true
	}
	s.fail(id, guestproto.Errorf(guestproto.CodeUnexpectedType, "restore barrier not complete for generation %d", s.gen))
	return false
}

// dispatch handles one frame; false ends the session.
func (s *session) dispatch(f *guestproto.Frame) bool {
	m, err := guestproto.DecodeMessage(f)
	if err != nil {
		if f.ID != 0 {
			s.fail(f.ID, err)
		}
		return true
	}
	// Host->guest stream frames.
	switch m := m.(type) {
	case *guestproto.Stdin, *guestproto.StdinClose, *guestproto.FileChunk:
		if dup, err := s.seqs.Accept(f); err != nil {
			s.a.cfg.Log.Warn("stream sequence error", "err", err)
			return false
		} else if dup {
			return true
		}
		switch m := m.(type) {
		case *guestproto.Stdin:
			s.stdinFor(f.Stream, false).feed(m.Data, false)
		case *guestproto.StdinClose:
			s.stdinFor(f.Stream, true).feed(nil, true)
		case *guestproto.FileChunk:
			s.chunk(f.Stream, m)
		}
		return true
	}
	if f.ID == 0 {
		return true // notifications are not used in v1
	}
	s.requests++
	switch m := m.(type) {
	case *guestproto.Ping:
		s.send(f.ID, guestproto.Pong{})
	case *guestproto.ReadinessProbe:
		go func() { s.send(f.ID, s.a.readiness(s.ctx, s, m.Services)) }() // systemctl may be slow
	case *guestproto.SetIdentity:
		go s.setIdentity(f.ID, *m)
	case *guestproto.Exec:
		if s.gate(f.ID) {
			s.exec(f.ID, *m)
		}
	case *guestproto.ProcessAttach:
		if s.gate(f.ID) {
			s.attach(f.ID, *m)
		}
	case *guestproto.ProcessSignal:
		if s.gate(f.ID) {
			s.signal(f.ID, *m)
		}
	case *guestproto.ProcessStatusRequest:
		if s.gate(f.ID) {
			s.status(f.ID, *m)
		}
	case *guestproto.ReadFile:
		if s.gate(f.ID) {
			go s.readFile(f.ID, *m)
		}
	case *guestproto.WriteFile:
		if s.gate(f.ID) {
			s.writeFile(f.ID, *m)
		}
	case *guestproto.FileCommit:
		go s.commit(f.ID, f.Stream, *m)
	case *guestproto.Shutdown:
		if s.gate(f.ID) {
			s.shutdown(f.ID, *m)
		}
	case *guestproto.DialTCP:
		if s.gate(f.ID) && s.dialTCP(f.ID, *m) {
			return false // the frame loop ends; run splices the stream
		}
	case *guestproto.ProcessLookup:
		if s.gate(f.ID) {
			s.lookup(f.ID, *m)
		}
	case *guestproto.ProcessListRequest:
		if s.gate(f.ID) {
			go s.processList(f.ID) // reads all of /proc
		}
	case *guestproto.Listen:
		if s.gate(f.ID) {
			s.listen(f.ID, *m)
		}
	case *guestproto.Unlisten:
		if s.gate(f.ID) {
			s.unlisten(f.ID, *m)
		}
	case *guestproto.ReverseConnect:
		if s.gate(f.ID) && s.reverseConnect(f.ID, *m) {
			return false // as for DialTCP
		}
	case *guestproto.WorkspacePrepare:
		if s.gate(f.ID) {
			go s.prepareWorkspace(f.ID, *m)
		}
	default:
		s.fail(f.ID, guestproto.Errorf(guestproto.CodeUnexpectedType, "%q is not a request", f.Type))
	}
	return true
}

func (s *session) setIdentity(id uint64, req guestproto.SetIdentity) {
	if req.VMID != s.vmID || req.Generation != s.gen {
		s.fail(id, guestproto.Errorf(guestproto.CodeInvalidArgument, "identity must match the session's vmId and generation"))
		return
	}
	res, err := s.a.runBarrier(s.ctx, req)
	if err != nil {
		s.a.cfg.Log.Error("restore barrier failed", "err", err)
		s.fail(id, err)
		return
	}
	s.send(id, res)
}

func (s *session) shutdown(id uint64, req guestproto.Shutdown) {
	s.send(id, guestproto.Ack{})
	go func() {
		time.Sleep(time.Duration(req.GraceSeconds) * time.Second)
		if err := s.a.cfg.System.PowerOff(context.Background()); err != nil {
			s.a.cfg.Log.Error("power off", "err", err)
		}
	}()
}

// dialTimeout bounds DialTCP's connect to the guest loopback port.
const dialTimeout = 10 * time.Second

// dialTCP connects to 127.0.0.1:port and sends the Ack; true means the
// session is now a raw byte stream to s.tcp. It runs on the reader
// goroutine and is refused unless it is the session's only request, so no
// other goroutine can write a frame after the Ack. A failed dial is
// answered with an error frame and the session continues.
func (s *session) dialTCP(id uint64, req guestproto.DialTCP) bool {
	if s.busy() {
		s.fail(id, guestproto.Errorf(guestproto.CodeInvalidArgument, "tcp.dial must be the only request on its session"))
		return false
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(req.Port)))
	ctx, cancel := context.WithTimeout(s.ctx, dialTimeout)
	defer cancel()
	var c net.Conn
	var err error
	if dial := s.a.cfg.DialTCP; dial != nil {
		c, err = dial(ctx, req.Port)
	} else {
		var d net.Dialer
		c, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			s.fail(id, guestproto.Errorf(guestproto.CodeNotFound, "nothing listening on %s", addr))
		} else {
			s.fail(id, guestproto.Errorf(guestproto.CodeInternal, "connect to %s: %v", addr, err))
		}
		return false
	}
	if err := s.pc.Send(id, guestproto.Ack{}); err != nil {
		c.Close()
		s.close()
		return false
	}
	s.tcp = c
	return true
}

// busy reports whether the session served anything besides the current
// request, which a request turning it into a raw stream must not allow.
func (s *session) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests != 1 || len(s.stdin) != 0 || len(s.writes) != 0
}

// splice copies bytes between the session connection and s.tcp until both
// directions end or the session is closed. EOF in one direction half-closes
// the other side's write half when it supports CloseWrite; otherwise, or on
// an error, both connections are closed.
func (s *session) splice() {
	t := s.tcp
	stop := context.AfterFunc(s.ctx, func() { t.Close() })
	defer stop()
	defer t.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe(t, s.conn)
	}()
	pipe(s.conn, t)
	wg.Wait()
}

// pipe copies src to dst, then half-closes dst or, failing that, closes
// both.
func pipe(dst, src net.Conn) {
	_, err := io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok && err == nil && cw.CloseWrite() == nil {
		return
	}
	dst.Close()
	src.Close()
}
