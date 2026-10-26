package reverse

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"davit/internal/domain"
)

// fakeGuest stands in for the guest agent: each guest listener is a loopback
// TCP listener, and a "guest process" connects by dialing its address.
type fakeGuest struct {
	mu  sync.Mutex
	lns map[key]net.Listener
}

func newFakeGuest() *fakeGuest { return &fakeGuest{lns: map[key]net.Listener{}} }

func (f *fakeGuest) ListenGuest(_ context.Context, vm domain.VMID, gen int64, port uint16) (GuestListener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.lns[key{vm, gen, port}] = ln
	f.mu.Unlock()
	return ln, nil
}

func (f *fakeGuest) listener(t *testing.T, vm domain.VMID, gen int64, port uint16) net.Listener {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	ln, ok := f.lns[key{vm, gen, port}]
	if !ok {
		t.Fatalf("no guest listener for %s/%d/%d", vm, gen, port)
	}
	return ln
}

// guestDial is a guest process connecting to its loopback listener.
func (f *fakeGuest) guestDial(t *testing.T, vm domain.VMID, gen int64, port uint16) *net.TCPConn {
	t.Helper()
	c, err := net.Dial("tcp", f.listener(t, vm, gen, port).Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	return c.(*net.TCPConn)
}

// hostServer runs handle for every connection on a 127.0.0.1 listener and
// returns a Target permitted for it.
func hostServer(t *testing.T, handle func(net.Conn)) HostService {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); handle(c) }()
		}
	}()
	addr := netip.MustParseAddrPort(ln.Addr().String())
	hs, err := Policy{HostServices: []netip.AddrPort{addr}}.HostService(addr)
	if err != nil {
		t.Fatal(err)
	}
	return hs
}

func echo(c net.Conn) { _, _ = io.Copy(c, c) }

type failTarget struct{ dials atomic.Int32 }

func (f *failTarget) DialReverse(context.Context) (net.Conn, error) {
	f.dials.Add(1)
	return nil, errors.New("laptop gone")
}
func (f *failTarget) String() string { return "fail" }

func newManager(t *testing.T) (*Manager, *fakeGuest) {
	t.Helper()
	fg := newFakeGuest()
	m := NewManager(fg, nil)
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { m.Shutdown(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Shutdown did not return: goroutines leaked")
		}
	})
	return m, fg
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func info(m *Manager, id string) (Info, bool) {
	for _, i := range m.List() {
		if i.ID == id {
			return i, true
		}
	}
	return Info{}, false
}

func activeIs(m *Manager, id string, n int) func() bool {
	return func() bool { i, ok := info(m, id); return ok && i.Active == n }
}

const vm = domain.VMID("vm_a")

func TestBridgesBothWaysWithHalfClose(t *testing.T) {
	m, fg := newManager(t)
	// The host service answers only after the guest's write side is closed.
	target := hostServer(t, func(c net.Conn) {
		in, _ := io.ReadAll(c)
		_, _ = c.Write(append([]byte("got:"), in...))
	})
	id, err := m.Open(context.Background(), Grant{VM: vm, Generation: 1, GuestPort: 8080, Target: target})
	if err != nil {
		t.Fatal(err)
	}

	c := fg.guestDial(t, vm, 1, 8080)
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(c)
	if err != nil || string(out) != "got:hello" {
		t.Fatalf("read %q, %v; want got:hello", out, err)
	}
	eventually(t, "bridge to finish", activeIs(m, id, 0))
}

func TestRevokeCutsActiveAndRefusesNew(t *testing.T) {
	m, fg := newManager(t)
	id, err := m.Open(context.Background(), Grant{VM: vm, Generation: 1, GuestPort: 9000, Target: hostServer(t, echo)})
	if err != nil {
		t.Fatal(err)
	}
	addr := fg.listener(t, vm, 1, 9000).Addr().String()
	c := fg.guestDial(t, vm, 1, 9000)
	buf := make([]byte, 4)
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q, %v", buf, err)
	}
	eventually(t, "one active bridge", activeIs(m, id, 1))

	if err := m.Close(id); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(buf); err == nil {
		t.Fatal("bridged connection survived revoke")
	}
	if c2, err := net.Dial("tcp", addr); err == nil {
		_ = c2.Close()
		t.Fatal("guest listener still accepts after revoke")
	}
	if len(m.List()) != 0 {
		t.Fatalf("grant still listed: %+v", m.List())
	}
	if err := m.Close(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Close = %v, want ErrNotFound", err)
	}
}

func TestCloseVMInvalidatesOtherGenerations(t *testing.T) {
	m, fg := newManager(t)
	target := hostServer(t, echo)
	open := func(v domain.VMID, gen int64) string {
		id, err := m.Open(context.Background(), Grant{VM: v, Generation: gen, GuestPort: 7000, Target: target})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	old, cur, other := open(vm, 1), open(vm, 2), open("vm_b", 1)
	c := fg.guestDial(t, vm, 1, 7000)
	eventually(t, "old-generation bridge", activeIs(m, old, 1))

	if n := m.CloseVM(vm, 2); n != 1 {
		t.Fatalf("CloseVM(keep 2) closed %d, want 1", n)
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("old-generation connection survived")
	}
	for id, want := range map[string]bool{old: false, cur: true, other: true} {
		if _, ok := info(m, id); ok != want {
			t.Errorf("grant %s listed=%v, want %v", id, ok, want)
		}
	}
	if n := m.CloseVM(vm, 0); n != 1 {
		t.Fatalf("CloseVM(keep 0) closed %d, want 1", n)
	}
	if _, ok := info(m, other); !ok || len(m.List()) != 1 {
		t.Fatalf("other VM's grant affected: %+v", m.List())
	}
}

func TestOpenValidatesAndRefusesDuplicate(t *testing.T) {
	m, _ := newManager(t)
	target := &failTarget{}
	for _, g := range []Grant{
		{Generation: 1, GuestPort: 1, Target: target},
		{VM: vm, GuestPort: 1, Target: target},
		{VM: vm, Generation: 1, Target: target},
		{VM: vm, Generation: 1, GuestPort: 1},
	} {
		if _, err := m.Open(context.Background(), g); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("Open(%+v) = %v, want ErrInvalidGrant", g, err)
		}
	}
	g := Grant{VM: vm, Generation: 1, GuestPort: 5432, Target: target}
	if _, err := m.Open(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(context.Background(), g); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate Open = %v, want ErrDuplicate", err)
	}
	g.Generation = 2 // same port, new generation is a different grant
	if _, err := m.Open(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	m.Shutdown()
	if _, err := m.Open(context.Background(), Grant{VM: vm, Generation: 3, GuestPort: 1, Target: target}); !errors.Is(err, ErrShutdown) {
		t.Fatalf("Open after Shutdown = %v, want ErrShutdown", err)
	}
}

func TestDialFailureKeepsGrant(t *testing.T) {
	m, fg := newManager(t)
	target := &failTarget{}
	id, err := m.Open(context.Background(), Grant{VM: vm, Generation: 1, GuestPort: 3000, Target: target})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		c := fg.guestDial(t, vm, 1, 3000)
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("guest connection open after failed dial")
		}
		if got := target.dials.Load(); got != int32(i) {
			t.Fatalf("dials = %d, want %d", got, i)
		}
	}
	eventually(t, "failed bridges to drain", activeIs(m, id, 0))
}

func TestListenerErrorRemovesGrant(t *testing.T) {
	m, fg := newManager(t)
	id, err := m.Open(context.Background(), Grant{VM: vm, Generation: 1, GuestPort: 4000, Target: &failTarget{}})
	if err != nil {
		t.Fatal(err)
	}
	_ = fg.listener(t, vm, 1, 4000).Close() // the guest side goes away
	eventually(t, "grant removal", func() bool { _, ok := info(m, id); return !ok })
	// The port is free for a new grant.
	if _, err := m.Open(context.Background(), Grant{VM: vm, Generation: 1, GuestPort: 4000, Target: &failTarget{}}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyHostService(t *testing.T) {
	listed := netip.MustParseAddrPort("127.0.0.1:5432")
	p := Policy{HostServices: []netip.AddrPort{listed, netip.MustParseAddrPort("0.0.0.0:80")}}
	for _, a := range []string{"127.0.0.1:5433", "127.0.0.2:5432", "0.0.0.0:80", "[::]:5432", "127.0.0.1:0"} {
		if _, err := p.HostService(netip.MustParseAddrPort(a)); !errors.Is(err, ErrNotPermitted) {
			t.Errorf("HostService(%s) = %v, want ErrNotPermitted", a, err)
		}
	}
	if _, err := p.HostService(netip.AddrPort{}); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("HostService(invalid) = %v, want ErrNotPermitted", err)
	}

	// Metadata, link-local, multicast and reserved addresses are refused
	// even when listed (SEC-12); LAN and loopback services stay allowed.
	internal := []string{"169.254.169.254:80", "[fd00:ec2::254]:80", "[fe80::1]:22", "224.0.0.251:5353", "240.0.0.1:80", "[::ffff:169.254.169.254]:80"}
	var all []netip.AddrPort
	for _, a := range internal {
		all = append(all, netip.MustParseAddrPort(a))
	}
	wide := Policy{HostServices: append(all, netip.MustParseAddrPort("192.168.1.10:5432"))}
	for _, a := range all {
		if _, err := wide.HostService(a); !errors.Is(err, ErrNotPermitted) {
			t.Errorf("listed internal HostService(%s) = %v, want ErrNotPermitted", a, err)
		}
	}
	if _, err := wide.HostService(netip.MustParseAddrPort("192.168.1.10:5432")); err != nil {
		t.Errorf("listed LAN service refused: %v", err)
	}
	hs, err := p.HostService(netip.MustParseAddrPort("[::ffff:127.0.0.1]:5432"))
	if err != nil || hs.Addr() != listed {
		t.Errorf("mapped form of a listed address = %v, %v", hs.Addr(), err)
	}
	if _, err := (HostService{}).DialReverse(context.Background()); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("zero HostService dial = %v, want ErrNotPermitted", err)
	}

	// A listed address really dials.
	hs = hostServer(t, echo)
	c, err := hs.DialReverse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}
