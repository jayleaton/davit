package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
	"davit/internal/state"
)

// record returns the journaled VM record of ref.
func (e *env) record(ref string) *state.VMRecord {
	e.t.Helper()
	var rec *state.VMRecord
	if err := e.db.Tx(context.Background(), func(tx *state.Tx) (err error) {
		rec, err = lookupVM(tx, owner, ref)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return rec
}

// bootedDisk is the disk path the fake runtime booted generation gen of vm
// on (its instance record).
func (e *env) bootedDisk(vm domain.VMID, gen int64) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "runtime", fmt.Sprintf("%s.g%d.json", vm, gen)))
	if err != nil {
		e.t.Fatal(err)
	}
	var rec struct {
		DiskPath string `json:"diskPath"`
	}
	if err := json.Unmarshal(b, &rec); err != nil || rec.DiskPath == "" {
		e.t.Fatalf("instance record %s: %v", b, err)
	}
	return rec.DiskPath
}

// scribble writes marker at the start of the disk file, as an uncommitted
// guest write. (Disks are sparse and GiB-sized: only the head is touched.)
func scribble(t *testing.T, path, marker string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(marker), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func holds(t *testing.T, path, marker string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, len(marker))
	if _, err := f.ReadAt(b, 0); err != nil {
		t.Fatal(err)
	}
	return string(b) == marker
}

// crashWhileDown kills the VM's instance while the daemon is down, so the
// restarted daemon finds it in error.
func (e *env) crashWhileDown(ref string) {
	e.t.Helper()
	vm := e.vm(ref)
	e.stop()
	if err := e.rt.Crash(runtime.Key{VMID: vm.ID, Generation: vm.InstanceGeneration}); err != nil {
		e.t.Fatal(err)
	}
	e.start()
	if vm := e.vm(ref); vm.State != domain.StateError {
		e.t.Fatalf("after crash: %s", vm.State)
	}
}

func eventMessages(t *testing.T, e *env, id domain.OperationID, phase string) []string {
	t.Helper()
	_, evs, err := e.svc.Events(context.Background(), owner, id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Phase == phase {
			out = append(out, ev.Message)
		}
	}
	return out
}

// TestRecoverPowerCyclesOnSameDisk: recover of a ready VM kills its VMM
// without a commit and cold boots the same working disk in a new
// generation; what the guest wrote survives and no snapshot is taken.
func TestRecoverPowerCyclesOnSameDisk(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "w"}))
	before := e.record("w")
	disk := e.bootedDisk(before.VM.ID, 1)
	scribble(t, disk, "uncommitted-work")

	key := e.newKey()
	a, err := e.svc.Recover(ctx, e.req(key), "w", api.RecoverVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Operation.Kind != domain.OpRecover || a.VM.State != domain.StateProvisioning || a.VM.InstanceGeneration != 2 ||
		!a.Operation.Params.ReuseDisk || a.Operation.Params.Generation != 2 {
		t.Fatalf("accepted %+v / %+v", a.Operation, a.VM)
	}
	op := e.mustSucceed(a, nil)
	if op.Result.BootMode != domain.BootModeColdBoot || op.Params.BootPlan != "cold" || op.Result.SnapshotID != "" {
		t.Fatalf("result %+v params %+v", op.Result, op.Params)
	}
	vm := e.vm("w")
	if vm.State != domain.StateReady || vm.InstanceGeneration != 2 || vm.BootMode != domain.BootModeColdBoot || vm.ExpiresAt == nil {
		t.Fatalf("after recover: %+v", vm)
	}
	after := e.record("w")
	if after.DiskID != before.DiskID || after.RetainedDiskID != "" {
		t.Fatalf("disk %q (retained %q), want %q kept", after.DiskID, after.RetainedDiskID, before.DiskID)
	}
	if got := e.bootedDisk(vm.ID, 2); got != disk {
		t.Fatalf("generation 2 booted %s, want the same disk %s", got, disk)
	}
	if !holds(t, disk, "uncommitted-work") {
		t.Fatal("the guest's uncommitted write did not survive the power cycle")
	}
	hs, _ := e.rt.Discover(ctx)
	if len(hs) != 1 || hs[0].Generation != 2 {
		t.Fatalf("instances after recover: %+v", hs)
	}
	msgs := eventMessages(t, e, op.ID, "runtime.power-cycle")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "VMM was killed without a RAM or disk commit") ||
		!strings.Contains(msgs[0], "crash-consistent") || !strings.Contains(msgs[0], "no snapshot is taken") {
		t.Fatalf("power-cycle events %q", msgs)
	}
	if snaps, _, err := e.svc.ListSnapshots(ctx, owner, "w", Page{Limit: 10}); err != nil || len(snaps) != 0 {
		t.Fatalf("recover took a snapshot: %+v %v", snaps, err)
	}

	again, err := e.svc.Recover(ctx, e.req(key), string(vm.ID), api.RecoverVMRequest{})
	if err != nil || !again.Replayed || again.Operation.ID != op.ID {
		t.Fatalf("replay by ID of a key used by name: %+v %v", again, err)
	}
	if _, err := e.svc.Recover(ctx, e.req(""), "w", api.RecoverVMRequest{}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("recover without Idempotency-Key: %v", err)
	}

	// The recovered disk is the VM's disk from now on: stop commits it.
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "w", false))
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "w", api.ResumeVMRequest{}))
	if !holds(t, e.bootedDisk(vm.ID, 3), "uncommitted-work") {
		t.Fatal("stop after recover lost the write")
	}
}

func TestRecoverRefusals(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "r"}))

	stale := e.vm("r").StateRevision - 1
	r := e.req(e.newKey())
	r.IfMatch = &stale
	if _, err := e.svc.Recover(ctx, r, "r", api.RecoverVMRequest{}); !domain.IsCode(err, domain.CodeRevisionMismatch) {
		t.Fatalf("If-Match stale: %v", err)
	}
	cur := e.vm("r").StateRevision
	r = e.req(e.newKey())
	r.IfMatch = &cur
	e.mustSucceed(e.svc.Recover(ctx, r, "r", api.RecoverVMRequest{}))

	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "r", false))
	_, err := e.svc.Recover(ctx, e.req(e.newKey()), "r", api.RecoverVMRequest{})
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeInvalidState || de.Details["hint"] != "resume" {
		t.Fatalf("recover of a stopped vm: %v", err)
	}
	if _, err := e.svc.Recover(ctx, e.req(e.newKey()), "nope", api.RecoverVMRequest{}); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("recover of an unknown vm: %v", err)
	}

	slow := newEnv(t, fake.Options{BootDelay: 300 * time.Millisecond}, nil)
	a, err := slow.svc.Create(ctx, slow.req(slow.newKey()), api.CreateVMRequest{Name: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := slow.svc.Recover(ctx, slow.req(slow.newKey()), "p", api.RecoverVMRequest{}); !domain.IsCode(err, domain.CodeOperationInProgress) {
		t.Fatalf("recover while provisioning: %v", err)
	}
	slow.wait(a.Operation.ID)
}

// TestRecoverFromErrorKeepsDisk: an error VM whose instance died recovers
// on its working disk; a failed recover leaves it in error with that disk
// still there (garbage collection keeps it), and a later recover works.
func TestRecoverFromErrorKeepsDisk(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "x"}))
	id := e.vm("x").ID
	disk := e.bootedDisk(id, 1)
	scribble(t, disk, "work-before-crash")
	e.crashWhileDown("x")
	diskID := e.record("x").DiskID

	e.rt.InjectFault("ColdBoot", domain.Errorf(domain.CodeUnavailable, "injected boot failure"))
	a, err := e.svc.Recover(ctx, e.req(e.newKey()), "x", api.RecoverVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if op := e.wait(a.Operation.ID); op.State != domain.OpFailed {
		t.Fatalf("recover with a failing boot: %s", op.State)
	}
	if vm := e.vm("x"); vm.State != domain.StateError || vm.LastError == nil {
		t.Fatalf("after failed recover: %+v", vm)
	}
	if rec := e.record("x"); rec.DiskID != diskID || rec.Handle != nil {
		t.Fatalf("failed recover changed the disk: %q -> %q", diskID, rec.DiskID)
	}
	if err := e.svc.collect(ctx); err != nil {
		t.Fatal(err)
	}
	if !holds(t, disk, "work-before-crash") {
		t.Fatal("the working disk was lost after a failed recover")
	}
	if hs, _ := e.rt.Discover(ctx); len(hs) != 0 {
		t.Fatalf("a failed recover left instances: %+v", hs)
	}

	op := e.mustSucceed(e.svc.Recover(ctx, e.req(e.newKey()), "x", api.RecoverVMRequest{}))
	vm := e.vm("x")
	if vm.State != domain.StateReady || vm.InstanceGeneration != 3 || vm.LastError != nil {
		t.Fatalf("recover from error: %+v", vm)
	}
	if e.bootedDisk(id, 3) != disk || !holds(t, disk, "work-before-crash") {
		t.Fatal("recover from error did not boot the working disk")
	}
	if msgs := eventMessages(t, e, op.ID, "runtime.power-cycle"); len(msgs) != 1 || !strings.Contains(msgs[0], "no VMM was left to kill") {
		t.Fatalf("power-cycle event of an error vm: %q", msgs)
	}
}

// TestRecoverWithoutDiskRefused: an error VM whose working disk is gone is
// told to resume from its last snapshot.
func TestRecoverWithoutDiskRefused(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "g"}))
	disk := e.bootedDisk(e.vm("g").ID, 1)
	e.crashWhileDown("g")
	if err := os.Remove(disk); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Recover(ctx, e.req(e.newKey()), "g", api.RecoverVMRequest{})
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeInvalidState || !strings.Contains(de.Message, "resume it from its last snapshot") {
		t.Fatalf("recover without a disk: %v", err)
	}
}

// TestRecoverAdmission: a live VM's power cycle needs no new capacity (it
// already holds its CPUs and memory); an error VM's is admitted like resume.
func TestRecoverAdmission(t *testing.T) {
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Limits = domain.Limits{CPUs: 3} })
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "one"}))
	e.mustSucceed(e.svc.Recover(ctx, e.req(e.newKey()), "one", api.RecoverVMRequest{}))
	e.crashWhileDown("one")
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "two"}))
	if _, err := e.svc.Recover(ctx, e.req(e.newKey()), "one", api.RecoverVMRequest{}); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("recover of an error vm over the CPU limit: %v", err)
	}
	if vm := e.vm("one"); vm.State != domain.StateError {
		t.Fatalf("refused recover changed the vm: %s", vm.State)
	}
}

// TestRecoverReplaysAfterDaemonCrash: a daemon that dies mid-recover
// replays it on start and ends with one instance on the same disk.
func TestRecoverReplaysAfterDaemonCrash(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "d"}))
	id := e.vm("d").ID
	disk := e.bootedDisk(id, 1)
	scribble(t, disk, "kept-across-restart")

	e.stop()
	e.opts.BootDelay = 400 * time.Millisecond
	e.start()
	a, err := e.svc.Recover(ctx, e.req(e.newKey()), "d", api.RecoverVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, a.Operation.ID, "runtime.boot")
	e.stop() // the daemon dies while generation 2 boots
	e.opts.BootDelay = 0
	e.start()
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpSucceeded || op.Result.BootMode != domain.BootModeColdBoot {
		t.Fatalf("replayed recover %s: %+v", op.State, op.Error)
	}
	if vm := e.vm("d"); vm.State != domain.StateReady || vm.InstanceGeneration != 2 {
		t.Fatalf("after replay: %+v", vm)
	}
	hs, _ := e.rt.Discover(ctx)
	if len(hs) != 1 || hs[0].Generation != 2 {
		t.Fatalf("instances after replay: %+v", hs)
	}
	if e.bootedDisk(id, 2) != disk || !holds(t, disk, "kept-across-restart") {
		t.Fatal("replayed recover did not boot the working disk")
	}
}

// TestRecoverKeepsGrants: a power cycle is a resume for credential grants
// (never a revoke) and the proxy follows the network to the new generation.
func TestRecoverKeepsGrants(t *testing.T) {
	hooks := &credHooks{}
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Credentials = hooks })
	ctx := t.Context()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"}))
	id := string(e.vm("c").ID)
	hooks.mu.Lock()
	hooks.events = nil
	hooks.mu.Unlock()
	e.mustSucceed(e.svc.Recover(ctx, e.req(e.newKey()), "c", api.RecoverVMRequest{}))
	hooks.mu.Lock()
	got := strings.Join(hooks.events, ",")
	hooks.mu.Unlock()
	if strings.Contains(got, "delete") || !strings.Contains(got, "resume "+id) {
		t.Fatalf("grant events of a recover: %q", got)
	}
}
