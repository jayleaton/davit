package helperproto

import (
	"context"
	"errors"
	"net"
	"os"
	"time"
)

// Client is one control connection to a helper. It is not safe for
// concurrent calls: one request is in flight at a time, as the protocol
// requires; open another connection for concurrent work.
type Client struct {
	conn    net.Conn
	version uint16
	next    uint64
	// Hello is the helper's identity from the handshake.
	Hello HelloResult
}

// dialUnix connects to a helper socket and checks that its peer runs as
// this user: same-user host processes are the trust boundary.
func dialUnix(ctx context.Context, socket string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	uid, err := PeerUID(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if uid != os.Getuid() {
		conn.Close()
		return nil, Errorf(CodeUnauthorized, "helper socket %s is served by uid %d, not %d", socket, uid, os.Getuid())
	}
	return conn, nil
}

// handshake sends Hello and checks the reply against want (the helper.json
// incarnation; empty skips the check, for a helper not yet recorded).
func handshake(ctx context.Context, conn net.Conn, client, want string) (HelloResult, uint16, error) {
	deadline(ctx, conn)
	defer conn.SetDeadline(time.Time{})
	f, err := NewFrame(MinProtocolVersion, 1, TypeHello, Hello{MinVersion: MinProtocolVersion, MaxVersion: ProtocolVersion, Client: client})
	if err != nil {
		return HelloResult{}, 0, err
	}
	if err := WriteFrame(conn, f); err != nil {
		return HelloResult{}, 0, err
	}
	r, err := ReadFrame(conn)
	if err != nil {
		return HelloResult{}, 0, err
	}
	if r.Error != nil {
		return HelloResult{}, 0, r.Error
	}
	var hr HelloResult
	if r.Type != TypeHello || r.ID != 1 {
		return hr, 0, Errorf(CodeMalformed, "expected hello reply, got %s/%d", r.Type, r.ID)
	}
	if err := r.Decode(&hr); err != nil {
		return hr, 0, err
	}
	if hr.Version < MinProtocolVersion || hr.Version > ProtocolVersion {
		return hr, 0, Errorf(CodeVersion, "helper chose version %d", hr.Version)
	}
	if want != "" && hr.Incarnation != want {
		return hr, 0, Errorf(CodeUnauthorized, "helper incarnation %s, recorded %s", hr.Incarnation, want)
	}
	return hr, hr.Version, nil
}

// Open connects to a helper's control socket and runs the handshake. want
// is the incarnation from helper.json ("" before it is known).
func Open(ctx context.Context, socket, client, want string) (*Client, error) {
	conn, err := dialUnix(ctx, socket)
	if err != nil {
		return nil, err
	}
	hr, v, err := handshake(ctx, conn, client, want)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &Client{conn: conn, version: v, next: 1, Hello: hr}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

func deadline(ctx context.Context, conn net.Conn) {
	if d, ok := ctx.Deadline(); ok {
		conn.SetDeadline(d)
	}
}

// Call sends one request and decodes its reply into resp (nil to ignore).
// Context cancellation closes the connection (it is then unusable).
func (c *Client) Call(ctx context.Context, t MsgType, req, resp any) error {
	c.next++
	f, err := NewFrame(c.version, c.next, t, req)
	if err != nil {
		return err
	}
	deadline(ctx, c.conn)
	defer c.conn.SetDeadline(time.Time{})
	stop := context.AfterFunc(ctx, func() { c.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if err := WriteFrame(c.conn, f); err != nil {
		return c.ctxErr(ctx, err)
	}
	r, err := ReadFrame(c.conn)
	if err != nil {
		return c.ctxErr(ctx, err)
	}
	if r.ID != c.next || r.Type != t {
		return Errorf(CodeMalformed, "reply %s/%d to %s/%d", r.Type, r.ID, t, c.next)
	}
	if r.Error != nil {
		return r.Error
	}
	if resp == nil {
		return nil
	}
	return r.Decode(resp)
}

func (c *Client) ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return context.DeadlineExceeded
	}
	return err
}

// Watch turns the connection into an event stream and calls fn with each
// event until fn returns false, the context ends or the helper closes the
// stream (after a terminal state). The client is unusable afterwards.
func (c *Client) Watch(ctx context.Context, fn func(StateResult) bool) error {
	c.next++
	id := c.next
	f, _ := NewFrame(c.version, id, TypeWatch, nil)
	stop := context.AfterFunc(ctx, func() { c.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if err := WriteFrame(c.conn, f); err != nil {
		return c.ctxErr(ctx, err)
	}
	var seq uint64
	for {
		r, err := ReadFrame(c.conn)
		if err != nil {
			return c.ctxErr(ctx, err)
		}
		if r.Error != nil {
			return r.Error
		}
		if r.ID != id || r.Type != TypeEvent || r.Seq != seq+1 {
			return Errorf(CodeMalformed, "event %s/%d seq %d after %d", r.Type, r.ID, r.Seq, seq)
		}
		seq = r.Seq
		var ev StateResult
		if err := r.Decode(&ev); err != nil {
			return err
		}
		if !fn(ev) {
			return nil
		}
	}
}

// DialGuest opens a raw stream to the guest's vsock port through the
// helper's stream socket.
func DialGuest(ctx context.Context, socket, client, want string, port uint32) (net.Conn, error) {
	conn, err := dialUnix(ctx, socket)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}
	_, v, err := handshake(ctx, conn, client, want)
	if err != nil {
		return fail(err)
	}
	ms := int64(10_000)
	if d, ok := ctx.Deadline(); ok {
		ms = max(1, time.Until(d).Milliseconds())
	}
	f, err := NewFrame(v, 2, TypeDial, Dial{Port: port, TimeoutMillis: ms})
	if err != nil {
		return fail(err)
	}
	deadline(ctx, conn)
	if err := WriteFrame(conn, f); err != nil {
		return fail(err)
	}
	r, err := ReadFrame(conn)
	if err != nil {
		return fail(err)
	}
	if r.Error != nil {
		return fail(r.Error)
	}
	if r.ID != 2 || r.Type != TypeDial {
		return fail(Errorf(CodeMalformed, "reply %s/%d to dial", r.Type, r.ID))
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// Reply writes the reply to req: body on success, or err as an error frame.
// Servers (the Swift helper, the Go test fake) answer every request with it.
func Reply(conn net.Conn, version uint16, req *Frame, body any, err error) error {
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = Errorf(CodeInternal, "%v", err)
		}
		return WriteFrame(conn, &Frame{V: version, ID: req.ID, Type: req.Type, Error: e})
	}
	f, ferr := NewFrame(version, req.ID, req.Type, body)
	if ferr != nil {
		return ferr
	}
	return WriteFrame(conn, f)
}
