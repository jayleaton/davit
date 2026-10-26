package reversefwd

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/network/reverse"
)

// fakeVMs is one VM per principal "alice" with a mutable state/generation.
type fakeVMs struct {
	mu      sync.Mutex
	vm      domain.VM
	changed chan struct{}
}

func newVMs() *fakeVMs {
	return &fakeVMs{vm: domain.VM{ID: "vm_1", Name: "box", State: domain.StateReady, InstanceGeneration: 1}, changed: make(chan struct{})}
}

func (f *fakeVMs) GetVM(_ context.Context, p domain.PrincipalID, ref string) (*domain.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p != "alice" || (ref != "box" && ref != string(f.vm.ID)) {
		return nil, domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
	}
	vm := f.vm
	return &vm, nil
}

func (f *fakeVMs) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *fakeVMs) set(state domain.VMState, gen int64) {
	f.mu.Lock()
	f.vm.State, f.vm.InstanceGeneration = state, gen
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

// pipeListen is a guest listener fed by connect (a guest process).
type pipeListen struct{ lns sync.Map } // port -> *pipeListener

type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }

func (p *pipeListen) ListenGuest(_ context.Context, _ domain.VMID, _ int64, port uint16) (reverse.GuestListener, error) {
	l := &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
	p.lns.Store(port, l)
	return l, nil
}

// connect is a guest process dialing the guest port.
func (p *pipeListen) connect(t *testing.T, port uint16) net.Conn {
	t.Helper()
	v, ok := p.lns.Load(port)
	if !ok {
		t.Fatalf("no guest listener on %d", port)
	}
	guest, host := net.Pipe()
	select {
	case v.(*pipeListener).conns <- host:
	case <-time.After(5 * time.Second):
		t.Fatal("guest listener not accepting")
	}
	return guest
}

func newService(t *testing.T, policy reverse.Policy) (*Service, *fakeVMs, *pipeListen) {
	t.Helper()
	vms, ln := newVMs(), &pipeListen{}
	s := New(Config{Listen: ln, Policy: policy, VMs: vms, AttachWindow: 200 * time.Millisecond})
	t.Cleanup(s.Close)
	return s, vms, ln
}

func code(err error) domain.Code {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func sessionReq(port int) api.CreateReverseForwardRequest {
	return api.CreateReverseForwardRequest{GuestPort: port, Target: api.ReverseTarget{Kind: api.ReverseTargetSession, Address: "localhost:3000"}}
}

func TestCreateValidatesAndScopes(t *testing.T) {
	ctx := context.Background()
	if _, err := New(Config{VMs: newVMs()}).Create(ctx, "alice", "box", sessionReq(80)); code(err) != domain.CodeUnsupportedCapability {
		t.Fatalf("without guest listeners: %v", err)
	}
	allowed := netip.MustParseAddrPort("127.0.0.1:5432")
	s, _, _ := newService(t, reverse.Policy{HostServices: []netip.AddrPort{allowed}})
	for _, tc := range []struct {
		req  api.CreateReverseForwardRequest
		want domain.Code
	}{
		{sessionReq(0), domain.CodeInvalidArgument},
		{api.CreateReverseForwardRequest{GuestPort: 80, Target: api.ReverseTarget{Kind: "laptop"}}, domain.CodeInvalidArgument},
		{api.CreateReverseForwardRequest{GuestPort: 80, Target: api.ReverseTarget{Kind: api.ReverseTargetHost, Address: "db:5432"}}, domain.CodeInvalidArgument},
		{api.CreateReverseForwardRequest{GuestPort: 80, Target: api.ReverseTarget{Kind: api.ReverseTargetHost, Address: "127.0.0.1:22"}}, domain.CodePermissionDenied},
	} {
		if _, err := s.Create(ctx, "alice", "box", tc.req); code(err) != tc.want {
			t.Errorf("%+v: %v, want %s", tc.req, err, tc.want)
		}
	}
	fw, err := s.Create(ctx, "alice", "box", api.CreateReverseForwardRequest{GuestPort: 5432,
		Target: api.ReverseTarget{Kind: api.ReverseTargetHost, Address: allowed.String()}})
	if err != nil || fw.State != api.ReverseActive || fw.Generation != 1 {
		t.Fatalf("host forward: %+v %v", fw, err)
	}
	if _, err := s.Create(ctx, "alice", "box", sessionReq(5432)); code(err) != domain.CodeInvalidState {
		t.Fatalf("second forward on the port: %v", err)
	}
	// Another principal sees nothing.
	if _, err := s.List(ctx, "mallory", "box"); code(err) != domain.CodeNotFound {
		t.Fatalf("other principal list: %v", err)
	}
	if err := s.Revoke(ctx, "mallory", "vm_1", fw.ID); code(err) != domain.CodeNotFound {
		t.Fatalf("other principal revoke: %v", err)
	}
}

// TestSessionBridgesAndEnds: a session forward announces each guest
// connection; Connect completes it; the session ends when revoked.
func TestSessionBridgesAndEnds(t *testing.T) {
	ctx := context.Background()
	s, _, ln := newService(t, reverse.Policy{})
	fw, err := s.Create(ctx, "alice", "box", sessionReq(8080))
	if err != nil || fw.State != api.ReverseAwaitingSession || fw.SessionExpiresAt == nil {
		t.Fatalf("create: %+v %v", fw, err)
	}
	events := make(chan api.ReverseSessionEvent, 8)
	done := make(chan string, 1)
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		reason, _ := s.RunSession(sctx, "alice", "box", fw.ID, func(e api.ReverseSessionEvent) error { events <- e; return nil })
		done <- reason
	}()
	guest := ln.connect(t, 8080)
	ev := <-events
	if ev.Type != api.ReverseEventConnection || ev.ConnectionID == "" {
		t.Fatalf("event %+v", ev)
	}
	laptopSide, daemonSide := net.Pipe()
	if err := s.Connect(ctx, "alice", "box", fw.ID, ev.ConnectionID, daemonSide); err != nil {
		t.Fatal(err)
	}
	if err := s.Connect(ctx, "alice", "box", fw.ID, ev.ConnectionID, daemonSide); code(err) != domain.CodeNotFound {
		t.Fatalf("second claim: %v", err)
	}
	go guest.Write([]byte("hi"))
	b := make([]byte, 2)
	if _, err := io.ReadFull(laptopSide, b); err != nil || string(b) != "hi" {
		t.Fatalf("bridged bytes %q %v", b, err)
	}
	if l, _ := s.List(ctx, "alice", "box"); len(l) != 1 || l[0].State != api.ReverseActive || l[0].Active != 1 {
		t.Fatalf("list %+v", l)
	}
	if err := s.Revoke(ctx, "alice", "box", fw.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := laptopSide.Read(b); err == nil {
		t.Fatal("revoke left the bridged connection open")
	}
	if ev := <-events; ev.Type != api.ReverseEventRevoked || ev.Reason != "revoked" {
		t.Fatalf("event %+v", ev)
	}
	if r := <-done; r != "revoked" {
		t.Fatalf("session ended with %q", r)
	}
}

func TestUnattachedSessionExpires(t *testing.T) {
	s, _, _ := newService(t, reverse.Policy{})
	fw, err := s.Create(context.Background(), "alice", "box", sessionReq(9000))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if l, _ := s.List(context.Background(), "alice", "box"); len(l) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unattached session forward was not revoked")
		}
	}
	if _, err := s.RunSession(context.Background(), "alice", "box", fw.ID, func(api.ReverseSessionEvent) error { return nil }); code(err) != domain.CodeNotFound {
		t.Fatalf("attach after expiry: %v", err)
	}
}

// TestLifecycleRevokes: stop and a new generation revoke and cut.
func TestLifecycleRevokes(t *testing.T) {
	ctx := context.Background()
	allowed := netip.MustParseAddrPort("127.0.0.1:1")
	s, vms, _ := newService(t, reverse.Policy{HostServices: []netip.AddrPort{allowed}})
	host := api.CreateReverseForwardRequest{GuestPort: 70, Target: api.ReverseTarget{Kind: api.ReverseTargetHost, Address: allowed.String()}}
	gone := func() bool { l, _ := s.List(ctx, "alice", "box"); return len(l) == 0 }
	wait := func(why string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !gone(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s did not revoke", why)
			}
		}
	}
	if _, err := s.Create(ctx, "alice", "box", host); err != nil {
		t.Fatal(err)
	}
	vms.set(domain.StateRunning, 2) // a new generation
	wait("a new generation")
	if _, err := s.Create(ctx, "alice", "box", host); err != nil {
		t.Fatal(err)
	}
	vms.set(domain.StateStopping, 2)
	wait("stop")
	if _, err := s.Create(ctx, "alice", "box", host); code(err) != domain.CodeInvalidState {
		t.Fatalf("create on a stopping VM: %v", err)
	}
}
