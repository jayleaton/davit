package lifecycle

import (
	"context"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
)

func ptr[T any](v T) *T { return &v }

// TestUpdateRenameAndExtend: PATCH renames a VM and restarts its auto-stop
// countdown; it is guarded by If-Match and refuses deleted VMs.
func TestUpdateRenameAndExtend(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Now = clock; c.SweepInterval = time.Hour })
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "box", TTLSeconds: ptr(int64(600))}))
	first := *e.vm("box").ExpiresAt

	mu.Lock()
	now = now.Add(5 * time.Minute)
	mu.Unlock()
	vm, err := e.svc.Update(ctx, e.req(""), "box", api.UpdateVMRequest{TTLSeconds: ptr(int64(3600))})
	if err != nil || vm.TTLSeconds != 3600 || !vm.ExpiresAt.Equal(clock().UTC().Add(time.Hour)) || !vm.ExpiresAt.After(first) {
		t.Fatalf("extend: %+v %v", vm, err)
	}
	rev := vm.StateRevision
	vm, err = e.svc.Update(ctx, e.req(""), "box", api.UpdateVMRequest{Name: ptr("renamed")})
	if err != nil || vm.Name != "renamed" || vm.StateRevision != rev+1 {
		t.Fatalf("rename: %+v %v", vm, err)
	}
	if _, err := e.svc.GetVM(ctx, owner, "box"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("old name still resolves: %v", err)
	}
	stale := Request{Principal: owner, IfMatch: ptr(rev)}
	if _, err := e.svc.Update(ctx, stale, "renamed", api.UpdateVMRequest{Name: ptr("x")}); !domain.IsCode(err, domain.CodeRevisionMismatch) {
		t.Fatalf("stale If-Match: %v", err)
	}
	if vm, err := e.svc.Update(ctx, e.req(""), "renamed", api.UpdateVMRequest{AutoStop: ptr(false)}); err != nil || vm.ExpiresAt != nil || vm.AutoStop {
		t.Fatalf("no auto-stop: %+v %v", vm, err)
	}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "other"}))
	if _, err := e.svc.Update(ctx, e.req(""), "renamed", api.UpdateVMRequest{Name: ptr("other")}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("rename onto a taken name: %v", err)
	}
	e.mustSucceed(e.svc.Delete(ctx, e.req(""), "other"))
	if _, err := e.svc.Update(ctx, e.req(""), string(e.vm(string(e.vm("renamed").ID)).ID), api.UpdateVMRequest{}); err != nil {
		t.Fatalf("empty patch: %v", err)
	}
}

// TestTTLWarningOncePerExpiry: the warning hook fires when the window opens,
// once per expiry, again after an extend, and the VM reports the window.
func TestTTLWarningOncePerExpiry(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	var warned []domain.VM
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.Now, c.SweepInterval, c.TTLWarning = clock, time.Hour, 10*time.Minute
		c.OnTTLWarning = func(_ context.Context, vm domain.VM) { warned = append(warned, vm) }
	})
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "box", TTLSeconds: ptr(int64(3600))}))
	vm := e.vm("box")
	if vm.AutoStopWarningAt == nil || !vm.AutoStopWarningAt.Equal(vm.ExpiresAt.Add(-10*time.Minute)) {
		t.Fatalf("autoStopWarningAt: %+v", vm.AutoStopWarningAt)
	}
	advance(45 * time.Minute)
	e.svc.Sweep(ctx)
	if len(warned) != 0 {
		t.Fatal("warned before the window")
	}
	advance(6 * time.Minute)
	e.svc.Sweep(ctx)
	e.svc.Sweep(ctx)
	if len(warned) != 1 || warned[0].Name != "box" || warned[0].AutoStopWarningAt == nil {
		t.Fatalf("want exactly one warning, got %d", len(warned))
	}
	if _, err := e.svc.Update(ctx, e.req(""), "box", api.UpdateVMRequest{TTLSeconds: ptr(int64(900))}); err != nil {
		t.Fatal(err)
	}
	advance(6 * time.Minute)
	e.svc.Sweep(ctx)
	if len(warned) != 2 {
		t.Fatalf("an extended VM must be warned again for its new expiry: %d", len(warned))
	}
	if s := e.vm("box").State; s != domain.StateReady {
		t.Fatalf("warned VM stopped early: %s", s)
	}
}

// TestProfilesAndCapabilities: --type picks an administrator shape, shape
// fields override it, unknown profiles are refused, and capabilities list
// profiles, listeners, services and the warning window.
func TestProfilesAndCapabilities(t *testing.T) {
	big := domain.Profile{Name: "big", Shape: domain.Shape{CPUs: 4, MemoryBytes: 4 * domain.GiB}}
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.Profiles = []domain.Profile{big}
		c.Listeners, c.Services = []string{"unix", "tailnet"}, []string{"exec"}
	})
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "a", Type: "big"}))
	if s := e.vm("a").Shape; s.CPUs != 4 || s.MemoryBytes != 4*domain.GiB || s.DiskBytes != domain.DefaultShape.DiskBytes {
		t.Fatalf("profile shape: %+v", s)
	}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "b", Type: "big", Shape: &api.ShapeOverride{CPUs: 2}}))
	if s := e.vm("b").Shape; s.CPUs != 2 || s.MemoryBytes != 4*domain.GiB {
		t.Fatalf("override on profile: %+v", s)
	}
	if _, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c", Type: "huge"}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("unknown profile: %v", err)
	}
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "a", false))
	e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "a", api.ForkVMRequest{Name: "a2", Type: "big"}))
	caps := e.svc.Capabilities()
	if len(caps.Profiles) != 1 || caps.Profiles[0].Name != "big" || len(caps.Network.Listeners) != 2 ||
		len(caps.Services) != 1 || caps.TTLWarningSeconds != 600 {
		t.Fatalf("capabilities: %+v", caps)
	}
}

// TestIdempotencyScopeIsCanonicalVM: one key addresses one operation whether
// the VM is named by ID or name, also after deletion.
func TestIdempotencyScopeIsCanonicalVM(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "box"}))
	id := string(e.vm("box").ID)
	byName := Request{Principal: owner, IdempotencyKey: "k-stop", Method: "POST", Path: "/api/v1/vms/box/stop"}
	byID := Request{Principal: owner, IdempotencyKey: "k-stop", Method: "POST", Path: "/api/v1/vms/" + id + "/stop"}
	a1, err := e.svc.Stop(ctx, byName, "box", false)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := e.svc.Stop(ctx, byID, id, false)
	if err != nil || a2.Operation.ID != a1.Operation.ID || !a2.Replayed {
		t.Fatalf("same key by ID must replay the by-name operation: %+v %v", a2, err)
	}
	e.wait(a1.Operation.ID)
	del := Request{Principal: owner, IdempotencyKey: "k-del", Method: "DELETE", Path: "/api/v1/vms/box"}
	d1 := e.mustSucceed(e.svc.Delete(ctx, del, "box"))
	d2, err := e.svc.Delete(ctx, del, "box")
	if err != nil || d2.Operation.ID != d1.ID {
		t.Fatalf("replay by name after deletion: %+v %v", d2, err)
	}
}

// TestListImagesPages: images page in (createdAt, id) order with a cursor.
func TestListImagesPages(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	imgs := []domain.Image{{ID: "img_b", Ref: "b", CreatedAt: t0}, {ID: "img_a", Ref: "a", CreatedAt: t0}, {ID: "img_c", Ref: "c", CreatedAt: t0.Add(time.Second)}}
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Images = NewStaticCatalog(imgs...) })
	ctx := context.Background()
	var got []domain.ImageID
	p := Page{Limit: 1}
	for {
		page, next, err := e.svc.ListImages(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		for _, i := range page {
			got = append(got, i.ID)
		}
		if next == "" {
			break
		}
		p.Cursor = next
	}
	if len(got) != 3 || got[0] != "img_a" || got[1] != "img_b" || got[2] != "img_c" {
		t.Fatalf("pages: %v", got)
	}
	if _, _, err := e.svc.ListImages(ctx, Page{Limit: 1, Cursor: "garbage"}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("bad cursor: %v", err)
	}
}

// TestLiveForkProfile: a live fork keeps the source's shape, so --type is
// accepted only when the profile matches it.
func TestLiveForkProfile(t *testing.T) {
	e := newEnv(t, fake.Options{LiveFork: true}, func(c *Config) {
		c.Profiles = []domain.Profile{{Name: "big", Shape: domain.Shape{CPUs: 4}}, {Name: "same", Shape: domain.DefaultShape}}
	})
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "src"}))
	if _, err := e.svc.Fork(ctx, e.req(e.newKey()), "src", api.ForkVMRequest{Name: "k1", Live: true, Type: "big"}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("live fork onto a different profile: %v", err)
	}
	e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "src", api.ForkVMRequest{Name: "k2", Live: true, Type: "same"}))
}
