package lifecycle

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
	"davit/internal/state"
)

// TestReclaimForRoom: a create refused for room stops the idle VM with the
// oldest activity first (reason reclaim, in the event stream), never a busy
// one, one without an idle timeout, an ephemeral one or one idle less than
// reclaimIdleAfter; with nothing reclaimable it fails at once.
func TestReclaimForRoom(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	var busy sync.Map
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.Now, c.SweepInterval, c.Limits = clock, time.Hour, domain.Limits{VMs: 5}
		c.Busy = func(vm domain.VMID, _ int64) bool { _, ok := busy.Load(vm); return ok }
	})
	ctx := context.Background()
	no := false
	box := func(name string, idle int64, privacy *api.PrivacyPolicy) {
		e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: name, AutoStop: &no, IdleTimeoutSeconds: idle, Privacy: privacy}))
	}
	box("old", 3600, nil)
	advance(time.Minute)
	box("newer", 3600, nil)
	box("busy", 3600, nil)
	box("plain", 0, nil)
	box("eph", 3600, &api.PrivacyPolicy{Ephemeral: true})
	busy.Store(e.vm("busy").ID, true)

	// "old" has been idle 2.5 minutes, "newer" 1.5 (under the default 2).
	advance(90 * time.Second)
	if c := e.svc.Capabilities().Capacity; c.Reclaimable != 1 {
		t.Fatalf("reclaimable: %d", c.Reclaimable)
	}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "x"}))
	if s := e.vm("old").State; s != domain.StateStopped {
		t.Fatalf("oldest idle VM is %s, want stopped", s)
	}
	for _, n := range []string{"newer", "busy", "plain", "eph"} {
		if s := e.vm(n).State; s != domain.StateReady {
			t.Fatalf("%s is %s", n, s)
		}
	}
	if _, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "y"}); !domain.IsRoomError(err) {
		t.Fatalf("create with nothing idle long enough: %v", err)
	}
	advance(time.Minute)
	e.svc.Touch(e.vm("newer").ID) // active again
	if _, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "y"}); !domain.IsRoomError(err) {
		t.Fatalf("create with nothing reclaimable: %v", err)
	}

	// The reclaim is a stop with reason reclaim, visible in the stream.
	var reasons []string
	oldID := e.vm("old").ID
	if err := e.db.Tx(ctx, func(tx *state.Tx) error {
		rows, err := tx.ListEvents("", 0, 1000, state.EventFilter{Types: []string{"operation.created"}, VMID: oldID})
		for _, r := range rows {
			if strings.Contains(string(r.Data), `"reason":"`) {
				reasons = append(reasons, string(r.Data))
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 1 || !strings.Contains(reasons[0], `"reason":"reclaim"`) || !strings.Contains(reasons[0], `"kind":"stop"`) {
		t.Fatalf("operation.created events with a reason: %v", reasons)
	}
}

// TestCapacityWait: with no room, a request with a wait queues FIFO (seen in
// the capacity report under its ticket) and is admitted when a VM stops; a
// request without a wait fails at once while someone waits; a wait that
// runs out answers resource_exhausted with waitedSeconds.
func TestCapacityWait(t *testing.T) {
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Limits = domain.Limits{VMs: 1} })
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "a"}))

	_, err := e.svc.Create(ctx, waitReq(e, e.newKey(), time.Second), api.CreateVMRequest{Name: "late"})
	if de := domain.AsError(err); de.Code != domain.CodeResourceExhausted || de.Details["waitedSeconds"] != int64(1) || de.Details["resource"] != "vms" {
		t.Fatalf("wait ran out: %+v", de)
	}

	key := e.newKey()
	type result struct {
		a   *Accepted
		err error
	}
	done := make(chan result, 1)
	go func() {
		a, err := e.svc.Create(ctx, waitReq(e, key, 30*time.Second), api.CreateVMRequest{Name: "b"})
		done <- result{a, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := e.svc.Capabilities().Capacity.Waiting
		if len(w) == 1 && w[0].Ticket == domain.CapacityTicket(key) && w[0].MemoryBytes == domain.DefaultShape.MemoryBytes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting: %+v", w)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, err = e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"})
	if de := domain.AsError(err); de.Code != domain.CodeResourceExhausted || de.Details["resource"] != "queue" || de.Details["waiting"] != 1 {
		t.Fatalf("no-wait request behind a waiter: %+v", de)
	}
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "a", false))
	select {
	case r := <-done:
		e.mustSucceed(r.a, r.err)
	case <-time.After(10 * time.Second):
		t.Fatal("waiter not admitted after the stop")
	}
	if w := e.svc.Capabilities().Capacity.Waiting; len(w) != 0 {
		t.Fatalf("queue after admission: %+v", w)
	}
	if _, err := e.svc.Create(ctx, waitReq(e, e.newKey(), 2*time.Hour), api.CreateVMRequest{Name: "d"}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("wait over the maximum: %v", err)
	}
}

func waitReq(e *env, key string, wait time.Duration) Request {
	r := e.req(key)
	r.CapacityWait = wait
	return r
}

// TestMemoryReservation: live VMs' declared memory may sum to
// limits.memoryBytes × memoryOvercommit (default 1.5), reported as
// memoryBudgetBytes.
func TestMemoryReservation(t *testing.T) {
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Limits = domain.Limits{MemoryBytes: 4 * domain.GiB} })
	ctx := context.Background()
	for _, n := range []string{"a", "b", "c"} { // 3 × 2 GiB = 6 GiB = 4 GiB × 1.5
		e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: n}))
	}
	_, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "d"})
	if de := domain.AsError(err); de.Code != domain.CodeResourceExhausted || de.Details["resource"] != "memory" ||
		de.Details["budgetBytes"] != int64(6*domain.GiB) || de.Details["reservedBytes"] != int64(6*domain.GiB) {
		t.Fatalf("over the budget: %+v", de)
	}
	if c := e.svc.Capabilities().Capacity; c.MemoryBudgetBytes != 6*domain.GiB || c.Slots != 0 {
		t.Fatalf("capacity: %+v", c)
	}
	if b := (domain.Limits{MemoryBytes: 4 * domain.GiB, MemoryOvercommit: 1}).MemoryBudget(); b != 4*domain.GiB {
		t.Fatalf("overcommit 1: %d", b)
	}
}

// TestReclaimVMs: reclaimVMs stops the caller's VMs idle at least
// idleSeconds (no idle timeout needed), filtered by labels, never busy
// ones; dryRun only lists.
func TestReclaimVMs(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	var busy sync.Map
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.Now, c.SweepInterval = clock, time.Hour
		c.Busy = func(vm domain.VMID, _ int64) bool { _, ok := busy.Load(vm); return ok }
	})
	ctx := context.Background()
	no := false
	lbl := map[string]string{domain.LabelBox: "p"}
	for _, n := range []string{"b1", "b2", "other"} {
		body := api.CreateVMRequest{Name: n, AutoStop: &no}
		if n != "other" {
			body.Labels = lbl
		}
		e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), body))
	}
	busy.Store(e.vm("b2").ID, true)
	mu.Lock()
	now = now.Add(11 * time.Minute)
	mu.Unlock()
	if r, err := e.svc.Reclaim(ctx, owner, api.ReclaimVMsRequest{IdleSeconds: 3600, Labels: lbl}); err != nil || len(r.Items) != 0 {
		t.Fatalf("nothing idle an hour: %+v %v", r, err)
	}
	r, err := e.svc.Reclaim(ctx, owner, api.ReclaimVMsRequest{IdleSeconds: 600, Labels: lbl, DryRun: true})
	if err != nil || len(r.Items) != 1 || r.Items[0].Name != "b1" || r.Items[0].OperationID != "" || r.Items[0].IdleSeconds < 600 {
		t.Fatalf("dry run: %+v %v", r, err)
	}
	if s := e.vm("b1").State; s != domain.StateReady {
		t.Fatalf("dry run stopped b1: %s", s)
	}
	r, err = e.svc.Reclaim(ctx, owner, api.ReclaimVMsRequest{IdleSeconds: 600, Labels: lbl})
	if err != nil || len(r.Items) != 1 || r.Items[0].OperationID == "" {
		t.Fatalf("reclaim: %+v %v", r, err)
	}
	if op := e.wait(r.Items[0].OperationID); op.State != domain.OpSucceeded || op.Reason != "gc" {
		t.Fatalf("gc stop: %+v", op)
	}
	for n, want := range map[string]domain.VMState{"b1": domain.StateStopped, "b2": domain.StateReady, "other": domain.StateReady} {
		if s := e.vm(n).State; s != want {
			t.Fatalf("%s is %s, want %s", n, s, want)
		}
	}
	if r, err := e.svc.Reclaim(ctx, "local:someone-else", api.ReclaimVMsRequest{}); err != nil || len(r.Items) != 0 {
		t.Fatalf("another principal reclaimed: %+v %v", r, err)
	}
}
