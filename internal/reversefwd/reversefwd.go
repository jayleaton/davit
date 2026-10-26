// Package reversefwd is the API side of reverse forwards (feature
// reverseForward): it turns createReverseForward requests into
// internal/network/reverse grants on a VM's current generation, provides the
// laptop end ("session" target: a client holding attachReverseSession, which
// carries each guest connection over attachReverseConnection), and revokes
// grants when their VM stops, is deleted or starts a new generation.
//
// It is transport-agnostic: httpapi upgrades the WebSockets and hands it a
// net.Conn per guest connection and an event sink per session.
package reversefwd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/network/reverse"
)

// Defaults.
const (
	// AttachWindow is how long a session forward waits for its session.
	AttachWindow = 30 * time.Second
	// PingInterval keeps idle sessions alive through proxies and NATs.
	PingInterval = 20 * time.Second
	// reconcileEvery backstops lifecycle notifications (and notices guest
	// listeners that ended on their own).
	reconcileEvery = 2 * time.Second
	// eventBacklog bounds connection announcements a session has not sent.
	eventBacklog = 64
)

// VMs resolves VMs in a principal's scope and reports changes
// (lifecycle.Service).
type VMs interface {
	GetVM(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.VM, error)
	Changed() <-chan struct{}
}

// Config configures a Service.
type Config struct {
	// Listen opens guest loopback listeners; nil means the guests cannot
	// listen and every operation answers unsupported_capability.
	Listen reverse.GuestListen
	// Policy is the operator's allowlist of host services.
	Policy reverse.Policy
	VMs    VMs
	Log    *slog.Logger
	// AttachWindow defaults to AttachWindow.
	AttachWindow time.Duration
}

// Service owns the reverse forwards of every VM on this host.
type Service struct {
	cfg  Config
	mgr  *reverse.Manager
	log  *slog.Logger
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu       sync.Mutex
	forwards map[string]*forward
}

type forward struct {
	id        string
	principal domain.PrincipalID
	vm        domain.VMID
	gen       int64
	port      int
	target    api.ReverseTarget
	created   time.Time
	expires   time.Time // session forwards only
	sess      *session  // nil for a host target
	timer     *time.Timer
}

// New starts a Service. Close it to revoke everything.
func New(cfg Config) *Service {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.AttachWindow <= 0 {
		cfg.AttachWindow = AttachWindow
	}
	s := &Service{cfg: cfg, log: cfg.Log, forwards: map[string]*forward{}}
	if cfg.Listen != nil {
		s.mgr = reverse.NewManager(cfg.Listen, cfg.Log.With("component", "reverse"))
		ctx, cancel := context.WithCancel(context.Background())
		s.stop = cancel
		s.wg.Go(func() { s.watch(ctx) })
	}
	return s
}

// Available reports whether guests can listen on this host.
func (s *Service) Available() bool { return s != nil && s.mgr != nil }

// Close revokes every forward and stops the watcher.
func (s *Service) Close() {
	if !s.Available() {
		return
	}
	s.stop()
	s.wg.Wait()
	s.mu.Lock()
	ids := make([]string, 0, len(s.forwards))
	for id := range s.forwards {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.revoke(id, "daemon shutting down")
	}
	s.mgr.Shutdown()
}

func (s *Service) unsupported() error {
	return domain.Errorf(domain.CodeUnsupportedCapability,
		"reverse forwards need guest listeners, which this host's guest agent does not provide").
		WithDetail("feature", domain.FeatureReverseForward)
}

func live(vm *domain.VM) bool {
	return vm.State == domain.StateReady || vm.State == domain.StateRunning
}

// Create opens a reverse forward on ref's current generation.
func (s *Service) Create(ctx context.Context, p domain.PrincipalID, ref string, req api.CreateReverseForwardRequest) (api.ReverseForward, error) {
	if !s.Available() {
		return api.ReverseForward{}, s.unsupported()
	}
	invalid := func(field, format string, args ...any) error {
		return domain.Errorf(domain.CodeInvalidArgument, format, args...).WithDetail("field", field)
	}
	if req.GuestPort < 1 || req.GuestPort > 65535 {
		return api.ReverseForward{}, invalid("guestPort", "guestPort must be 1..65535")
	}
	vm, err := s.cfg.VMs.GetVM(ctx, p, ref)
	if err != nil {
		return api.ReverseForward{}, err
	}
	if !live(vm) {
		return api.ReverseForward{}, domain.Errorf(domain.CodeInvalidState, "vm %s is %s; reverse forwards need it ready or running", vm.ID, vm.State).
			WithDetail("state", vm.State)
	}
	fw := &forward{principal: p, vm: vm.ID, gen: vm.InstanceGeneration, port: req.GuestPort, target: req.Target, created: time.Now().UTC()}
	var target reverse.Target
	switch req.Target.Kind {
	case api.ReverseTargetHost:
		ap, err := netip.ParseAddrPort(req.Target.Address)
		if err != nil {
			return api.ReverseForward{}, invalid("target.address", "a host target's address must be ip:port: %v", err)
		}
		hs, err := s.cfg.Policy.HostService(ap)
		if err != nil {
			return api.ReverseForward{}, domain.Errorf(domain.CodePermissionDenied,
				"host service %s is not in the operator's network.reverseHostServices", ap).WithDetail("address", ap.String())
		}
		fw.target.Address = hs.Addr().String()
		target = hs
	case api.ReverseTargetSession:
		fw.sess = newSession(req.Target.Address)
		fw.expires = fw.created.Add(s.cfg.AttachWindow)
		target = fw.sess
	default:
		return api.ReverseForward{}, invalid("target.kind", "target.kind must be %q or %q", api.ReverseTargetSession, api.ReverseTargetHost)
	}
	id, err := s.mgr.Open(ctx, reverse.Grant{VM: vm.ID, Generation: vm.InstanceGeneration, GuestPort: uint16(req.GuestPort), Target: target})
	switch {
	case errors.Is(err, reverse.ErrDuplicate):
		return api.ReverseForward{}, domain.Errorf(domain.CodeInvalidState, "guest port %d of vm %s already has a reverse forward", req.GuestPort, vm.ID).
			WithDetail("guestPort", req.GuestPort)
	case err != nil:
		var de *domain.Error
		if errors.As(err, &de) {
			return api.ReverseForward{}, err
		}
		return api.ReverseForward{}, domain.Errorf(domain.CodeUnavailable, "vm %s: %v", vm.ID, err)
	}
	fw.id = id
	s.mu.Lock()
	s.forwards[id] = fw
	if fw.sess != nil {
		fw.timer = time.AfterFunc(s.cfg.AttachWindow, func() {
			if !fw.sess.isAttached() {
				s.revoke(id, "no session attached within "+s.cfg.AttachWindow.String())
			}
		})
	}
	out := s.view(fw, 0)
	s.mu.Unlock()
	s.log.Info("reverse forward created", "id", id, "vm", vm.ID, "generation", fw.gen, "guestPort", fw.port, "target", target.String())
	return out, nil
}

// view renders fw; s.mu held.
func (s *Service) view(fw *forward, active int) api.ReverseForward {
	out := api.ReverseForward{ID: fw.id, VMID: fw.vm, Generation: fw.gen, GuestPort: fw.port, Target: fw.target,
		State: api.ReverseActive, Active: active, CreatedAt: fw.created}
	if fw.sess != nil && !fw.sess.isAttached() {
		out.State = api.ReverseAwaitingSession
		exp := fw.expires
		out.SessionExpiresAt = &exp
	}
	return out
}

// lookup resolves ref and id in p's scope.
func (s *Service) lookup(ctx context.Context, p domain.PrincipalID, ref, id string) (*forward, error) {
	if !s.Available() {
		return nil, s.unsupported()
	}
	vm, err := s.cfg.VMs.GetVM(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	fw := s.forwards[id]
	s.mu.Unlock()
	if fw == nil || fw.vm != vm.ID || fw.principal != p {
		return nil, domain.Errorf(domain.CodeNotFound, "reverse forward %q not found on vm %s", id, vm.ID)
	}
	return fw, nil
}

// Get returns one forward of ref.
func (s *Service) Get(ctx context.Context, p domain.PrincipalID, ref, id string) (api.ReverseForward, error) {
	fw, err := s.lookup(ctx, p, ref, id)
	if err != nil {
		return api.ReverseForward{}, err
	}
	active := 0
	for _, in := range s.mgr.List() {
		if in.ID == id {
			active = in.Active
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(fw, active), nil
}

// List returns ref's open forwards, oldest first.
func (s *Service) List(ctx context.Context, p domain.PrincipalID, ref string) ([]api.ReverseForward, error) {
	if !s.Available() {
		return nil, s.unsupported()
	}
	vm, err := s.cfg.VMs.GetVM(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	active := map[string]int{}
	for _, in := range s.mgr.List() {
		active[in.ID] = in.Active
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []api.ReverseForward{}
	for _, fw := range s.forwards {
		if fw.vm == vm.ID && fw.principal == p {
			out = append(out, s.view(fw, active[fw.id]))
		}
	}
	slices.SortFunc(out, func(a, b api.ReverseForward) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}

// Revoke ends a forward and cuts its live connections.
func (s *Service) Revoke(ctx context.Context, p domain.PrincipalID, ref, id string) error {
	fw, err := s.lookup(ctx, p, ref, id)
	if err != nil {
		return err
	}
	s.revoke(fw.id, "revoked")
	return nil
}

// revoke removes forward id, closes its grant (cutting bridged
// connections) and ends its session with reason. Idempotent.
func (s *Service) revoke(id, reason string) {
	s.mu.Lock()
	fw := s.forwards[id]
	delete(s.forwards, id)
	s.mu.Unlock()
	if fw == nil {
		return
	}
	if fw.timer != nil {
		fw.timer.Stop()
	}
	_ = s.mgr.Close(id)
	if fw.sess != nil {
		fw.sess.end(reason)
	}
	s.log.Info("reverse forward revoked", "id", id, "vm", fw.vm, "reason", reason)
}

// RunSession serves the session of a session forward: it sends each guest
// connection's announcement (and pings) through send until the forward
// ends, then sends the revoked event. ctx ending (the client went away)
// revokes the forward. It returns the reason the session ended.
func (s *Service) RunSession(ctx context.Context, p domain.PrincipalID, ref, id string, send func(api.ReverseSessionEvent) error) (string, error) {
	fw, err := s.lookup(ctx, p, ref, id)
	if err != nil {
		return "", err
	}
	if fw.sess == nil {
		return "", domain.Errorf(domain.CodeInvalidArgument, "reverse forward %s targets a host service; it has no session", id)
	}
	if !fw.sess.attach() {
		return "", domain.Errorf(domain.CodeInvalidState, "reverse forward %s already has a session attached", id)
	}
	ping := time.NewTicker(PingInterval)
	defer ping.Stop()
	for {
		select {
		case cid := <-fw.sess.events:
			if err := send(api.ReverseSessionEvent{Type: api.ReverseEventConnection, ConnectionID: cid}); err != nil {
				s.revoke(id, "session lost")
				return "session lost", nil
			}
		case <-ping.C:
			if err := send(api.ReverseSessionEvent{Type: api.ReverseEventPing}); err != nil {
				s.revoke(id, "session lost")
				return "session lost", nil
			}
		case <-ctx.Done():
			s.revoke(id, "session closed")
			return "session closed", nil
		case <-fw.sess.done:
			reason := fw.sess.reason()
			_ = send(api.ReverseSessionEvent{Type: api.ReverseEventRevoked, Reason: reason})
			return reason, nil
		}
	}
}

// Connect hands conn to the guest connection cid of a session forward. The
// caller keeps serving conn until it is closed.
func (s *Service) Connect(ctx context.Context, p domain.PrincipalID, ref, id, cid string, conn net.Conn) error {
	fw, err := s.lookup(ctx, p, ref, id)
	if err != nil {
		return err
	}
	if fw.sess == nil || !fw.sess.deliver(cid, conn) {
		return domain.Errorf(domain.CodeNotFound, "connection %q of reverse forward %s is not waiting (claim it within %v of the announcement)",
			cid, id, reverse.DialTimeout)
	}
	return nil
}

// watch revokes forwards whose VM left ready/running, is gone, or runs a
// new generation, and forwards whose guest listener ended.
func (s *Service) watch(ctx context.Context) {
	tick := time.NewTicker(reconcileEvery)
	defer tick.Stop()
	for {
		changed := s.cfg.VMs.Changed()
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-tick.C:
		}
	}
}

func (s *Service) reconcile(ctx context.Context) {
	s.mu.Lock()
	fws := make([]*forward, 0, len(s.forwards))
	for _, fw := range s.forwards {
		fws = append(fws, fw)
	}
	s.mu.Unlock()
	if len(fws) == 0 {
		return
	}
	open := map[string]bool{}
	for _, in := range s.mgr.List() {
		open[in.ID] = true
	}
	for _, fw := range fws {
		reason := ""
		vm, err := s.cfg.VMs.GetVM(ctx, fw.principal, string(fw.vm))
		switch {
		case err != nil && ctx.Err() != nil:
			return
		case err != nil || vm.State == domain.StateDeleted || vm.State == domain.StateDeleting:
			reason = "vm deleted"
		case !live(vm):
			reason = fmt.Sprintf("vm %s", vm.State)
		case vm.InstanceGeneration != fw.gen:
			reason = "vm started a new generation"
		case !open[fw.id]:
			reason = "guest listener ended"
		}
		if reason != "" {
			s.revoke(fw.id, reason)
		}
	}
}

// session is the laptop end of a session forward: a reverse.Target whose
// dials are announced to the attached client and completed by Connect.
type session struct {
	label  string
	events chan string

	mu       sync.Mutex
	attached bool
	onAttach chan struct{} // closed when the session attaches
	pending  map[string]chan net.Conn
	done     chan struct{}
	why      string
}

func newSession(label string) *session {
	return &session{label: label, events: make(chan string, eventBacklog), onAttach: make(chan struct{}),
		pending: map[string]chan net.Conn{}, done: make(chan struct{})}
}

func (t *session) String() string { return "session:" + t.label }

func (t *session) isAttached() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.attached
}

func (t *session) attach() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.done:
		return false
	default:
	}
	if t.attached {
		return false
	}
	t.attached = true
	close(t.onAttach)
	return true
}

func (t *session) end(reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.done:
		return
	default:
	}
	t.why = reason
	close(t.done)
	for cid, ch := range t.pending {
		delete(t.pending, cid)
		close(ch)
	}
}

func (t *session) reason() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.why
}

var errNoSession = errors.New("reversefwd: no session attached")

// DialReverse announces a guest connection to the session and waits for the
// client to claim it. A guest connecting before the session attached waits
// for it (within ctx, the dial window).
func (t *session) DialReverse(ctx context.Context) (net.Conn, error) {
	select {
	case <-t.onAttach:
	case <-t.done:
		return nil, errNoSession
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", errNoSession, ctx.Err())
	}
	cid := domain.NewID("conn")
	ch := make(chan net.Conn, 1)
	t.mu.Lock()
	select {
	case <-t.done:
		t.mu.Unlock()
		return nil, errNoSession
	default:
	}
	select {
	case t.events <- cid:
	default:
		t.mu.Unlock()
		return nil, errors.New("reversefwd: session has too many unclaimed connections")
	}
	t.pending[cid] = ch
	t.mu.Unlock()

	select {
	case c, ok := <-ch:
		if !ok {
			return nil, errNoSession
		}
		return c, nil
	case <-ctx.Done():
		t.mu.Lock()
		_, waiting := t.pending[cid]
		delete(t.pending, cid)
		t.mu.Unlock()
		if !waiting { // Connect won the race: drop what it delivered
			if c, ok := <-ch; ok {
				c.Close()
			}
		}
		return nil, ctx.Err()
	}
}

// deliver completes the dial of cid; false if nobody waits for it.
func (t *session) deliver(cid string, c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch, ok := t.pending[cid]
	if !ok {
		return false
	}
	delete(t.pending, cid)
	ch <- c
	return true
}
