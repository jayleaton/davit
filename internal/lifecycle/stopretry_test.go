package lifecycle

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
)

// stopEvents returns the retry policy's messages of op (attempts, force).
func (e *env) stopEvents(op domain.OperationID) []string {
	e.t.Helper()
	_, evs, err := e.svc.Events(context.Background(), owner, op, 0, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Phase == stopPhase && (strings.HasPrefix(ev.Message, stopAttemptPrefix) || strings.HasPrefix(ev.Message, stopForcedPrefix)) {
			out = append(out, ev.Message)
		}
	}
	return out
}

const testGrace = 20 * time.Millisecond

func stopPolicy(attempts int, noForce bool) func(*Config) {
	return func(c *Config) { c.StopGrace, c.StopAttempts, c.NoStopForce = testGrace, attempts, noForce }
}

func TestStopRetriesThenForces(t *testing.T) {
	// The guest never shuts down within the grace.
	e := newEnv(t, fake.Options{StopDelay: time.Hour}, stopPolicy(3, false))
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "hung"}))
	op := e.mustSucceed(e.svc.Stop(ctx, e.req(""), "hung", false))

	evs := e.stopEvents(op.ID)
	if len(evs) != 4 {
		t.Fatalf("events %q, want 3 attempts and the force", evs)
	}
	for i := 0; i < 3; i++ {
		if want := fmt.Sprintf("attempt %d/3: guest did not shut down within %s", i+1, testGrace); evs[i] != want {
			t.Fatalf("event %d = %q, want %q", i, evs[i], want)
		}
	}
	if !strings.HasPrefix(evs[3], "stop forced after 3 graceful attempts") {
		t.Fatalf("force event %q", evs[3])
	}
	snap, err := e.svc.GetSnapshot(ctx, owner, string(op.Result.SnapshotID))
	if err != nil || snap.Consistency != domain.ConsistencyCrash {
		t.Fatalf("forced stop snapshot: %+v %v", snap, err)
	}
	if vm := e.vm("hung"); vm.State != domain.StateStopped {
		t.Fatalf("vm %s", vm.State)
	}
}

func TestStopSucceedsOnSecondAttempt(t *testing.T) {
	e := newEnv(t, fake.Options{}, stopPolicy(3, false))
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "slow"}))
	e.rt.InjectFault("Stop", domain.Errorf(domain.CodeTimeout, "guest hung")) // first attempt only
	op := e.mustSucceed(e.svc.Stop(ctx, e.req(""), "slow", false))
	if evs := e.stopEvents(op.ID); len(evs) != 1 || !strings.HasPrefix(evs[0], "attempt 1/3:") {
		t.Fatalf("events %q, want one failed attempt", evs)
	}
	snap, err := e.svc.GetSnapshot(ctx, owner, string(op.Result.SnapshotID))
	if err != nil || snap.Consistency != domain.ConsistencyClean {
		t.Fatalf("clean stop on attempt 2: %+v %v", snap, err)
	}
}

func TestStopRetriesWithoutForceFails(t *testing.T) {
	e := newEnv(t, fake.Options{StopDelay: time.Hour}, stopPolicy(2, true))
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "nf"}))
	first := e.vm("nf")
	a, err := e.svc.Stop(ctx, e.req(""), "nf", false)
	if err != nil {
		t.Fatal(err)
	}
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpFailed || op.Error == nil || op.Error.Code != domain.CodeTimeout {
		t.Fatalf("stop without force: %s %+v", op.State, op.Error)
	}
	if evs := e.stopEvents(op.ID); len(evs) != 2 || !strings.HasPrefix(evs[1], "attempt 2/2:") {
		t.Fatalf("events %q, want 2 attempts and no force", evs)
	}
	if vm := e.vm("nf"); vm.State != domain.StateError || vm.LastSnapshotID != first.LastSnapshotID {
		t.Fatalf("failed stop: %s last %s", vm.State, vm.LastSnapshotID)
	}
}

func TestStopRetryCountSurvivesRestart(t *testing.T) {
	// One attempt fails before the daemon dies; the replay has two left.
	e := newEnv(t, fake.Options{StopDelay: time.Hour}, func(c *Config) {
		c.StopGrace, c.StopAttempts = 300*time.Millisecond, 3
	})
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "rs"}))
	a, err := e.svc.Stop(ctx, e.req(""), "rs", false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(e.stopEvents(a.Operation.ID)) < 1 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	e.stop()
	e.cfg = stopPolicy(3, false)
	e.start()
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpSucceeded {
		t.Fatalf("replayed stop: %s %+v", op.State, op.Error)
	}
	evs := e.stopEvents(op.ID)
	var attempts []string
	for _, m := range evs {
		if strings.HasPrefix(m, "attempt ") {
			attempts = append(attempts, m[:len("attempt 1/3")])
		}
	}
	if strings.Join(attempts, ",") != "attempt 1/3,attempt 2/3,attempt 3/3" || !strings.HasPrefix(evs[len(evs)-1], "stop forced after 3") {
		t.Fatalf("events across restart %q", evs)
	}
	snap, _ := e.svc.GetSnapshot(ctx, owner, string(op.Result.SnapshotID))
	if snap.Consistency != domain.ConsistencyCrash {
		t.Fatalf("snapshot %s", snap.Consistency)
	}
}
