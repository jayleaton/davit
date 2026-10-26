// Package guestclient is the host side of the guest-agent protocol
// (internal/guestproto): it dials the agent through an injected transport,
// runs the authenticated handshake, and exposes readiness, the restore
// barrier, exec and file transfer. It never knows how the transport reaches
// the guest; the runtime adapter supplies a DialFunc (Firecracker hybrid
// vsock, Mac native vsock, or a unix socket in tests).
//
// One Client is one control session. A session multiplexes concurrent
// requests and streams over a single connection; the reader delivers
// responses by request ID and stream frames by stream ID. Stream consumers
// that stall eventually stall the whole session (bounded per-stream queues
// give backpressure instead of unbounded memory).
//
// DialTCP and ReverseConnect are the exception: each authenticates a
// dedicated session whose only request turns the connection into a raw byte
// stream to a TCP connection on the guest's loopback. Listen also runs on a
// dedicated session (see Listen).
package guestclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"sync/atomic"

	"davit/internal/guestproto"
)

// DefaultPort is the guest vsock port the agent listens on (SPEC §5).
const DefaultPort uint32 = 1024

// DialFunc opens a new byte stream to the guest agent.
type DialFunc func(ctx context.Context) (net.Conn, error)

// ErrClosed is returned for requests on a closed or failed session.
var ErrClosed = errors.New("guestclient: session closed")

// ErrRejected marks a handshake the guest refused (as opposed to one the
// host refused to verify); errors.As still finds the guest's *guestproto.Error.
var ErrRejected = errors.New("guestclient: guest rejected hello")

// Session identifies the instance a control session is bound to. VMID and
// Generation come from the host's records, never from the guest.
type Session struct {
	VMID       string
	Generation uint64
	// Secrets are candidate per-generation secrets; the guest's proof must
	// verify under one of them. Ignored when Bootstrap is set.
	Secrets [][]byte
	// Bootstrap, when set, provisions this generation's first secret in Hello
	// (first session after cold boot or restore). The proof is then verified
	// under it.
	Bootstrap []byte
}

// Client is one authenticated control session.
type Client struct {
	conn net.Conn
	pc   *guestproto.Conn
	ack  guestproto.HelloAck
	// secret is the candidate the guest's proof verified under.
	secret []byte
	ids    atomic.Uint64
	done   chan struct{}
	close  sync.Once

	mu      sync.Mutex
	err     error // first fatal session error
	pending map[uint64]chan *guestproto.Frame
	streams map[uint64]chan *guestproto.Frame
}

// streamQueue bounds frames buffered per stream before the reader blocks.
const streamQueue = 64

// Connect dials, performs the handshake and returns a session. On any error
// the connection is closed.
func Connect(ctx context.Context, dial DialFunc, s Session) (*Client, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("guestclient: dial: %w", err)
	}
	c, err := Handshake(ctx, conn, s)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// Handshake runs Hello/HelloAck over an established connection and starts
// the session reader. The caller keeps ownership of conn on error.
func Handshake(ctx context.Context, conn net.Conn, s Session) (*Client, error) {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	pc, ack, proved, err := handshake(ctx, conn, s)
	if err != nil {
		return nil, err
	}
	if !stop() {
		return nil, ctx.Err()
	}
	c := &Client{
		conn: conn, pc: pc, ack: ack, secret: proved, done: make(chan struct{}),
		pending: map[uint64]chan *guestproto.Frame{}, streams: map[uint64]chan *guestproto.Frame{},
	}
	c.ids.Store(1)
	go c.readLoop()
	return c, nil
}

// handshake sends Hello (request id 1) and verifies the HelloAck. It
// returns the framed connection at the negotiated version, the ack and the
// secret the proof verified under, without starting a reader. The caller
// bounds it by ctx (closing conn when ctx ends).
func handshake(ctx context.Context, conn net.Conn, s Session) (*guestproto.Conn, guestproto.HelloAck, []byte, error) {
	var none guestproto.HelloAck
	hello := guestproto.Hello{
		Versions: guestproto.SupportedVersions, Features: Features, VMID: s.VMID, Generation: s.Generation,
		Challenge: guestproto.NewChallenge(), BootstrapSecret: s.Bootstrap,
	}
	if err := hello.Validate(); err != nil {
		return nil, none, nil, err
	}
	secrets := s.Secrets
	if s.Bootstrap != nil {
		secrets = [][]byte{s.Bootstrap}
	}
	if len(secrets) == 0 {
		return nil, none, nil, guestproto.Errorf(guestproto.CodeUnauthenticated, "no secret for %s generation %d", s.VMID, s.Generation)
	}

	pc := guestproto.NewConn(conn)
	if err := pc.Send(1, hello); err != nil {
		return nil, none, nil, ctxErr(ctx, err)
	}
	f, err := pc.ReadFrame()
	if err != nil {
		return nil, none, nil, ctxErr(ctx, err)
	}
	if f.ID != 1 {
		return nil, none, nil, guestproto.Errorf(guestproto.CodeUnexpectedType, "hello answered with id %d", f.ID)
	}
	if f.Error != nil {
		return nil, none, nil, fmt.Errorf("%w: %w", ErrRejected, f.Error)
	}
	ack, err := guestproto.Decode[guestproto.HelloAck](f)
	if err != nil {
		return nil, none, nil, err
	}
	var verr error
	var proved []byte
	for _, sec := range secrets {
		if verr = guestproto.VerifyHelloAck(sec, hello, ack); verr == nil {
			proved = sec
			break
		}
	}
	if verr != nil {
		return nil, none, nil, verr
	}
	pc.SetVersion(ack.Version)
	return pc, ack, proved, nil
}

// DialTCP opens a byte stream to 127.0.0.1:port inside the guest over a
// fresh, dedicated agent session: it dials, authenticates like Connect,
// requires feature tcp, sends DialTCP and reads exactly its answer. The
// returned conn then carries raw bytes (no frames); closing it ends the
// session. ctx bounds only the dial and setup, as with net.Dialer. A guest
// refusal keeps its *guestproto.Error (e.g. not_found when nothing listens).
// On any error the connection is closed.
func DialTCP(ctx context.Context, dial DialFunc, s Session, port uint16) (net.Conn, error) {
	req := guestproto.DialTCP{Port: port}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	conn, err := rawSession(ctx, dial, s, guestproto.FeatureTCP, req)
	if err != nil {
		return nil, fmt.Errorf("guestclient: tcp dial port %d: %w", port, err)
	}
	return conn, nil
}

// rawSession dials a dedicated session, requires feature, sends req (a
// request that turns the session into a raw byte stream once acknowledged)
// and returns the connection after its Ack. On any error the connection is
// closed.
func rawSession(ctx context.Context, dial DialFunc, s Session, feature string, req guestproto.Message) (net.Conn, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("guestclient: dial: %w", err)
	}
	c, err := rawRequest(ctx, conn, s, feature, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func rawRequest(ctx context.Context, conn net.Conn, s Session, feature string, req guestproto.Message) (net.Conn, error) {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	pc, ack, _, err := handshake(ctx, conn, s)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(ack.Features, feature) {
		return nil, guestproto.Errorf(guestproto.CodeUnsupported, "guest agent %s does not support %s", ack.AgentVersion, feature)
	}
	// Request id 2 follows Hello's id 1. Nothing else is written until the
	// answer is read: after an Ack the connection is the raw stream, and
	// Conn.ReadFrame consumes exactly one frame (no read-ahead).
	if err := pc.Send(2, req); err != nil {
		return nil, ctxErr(ctx, err)
	}
	f, err := pc.ReadFrame()
	if err != nil {
		return nil, ctxErr(ctx, err)
	}
	if f.ID != 2 || f.V != ack.Version {
		return nil, guestproto.Errorf(guestproto.CodeUnexpectedType, "%s answered with id %d version %d", req.MsgType(), f.ID, f.V)
	}
	if _, err := guestproto.Decode[guestproto.Ack](f); err != nil {
		return nil, err
	}
	if !stop() {
		return nil, ctx.Err()
	}
	return conn, nil
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Features are offered in every Hello; the session uses the intersection.
var Features = []string{guestproto.FeatureExec, guestproto.FeatureFiles, guestproto.FeatureIdentity, guestproto.FeatureFilesTar,
	guestproto.FeatureTCP, guestproto.FeatureJobs, guestproto.FeaturePS, guestproto.FeatureListen, guestproto.FeatureWorkspace}

// Has reports whether the session negotiated feature.
func (c *Client) Has(feature string) bool { return slices.Contains(c.ack.Features, feature) }

// AgentVersion is the agent's self-reported build version (informational).
func (c *Client) AgentVersion() string { return c.ack.AgentVersion }

// Version is the negotiated protocol version.
func (c *Client) Version() uint16 { return c.ack.Version }

// Close ends the session. Non-detached processes started on it are killed
// by the agent.
func (c *Client) Close() error {
	c.fail(ErrClosed)
	return nil
}

// Done is closed when the session ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the session ended, or nil while it is live.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) fail(err error) {
	c.close.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		c.conn.Close()
		close(c.done)
	})
}

func (c *Client) readLoop() {
	for {
		f, err := c.pc.ReadFrame()
		if err != nil {
			c.fail(fmt.Errorf("%w: %v", ErrClosed, err))
			return
		}
		if f.V != c.ack.Version {
			c.fail(guestproto.Errorf(guestproto.CodeMalformedFrame, "frame version %d, negotiated %d", f.V, c.ack.Version))
			return
		}
		var ch chan *guestproto.Frame
		c.mu.Lock()
		switch {
		case f.ID != 0:
			ch = c.pending[f.ID]
			delete(c.pending, f.ID)
		case f.Stream != 0:
			ch = c.streams[f.Stream]
		}
		c.mu.Unlock()
		if ch == nil {
			continue // late frame for an abandoned request or stream
		}
		select {
		case ch <- f:
		case <-c.done:
			return
		}
	}
}

// call is a request whose response, and optionally stream, are registered
// before the request is written so no frame can arrive unrouted.
type call struct {
	c      *Client
	id     uint64
	resp   chan *guestproto.Frame
	stream chan *guestproto.Frame
}

func (c *Client) start(body guestproto.Message, withStream bool) (*call, error) {
	return c.startOn(body, 0, withStream)
}

// startOn is start for a request that also names an existing stream (the
// Stream member), e.g. FileCommit finishing a WriteFile.
func (c *Client) startOn(body guestproto.Message, stream uint64, withStream bool) (*call, error) {
	f, err := guestproto.Encode(0, body)
	if err != nil {
		return nil, err
	}
	cl := &call{c: c, id: c.ids.Add(1), resp: make(chan *guestproto.Frame, 1)}
	f.ID, f.Stream, f.V = cl.id, stream, c.ack.Version
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.pending[cl.id] = cl.resp
	if withStream {
		cl.stream = make(chan *guestproto.Frame, streamQueue)
		c.streams[cl.id] = cl.stream
	}
	c.mu.Unlock()
	if err := c.pc.WriteFrame(f); err != nil {
		cl.release()
		c.fail(fmt.Errorf("%w: %v", ErrClosed, err))
		return nil, err
	}
	return cl, nil
}

func (cl *call) release() {
	cl.c.mu.Lock()
	delete(cl.c.pending, cl.id)
	delete(cl.c.streams, cl.id)
	cl.c.mu.Unlock()
}

func (cl *call) wait(ctx context.Context) (*guestproto.Frame, error) {
	select {
	case f := <-cl.resp:
		return f, nil
	case <-cl.c.done:
		return nil, cl.c.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// first returns the first frame of a request answered on its own stream
// (Listen, WorkspacePrepare): a stream frame, or the error response that
// refuses the request.
func (cl *call) first(ctx context.Context) (*guestproto.Frame, error) {
	select {
	case f := <-cl.stream:
		return f, nil
	case f := <-cl.resp:
		if f.Error != nil {
			return nil, f.Error
		}
		return nil, guestproto.Errorf(guestproto.CodeUnexpectedType, "%s answered with %q", f.Type, f.Type)
	case <-cl.c.done:
		return nil, cl.c.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// require refuses a request the session did not negotiate.
func (c *Client) require(feature string) error {
	if c.Has(feature) {
		return nil
	}
	return guestproto.Errorf(guestproto.CodeUnsupported, "guest agent %s does not support %s", c.ack.AgentVersion, feature)
}

// next returns the next stream frame.
func (cl *call) next(ctx context.Context) (*guestproto.Frame, error) {
	select {
	case f := <-cl.stream:
		return f, nil
	case <-cl.c.done:
		return nil, cl.c.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// roundTrip sends req and decodes a response of type T.
func roundTrip[T guestproto.Message](ctx context.Context, c *Client, req guestproto.Message) (T, error) {
	var zero T
	cl, err := c.start(req, false)
	if err != nil {
		return zero, err
	}
	defer cl.release()
	f, err := cl.wait(ctx)
	if err != nil {
		return zero, err
	}
	return guestproto.Decode[T](f)
}

// Ping checks liveness.
func (c *Client) Ping(ctx context.Context) error {
	_, err := roundTrip[guestproto.Pong](ctx, c, guestproto.Ping{})
	return err
}

// Readiness probes the agent; services optionally names extra units.
func (c *Client) Readiness(ctx context.Context, services []string) (guestproto.Readiness, error) {
	return roundTrip[guestproto.Readiness](ctx, c, guestproto.ReadinessProbe{Services: services})
}

// SetIdentity runs barrier steps 2-4 and returns the guest's report.
func (c *Client) SetIdentity(ctx context.Context, id guestproto.SetIdentity) (guestproto.IdentityResult, error) {
	return roundTrip[guestproto.IdentityResult](ctx, c, id)
}

// Signal signals a process group.
func (c *Client) Signal(ctx context.Context, processID, signal string) error {
	_, err := roundTrip[guestproto.Ack](ctx, c, guestproto.ProcessSignal{ProcessID: processID, Signal: signal})
	return err
}

// Status returns a process's status.
func (c *Client) Status(ctx context.Context, processID string) (guestproto.ProcessStatus, error) {
	return roundTrip[guestproto.ProcessStatus](ctx, c, guestproto.ProcessStatusRequest{ProcessID: processID})
}

// Shutdown asks the agent to power off the guest after grace seconds.
func (c *Client) Shutdown(ctx context.Context, graceSeconds int64) error {
	_, err := roundTrip[guestproto.Ack](ctx, c, guestproto.Shutdown{GraceSeconds: graceSeconds})
	return err
}
