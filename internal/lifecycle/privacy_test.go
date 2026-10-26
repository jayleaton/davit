package lifecycle

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
	"davit/internal/state"
)

// storageMentions lists files under the env's storage dir whose path names
// vm (its working disk is <vm>-g<gen>).
func storageMentions(t *testing.T, e *env, vm domain.VMID) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(filepath.Join(e.dir, "storage"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(p, string(vm)) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// storageGone waits until storage holds nothing of vm: a discarded disk is
// collected right after its operation succeeds (the record is updated
// first, so collection finds it unreferenced), not before.
func (e *env) storageGone(vm domain.VMID) {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		left := storageMentions(e.t, e, vm)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			e.t.Errorf("storage still holds %v", left)
			return
		}
	}
}

func (e *env) snapshotsOf(vm domain.VMID) []domain.Snapshot {
	e.t.Helper()
	snaps, _, err := e.svc.ListSnapshots(context.Background(), owner, string(vm), Page{Limit: 100})
	if err != nil && !domain.IsCode(err, domain.CodeNotFound) {
		e.t.Fatal(err)
	}
	return snaps
}

// TestEphemeralDeletedOnStop: stop deletes an ephemeral VM, commits no
// snapshot, leaves no disk, frees the name.
func TestEphemeralDeletedOnStop(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := t.Context()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "tmp", Privacy: &domain.PrivacyPolicy{Ephemeral: true}}))
	vm := e.vm("tmp")
	if !vm.Privacy.Ephemeral {
		t.Fatalf("privacy not recorded: %+v", vm.Privacy)
	}
	op := e.mustSucceed(e.svc.Stop(ctx, e.req(e.newKey()), "tmp", false))
	if op.Result == nil || op.Result.SnapshotID != "" {
		t.Fatalf("ephemeral stop committed a snapshot: %+v", op.Result)
	}
	if got := e.vm(string(vm.ID)); got.State != domain.StateDeleted {
		t.Fatalf("ephemeral VM after stop: %s", got.State)
	}
	if s := e.snapshotsOf(vm.ID); len(s) != 0 {
		t.Errorf("snapshots kept: %+v", s)
	}
	e.storageGone(vm.ID)
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "tmp"}))

	// The host event stream (G16) shows the delete like an explicit one:
	// stopping -> deleting -> deleted.
	var rows []state.EventRow
	if err := e.svc.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		rows, err = tx.ListEvents(owner, 0, 500, state.EventFilter{VMID: vm.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var states []string
	for _, r := range rows {
		if r.Type == "vm.state" {
			var d struct{ State string }
			json.Unmarshal(r.Data, &d)
			states = append(states, d.State)
		}
	}
	if n := len(states); n < 3 || states[n-3] != "stopping" || states[n-2] != "deleting" || states[n-1] != "deleted" {
		t.Errorf("vm.state events: %v, want ... stopping, deleting, deleted", states)
	}
}

// TestNoSnapshotStop: a noSnapshot VM stops without committing, cannot be
// resumed, snapshotted or forked, and its disk is gone; forks of a VM
// that keeps its disk inherit the rest of the policy.
func TestNoSnapshotStop(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := t.Context()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "ns", Privacy: &domain.PrivacyPolicy{NoSnapshot: true}}))
	vm := e.vm("ns")
	op := e.mustSucceed(e.svc.Stop(ctx, e.req(e.newKey()), "ns", false))
	got := e.vm("ns")
	if got.State != domain.StateStopped || got.LastSnapshotID != "" || op.Result.SnapshotID != "" {
		t.Fatalf("noSnapshot stop: %+v / %+v", got, op.Result)
	}
	e.storageGone(vm.ID)
	if _, err := e.svc.Resume(ctx, e.req(e.newKey()), "ns", api.ResumeVMRequest{}); !domain.IsCode(err, domain.CodeInvalidState) {
		t.Errorf("resume of a noSnapshot VM: %v", err)
	}
	if _, err := e.svc.Snapshot(ctx, e.req(e.newKey()), "ns", api.SnapshotVMRequest{Name: "keep"}); !domain.IsCode(err, domain.CodeInvalidState) {
		t.Errorf("snapshot of a noSnapshot VM: %v", err)
	}
	if _, err := e.svc.Fork(ctx, e.req(e.newKey()), "ns", api.ForkVMRequest{Name: "child"}); !domain.IsCode(err, domain.CodeInvalidState) {
		t.Errorf("fork of a noSnapshot VM: %v", err)
	}

	quiet := &domain.PrivacyPolicy{NoOutputRetention: true, NoProcessSamples: true}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "quiet", Privacy: quiet}))
	e.mustSucceed(e.svc.Stop(ctx, e.req(e.newKey()), "quiet", false))
	if e.vm("quiet").LastSnapshotID == "" {
		t.Fatal("a VM that keeps its disk committed nothing")
	}
	e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "quiet", api.ForkVMRequest{Name: "quiet-child"}))
	if p := e.vm("quiet-child").Privacy; p != *quiet {
		t.Errorf("fork did not inherit the policy: %+v", p)
	}
}

// TestEphemeralStopCrashReplays: the daemon dies in the middle of an
// ephemeral VM's stop; recovery still deletes it, and a policy change
// during the stop is refused.
func TestEphemeralStopCrashReplays(t *testing.T) {
	e := newEnv(t, fake.Options{StopDelay: 400 * time.Millisecond}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "crash", Privacy: &domain.PrivacyPolicy{Ephemeral: true}}))
	vm := e.vm("crash")
	a, err := e.svc.Stop(ctx, e.req(e.newKey()), "crash", false)
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, a.Operation.ID, "guest.shutdown")
	if _, err := e.svc.Update(ctx, e.req(""), "crash", api.UpdateVMRequest{Privacy: &domain.PrivacyPolicy{}}); !domain.IsCode(err, domain.CodeOperationInProgress) {
		t.Errorf("policy change during the stop: %v", err)
	}
	e.stop() // the daemon dies mid-stop

	e.opts.StopDelay = 0
	e.start()
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpSucceeded {
		t.Fatalf("replayed stop: %s %+v", op.State, op.Error)
	}
	if got := e.vm(string(vm.ID)); got.State != domain.StateDeleted {
		t.Fatalf("ephemeral VM after the replayed stop: %s", got.State)
	}
	e.storageGone(vm.ID)
}

// TestPrivacyPatch: the policy is set by PATCH; turning on ephemeral on a
// running VM makes its next stop delete it, and turning it on journals the
// decision at stop time only.
func TestPrivacyPatch(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := t.Context()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "p"}))
	vm, err := e.svc.Update(ctx, e.req(""), "p", api.UpdateVMRequest{Privacy: &domain.PrivacyPolicy{Ephemeral: true, NoOutputRetention: true}})
	if err != nil || !vm.Privacy.Ephemeral || !vm.Privacy.NoOutputRetention {
		t.Fatalf("PATCH privacy: %+v %v", vm, err)
	}
	same, err := e.svc.Update(ctx, e.req(""), "p", api.UpdateVMRequest{Privacy: &domain.PrivacyPolicy{Ephemeral: true, NoOutputRetention: true}})
	if err != nil || same.StateRevision != vm.StateRevision {
		t.Fatalf("same policy again bumped the revision: %+v %v", same, err)
	}
	e.mustSucceed(e.svc.Stop(ctx, e.req(e.newKey()), "p", false))
	if got := e.vm(string(vm.ID)); got.State != domain.StateDeleted {
		t.Fatalf("after stop: %s", got.State)
	}
}
