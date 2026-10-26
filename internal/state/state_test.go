package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"davit/internal/domain"
)

func openT(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testVM(id domain.VMID, created time.Time) *VMRecord {
	return &VMRecord{VM: domain.VM{
		ID: id, Owner: "owner", Name: string(id), State: domain.StateStopped,
		StateRevision: 1, InstanceGeneration: 1, CreatedAt: created, UpdatedAt: created,
	}, Env: []string{"SECRET=1"}, DiskID: string(id) + "-g1"}
}

func TestMigrationsAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openT(t, path)
	v, err := db.SchemaVersion(context.Background())
	if err != nil || v != len(migrations)-1 {
		t.Fatalf("schema version %d %v", v, err)
	}
	if _, err := Open(context.Background(), path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second open: %v, want ErrLocked", err)
	}
	db.Close()
	db2 := openT(t, path) // reopening re-runs no migrations
	if v2, _ := db2.SchemaVersion(context.Background()); v2 != v {
		t.Fatalf("version changed on reopen: %d", v2)
	}
}

func TestVMRoundTripAndPaging(t *testing.T) {
	db := openT(t, filepath.Join(t.TempDir(), "s.db"))
	ctx := context.Background()
	base := time.Unix(1000, 0).UTC()
	err := db.Tx(ctx, func(tx *Tx) error {
		for i := range 5 {
			if err := tx.PutVM(testVM(domain.VMID(fmt.Sprintf("vm-%d", i)), base.Add(time.Duration(i)*time.Second))); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Tx(ctx, func(tx *Tx) error {
		rec, err := tx.GetVM("vm-2")
		if err != nil {
			return err
		}
		if rec.Env[0] != "SECRET=1" || rec.DiskID != "vm-2-g1" || !rec.VM.CreatedAt.Equal(base.Add(2*time.Second)) {
			t.Errorf("round trip lost fields: %+v", rec)
		}
		// Name uniqueness among live VMs.
		dup := testVM("vm-x", base)
		dup.VM.Name = "vm-1"
		if err := tx.PutVM(dup); !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Errorf("duplicate name: %v", err)
		}
		// Invariants are enforced on write.
		bad := testVM("vm-y", base)
		bad.VM.State = domain.StateProvisioning
		if err := tx.PutVM(bad); !domain.IsCode(err, domain.CodeInternal) {
			t.Errorf("invariant: %v", err)
		}
		var got []domain.VMID
		p := Page{Limit: 2, Filter: "stopped"}
		for {
			page, next, err := tx.ListVMs("owner", []domain.VMState{domain.StateStopped}, p)
			if err != nil {
				return err
			}
			for _, r := range page {
				got = append(got, r.VM.ID)
			}
			if next == "" {
				break
			}
			p.Cursor = next
		}
		if fmt.Sprint(got) != "[vm-0 vm-1 vm-2 vm-3 vm-4]" {
			t.Errorf("paging: %v", got)
		}
		if _, _, err := tx.ListVMs("owner", nil, Page{Limit: 2, Cursor: p.Cursor, Filter: "other"}); p.Cursor != "" && !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Errorf("cursor bound to filter: %v", err)
		}
		if _, err := tx.GetVM("nope"); !domain.IsCode(err, domain.CodeNotFound) {
			t.Errorf("not found: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOperationJournalAndIdempotency(t *testing.T) {
	db := openT(t, filepath.Join(t.TempDir(), "s.db"))
	ctx := context.Background()
	now := time.Unix(2000, 0).UTC()
	op := &domain.Operation{ID: "op-1", Kind: domain.OpCreate, VMID: "vm-0", Owner: "owner", State: domain.OpQueued,
		Params: domain.OpParams{Generation: 1}, CreatedAt: now, UpdatedAt: now}
	err := db.Tx(ctx, func(tx *Tx) error {
		if err := tx.PutVM(testVM("vm-0", now)); err != nil {
			return err
		}
		if err := tx.PutOperation(op); err != nil {
			return err
		}
		for range 3 {
			if err := tx.AppendEvent(&domain.OperationEvent{OperationID: op.ID, Time: now, State: op.State}); err != nil {
				return err
			}
		}
		return tx.PutIdempotency(&Idempotency{Scope: "s", BodyHash: "h", OperationID: op.ID, CreatedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Tx(ctx, func(tx *Tx) error {
		evs, err := tx.Events(op.ID, 1, 10)
		if err != nil || len(evs) != 2 || evs[0].Seq != 2 || evs[1].Seq != 3 {
			t.Errorf("events after 1: %+v %v", evs, err)
		}
		un, err := tx.UnfinishedOperations()
		if err != nil || len(un) != 1 || un[0].Params.Generation != 1 {
			t.Errorf("unfinished: %+v %v", un, err)
		}
		if err := tx.PutIdempotency(&Idempotency{Scope: "s", BodyHash: "h2", OperationID: op.ID, CreatedAt: now}); err == nil {
			t.Error("duplicate idempotency scope accepted")
		}
		op.State = domain.OpSucceeded
		if err := tx.PutOperation(op); !domain.IsCode(err, domain.CodeInternal) {
			t.Errorf("queued -> succeeded must be rejected: %v", err)
		}
		op.State = domain.OpRunning
		if err := tx.PutOperation(op); err != nil {
			return err
		}
		op.State = domain.OpSucceeded
		if err := tx.PutOperation(op); err != nil {
			return err
		}
		op.State = domain.OpFailed
		if err := tx.PutOperation(op); !domain.IsCode(err, domain.CodeInternal) {
			t.Errorf("terminal operation changed: %v", err)
		}
		if err := tx.ExpireIdempotency(op.ID, now.Add(24*time.Hour)); err != nil {
			return err
		}
		if n, _ := tx.PurgeIdempotency(now.Add(time.Hour)); n != 0 {
			t.Errorf("purged key inside retention")
		}
		if n, _ := tx.PurgeIdempotency(now.Add(25 * time.Hour)); n != 1 {
			t.Errorf("did not purge expired key")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestCrashRecovery SIGKILLs a writer process mid-stream and checks that the
// reopened store holds only whole transactions: every committed operation
// has its VM, its first event and its idempotency key, and nothing else.
func TestCrashRecovery(t *testing.T) {
	if os.Getenv("DAVIT_STATE_CRASH_WRITER") != "" {
		crashWriter(os.Getenv("DAVIT_STATE_CRASH_WRITER"))
		return
	}
	path := filepath.Join(t.TempDir(), "s.db")
	for round := range 3 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashRecovery$")
		cmd.Env = append(os.Environ(), "DAVIT_STATE_CRASH_WRITER="+path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		started := path + ".started"
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(started); err == nil {
				break
			}
			if time.Now().After(deadline) {
				cmd.Process.Kill()
				t.Fatal("writer never committed")
			}
		}
		os.Remove(started)
		time.Sleep(time.Duration(20+40*round) * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
		cmd.Wait()

		db, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("round %d: reopen after kill: %v", round, err)
		}
		if err := db.IntegrityCheck(context.Background()); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		var vms, ops, keys, events int
		err = db.Tx(context.Background(), func(tx *Tx) error {
			for q, dst := range map[string]*int{
				`SELECT COUNT(*) FROM vms`: &vms, `SELECT COUNT(*) FROM operations`: &ops,
				`SELECT COUNT(*) FROM idempotency`: &keys, `SELECT COUNT(*) FROM operation_events`: &events,
			} {
				if err := tx.row(q).Scan(dst); err != nil {
					return err
				}
			}
			return nil
		})
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("round %d: %d committed transactions survived SIGKILL", round, ops)
		if vms != ops || ops != keys || ops != events || ops == 0 {
			t.Fatalf("round %d: torn state: vms=%d ops=%d keys=%d events=%d", round, vms, ops, keys, events)
		}
	}
}

// crashWriter commits create-shaped transactions until killed.
func crashWriter(path string) {
	db, err := Open(context.Background(), path)
	for err != nil { // previous writer's lock may still be releasing
		time.Sleep(5 * time.Millisecond)
		db, err = Open(context.Background(), path)
	}
	now := time.Now().UTC()
	for i := 0; ; i++ {
		id := strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.Itoa(i)
		_ = db.Tx(context.Background(), func(tx *Tx) error {
			vm := testVM(domain.VMID("vm-"+id), now)
			vm.VM.State, vm.VM.ActiveOperationID = domain.StateProvisioning, domain.OperationID("op-"+id)
			if err := tx.PutVM(vm); err != nil {
				return err
			}
			op := &domain.Operation{ID: vm.VM.ActiveOperationID, Kind: domain.OpCreate, VMID: vm.VM.ID, Owner: "owner", State: domain.OpQueued, CreatedAt: now}
			if err := tx.PutOperation(op); err != nil {
				return err
			}
			if err := tx.AppendEvent(&domain.OperationEvent{OperationID: op.ID, Time: now}); err != nil {
				return err
			}
			return tx.PutIdempotency(&Idempotency{Scope: id, BodyHash: "h", OperationID: op.ID, CreatedAt: now})
		})
		if i == 0 {
			os.WriteFile(path+".started", nil, 0o600)
		}
	}
}

// TestDiskFullReleasesReserve simulates a host disk filling up: the first
// journal write that hits ENOSPC frees the reserve and succeeds on retry;
// once nothing is left, writes fail as resource_exhausted and commit nothing.
func TestDiskFullReleasesReserve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	db := openT(t, path)
	ctx := context.Background()
	if !db.ReserveArmed() {
		t.Fatal("reserve not armed at open")
	}
	full := func() error { return &os.PathError{Op: "write", Path: path + "-wal", Err: syscall.ENOSPC} }
	// The disk is full until the reserve is released.
	db.InjectWriteFault(func() error {
		if _, err := os.Stat(path + ".reserve"); err == nil {
			return full()
		}
		return nil
	})
	now := time.Unix(10, 0).UTC()
	if err := db.Tx(ctx, func(tx *Tx) error { return tx.PutVM(testVM("vm-a", now)) }); err != nil {
		t.Fatalf("write with reserve available: %v", err)
	}
	if db.ReserveArmed() {
		t.Fatal("reserve should have been released")
	}

	db.InjectWriteFault(full) // nothing left
	err := db.Tx(ctx, func(tx *Tx) error { return tx.PutVM(testVM("vm-b", now)) })
	if de := domain.AsError(err); de.Code != domain.CodeResourceExhausted || !de.Retryable {
		t.Fatalf("full disk: %v -> %+v", err, de)
	}

	db.InjectWriteFault(nil)
	if err := db.Rearm(); err != nil || !db.ReserveArmed() {
		t.Fatalf("rearm: %v", err)
	}
	err = db.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.GetVM("vm-a"); err != nil {
			return err
		}
		if _, err := tx.GetVM("vm-b"); !domain.IsCode(err, domain.CodeNotFound) {
			t.Errorf("failed write left vm-b behind: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestProcessPagingAndReplace(t *testing.T) {
	db := openT(t, filepath.Join(t.TempDir(), "s.db"))
	ctx := context.Background()
	base := time.Unix(1000, 0).UTC()
	pg := func(cursor, filter string) Page { return Page{Limit: 1, Cursor: cursor, Filter: filter} }
	err := db.Tx(ctx, func(tx *Tx) error {
		if err := tx.PutVM(testVM("vm-a", base)); err != nil {
			return err
		}
		for i := range 3 {
			p := &ProcessRecord{ID: fmt.Sprintf("p%d", 2-i), VMID: "vm-a", Owner: "owner", Generation: 1,
				CreatedAt: base.Add(time.Duration(i) * time.Second), IdempotencyKey: fmt.Sprint("k", i), Status: "running"}
			if err := tx.PutProcess(p); err != nil {
				return err
			}
		}
		var got []string
		cur := ""
		for {
			ps, next, err := tx.ListProcesses("owner", "vm-a", pg(cur, "owner|vm-a"))
			if err != nil {
				return err
			}
			for _, p := range ps {
				got = append(got, p.ID)
			}
			if next == "" {
				break
			}
			if _, _, err := tx.ListProcesses("owner", "vm-a", pg(next, "owner|vm-b")); !domain.IsCode(err, domain.CodeInvalidArgument) {
				return fmt.Errorf("cursor for another VM accepted: %v", err)
			}
			cur = next
		}
		if fmt.Sprint(got) != "[p2 p1 p0]" {
			return fmt.Errorf("order %v, want oldest first", got)
		}
		// A reservation swapped for its process keeps the key unique.
		if err := tx.DeleteProcess("p0"); err != nil {
			return err
		}
		return tx.PutProcess(&ProcessRecord{ID: "proc_x", VMID: "vm-a", Owner: "owner", CreatedAt: base, IdempotencyKey: "k2", Status: "running"})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestOpenPathWithURIChars: a data directory containing characters that are
// special in a SQLite URI ('?', '#', '%') opens the database at exactly that
// path, with its pragmas (WAL, foreign keys) applied.
func TestOpenPathWithURIChars(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a#b?c%41d")
	path := filepath.Join(dir, "state.db")
	db := openT(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not at its path: %v", err)
	}
	var mode string
	var fk int
	if err := db.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode %q %v", mode, err)
	}
	if err := db.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys %d %v", fk, err)
	}
}
