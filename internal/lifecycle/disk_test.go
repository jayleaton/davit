package lifecycle

import (
	"context"
	"os"
	"sync/atomic"
	"syscall"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
	"davit/internal/state"
	"davit/internal/storage"
)

// fullStore makes every storage write fail with ENOSPC while full is set,
// as a full host filesystem would.
type fullStore struct {
	storage.Store
	full *atomic.Bool
}

func enospc(op string) error { return &os.PathError{Op: op, Path: "disk.img", Err: syscall.ENOSPC} }

func (f fullStore) CreateWritable(ctx context.Context, id storage.DiskID, base string, size int64) (storage.Disk, error) {
	if f.full.Load() {
		return storage.Disk{}, enospc("write")
	}
	return f.Store.CreateWritable(ctx, id, base, size)
}

func (f fullStore) CloneCommitted(ctx context.Context, from storage.CommitID, id storage.DiskID, size int64) (storage.Disk, error) {
	if f.full.Load() {
		return storage.Disk{}, enospc("write")
	}
	return f.Store.CloneCommitted(ctx, from, id, size)
}

func (f fullStore) Commit(ctx context.Context, disk storage.DiskID, id storage.CommitID) (storage.Commit, error) {
	if f.full.Load() {
		return storage.Commit{}, enospc("write")
	}
	return f.Store.Commit(ctx, disk, id)
}

// diskFullEnv returns an env whose storage and journal both run out of space
// while the returned flag is set. The journal can still use its reserve.
func diskFullEnv(t *testing.T) (*env, *atomic.Bool) {
	full := new(atomic.Bool)
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.Storage = fullStore{Store: c.Storage, full: full}
	})
	e.db.InjectWriteFault(func() error {
		if full.Load() && e.db.ReserveArmed() {
			return enospc("write")
		}
		return nil // the released reserve is what the journal writes into
	})
	return e, full
}

// TestDiskFullFailsOperationsCleanly: an operation that hits ENOSPC fails
// with a retryable resource_exhausted, the journal stays consistent, no
// operation is left running, reads keep working, and after space returns
// the reserve is re-armed and the VM recovers.
func TestDiskFullFailsOperationsCleanly(t *testing.T) {
	e, full := diskFullEnv(t)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "ok"}))

	full.Store(true)
	a, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "doomed"})
	if err != nil {
		t.Fatalf("accepting the create must still work (reserve): %v", err)
	}
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpFailed || op.Error == nil || op.Error.Code != domain.CodeResourceExhausted || !op.Error.Retryable {
		t.Fatalf("create on a full disk: %s %+v", op.State, op.Error)
	}
	vm := e.vm("doomed")
	if vm.State != domain.StateError || vm.ActiveOperationID != "" || vm.LastError.Code != domain.CodeResourceExhausted {
		t.Fatalf("vm after ENOSPC: %+v", vm)
	}
	if hs, _ := e.rt.Discover(ctx); len(hs) != 1 {
		t.Fatalf("a half-booted instance was left behind: %+v", hs)
	}

	// Stop of a healthy VM needs a commit, which also fails: the VM keeps
	// its working disk and the last committed snapshot is untouched.
	stop, err := e.svc.Stop(ctx, e.req(""), "ok", false)
	if err != nil {
		t.Fatalf("stop must never be refused for space: %v", err)
	}
	if op := e.wait(stop.Operation.ID); op.State != domain.OpFailed || op.Error.Code != domain.CodeResourceExhausted {
		t.Fatalf("stop on a full disk: %s %+v", op.State, op.Error)
	}

	// Reads keep working while full.
	if vms, _, err := e.svc.ListVMs(ctx, owner, nil, true, Page{}); err != nil || len(vms) != 2 {
		t.Fatalf("list while full: %d %v", len(vms), err)
	}
	if _, evs, err := e.svc.Events(ctx, owner, op.ID, 0, 0); err != nil || len(evs) == 0 {
		t.Fatalf("events while full: %v", err)
	}
	var unfinished []*domain.Operation
	if err := e.db.Tx(ctx, func(tx *state.Tx) (err error) { unfinished, err = tx.UnfinishedOperations(); return err }); err != nil || len(unfinished) != 0 {
		t.Fatalf("operations left running after disk-full: %d %v", len(unfinished), err)
	}
	if err := e.db.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}

	// Space comes back: the sweeper re-arms the reserve and the VMs recover.
	full.Store(false)
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if !e.db.ReserveArmed() {
		t.Fatal("reserve not re-armed after space returned")
	}
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "doomed", api.ResumeVMRequest{}))
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "ok", api.ResumeVMRequest{}))
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "ok", false))
}

// TestFreeSpaceFloor: below the floor, new/fork/resume are refused up front
// (no operation is created), while stop and delete still work.
func TestFreeSpaceFloor(t *testing.T) {
	free := new(atomic.Int64)
	free.Store(100 * domain.GiB)
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.FreeSpace = func() (int64, error) { return free.Load(), nil }
		c.Limits.MinFreeDiskBytes = 2 * domain.GiB
	})
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "a"}))
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "a", false))

	free.Store(domain.GiB)
	for name, call := range map[string]func() error{
		"new": func() error {
			_, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "b"})
			return err
		},
		"fork":   func() error { _, err := e.svc.Fork(ctx, e.req(e.newKey()), "a", api.ForkVMRequest{}); return err },
		"resume": func() error { _, err := e.svc.Resume(ctx, e.req(""), "a", api.ResumeVMRequest{}); return err },
	} {
		err := call()
		de := domain.AsError(err)
		if de.Code != domain.CodeResourceExhausted || !de.Retryable || de.Details["floorBytes"] == nil {
			t.Errorf("%s below the floor: %v", name, err)
		}
	}
	e.mustSucceed(e.svc.Delete(ctx, e.req(""), "a"))
	free.Store(100 * domain.GiB)
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "b"}))
}
