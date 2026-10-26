// Package reverse is the host side of reverse connections ("davit forward
// --reverse"): a service on the user's laptop, or an operator-permitted
// service on the host, becomes reachable from inside a VM.
//
// Model: for each Grant the guest agent listens on a guest loopback port
// (GuestListen) and hands every connection a guest process makes to it to
// the host as a stream (GuestListener.Accept). The Manager dials the grant's
// Target for each such stream and splices the two, with half-close.
//
// Trust boundary: guests are untrusted. A Grant is the only way a guest
// reaches a laptop or host service, and the guest never chooses the far end:
// the Target is fixed when the grant is opened. A host Target exists only for
// addresses the operator listed in Policy. Egress policy (internal/network)
// is unrelated, because this path never uses the guest network; it rides the
// same host<->guest control transport as exec and forwards. Revoking a grant,
// stopping or deleting the VM, or starting a new generation cuts every bridged
// connection of the affected grants.
package reverse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"davit/internal/domain"
	"davit/internal/network/netguard"
)

// DialTimeout bounds each dial of a grant's Target.
const DialTimeout = 10 * time.Second

// Errors returned by this package; match with errors.Is.
var (
	ErrInvalidGrant  = errors.New("reverse: invalid grant")
	ErrDuplicate     = errors.New("reverse: a grant is already open on this guest port")
	ErrNotFound      = errors.New("reverse: no such grant")
	ErrNotPermitted  = errors.New("reverse: host service not permitted")
	ErrShutdown      = errors.New("reverse: manager is shut down")
	errRevokedOnOpen = errors.New("reverse: grant revoked while opening")
)

// GuestListener yields the connections guest processes made to a guest
// loopback listener. Accept returning an error ends the grant.
type GuestListener interface {
	Accept() (net.Conn, error)
	Close() error
}

// GuestListen opens a loopback listener inside a VM. The guest-agent client
// implements it; it must refuse a stale generation.
type GuestListen interface {
	ListenGuest(ctx context.Context, vm domain.VMID, generation int64, port uint16) (GuestListener, error)
}

// Target is the authorized far end of a grant. The laptop end (a CLI session
// tunneled through the gateway) lives with the gateway/CLI; this package
// provides HostService.
type Target interface {
	DialReverse(ctx context.Context) (net.Conn, error)
	String() string
}

// Policy is the operator's allowlist of host services guests may be granted.
type Policy struct {
	HostServices []netip.AddrPort
}

// HostService is a Target that dials TCP on the host. It is obtained only from
// Policy.HostService; the zero value refuses to dial.
type HostService struct {
	addr netip.AddrPort
}

// HostService returns a Target for addr if the operator listed it exactly.
// Unspecified addresses, port 0 and addresses netguard always refuses
// (link-local, cloud metadata, multicast, reserved) are refused even if listed.
func (p Policy) HostService(addr netip.AddrPort) (HostService, error) {
	if !addr.IsValid() || addr.Addr().IsUnspecified() || addr.Port() == 0 {
		return HostService{}, fmt.Errorf("%w: %s", ErrNotPermitted, addr)
	}
	if err := (netguard.Policy{AllowPrivate: true}).Check(addr.Addr()); err != nil {
		return HostService{}, fmt.Errorf("%w: %s: %v", ErrNotPermitted, addr, err)
	}
	want := netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	for _, a := range p.HostServices {
		if netip.AddrPortFrom(a.Addr().Unmap(), a.Port()) == want {
			return HostService{addr: want}, nil
		}
	}
	return HostService{}, fmt.Errorf("%w: %s is not in the operator allowlist", ErrNotPermitted, addr)
}

// Addr is the permitted host address.
func (h HostService) Addr() netip.AddrPort { return h.addr }

func (h HostService) String() string { return "host:" + h.addr.String() }

// DialReverse dials the host service, bounded by DialTimeout.
func (h HostService) DialReverse(ctx context.Context) (net.Conn, error) {
	if !h.addr.IsValid() {
		return nil, fmt.Errorf("%w: host service was not built from a Policy", ErrNotPermitted)
	}
	d := net.Dialer{Timeout: DialTimeout}
	return d.DialContext(ctx, "tcp", h.addr.String())
}

// Grant lets the guest of VM (at Generation) reach Target through guest
// loopback port GuestPort.
type Grant struct {
	VM         domain.VMID
	Generation int64
	GuestPort  uint16
	Target     Target
}

func (g Grant) validate() error {
	switch {
	case g.VM == "":
		return fmt.Errorf("%w: VM is required", ErrInvalidGrant)
	case g.Generation < 1:
		return fmt.Errorf("%w: generation must be >= 1", ErrInvalidGrant)
	case g.GuestPort == 0:
		return fmt.Errorf("%w: guest port is required", ErrInvalidGrant)
	case g.Target == nil:
		return fmt.Errorf("%w: target is required", ErrInvalidGrant)
	}
	return nil
}

// Info is a snapshot of one open grant.
type Info struct {
	ID         string
	VM         domain.VMID
	Generation int64
	GuestPort  uint16
	Target     string
	Active     int // bridged connections in flight
}

type key struct {
	vm   domain.VMID
	gen  int64
	port uint16
}

// bridge is one guest connection and, once dialed, its target. target is
// written under Manager.mu.
type bridge struct {
	guest, target net.Conn
}

type grant struct {
	id      string
	g       Grant
	ctx     context.Context
	cancel  context.CancelFunc
	ln      GuestListener // nil until ListenGuest returns
	bridges map[*bridge]struct{}
	closed  bool
}

// Manager owns open grants and their bridged connections. It is safe for
// concurrent use.
type Manager struct {
	listen GuestListen
	log    *slog.Logger
	wg     sync.WaitGroup

	mu       sync.Mutex
	grants   map[string]*grant
	byKey    map[key]*grant
	shutdown bool
}

// NewManager returns a Manager that opens guest listeners through listen.
func NewManager(listen GuestListen, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{listen: listen, log: log, grants: map[string]*grant{}, byKey: map[key]*grant{}}
}

// Open validates g, opens the guest listener and starts bridging. At most one
// grant may hold a (VM, generation, guest port).
func (m *Manager) Open(ctx context.Context, g Grant) (string, error) {
	if err := g.validate(); err != nil {
		return "", err
	}
	k := key{g.VM, g.Generation, g.GuestPort}
	gctx, cancel := context.WithCancel(context.Background())
	gr := &grant{id: domain.NewID("rev"), g: g, ctx: gctx, cancel: cancel, bridges: map[*bridge]struct{}{}}

	m.mu.Lock()
	if m.shutdown {
		m.mu.Unlock()
		cancel()
		return "", ErrShutdown
	}
	if _, dup := m.byKey[k]; dup {
		m.mu.Unlock()
		cancel()
		return "", fmt.Errorf("%w: vm %s generation %d port %d", ErrDuplicate, g.VM, g.Generation, g.GuestPort)
	}
	// Reserve the key so a concurrent Open of the same port is refused.
	m.grants[gr.id] = gr
	m.byKey[k] = gr
	m.mu.Unlock()

	ln, err := m.listen.ListenGuest(ctx, g.VM, g.Generation, g.GuestPort)
	if err != nil {
		m.closeGrant(gr)
		return "", fmt.Errorf("reverse: listen on guest port %d: %w", g.GuestPort, err)
	}

	m.mu.Lock()
	if gr.closed { // CloseVM or Shutdown raced the listen
		m.mu.Unlock()
		_ = ln.Close()
		return "", errRevokedOnOpen
	}
	gr.ln = ln
	m.wg.Add(1)
	m.mu.Unlock()

	go m.acceptLoop(gr)
	m.log.Info("reverse grant opened", "id", gr.id, "vm", g.VM, "generation", g.Generation, "port", g.GuestPort, "target", g.Target.String())
	return gr.id, nil
}

// Close revokes a grant: its guest listener and every bridged connection are
// closed. A second Close of the same id reports ErrNotFound and does nothing.
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	gr, ok := m.grants[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	m.closeGrant(gr)
	return nil
}

// CloseVM revokes every grant of vm whose generation is not keepGeneration and
// returns how many it closed. Stop and delete pass 0 (closes all); a new
// generation passes itself, so nothing granted to an older run survives.
func (m *Manager) CloseVM(vm domain.VMID, keepGeneration int64) int {
	m.mu.Lock()
	var victims []*grant
	for _, gr := range m.grants {
		if gr.g.VM == vm && gr.g.Generation != keepGeneration {
			victims = append(victims, gr)
		}
	}
	m.mu.Unlock()
	for _, gr := range victims {
		m.closeGrant(gr)
	}
	return len(victims)
}

// List returns a snapshot of the open grants.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.grants))
	for _, gr := range m.grants {
		if gr.ln == nil {
			continue // still opening
		}
		out = append(out, Info{
			ID: gr.id, VM: gr.g.VM, Generation: gr.g.Generation, GuestPort: gr.g.GuestPort,
			Target: gr.g.Target.String(), Active: len(gr.bridges),
		})
	}
	return out
}

// Shutdown revokes every grant, refuses new ones and waits for all bridging
// goroutines to finish.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.shutdown = true
	victims := make([]*grant, 0, len(m.grants))
	for _, gr := range m.grants {
		victims = append(victims, gr)
	}
	m.mu.Unlock()
	for _, gr := range victims {
		m.closeGrant(gr)
	}
	m.wg.Wait()
}

// closeGrant removes gr and closes its listener and connections. Safe to call
// more than once.
func (m *Manager) closeGrant(gr *grant) {
	m.mu.Lock()
	if gr.closed {
		m.mu.Unlock()
		return
	}
	gr.closed = true
	delete(m.grants, gr.id)
	delete(m.byKey, key{gr.g.VM, gr.g.Generation, gr.g.GuestPort})
	ln := gr.ln
	var conns []net.Conn
	for b := range gr.bridges {
		conns = append(conns, b.guest)
		if b.target != nil {
			conns = append(conns, b.target)
		}
	}
	m.mu.Unlock()

	gr.cancel() // aborts dials in flight
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

func (m *Manager) acceptLoop(gr *grant) {
	defer m.wg.Done()
	for {
		c, err := gr.ln.Accept()
		if err != nil {
			m.mu.Lock()
			revoked := gr.closed
			m.mu.Unlock()
			if !revoked {
				m.log.Warn("reverse grant ended: guest listener failed", "id", gr.id, "vm", gr.g.VM, "err", err)
				m.closeGrant(gr)
			}
			return
		}
		b := &bridge{guest: c}
		m.mu.Lock()
		if gr.closed {
			m.mu.Unlock()
			_ = c.Close()
			return
		}
		gr.bridges[b] = struct{}{}
		m.wg.Add(1)
		m.mu.Unlock()
		go m.serve(gr, b)
	}
}

func (m *Manager) serve(gr *grant, b *bridge) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		delete(gr.bridges, b)
		t := b.target
		m.mu.Unlock()
		_ = b.guest.Close()
		if t != nil {
			_ = t.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(gr.ctx, DialTimeout)
	t, err := gr.g.Target.DialReverse(ctx)
	cancel()
	if err != nil {
		m.log.Warn("reverse dial failed; dropping guest connection", "id", gr.id, "target", gr.g.Target.String(), "err", err)
		return
	}
	m.mu.Lock()
	if gr.closed {
		m.mu.Unlock()
		_ = t.Close()
		return
	}
	b.target = t
	m.mu.Unlock()

	splice(b.guest, t)
}

// splice copies both ways. When one side reaches EOF the other gets a
// half-close if it supports CloseWrite, else a full close; any error closes
// both.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		if _, err := io.Copy(dst, src); err != nil {
			_ = a.Close()
			_ = b.Close()
			return
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go pipe(a, b)
	go pipe(b, a)
	<-done
	<-done
}
