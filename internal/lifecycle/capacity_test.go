package lifecycle

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
)

// TestVMLimit: Limits.VMs bounds live VMs (provisioning to stopping); a
// stopped VM frees its slot, and resume needs one again.
func TestVMLimit(t *testing.T) {
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Limits = domain.Limits{VMs: 2} })
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "a"}))
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "b"}))
	_, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"})
	de := domain.AsError(err)
	if de.Code != domain.CodeResourceExhausted || !de.Retryable || de.Details["resource"] != "vms" ||
		de.Details["live"] != 2 || de.Details["limit"] != 2 {
		t.Fatalf("third VM: %+v", de)
	}
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "a", false))
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"}))
	if _, err := e.svc.Resume(ctx, e.req(""), "a", api.ResumeVMRequest{}); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("resume over the limit: %v", err)
	}
	if _, err := e.svc.Fork(ctx, e.req(e.newKey()), "a", api.ForkVMRequest{}); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("fork over the limit: %v", err)
	}
	if c := e.svc.Capabilities().Capacity; c == nil || c.LiveVMs != 2 || c.Slots != 0 || c.Limits.VMs != 2 {
		t.Fatalf("capacity at the limit: %+v", c)
	}
}

// TestMemoryFloor: admission keeps the floor of host memory available,
// counting VMs still provisioning (not yet in the measurement).
func TestMemoryFloor(t *testing.T) {
	free := new(atomic.Int64)
	free.Store(2 * domain.GiB)
	e := newEnv(t, fake.Options{BootDelay: 500 * time.Millisecond}, func(c *Config) {
		c.FreeMemory = func() (int64, error) { return free.Load(), nil }
	})
	ctx := context.Background()
	_, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "a"})
	de := domain.AsError(err)
	if de.Code != domain.CodeResourceExhausted || !de.Retryable || de.Details["resource"] != "memory" ||
		de.Details["freeBytes"] != int64(2*domain.GiB) ||
		de.Details["needBytes"] != domain.DefaultShape.MemoryBytes+domain.DefaultMinFreeMemoryBytes {
		t.Fatalf("below the default floor: %+v", de)
	}
	// 5 GiB free: two default VMs (2 GiB each) fit while both provision;
	// a third would leave 1 GiB - 2 GiB.
	free.Store(5 * domain.GiB)
	a, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if c := e.svc.Capabilities().Capacity; c == nil || c.FreeMemoryBytes != 5*domain.GiB || c.LiveVMs != 2 || c.Slots != 0 ||
		c.Allocated.MemoryBytes != 4*domain.GiB {
		t.Fatalf("capacity while provisioning: %+v", c)
	}
	if _, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"}); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("third VM against pending memory: %v", err)
	}
	e.mustSucceed(a, nil)
	e.mustSucceed(b, nil)
	// Booted VMs are no longer pending: only the measurement counts.
	if _, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"}); err != nil {
		t.Fatalf("after boot: %v", err)
	}
}

func TestMemoryFloorDisabledAndUnsupported(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.FreeMemory = func() (int64, error) { return 0, nil }
		c.Limits.MinFreeMemoryBytes = -1
	})
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "a"}))

	u := newEnv(t, fake.Options{}, func(c *Config) {
		c.FreeMemory = func() (int64, error) { return 0, errors.ErrUnsupported }
	})
	u.mustSucceed(u.svc.Create(ctx, u.req(u.newKey()), api.CreateVMRequest{Name: "a"}))
	if c := u.svc.Capabilities().Capacity; c == nil || c.FreeMemoryBytes != 0 || c.Slots != domain.MaxSlots {
		t.Fatalf("unmeasured memory: %+v", c)
	}

	f := newEnv(t, fake.Options{}, func(c *Config) {
		c.FreeMemory = func() (int64, error) { return 0, errors.New("boom") }
	})
	if _, err := f.svc.Create(ctx, f.req(f.newKey()), api.CreateVMRequest{Name: "a"}); err == nil {
		t.Fatal("a failing memory probe must not admit")
	}
}

// TestCapacityReport: slots are the minimum over the limits and floors for
// the default shape.
func TestCapacityReport(t *testing.T) {
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.FreeMemory = func() (int64, error) { return 64 * domain.GiB, nil }
		c.DiskSpace = func() (domain.DiskUsage, error) { return domain.DiskUsage{FreeBytes: 500 * domain.GiB}, nil }
		c.Limits = domain.Limits{CPUs: 9, VMs: 10, MinFreeMemoryBytes: domain.GiB}
	})
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "a"}))
	c := e.svc.Capabilities().Capacity
	if c == nil {
		t.Fatal("no capacity report")
	}
	// CPUs: (9-2)/2 = 3; VMs: 9; memory: (64-1)/2 = 31.
	if c.Slots != 3 || c.LiveVMs != 1 || c.Allocated.CPUs != 2 || c.FreeMemoryBytes != 64*domain.GiB ||
		c.FreeDiskBytes != 500*domain.GiB || c.Limits.CPUs != 9 {
		t.Fatalf("capacity: %+v", c)
	}
	plain := newEnv(t, fake.Options{}, nil)
	if c := plain.svc.Capabilities().Capacity; c == nil || c.Slots != domain.MaxSlots || c.LiveVMs != 0 {
		t.Fatalf("unlimited capacity: %+v", c)
	}
}
