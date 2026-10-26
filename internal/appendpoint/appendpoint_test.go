package appendpoint

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/gateway"
)

type fakeVMs struct {
	mu      sync.Mutex
	vm      domain.VM
	changed chan struct{}
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

type cut struct {
	vm   domain.VMID
	keep int64
}

type fakeGW struct {
	mu     sync.Mutex
	routes []gateway.AppRoute
	cuts   []cut
}

func (g *fakeGW) SetApps(r []gateway.AppRoute) { g.mu.Lock(); g.routes = r; g.mu.Unlock() }
func (g *fakeGW) CutApps(vm domain.VMID, keep int64) {
	g.mu.Lock()
	g.cuts = append(g.cuts, cut{vm, keep})
	g.mu.Unlock()
}
func (g *fakeGW) AppBase() string { return "https://gw.ts.net" }

func (g *fakeGW) state() ([]gateway.AppRoute, []cut) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gateway.AppRoute(nil), g.routes...), append([]cut(nil), g.cuts...)
}

func code(err error) domain.Code {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func newVMs() *fakeVMs {
	return &fakeVMs{vm: domain.VM{ID: "vm_1", Name: "box", State: domain.StateReady, InstanceGeneration: 1}, changed: make(chan struct{})}
}

func open(t *testing.T, path string, vms *fakeVMs) (*Service, *fakeGW) {
	t.Helper()
	s, err := New(Config{Path: path, VMs: vms, Ports: [2]int{20000, 20001}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	gw := &fakeGW{}
	s.SetGateway(gw)
	return s, gw
}

func TestCreateListUpdateDelete(t *testing.T) {
	ctx := context.Background()
	vms := newVMs()
	if _, err := New(Config{Path: filepath.Join(t.TempDir(), "x.json"), VMs: vms}); err != nil {
		t.Fatal(err)
	}
	noGW, _ := New(Config{Path: filepath.Join(t.TempDir(), "x.json"), VMs: vms})
	defer noGW.Close()
	if _, err := noGW.Create(ctx, "alice", "box", api.CreateAppEndpointRequest{Port: 3000}); code(err) != domain.CodeUnsupportedCapability {
		t.Fatalf("without a gateway: %v", err)
	}

	path := filepath.Join(t.TempDir(), "app-endpoints.json")
	s, gw := open(t, path, vms)
	ep, err := s.Create(ctx, "alice", "box", api.CreateAppEndpointRequest{Port: 3000})
	if err != nil || ep.Name != "port-3000" || ep.GatewayPort != 20000 || ep.URL != "https://gw.ts.net:20000/" || ep.State != api.AppEndpointActive {
		t.Fatalf("create: %+v %v", ep, err)
	}
	for _, tc := range []struct {
		req  api.CreateAppEndpointRequest
		want domain.Code
	}{
		{api.CreateAppEndpointRequest{Port: 3000, Name: "other"}, domain.CodeInvalidState}, // same guest port
		{api.CreateAppEndpointRequest{Port: 3001, Name: "port-3000"}, domain.CodeInvalidState},
		{api.CreateAppEndpointRequest{Port: 3001, Name: "a--b"}, domain.CodeInvalidArgument},
		{api.CreateAppEndpointRequest{Port: 3001, Name: "Web"}, domain.CodeInvalidArgument},
		{api.CreateAppEndpointRequest{Port: 0}, domain.CodeInvalidArgument},
	} {
		if _, err := s.Create(ctx, "alice", "box", tc.req); code(err) != tc.want {
			t.Errorf("%+v: %v, want %s", tc.req, err, tc.want)
		}
	}
	if _, err := s.Create(ctx, "mallory", "box", api.CreateAppEndpointRequest{Port: 4000}); code(err) != domain.CodeNotFound {
		t.Fatalf("other principal: %v", err)
	}
	web, err := s.Create(ctx, "alice", "box", api.CreateAppEndpointRequest{Port: 8080, Name: "web"})
	if err != nil || web.GatewayPort != 20001 {
		t.Fatalf("second: %+v %v", web, err)
	}
	if _, err := s.Create(ctx, "alice", "box", api.CreateAppEndpointRequest{Port: 9090}); code(err) != domain.CodeResourceExhausted {
		t.Fatalf("port range exhausted: %v", err)
	}
	if routes, _ := gw.state(); len(routes) != 2 {
		t.Fatalf("routes %+v", routes)
	}

	// Rename keeps the URL; hide withdraws the route; show restores it.
	name := "site"
	up, err := s.Update(ctx, "alice", "box", "web", api.UpdateAppEndpointRequest{Name: &name})
	if err != nil || up.Name != "site" || up.URL != web.URL {
		t.Fatalf("rename: %+v %v", up, err)
	}
	hidden := true
	if up, _ = s.Update(ctx, "alice", "box", "site", api.UpdateAppEndpointRequest{Hidden: &hidden}); up.State != api.AppEndpointHidden {
		t.Fatalf("hide: %+v", up)
	}
	if routes, _ := gw.state(); len(routes) != 1 {
		t.Fatalf("routes after hide %+v", routes)
	}

	// The store survives a restart.
	s.Close()
	s2, gw2 := open(t, path, vms)
	l, err := s2.List(ctx, "alice", "box")
	if err != nil || len(l) != 2 || l[1].Name != "site" || !l[1].Hidden {
		t.Fatalf("reloaded list: %+v %v", l, err)
	}
	if routes, _ := gw2.state(); len(routes) != 1 {
		t.Fatalf("reloaded routes %+v", routes)
	}
	// Delete frees the gateway port for the next endpoint.
	if err := s2.Delete(ctx, "alice", "box", ep.ID); err != nil {
		t.Fatal(err)
	}
	if err := s2.Delete(ctx, "alice", "box", ep.ID); code(err) != domain.CodeNotFound {
		t.Fatalf("second delete: %v", err)
	}
	if again, err := s2.Create(ctx, "alice", "box", api.CreateAppEndpointRequest{Port: 5000}); err != nil || again.GatewayPort != 20000 {
		t.Fatalf("port reuse: %+v %v", again, err)
	}
}

// TestLifecycleCutsAndForgets: a new generation cuts the old one's
// connections, stop cuts all and lists vm_not_running, delete forgets.
func TestLifecycleCutsAndForgets(t *testing.T) {
	ctx := context.Background()
	vms := newVMs()
	s, gw := open(t, filepath.Join(t.TempDir(), "e.json"), vms)
	if _, err := s.Create(ctx, "alice", "box", api.CreateAppEndpointRequest{Port: 3000}); err != nil {
		t.Fatal(err)
	}
	waitCut := func(want cut) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, cuts := gw.state(); len(cuts) > 0 && cuts[len(cuts)-1] == want {
				return
			}
			if time.Now().After(deadline) {
				_, cuts := gw.state()
				t.Fatalf("no cut %+v (cuts %+v)", want, cuts)
			}
		}
	}
	vms.set(domain.StateRunning, 2)
	waitCut(cut{"vm_1", 2})
	vms.set(domain.StateStopped, 2)
	waitCut(cut{"vm_1", 0})
	if l, _ := s.List(ctx, "alice", "box"); len(l) != 1 || l[0].State != api.AppEndpointVMNotRunning {
		t.Fatalf("list while stopped: %+v", l)
	}
	vms.set(domain.StateDeleted, 2)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if routes, _ := gw.state(); len(routes) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deleted VM's endpoint still routed")
		}
	}
}
