package guestclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"davit/internal/guestproto"
)

// Listener timing and bounds.
const (
	// reverseTimeout bounds one ReverseConnect (dial, handshake, claim).
	reverseTimeout = 10 * time.Second
	// unlistenTimeout bounds Close's Unlisten; the session closes anyway.
	unlistenTimeout = 2 * time.Second
	// announceQueue bounds announced connections not yet taken by Accept.
	// The agent bounds its unclaimed connections too (and expires them),
	// so on overflow the oldest announcement is dropped: it is the one
	// most likely to have expired already.
	announceQueue = 64
)

// Listener is a listener on guest 127.0.0.1 (feature listen) whose accepted
// connections reach the host; it implements net.Listener. It holds its own
// control session, so connection announcements never queue behind, or
// stall, another session's traffic. Accept claims each announced connection
// with ReverseConnect on a fresh session and returns it as a raw,
// half-close-aware byte stream. Close (or the session failing, e.g. on a new
// generation) closes the guest listener and every unclaimed connection.
type Listener struct {
	c    *Client
	dial DialFunc
	sess Session
	id   string
	port int

	announced chan string // connection IDs, oldest first
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{} // closed when the stream ended
	err       error         // why, once done is closed
}

// Listen opens a listener on guest 127.0.0.1:port (port 0 lets the agent
// pick) over a new session to the agent. ctx bounds only the setup.
func Listen(ctx context.Context, dial DialFunc, s Session, port int) (*Listener, error) {
	req := guestproto.Listen{Port: port}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	c, err := Connect(ctx, dial, s)
	if err != nil {
		return nil, err
	}
	l, err := listenOn(ctx, c, dial, s, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	return l, nil
}

func listenOn(ctx context.Context, c *Client, dial DialFunc, s Session, req guestproto.Listen) (*Listener, error) {
	if err := c.require(guestproto.FeatureListen); err != nil {
		return nil, err
	}
	cl, err := c.start(req, true)
	if err != nil {
		return nil, err
	}
	var seqs guestproto.SeqTracker
	f, err := cl.first(ctx)
	if err == nil {
		_, err = seqs.Accept(f)
	}
	var started guestproto.ListenStarted
	if err == nil {
		started, err = guestproto.Decode[guestproto.ListenStarted](f)
	}
	if err != nil {
		cl.release()
		return nil, fmt.Errorf("guestclient: listen on guest port %d: %w", req.Port, err)
	}
	lctx, cancel := context.WithCancel(context.Background())
	l := &Listener{c: c, dial: dial, sess: s, id: started.ListenerID, port: started.Port,
		announced: make(chan string, announceQueue), ctx: lctx, cancel: cancel, done: make(chan struct{})}
	go l.drain(cl, &seqs)
	return l, nil
}

// drain receives announcements until the stream or session ends.
func (l *Listener) drain(cl *call, seqs *guestproto.SeqTracker) {
	defer cl.release()
	err := func() error {
		for {
			f, err := cl.next(l.ctx)
			if err != nil {
				return err
			}
			if dup, err := seqs.Accept(f); err != nil {
				return err
			} else if dup {
				continue
			}
			a, err := guestproto.Decode[guestproto.ListenAccepted](f)
			if err != nil {
				return err
			}
			for {
				select {
				case l.announced <- a.ConnID:
				default:
					select {
					case <-l.announced: // drop the oldest
					default:
					}
					continue
				}
				break
			}
		}
	}()
	l.err = err
	close(l.done)
	l.c.Close()
}

// ID is the agent's listener ID.
func (l *Listener) ID() string { return l.id }

// Port is the bound guest port.
func (l *Listener) Port() int { return l.port }

// Addr is the guest address the listener is bound to.
func (l *Listener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: l.port} }

// Accept waits for the next guest connection and claims it. A connection
// that expired before it was claimed is skipped. After Close it returns
// net.ErrClosed.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		var connID string
		select {
		case connID = <-l.announced:
		case <-l.done:
			if l.ctx.Err() != nil || errors.Is(l.err, ErrClosed) {
				return nil, net.ErrClosed
			}
			return nil, fmt.Errorf("guestclient: guest listener %s ended: %w", l.id, l.err)
		}
		ctx, cancel := context.WithTimeout(l.ctx, reverseTimeout)
		conn, err := ReverseConnect(ctx, l.dial, l.sess, l.id, connID)
		cancel()
		switch {
		case err == nil:
			return conn, nil
		case l.ctx.Err() != nil:
			return nil, net.ErrClosed
		case errors.Is(err, guestproto.Errorf(guestproto.CodeNotFound, "")):
			continue // expired or dropped by the agent meanwhile
		}
		return nil, err
	}
}

// Close unlistens and ends the listener's session. Connections already
// accepted stay open.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), unlistenTimeout)
		_, _ = roundTrip[guestproto.Ack](ctx, l.c, guestproto.Unlisten{ListenerID: l.id})
		cancel()
		l.cancel()
		l.c.Close()
		<-l.done
	})
	return nil
}

// ReverseConnect claims accepted connection connID of guest listener
// listenerID (feature listen) over a fresh, dedicated session and returns it
// as a raw byte stream, like DialTCP. A connection that was already claimed,
// expired or closed is a CodeNotFound error. On any error the connection is
// closed.
func ReverseConnect(ctx context.Context, dial DialFunc, s Session, listenerID, connID string) (net.Conn, error) {
	req := guestproto.ReverseConnect{ListenerID: listenerID, ConnID: connID}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	conn, err := rawSession(ctx, dial, s, guestproto.FeatureListen, req)
	if err != nil {
		return nil, fmt.Errorf("guestclient: reverse connect %s: %w", connID, err)
	}
	return conn, nil
}
