package lifecycle

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
	"davit/internal/state"
)

// Crash consistency: the daemon is killed at every durable step of each
// lifecycle mutation, restarted on the same store, and must converge.
//
// A kill at step N is modelled with the store's write-fault hook: journal
// writes 1..N-1 of the run commit, write N and every later one fail (a
// killed process makes nothing durable after its last commit), then the
// service is dropped and a new one starts on the same files. N runs from 1
// until the mutation finishes before reaching N, so every commit point of
// every phase is a crash point. After the restart:
//   - every operation finishes (recovery replays or fails it) and no VM
//     keeps an active operation or a transitional state;
//   - the store passes SQLite's integrity and foreign-key checks and every
//     record decodes with its invariants;
//   - runtime instances match the journal: exactly the ready VMs of their
//     current generation have one;
//   - a VM left in error recovers (G9) to ready;
//   - a VM whose policy is noOutputRetention keeps no output.

var errKilled = errors.New("daemon killed (crash test)")

type crashCase struct {
	name string
	// setup runs before the crash point is armed; it returns the VM ref.
	setup func(e *env) string
	// act starts the mutation and returns its operation (nil for a
	// synchronous mutation).
	act func(e *env, ref string) (*domain.Operation, error)
}

func crashCases() []crashCase {
	ctx := context.Background()
	created := func(e *env) string {
		e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"}))
		return "c"
	}
	stopped := func(e *env) string {
		created(e)
		e.mustSucceed(e.svc.Stop(ctx, e.req(e.newKey()), "c", false))
		return "c"
	}
	accepted := func(a *Accepted, err error) (*domain.Operation, error) {
		if err != nil {
			return nil, err
		}
		return &a.Operation, nil
	}
	return []crashCase{
		{"create", func(*env) string { return "c" }, func(e *env, _ string) (*domain.Operation, error) {
			return accepted(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"}))
		}},
		{"stop", created, func(e *env, ref string) (*domain.Operation, error) {
			return accepted(e.svc.Stop(ctx, e.req(e.newKey()), ref, false))
		}},
		{"resume", stopped, func(e *env, ref string) (*domain.Operation, error) {
			return accepted(e.svc.Resume(ctx, e.req(e.newKey()), ref, api.ResumeVMRequest{}))
		}},
		{"snapshot", stopped, func(e *env, ref string) (*domain.Operation, error) {
			return accepted(e.svc.Snapshot(ctx, e.req(e.newKey()), ref, api.SnapshotVMRequest{Name: "s1"}))
		}},
		{"fork", stopped, func(e *env, ref string) (*domain.Operation, error) {
			return accepted(e.svc.Fork(ctx, e.req(e.newKey()), ref, api.ForkVMRequest{Name: "f"}))
		}},
		{"delete", created, func(e *env, ref string) (*domain.Operation, error) {
			return accepted(e.svc.Delete(ctx, e.req(e.newKey()), ref))
		}},
		{"privacy-purge", func(e *env) string {
			ref := created(e)
			vm := e.vm(ref)
			err := e.db.Tx(ctx, func(tx *state.Tx) error {
				if err := tx.PutProcess(&state.ProcessRecord{ID: "p-crash", VMID: vm.ID, Owner: owner,
					Generation: vm.InstanceGeneration, CreatedAt: time.Now(), Status: "exited"}); err != nil {
					return err
				}
				return tx.AppendOutput("p-crash", state.OutputChunk{Seq: 1, Stream: api.StreamStdout, Data: []byte("secret")})
			})
			if err != nil {
				e.t.Fatal(err)
			}
			return ref
		}, func(e *env, ref string) (*domain.Operation, error) {
			_, err := e.svc.Update(ctx, e.req(e.newKey()), ref,
				api.UpdateVMRequest{Privacy: &api.PrivacyPolicy{NoOutputRetention: true}})
			return nil, err
		}},
	}
}

func TestCrashConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("crash matrix")
	}
	// The gate kills at the first three writes and every third after (the
	// whole matrix takes ~90 s on a laptop); DAVIT_CRASH_MATRIX=full
	// kills at every one.
	full := os.Getenv("DAVIT_CRASH_MATRIX") == "full"
	for _, c := range crashCases() {
		t.Run(c.name, func(t *testing.T) {
			tried := 0
			for n := int64(1); ; n++ {
				if !full && n > 3 && n%3 != 0 {
					continue
				}
				tried++
				if crashAt(t, c, n) {
					t.Logf("%s: finishes before write %d; %d crash points tried, all converge", c.name, n, tried-1)
					return
				}
				if n > 200 {
					t.Fatalf("%s: still crashing at write %d; the mutation never finishes", c.name, n)
				}
			}
		})
	}
}

// crashAt kills the daemon at journal write n of c and checks recovery. It
// reports whether c finished before write n (no crash happened).
func crashAt(t *testing.T, c crashCase, n int64) (finished bool) {
	// A failure being recorded when the kill lands retries with backoff on
	// a context Close does not cancel; keep that short here.
	e := newEnv(t, fake.Options{}, func(cfg *Config) { cfg.FailureRetry = time.Millisecond })
	ref := c.setup(e)
	var writes atomic.Int64
	e.db.InjectWriteFault(func() error {
		if writes.Add(1) >= n {
			return errKilled
		}
		return nil
	})
	op, err := c.act(e, ref)
	if err != nil && writes.Load() == 0 {
		t.Fatalf("%s refused before any write (not a crash): %v", c.name, err)
	}
	// Let the mutation run until it hits the kill (or ends).
	deadline := time.Now().Add(10 * time.Second)
	for writes.Load() < n && time.Now().Before(deadline) {
		if op == nil && (err != nil || writes.Load() > 0) {
			break // a synchronous mutation is done
		}
		if op != nil {
			if cur, gerr := e.svc.GetOperation(context.Background(), owner, op.ID); gerr == nil && cur.State.Terminal() {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	finished = writes.Load() < n
	e.stop() // the kill: nothing after write n-1 is durable
	e.start()
	converged(t, e, c.name, n)
	return finished
}

// converged checks the invariants above on the restarted daemon.
func converged(t *testing.T, e *env, name string, n int64) {
	t.Helper()
	ctx := context.Background()
	where := func(f string, a ...any) {
		t.Helper()
		t.Fatalf("%s, killed at write %d: "+f, append([]any{name, n}, a...)...)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		var ops []*domain.Operation
		if err := e.db.Tx(ctx, func(tx *state.Tx) (err error) { ops, err = tx.UnfinishedOperations(); return err }); err != nil {
			where("unfinished operations: %v", err)
		}
		if len(ops) == 0 {
			break
		}
		if time.Now().After(deadline) {
			where("operation %s (%s) never finished after the restart: %s", ops[0].ID, ops[0].Kind, ops[0].Phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := e.db.IntegrityCheck(ctx); err != nil {
		where("%v", err)
	}
	var vms []*state.VMRecord
	if err := e.db.Tx(ctx, func(tx *state.Tx) (err error) {
		vms, _, err = tx.ListVMs(owner, nil, state.Page{Limit: 100})
		return err
	}); err != nil {
		where("list VMs: %v", err)
	}
	insts, err := e.rt.Discover(ctx)
	if err != nil {
		where("discover: %v", err)
	}
	running := map[domain.VMID]int64{}
	for _, h := range insts {
		running[h.Key.VMID] = h.Key.Generation
	}
	for _, r := range vms {
		vm := r.VM
		if err := vm.CheckInvariants(); err != nil {
			where("vm %s: %v", vm.ID, err)
		}
		if vm.ActiveOperationID != "" {
			where("vm %s keeps active operation %s", vm.ID, vm.ActiveOperationID)
		}
		switch vm.State {
		case domain.StateReady, domain.StateRunning:
			if g, ok := running[vm.ID]; !ok || g != vm.InstanceGeneration {
				where("vm %s is %s with no instance of generation %d (instances %v)", vm.ID, vm.State, vm.InstanceGeneration, running)
			}
		case domain.StateStopped, domain.StateError, domain.StateDeleted:
			if g, ok := running[vm.ID]; ok {
				where("vm %s is %s but instance generation %d still runs", vm.ID, vm.State, g)
			}
		default:
			where("vm %s left in transitional state %s", vm.ID, vm.State)
		}
		delete(running, vm.ID)
		if vm.Privacy.NoOutputRetention {
			var chunks []state.OutputChunk
			e.db.Tx(ctx, func(tx *state.Tx) (err error) { chunks, err = tx.ReadOutput("p-crash", 0, 10); return err })
			if len(chunks) != 0 {
				where("vm %s has noOutputRetention but %d output chunks survived", vm.ID, len(chunks))
			}
		}
		if vm.State == domain.StateError {
			a, err := e.svc.Recover(ctx, e.req(e.newKey()), string(vm.ID), api.RecoverVMRequest{})
			if err != nil {
				where("recover %s: %v", vm.ID, err)
			}
			if op := e.wait(a.Operation.ID); op.State != domain.OpSucceeded {
				where("recover %s: %s %+v", vm.ID, op.State, op.Error)
			}
		}
	}
	for id := range running {
		where("instance of %s has no VM record (orphan)", id)
	}
}
