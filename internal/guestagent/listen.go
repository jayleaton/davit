//go:build linux

package guestagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"strconv"
	"sync"
	"time"

	"davit/internal/guestproto"
)

// Reverse connections (feature listen). A Listen opens a loopback listener
// owned by its session: ListenStarted, then one ListenAccepted per accepted
// connection, on the Listen request's stream. An accepted connection waits
// for the host's ReverseConnect (on a fresh session of the same generation)
// for at most ListenClaimWindow and is then closed. At most ListenPending
// connections wait per listener; the listener stops accepting while that
// many do, so further guest connections queue in the kernel backlog
// (backpressure) instead of piling up in the agent.
//
// Unlisten, the owning session closing, or a new generation closes the
// listener and every unclaimed connection; claimed connections live on
// their own sessions.

const (
	defaultListenPending     = 32
	defaultListenClaimWindow = 30 * time.Second
)

type listener struct {
	id     string
	owner  *session
	stream uint64
	ln     net.Listener
	slots  chan struct{} // one token per unclaimed connection
	quit   chan struct{}
	done   chan struct{} // closed when the accept loop returned

	// sendMu is held while a frame is written and by close, so no frame
	// follows close (Unlisten's Ack).
	sendMu sync.Mutex
	seq    uint64

	mu      sync.Mutex
	closed  bool
	pending map[string]*pendingConn
}

type pendingConn struct {
	c     net.Conn
	timer *time.Timer
}

func newListenerID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}

// listen opens the listener and starts its stream.
func (s *session) listen(id uint64, req guestproto.Listen) {
	ln, err := s.a.cfg.Listen(s.ctx, req.Port)
	if err != nil {
		s.fail(id, guestproto.Errorf(guestproto.CodeInvalidArgument, "listen on 127.0.0.1:%d: %v", req.Port, unwrapExecErr(err)))
		return
	}
	port := req.Port
	if a, ok := ln.Addr().(*net.TCPAddr); ok {
		port = a.Port
	} else if _, p, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		port, _ = strconv.Atoi(p)
	}
	l := &listener{id: newListenerID("lst"), owner: s, stream: id, ln: ln,
		slots: make(chan struct{}, s.a.cfg.ListenPending), quit: make(chan struct{}), done: make(chan struct{}),
		pending: map[string]*pendingConn{}}
	s.a.lmu.Lock()
	s.a.listeners[l.id] = l
	s.a.lmu.Unlock()
	if !l.send(guestproto.ListenStarted{ListenerID: l.id, Port: port}) {
		l.close(s.a)
		return
	}
	go l.acceptLoop(s.a)
}

// send writes a frame on the listener's stream; false means it is closed or
// the session is gone. A failed write closes the session only after sendMu
// is released: closing it closes this listener, which takes sendMu.
func (l *listener) send(m guestproto.Message) bool {
	l.sendMu.Lock()
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		l.sendMu.Unlock()
		return false
	}
	l.seq++
	f, err := guestproto.EncodeStream(l.stream, l.seq, m)
	if err == nil {
		f.V = l.owner.pc.Version()
		err = l.owner.pc.WriteFrame(f)
	}
	l.sendMu.Unlock()
	if err != nil {
		l.owner.close()
		return false
	}
	return true
}

func (l *listener) acceptLoop(a *Agent) {
	defer close(l.done)
	defer l.close(a)
	for {
		select {
		case l.slots <- struct{}{}:
		case <-l.quit:
			return
		}
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		connID := newListenerID("conn")
		pc := &pendingConn{c: c}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			c.Close()
			return
		}
		l.pending[connID] = pc
		pc.timer = time.AfterFunc(a.cfg.ListenClaimWindow, func() {
			if l.take(connID) != nil {
				c.Close()
			}
		})
		l.mu.Unlock()
		if !l.send(guestproto.ListenAccepted{ListenerID: l.id, ConnID: connID}) {
			return
		}
	}
}

// take removes an unclaimed connection and frees its slot.
func (l *listener) take(connID string) net.Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	pc := l.pending[connID]
	if pc == nil {
		return nil
	}
	delete(l.pending, connID)
	pc.timer.Stop()
	<-l.slots
	return pc.c
}

// close stops the listener and drops its unclaimed connections. After it
// returns the stream carries no more frames.
func (l *listener) close(a *Agent) {
	l.sendMu.Lock()
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		l.sendMu.Unlock()
		return
	}
	l.closed = true
	close(l.quit)
	pending := l.pending
	l.pending = map[string]*pendingConn{}
	l.mu.Unlock()
	l.sendMu.Unlock()
	l.ln.Close()
	for _, pc := range pending {
		pc.timer.Stop()
		pc.c.Close()
	}
	a.lmu.Lock()
	if a.listeners[l.id] == l {
		delete(a.listeners, l.id)
	}
	a.lmu.Unlock()
}

// listenerFor returns listener id if it belongs to s's VM generation.
func (s *session) listenerFor(id string) *listener {
	s.a.lmu.Lock()
	defer s.a.lmu.Unlock()
	l := s.a.listeners[id]
	if l == nil || l.owner.vmID != s.vmID || l.owner.gen != s.gen {
		return nil
	}
	return l
}

func (s *session) unlisten(id uint64, req guestproto.Unlisten) {
	l := s.listenerFor(req.ListenerID)
	if l == nil {
		s.fail(id, guestproto.Errorf(guestproto.CodeNotFound, "no listener %s", req.ListenerID))
		return
	}
	l.close(s.a)
	s.send(id, guestproto.Ack{})
}

// closeListeners closes the listeners s owns (s is closing).
func (a *Agent) closeListeners(s *session) {
	a.lmu.Lock()
	var mine []*listener
	for _, l := range a.listeners {
		if l.owner == s {
			mine = append(mine, l)
		}
	}
	a.lmu.Unlock()
	for _, l := range mine {
		l.close(a)
	}
}

// reverseConnect claims an accepted connection and sends the Ack; true means
// the session is now a raw byte stream to it (see dialTCP for the rules).
func (s *session) reverseConnect(id uint64, req guestproto.ReverseConnect) bool {
	if s.busy() {
		s.fail(id, guestproto.Errorf(guestproto.CodeInvalidArgument, "tcp.reverse must be the only request on its session"))
		return false
	}
	l := s.listenerFor(req.ListenerID)
	var c net.Conn
	if l != nil {
		c = l.take(req.ConnID)
	}
	if c == nil {
		s.fail(id, guestproto.Errorf(guestproto.CodeNotFound, "no pending connection %s on listener %s", req.ConnID, req.ListenerID))
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

// listenLoopback is the default Config.Listen.
func listenLoopback(ctx context.Context, port int) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
}
