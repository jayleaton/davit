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

// TestIdleStop: a VM with an idle timeout is stopped once that long passed
// with no activity, independently of its TTL; Touch restarts the countdown,
// a busy VM is never stopped, and labels and the timeout survive the stop.
func TestIdleStop(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	var busy sync.Map
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.Now, c.SweepInterval = clock, time.Hour
		c.Busy = func(vm domain.VMID, _ int64) bool { _, ok := busy.Load(vm); return ok }
	})
	ctx := context.Background()
	no := false
	labels := map[string]string{domain.LabelBox: "proj"}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "proj", AutoStop: &no, IdleTimeoutSeconds: 600, Labels: labels}))
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "plain", AutoStop: &no}))
	sweep := func() {
		t.Helper()
		if err := e.svc.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"proj", "plain"} {
			if id := e.vm(n).ActiveOperationID; id != "" {
				e.wait(id)
			}
		}
	}
	if vm := e.vm("proj"); vm.LastActivityAt == nil {
		t.Fatalf("a VM with an idle timeout reports no lastActivityAt: %+v", vm)
	}

	advance(9 * time.Minute)
	e.svc.Touch(e.vm("proj").ID) // activity restarts the countdown
	advance(9 * time.Minute)
	sweep()
	if s := e.vm("proj").State; s != domain.StateReady {
		t.Fatalf("stopped %s after 9 idle minutes (of 10)", s)
	}

	busy.Store(e.vm("proj").ID, true) // an open ssh session
	advance(20 * time.Minute)
	sweep()
	if s := e.vm("proj").State; s != domain.StateReady {
		t.Fatalf("busy VM is %s", s)
	}
	busy.Delete(e.vm("proj").ID)
	advance(11 * time.Minute)
	sweep()
	vm := e.vm("proj")
	if vm.State != domain.StateStopped {
		t.Fatalf("idle VM is %s", vm.State)
	}
	if vm.Labels[domain.LabelBox] != "proj" || vm.IdleTimeoutSeconds != 600 || vm.LastActivityAt != nil {
		t.Fatalf("after the idle stop: labels %v idle %d lastActivity %v", vm.Labels, vm.IdleTimeoutSeconds, vm.LastActivityAt)
	}
	if s := e.vm("plain").State; s != domain.StateReady {
		t.Fatalf("a VM without an idle timeout is %s", s)
	}

	// The label filter lists only the box.
	vms, _, err := e.svc.ListVMs(ctx, owner, nil, true, Page{Labels: labels})
	if err != nil || len(vms) != 1 || vms[0].Name != "proj" {
		t.Fatalf("label filter: %v %v", vms, err)
	}
	if _, _, err := e.svc.ListVMs(ctx, owner, nil, true, Page{Labels: map[string]string{"Bad Key": "x"}}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("bad label key in a filter: %v", err)
	}
	if _, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "bad", IdleTimeoutSeconds: -1}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("negative idle timeout: %v", err)
	}
	// Update changes the timeout (0 disables it).
	zero := int64(0)
	if vm, err := e.svc.Update(ctx, e.req(""), "plain", api.UpdateVMRequest{IdleTimeoutSeconds: &zero}); err != nil || vm.IdleTimeoutSeconds != 0 {
		t.Fatalf("update idle: %+v %v", vm, err)
	}
}
