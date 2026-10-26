package guestexec

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
	"davit/internal/runtime"
)

// Tunnel limits. An opened tunnel holds a live guest connection, so one that
// is never attached is closed after TunnelAttachWindow, and the number of
// unattached tunnels is bounded.
const (
	TunnelAttachWindow = 30 * time.Second
	maxPendingTunnels  = 256
	// tunnelKeyRetention is how long an Idempotency-Key keeps answering with
	// the tunnel it opened. Tunnels are ephemeral, so keys live in memory.
	tunnelKeyRetention = 10 * time.Minute
)

// tunnelKey scopes an Idempotency-Key to its principal.
type tunnelKey struct {
	principal domain.PrincipalID
	key       string
}

// keyedTunnel is the result an Idempotency-Key replays; t is zero while the
// first request is still dialing.
type keyedTunnel struct {
	request string // ref + port of the first request
	t       api.Tunnel
	at      time.Time
}

// tunnel is an opened guest TCP connection waiting for its single attach.
type tunnel struct {
	owner domain.PrincipalID
	t     api.Tunnel
	conn  net.Conn
	timer *time.Timer

	once sync.Once
}

func (t *tunnel) stop() {
	t.once.Do(func() {
		if t.timer != nil {
			t.timer.Stop()
		}
		t.conn.Close()
	})
}

// OpenTunnel connects to port on ref's guest loopback (guestproto.DialTCP on
// a dedicated agent session) and holds the connection until AttachTunnel
// claims it or the attach window passes. With an idempotency key, a replay
// of the same request returns the tunnel the first one opened (it may have
// been attached or expired since) without dialing again.
func (m *Manager) OpenTunnel(ctx context.Context, principal domain.PrincipalID, ref string, port int, key string) (api.Tunnel, error) {
	if key == "" {
		return m.openTunnel(ctx, principal, ref, port)
	}
	k, request := tunnelKey{principal, key}, ref+"\x00"+strconv.Itoa(port)
	m.mu.Lock()
	now := m.cfg.Now()
	for kk, v := range m.tunnelKeys {
		if now.Sub(v.at) > tunnelKeyRetention {
			delete(m.tunnelKeys, kk)
		}
	}
	if prev, ok := m.tunnelKeys[k]; ok {
		m.mu.Unlock()
		switch {
		case prev.request != request:
			return api.Tunnel{}, domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key %q was used for a different tunnel request", key)
		case prev.t.ID == "":
			return api.Tunnel{}, domain.Errorf(domain.CodeOperationInProgress, "a tunnel request with Idempotency-Key %q is still opening", key)
		}
		return prev.t, nil
	}
	m.tunnelKeys[k] = keyedTunnel{request: request, at: now}
	m.mu.Unlock()

	t, err := m.openTunnel(ctx, principal, ref, port)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		delete(m.tunnelKeys, k) // nothing was opened; a retry may try again
		return api.Tunnel{}, err
	}
	m.tunnelKeys[k] = keyedTunnel{request: request, t: t, at: m.cfg.Now()}
	return t, nil
}

func (m *Manager) openTunnel(ctx context.Context, principal domain.PrincipalID, ref string, port int) (api.Tunnel, error) {
	if port < 1 || port > 65535 {
		return api.Tunnel{}, domain.Errorf(domain.CodeInvalidArgument, "port must be 1..65535").WithDetail("field", "port")
	}
	m.mu.Lock()
	full := len(m.tunnels) >= maxPendingTunnels
	m.mu.Unlock()
	if full {
		return api.Tunnel{}, domain.Errorf(domain.CodeResourceExhausted, "too many unattached tunnels")
	}
	conn, key, err := m.DialTCP(ctx, principal, ref, port)
	if err != nil {
		return api.Tunnel{}, err
	}
	now := m.cfg.Now().UTC()
	tn := &tunnel{owner: principal, conn: conn, t: api.Tunnel{
		ID: domain.NewID("tun"), VMID: key.VMID, Generation: key.Generation, Port: port,
		CreatedAt: now, ExpiresAt: now.Add(TunnelAttachWindow),
	}}
	m.mu.Lock()
	defer m.mu.Unlock()
	tn.timer = time.AfterFunc(TunnelAttachWindow, func() {
		m.mu.Lock()
		if m.tunnels[tn.t.ID] == tn {
			delete(m.tunnels, tn.t.ID)
		}
		m.mu.Unlock()
		tn.stop()
	})
	m.tunnels[tn.t.ID] = tn
	return tn.t, nil
}

// DialTCP connects to port on ref's guest loopback through the guest agent
// and returns the connection with the instance it reached. The VM must be
// ready or running.
func (m *Manager) DialTCP(ctx context.Context, principal domain.PrincipalID, ref string, port int) (net.Conn, runtime.Key, error) {
	if port < 1 || port > 65535 {
		return nil, runtime.Key{}, domain.Errorf(domain.CodeInvalidArgument, "port must be 1..65535").WithDetail("field", "port")
	}
	t, err := m.cfg.Targets.GuestTarget(ctx, principal, ref)
	if err != nil {
		return nil, runtime.Key{}, err
	}
	dial, sess, err := m.route(ctx, t)
	if err != nil {
		return nil, runtime.Key{}, err
	}
	conn, err := guestclient.DialTCP(ctx, dial, sess, uint16(port))
	if err != nil {
		return nil, runtime.Key{}, tunnelErr(port, err)
	}
	return conn, t.Key, nil
}

// claim removes principal's tunnel id from the pending set. Another
// principal's tunnel is reported as not found.
func (m *Manager) claim(principal domain.PrincipalID, id string) (*tunnel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tn := m.tunnels[id]
	if tn == nil || tn.owner != principal {
		return nil, domain.Errorf(domain.CodeNotFound, "tunnel %q not found (tunnels are single use and expire %v after opening)", id, TunnelAttachWindow)
	}
	delete(m.tunnels, id)
	tn.timer.Stop()
	return tn, nil
}

// AttachTunnel claims the tunnel; the caller owns and must close the
// returned connection.
func (m *Manager) AttachTunnel(principal domain.PrincipalID, id string) (net.Conn, api.Tunnel, error) {
	tn, err := m.claim(principal, id)
	if err != nil {
		return nil, api.Tunnel{}, err
	}
	// An attached tunnel (an ssh session, a forward) keeps its VM busy.
	done := m.busy(tn.t.VMID)
	return &busyConn{Conn: tn.conn, done: done}, tn.t, nil
}

// busyConn releases its VM's busy hold on the first Close.
type busyConn struct {
	net.Conn
	once sync.Once
	done func()
}

func (c *busyConn) Close() error {
	c.once.Do(c.done)
	return c.Conn.Close()
}

// CloseWrite half-closes when the underlying connection can.
func (c *busyConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// CloseTunnel closes an unattached tunnel. An attached tunnel ends when its
// stream closes, so it is no longer found here.
func (m *Manager) CloseTunnel(principal domain.PrincipalID, id string) error {
	tn, err := m.claim(principal, id)
	if err != nil {
		return err
	}
	tn.stop()
	return nil
}

// tunnelErr maps a tcp.dial failure. A port nobody listens on (the guest
// answers not_found) is unavailable, not a missing resource: the VM exists
// and the service may simply not be up yet.
func tunnelErr(port int, err error) error {
	if errors.Is(err, guestproto.Errorf(guestproto.CodeNotFound, "")) {
		return domain.Errorf(domain.CodeUnavailable, "nothing is listening on guest port %d", port).WithDetail("port", port)
	}
	return protoErr(err)
}

// TunnelVM returns the VM of principal's pending tunnel id (for scoping
// tunnel endpoints to a VM).
func (m *Manager) TunnelVM(principal domain.PrincipalID, id string) (domain.VMID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tn := m.tunnels[id]
	if tn == nil || tn.owner != principal {
		return "", domain.Errorf(domain.CodeNotFound, "tunnel %q not found", id)
	}
	return tn.t.VMID, nil
}
