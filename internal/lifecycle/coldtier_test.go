package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
	"davit/internal/state"
	"davit/internal/storage"
)

// tierStore is a diskStore with a cold tier: a VM's cost (fakeDisk.cost)
// leaves the hot fakeDisk for the cold one when collection finds it cold,
// and is given back where it lives when it is deleted.
type tierStore struct {
	diskStore
	fs   *storage.FS
	cold *fakeDisk
	mu   sync.Mutex
	gone map[domain.VMID]bool // cost moved to cold
	// slow, when set, paces demotions so a test can interrupt one;
	// copied, when set, receives each commit Demote published, which then
	// waits for hold.
	slow   int64
	copied chan storage.CommitID
	hold   chan struct{}
}

func (s *tierStore) ColdProbe() (storage.ProbeResult, bool) { return s.fs.ColdProbe() }
func (s *tierStore) DeltaBytes(ctx context.Context, id storage.CommitID, base string) (int64, error) {
	return s.fs.DeltaBytes(ctx, id, base)
}
func (s *tierStore) Restore(ctx context.Context, from storage.CommitID, id storage.DiskID, size int64, o storage.RestoreOptions) (storage.Disk, error) {
	return s.fs.Restore(ctx, from, id, size, o)
}
func (s *tierStore) Promote(ctx context.Context, id storage.CommitID, o storage.RestoreOptions) (*storage.Move, error) {
	return s.fs.Promote(ctx, id, o)
}
func (s *tierStore) Demote(ctx context.Context, id storage.CommitID, o storage.MoveOptions) (*storage.Move, error) {
	if s.slow > 0 {
		o.BytesPerSecond = s.slow
	}
	d, err := s.fs.Demote(ctx, id, o)
	if err == nil && s.copied != nil {
		s.copied <- id
		<-s.hold
	}
	return d, err
}

func (s *tierStore) Collect(ctx context.Context, keep storage.KeepSet) (storage.Collected, error) {
	_ = s.d.db.Tx(ctx, func(tx *state.Tx) error {
		all, err := tx.AllVMs()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, r := range all {
			id := r.VM.ID
			deleted := r.VM.State == domain.StateDeleting || r.VM.State == domain.StateDeleted
			switch {
			case deleted && s.gone[id]:
				s.cold.mu.Lock()
				s.cold.free += s.d.cost[id]
				s.cold.mu.Unlock()
				delete(s.gone, id)
				s.d.setCost(id, 0)
			case !deleted && !s.gone[id] && tierOf(r.VM) == domain.TierCold:
				s.d.mu.Lock()
				c := s.d.cost[id]
				s.d.free += c
				s.d.mu.Unlock()
				s.cold.setFree(s.cold.getFree() - c)
				s.gone[id] = true
			}
		}
		return err
	})
	return s.diskStore.Collect(ctx, keep)
}

type coldEnv struct {
	*diskEnv
	cold    *fakeDisk
	st      *tierStore
	coldDir string
	// missing, at the next start, leaves the cold tier unattached (its
	// disk is not mounted).
	missing bool
}

func newColdEnv(t *testing.T, hot, cold *fakeDisk, adjust func(*Config, *tierStore)) *coldEnv {
	ce := &coldEnv{cold: cold, coldDir: t.TempDir()}
	ce.diskEnv = newDiskEnv(t, hot, fake.Options{}, func(c *Config) {
		ds := c.Storage.(diskStore)
		fs := ds.Store.(*storage.FS)
		if ce.missing {
			c.ColdUnavailable = "test: cold disk not mounted"
			return
		}
		if err := fs.AttachCold(ce.coldDir); err != nil {
			t.Fatal(err)
		}
		if ce.st == nil {
			ce.st = &tierStore{gone: map[domain.VMID]bool{}}
		}
		ce.st.diskStore, ce.st.fs, ce.st.cold = ds, fs, cold
		c.Storage = ce.st
		c.Cold = &ColdPolicy{MinBytes: -1, BytesPerSecond: -1, DiskSpace: cold.usage}
		if adjust != nil {
			adjust(c, ce.st)
		}
	})
	return ce
}

// sweepCold runs one cold sweep and waits for it.
func (e *coldEnv) sweepCold() {
	e.svc.coldSweep()
	for !e.svc.coldMu.TryLock() {
		time.Sleep(time.Millisecond)
	}
	e.svc.coldMu.Unlock()
}

// demoting lists the VMs being moved now.
func (s *Service) demotingNow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.demoting)
}

func (e *coldEnv) record(ref string) *state.VMRecord {
	e.t.Helper()
	var rec *state.VMRecord
	if err := e.db.Tx(e.ctx, func(tx *state.Tx) (err error) { rec, err = lookupVM(tx, owner, ref); return err }); err != nil {
		e.t.Fatal(err)
	}
	return rec
}

// lastCommit is the storage commit ref resumes from.
func (e *coldEnv) lastCommit(ref string) storage.Commit {
	e.t.Helper()
	vm := e.vm(ref)
	var sn *domain.Snapshot
	if err := e.db.Tx(e.ctx, func(tx *state.Tx) (err error) { sn, err = tx.GetSnapshot(vm.LastSnapshotID); return err }); err != nil {
		e.t.Fatal(err)
	}
	c, err := e.st.fs.GetCommit(e.ctx, storage.CommitID(sn.CommitID))
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// diskPath is ref's current working disk.
func (e *coldEnv) diskPath(ref string) string {
	return filepath.Join(e.dir, "storage", "disks", e.record(ref).DiskID+".img")
}

// runWithMarker creates a running box and writes marker into its disk.
func (e *coldEnv) runWithMarker(name string, labels map[string]string, marker string) {
	e.t.Helper()
	e.box(name, labels, false, false, 0)
	f, err := os.OpenFile(e.diskPath(name), os.O_WRONLY, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(marker), 1<<20); err != nil {
		e.t.Fatal(err)
	}
	f.Close()
}

func (e *coldEnv) hasMarker(name, marker string) bool {
	e.t.Helper()
	b := make([]byte, len(marker))
	f, err := os.Open(e.diskPath(name))
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	f.ReadAt(b, 1<<20)
	return string(b) == marker
}

func (e *coldEnv) noColdPartials() {
	e.t.Helper()
	if ents, _ := os.ReadDir(filepath.Join(e.coldDir, "tmp")); len(ents) > 0 {
		e.t.Fatalf("cold tmp holds %d leftovers", len(ents))
	}
}

// TestColdTierDemoteAndRestore: the sweep moves stopped VMs unused for
// coldAfter (pinned ones too), never running, recent, kept-hot or image
// ones; the data survives the trip and a resume copies it back.
func TestColdTierDemoteAndRestore(t *testing.T) {
	gib := domain.GiB
	e := newColdEnv(t, &fakeDisk{free: 60 * gib, total: 100 * gib}, &fakeDisk{free: 200 * gib, total: 250 * gib}, nil)
	e.runWithMarker("p--a", instance("p", "a"), "survives the trip")
	e.mustSucceed(e.svc.Stop(e.ctx, e.req(""), "p--a", false))
	e.box("p--pinned", instance("p", "pinned"), true, true, 0)
	e.box("p--hot", instance("p", "hot"), false, true, 0)
	no, yes := false, true
	if _, err := e.svc.Update(e.ctx, e.req(""), "p--hot", api.UpdateVMRequest{KeepHot: &yes}); err != nil {
		t.Fatal(err)
	}
	e.box("p--image", builder("p"), false, true, 0)
	e.box("p--running", instance("p", "running"), false, false, 0)
	e.advance(7 * time.Hour)
	e.box("p--recent", instance("p", "recent"), false, true, 0)
	e.advance(time.Hour)

	e.sweepCold()
	for name, want := range map[string]domain.StorageTier{"p--a": domain.TierCold, "p--pinned": domain.TierCold, "p--hot": domain.TierHot,
		"p--image": domain.TierHot, "p--running": domain.TierHot, "p--recent": domain.TierHot} {
		if got := tierOf(*e.vm(name)); got != want {
			t.Errorf("%s is %s, want %s", name, got, want)
		}
	}
	a := e.vm("p--a")
	if a.ColdBytes <= 0 || a.ColdAt == nil || e.record("p--a").DiskID != "" || len(e.record("p--a").ColdCommits) != 1 {
		t.Fatalf("cold vm record: %+v disk %q", a, e.record("p--a").DiskID)
	}
	if c := e.lastCommit("p--a"); c.Tier != domain.TierCold {
		t.Fatalf("p--a's commit is %s", c.Tier)
	}
	caps := e.svc.Capabilities().Capacity.Cold
	if caps == nil || caps.VMs != 2 || caps.FloorBytes != 250*gib*DefaultColdMinFreePercent/100 || caps.AfterSeconds != int64(DefaultColdAfter/time.Second) {
		t.Fatalf("capacity.cold: %+v", caps)
	}
	e.noColdPartials()

	// The resume restores, says so, and the VM is hot again.
	op := e.mustSucceed(e.svc.Resume(e.ctx, e.req(e.newKey()), "p--a", api.ResumeVMRequest{}))
	_, evs, err := e.svc.Events(e.ctx, owner, op.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	said := false
	for _, ev := range evs {
		said = said || ev.Phase == "storage.restore" && strings.Contains(ev.Message, "restoring from cold storage (")
	}
	if !said {
		t.Fatalf("no restore event: %+v", evs)
	}
	if v := e.vm("p--a"); tierOf(*v) != domain.TierHot || v.ColdBytes != 0 || v.ColdAt != nil {
		t.Fatalf("after resume: %+v", v)
	}
	if !e.hasMarker("p--a", "survives the trip") {
		t.Fatal("the disk's data did not survive cold storage")
	}
	// Stopped again: a hot commit; the cold one is pruned with its snapshot.
	e.mustSucceed(e.svc.Stop(e.ctx, e.req(""), "p--a", false))
	if c := e.lastCommit("p--a"); c.Tier != domain.TierHot {
		t.Fatalf("new commit is %s", c.Tier)
	}
	if ents, _ := os.ReadDir(filepath.Join(e.coldDir, "commits")); len(ents) != 1 { // p--pinned's
		t.Fatalf("cold commits left: %d", len(ents))
	}
	// Unpinning hot lets it go cold on the next sweep.
	if _, err := e.svc.Update(e.ctx, e.req(""), "p--hot", api.UpdateVMRequest{KeepHot: &no}); err != nil {
		t.Fatal(err)
	}
	e.advance(7 * time.Hour)
	e.sweepCold()
	if tierOf(*e.vm("p--hot")) != domain.TierCold {
		t.Fatal("p--hot did not go cold once unpinned")
	}
	// Deleting a cold VM frees its cold copy.
	e.mustSucceed(e.svc.Delete(e.ctx, e.req(e.newKey()), "p--hot"))
	if ents, _ := os.ReadDir(filepath.Join(e.coldDir, "commits")); len(ents) != 3 { // p--pinned, p--a, p--recent (7h unused by now)
		t.Fatalf("cold commits after delete: %d", len(ents))
	}
}

// TestColdTierSkipsSmallAndBusy: a VM with less of its own data than
// minBytes stays hot (and is not measured again), and the sweep moves
// nothing while the host is busy.
func TestColdTierSkipsSmallAndBusy(t *testing.T) {
	gib := domain.GiB
	var busy atomic.Bool
	busy.Store(true)
	e := newColdEnv(t, &fakeDisk{free: 60 * gib, total: 100 * gib}, &fakeDisk{free: 200 * gib, total: 250 * gib}, func(c *Config, _ *tierStore) {
		c.Cold.HostBusy = busy.Load
	})
	e.box("p--a", instance("p", "a"), false, true, 0)
	e.advance(7 * time.Hour)
	e.sweepCold()
	if tierOf(*e.vm("p--a")) != domain.TierHot {
		t.Fatal("moved while the host was busy")
	}
	busy.Store(false)
	e.svc.cfg.Cold.MinBytes = 64 * gib
	e.sweepCold()
	if tierOf(*e.vm("p--a")) != domain.TierHot || len(e.svc.coldSkip) != 1 {
		t.Fatal("a VM with almost no data of its own went cold")
	}
	e.svc.cfg.Cold.MinBytes = -1
	e.sweepCold()
	if tierOf(*e.vm("p--a")) != domain.TierHot {
		t.Fatal("measured again at the same revision")
	}
}

// TestColdTierCrashPoints interrupts a move at each step (a daemon
// restart in between) and checks that the VM ends with one good copy and
// resumes with its data.
func TestColdTierCrashPoints(t *testing.T) {
	gib := domain.GiB
	setup := func(t *testing.T) *coldEnv {
		e := newColdEnv(t, &fakeDisk{free: 60 * gib, total: 100 * gib}, &fakeDisk{free: 200 * gib, total: 250 * gib}, nil)
		e.runWithMarker("p--a", instance("p", "a"), "crash-safe")
		e.mustSucceed(e.svc.Stop(e.ctx, e.req(""), "p--a", false))
		return e
	}
	resumes := func(t *testing.T, e *coldEnv) {
		t.Helper()
		e.mustSucceed(e.svc.Resume(e.ctx, e.req(e.newKey()), "p--a", api.ResumeVMRequest{}))
		if !e.hasMarker("p--a", "crash-safe") || tierOf(*e.vm("p--a")) != domain.TierHot {
			t.Fatal("data lost or tier wrong after recovery")
		}
		e.noColdPartials()
	}

	t.Run("copied, journal not switched", func(t *testing.T) {
		e := setup(t)
		c := e.lastCommit("p--a")
		if _, err := e.st.fs.Demote(e.ctx, c.ID, storage.MoveOptions{}); err != nil {
			t.Fatal(err)
		}
		e.stop()
		e.start()
		if got := e.lastCommit("p--a"); got.Tier != domain.TierHot || tierOf(*e.vm("p--a")) != domain.TierHot {
			t.Fatal("recovery did not keep the hot copy")
		}
		if _, err := os.Stat(filepath.Join(e.coldDir, "commits", string(c.ID))); !os.IsNotExist(err) {
			t.Fatalf("the unrecorded cold copy stayed: %v", err)
		}
		resumes(t, e)
	})

	t.Run("journal switched, hot copy not removed", func(t *testing.T) {
		e := setup(t)
		c := e.lastCommit("p--a")
		if _, err := e.st.fs.Demote(e.ctx, c.ID, storage.MoveOptions{}); err != nil {
			t.Fatal(err)
		}
		now := e.clock()
		if err := e.db.Tx(e.ctx, func(tx *state.Tx) error {
			rec, err := lookupVM(tx, owner, "p--a")
			if err != nil {
				return err
			}
			rec.VM.Tier, rec.VM.ColdBytes, rec.VM.ColdAt = domain.TierCold, 4096, &now
			rec.ColdCommits, rec.DiskID = []string{string(c.ID)}, ""
			return tx.PutVM(rec)
		}); err != nil {
			t.Fatal(err)
		}
		e.stop()
		e.start()
		if got := e.lastCommit("p--a"); got.Tier != domain.TierCold {
			t.Fatal("recovery did not finish the move")
		}
		if _, err := os.Stat(filepath.Join(e.dir, "storage", "commits", string(c.ID))); !os.IsNotExist(err) {
			t.Fatalf("the hot copy stayed: %v", err)
		}
		resumes(t, e)
	})

	t.Run("resume during the copy", func(t *testing.T) {
		e := setup(t)
		e.st.slow = 64 << 10 // a 4 MiB disk takes long enough at 64 KiB/s
		done := make(chan error, 1)
		go func() { done <- e.svc.demote(e.ctx, e.vm("p--a").ID, "test", false) }()
		deadline := time.Now().Add(5 * time.Second)
		for e.svc.demotingNow() == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		e.mustSucceed(e.svc.Resume(e.ctx, e.req(e.newKey()), "p--a", api.ResumeVMRequest{}))
		if err := <-done; err == nil {
			t.Fatal("the move finished after a resume")
		}
		if !e.hasMarker("p--a", "crash-safe") {
			t.Fatal("data lost")
		}
		if v := e.vm("p--a"); tierOf(*v) != domain.TierHot {
			t.Fatalf("tier %s after an aborted move", v.Tier)
		}
		e.noColdPartials()
	})

	t.Run("vm changed before the switch", func(t *testing.T) {
		e := setup(t)
		e.st.copied, e.st.hold = make(chan storage.CommitID), make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- e.svc.demote(e.ctx, e.vm("p--a").ID, "test", false) }()
		<-e.st.copied
		// Pinned while the copy finished: the move aborts at its switch.
		yes := true
		if _, err := e.svc.Update(e.ctx, e.req(""), "p--a", api.UpdateVMRequest{Keep: &yes}); err != nil {
			t.Fatal(err)
		}
		close(e.st.hold)
		if err := <-done; !errors.Is(err, errVMChanged) {
			t.Fatalf("demote: %v", err)
		}
		e.st.copied = nil
		if ents, _ := os.ReadDir(filepath.Join(e.coldDir, "commits")); len(ents) != 0 {
			t.Fatalf("aborted move left %d cold commits", len(ents))
		}
		resumes(t, e)
	})
}

// TestColdTierFirstInLowDiskCleanup: below the hot floor, VMs go cold
// (least recently used first) before anything is auto-deleted; a full
// cold tier first auto-deletes its own old boxes; only what cannot go
// cold is deleted from hot.
func TestColdTierFirstInLowDiskCleanup(t *testing.T) {
	gib := domain.GiB
	hot, cold := &fakeDisk{free: 60 * gib, total: 100 * gib}, &fakeDisk{free: 200 * gib, total: 250 * gib}
	e := newColdEnv(t, hot, cold, nil)
	e.box("p--old", instance("p", "old"), false, true, 6*gib)
	e.advance(4 * 24 * time.Hour)
	e.box("p--mid", instance("p", "mid"), false, true, 6*gib)
	e.advance(time.Hour)
	e.box("p--new", instance("p", "new"), false, true, 6*gib)
	e.advance(20 * time.Minute)
	e.box("p--now", instance("p", "now"), false, true, 6*gib) // stopped just now

	// Floor 10 GiB, target 12: two moves (+6 each from 3) reach 15.
	hot.setFree(3 * gib)
	e.svc.freeDisk(e.ctx, 0, "")
	if tierOf(*e.vm("p--old")) != domain.TierCold || tierOf(*e.vm("p--mid")) != domain.TierCold {
		t.Fatal("the least recently used VMs did not go cold first")
	}
	if tierOf(*e.vm("p--new")) != domain.TierHot || tierOf(*e.vm("p--now")) != domain.TierHot || len(e.autoDeletions()) != 0 {
		t.Fatalf("the cleanup went past its target or deleted: %+v", e.autoDeletions())
	}
	if hot.getFree() != 15*gib || cold.getFree() != 188*gib {
		t.Fatalf("hot free %d, cold free %d", hot.getFree(), cold.getFree())
	}

	// Cold below its floor (12.5 GiB, 5% of 250): its own cleanup deletes
	// its oldest box (older than the cold instance age) before the next
	// move.
	cold.setFree(12 * gib)
	hot.setFree(3 * gib)
	e.svc.freeDisk(e.ctx, 0, "")
	if !e.gone("p--old") || tierOf(*e.vm("p--new")) != domain.TierCold {
		t.Fatalf("cold cleanup: old gone %v, new %s", e.gone("p--old"), tierOf(*e.vm("p--new")))
	}
	if ds := e.autoDeletions(); len(ds) != 1 || !strings.Contains(e.vm(string(ds[0].VMID)).DeleteReason, "cold storage low") {
		t.Fatalf("cold auto-deletion: %+v", ds)
	}
	if e.gone("p--mid") {
		t.Fatal("a cold box younger than the auto-delete age was deleted")
	}

	// Cold full with nothing it may delete (pinned): hot auto-delete is the
	// last tier, and it skips cold boxes (deleting them frees nothing hot).
	yes := true
	for _, n := range []string{"p--mid", "p--new"} {
		if _, err := e.svc.Update(e.ctx, e.req(""), n, api.UpdateVMRequest{Keep: &yes}); err != nil {
			t.Fatal(err)
		}
	}
	cold.setFree(1 * gib)
	hot.setFree(3 * gib)
	e.advance(4 * 24 * time.Hour)
	e.svc.freeDisk(e.ctx, 0, "")
	if !e.gone("p--now") || e.gone("p--mid") || e.gone("p--new") {
		t.Fatalf("hot auto-delete: now %v mid %v new %v", e.gone("p--now"), e.gone("p--mid"), e.gone("p--new"))
	}
}

// TestColdTierRestoreAdmission: resuming a cold VM reserves the bytes it
// copies back; too little hot room refuses (and queues) it like a create.
func TestColdTierRestoreAdmission(t *testing.T) {
	gib := domain.GiB
	hot := &fakeDisk{free: 60 * gib, total: 100 * gib}
	e := newColdEnv(t, hot, &fakeDisk{free: 200 * gib, total: 250 * gib}, nil)
	e.box("p--a", instance("p", "a"), false, true, 0)
	e.advance(7 * time.Hour)
	e.sweepCold()
	if tierOf(*e.vm("p--a")) != domain.TierCold {
		t.Fatal("not cold")
	}
	if err := e.db.Tx(e.ctx, func(tx *state.Tx) error {
		rec, err := lookupVM(tx, owner, "p--a")
		if err != nil {
			return err
		}
		rec.VM.ColdBytes = 20 * gib
		return tx.PutVM(rec)
	}); err != nil {
		t.Fatal(err)
	}
	// 25 GiB free: floor 10 + headroom 2 + 20 to restore do not fit.
	hot.setFree(25 * gib)
	_, err := e.svc.Resume(e.ctx, e.req(e.newKey()), "p--a", api.ResumeVMRequest{})
	if !domain.IsDiskSpaceError(err) || !strings.Contains(err.Error(), "restoring from cold storage needs 20.0 GiB") {
		t.Fatalf("resume of a cold VM without room: %v", err)
	}
	hot.setFree(40 * gib)
	e.mustSucceed(e.svc.Resume(e.ctx, e.req(e.newKey()), "p--a", api.ResumeVMRequest{}))
}

// TestColdTierDamagedCopyFailsResume: a non-box VM goes cold too, and a
// cold copy that is damaged fails the resume, keeping the VM cold.
func TestColdTierDamagedCopyFailsResume(t *testing.T) {
	gib := domain.GiB
	e := newColdEnv(t, &fakeDisk{free: 60 * gib, total: 100 * gib}, &fakeDisk{free: 200 * gib, total: 250 * gib}, nil)
	e.runWithMarker("plain", nil, "x")
	e.mustSucceed(e.svc.Stop(e.ctx, e.req(""), "plain", false))
	e.advance(7 * time.Hour)
	e.sweepCold()
	c := e.lastCommit("plain")
	if c.Tier != domain.TierCold {
		t.Fatal("not cold")
	}
	os.Chmod(c.Path, 0o644)
	f, err := os.OpenFile(c.Path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt(bytes.Repeat([]byte{7}, 16), 1<<20)
	f.Close()
	a, err := e.svc.Resume(e.ctx, e.req(e.newKey()), "plain", api.ResumeVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if op := e.wait(a.Operation.ID); op.State != domain.OpFailed || !strings.Contains(op.Error.Message, "damaged") {
		t.Fatalf("resume of a damaged cold copy: %+v", op)
	}
	if v := e.vm("plain"); tierOf(*v) != domain.TierCold {
		t.Fatalf("a failed restore made the VM %s", v.Tier)
	}
}

// TestColdTierKeepHotAndDrain: pinning a cold VM hot, or draining the
// tier, brings VMs back without booting them; a draining tier takes
// nothing new.
func TestColdTierKeepHotAndDrain(t *testing.T) {
	gib := domain.GiB
	e := newColdEnv(t, &fakeDisk{free: 60 * gib, total: 100 * gib}, &fakeDisk{free: 200 * gib, total: 250 * gib}, nil)
	e.box("p--a", instance("p", "a"), false, true, 0)
	e.runWithMarker("p--b", instance("p", "b"), "drained back")
	e.mustSucceed(e.svc.Stop(e.ctx, e.req(""), "p--b", false))
	e.advance(7 * time.Hour)
	e.sweepCold()
	if tierOf(*e.vm("p--a")) != domain.TierCold || tierOf(*e.vm("p--b")) != domain.TierCold {
		t.Fatal("not cold")
	}
	yes := true
	if _, err := e.svc.Update(e.ctx, e.req(""), "p--a", api.UpdateVMRequest{KeepHot: &yes}); err != nil {
		t.Fatal(err)
	}
	e.sweepCold()
	if v := e.vm("p--a"); tierOf(*v) != domain.TierHot || e.lastCommit("p--a").Tier != domain.TierHot || len(e.record("p--a").ColdCommits) != 0 {
		t.Fatalf("pinned hot, still %s", v.Tier)
	}
	if tierOf(*e.vm("p--b")) != domain.TierCold {
		t.Fatal("an unpinned VM came back without drain")
	}
	e.svc.cfg.Cold.Drain = true
	e.sweepCold()
	if tierOf(*e.vm("p--b")) != domain.TierHot {
		t.Fatal("drain did not bring p--b back")
	}
	if ents, _ := os.ReadDir(filepath.Join(e.coldDir, "commits")); len(ents) != 0 {
		t.Fatalf("drained tier holds %d commits", len(ents))
	}
	if c := e.svc.Capabilities().Capacity.Cold; c == nil || !c.Draining || c.VMs != 0 {
		t.Fatalf("capacity.cold while draining: %+v", c)
	}
	e.advance(7 * time.Hour)
	e.sweepCold()
	if tierOf(*e.vm("p--b")) != domain.TierHot {
		t.Fatal("a draining tier took a VM")
	}
	e.mustSucceed(e.svc.Resume(e.ctx, e.req(e.newKey()), "p--b", api.ResumeVMRequest{}))
	if !e.hasMarker("p--b", "drained back") {
		t.Fatal("data lost on the way back")
	}
}

// TestColdTierMissingDisk: with the cold tier not attached (its disk not
// mounted at start), hot VMs work, cold ones fail to resume saying why
// and stay cold; once it is back they resume.
func TestColdTierMissingDisk(t *testing.T) {
	gib := domain.GiB
	e := newColdEnv(t, &fakeDisk{free: 60 * gib, total: 100 * gib}, &fakeDisk{free: 200 * gib, total: 250 * gib}, nil)
	e.runWithMarker("p--a", instance("p", "a"), "waits for its disk")
	e.mustSucceed(e.svc.Stop(e.ctx, e.req(""), "p--a", false))
	e.box("p--hot", instance("p", "hot"), false, true, 0)
	e.advance(7 * time.Hour)
	e.sweepCold()
	e.mustSucceed(e.svc.Resume(e.ctx, e.req(e.newKey()), "p--hot", api.ResumeVMRequest{})) // stays hot from here

	e.missing = true
	e.stop()
	e.start()
	if c := e.svc.Capabilities().Capacity.Cold; c == nil || c.Unavailable == "" || c.VMs != 1 {
		t.Fatalf("capacity.cold without its disk: %+v", c)
	}
	a, err := e.svc.Resume(e.ctx, e.req(e.newKey()), "p--a", api.ResumeVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if op := e.wait(a.Operation.ID); op.State != domain.OpFailed || !strings.Contains(op.Error.Message, "cold disk not mounted") {
		t.Fatalf("resume without the cold disk: %+v", op.Error)
	}
	if tierOf(*e.vm("p--a")) != domain.TierCold {
		t.Fatal("the VM left the cold tier while its disk was missing")
	}
	e.missing = false
	e.stop()
	e.start()
	e.mustSucceed(e.svc.Resume(e.ctx, e.req(e.newKey()), "p--a", api.ResumeVMRequest{}))
	if !e.hasMarker("p--a", "waits for its disk") {
		t.Fatal("data lost")
	}
}

// TestColdTierEviction: the cold cleanup deletes cold boxes unused for the
// cold instance age (12h, not the hot 72h), least recently used first; a
// move under pressure (hot below its floor, nothing else to free) deletes
// cold boxes past that age down to PressureDeleteAge to make room, then
// moves. Never a pinned, busy or recently used cold box; the hot pressure
// tier never deletes a box used within PressureDeleteAge either.
func TestColdTierEviction(t *testing.T) {
	gib := domain.GiB
	hot, cold := &fakeDisk{free: 60 * gib, total: 100 * gib}, &fakeDisk{free: 200 * gib, total: 250 * gib}
	e := newColdEnv(t, hot, cold, nil)
	for _, n := range []string{"p--old", "p--a", "p--b", "p--busy", "p--recent"} {
		e.box(n, instance("p", n[3:]), false, true, 6*gib)
		e.advance(time.Minute)
	}
	e.box("p--pinned", instance("p", "pinned"), true, true, 6*gib)
	e.advance(DefaultColdAfter)
	e.sweepCold()
	for _, n := range []string{"p--old", "p--a", "p--b", "p--busy", "p--recent", "p--pinned"} {
		if tierOf(*e.vm(n)) != domain.TierCold {
			t.Fatalf("%s did not go cold", n)
		}
	}

	// Cold below its floor at the sweep: only boxes past the cold age go.
	e.advance(DefaultAutoDeleteColdInstanceAge - DefaultColdAfter - 2*time.Minute) // p--old 12h+3m, p--a 12h+2m, p--b 12h+1m
	e.svc.mu.Lock()
	e.svc.activity[e.vm("p--b").ID] = e.clock().Add(-11 * time.Hour) // used since
	e.svc.mu.Unlock()
	cold.setFree(7 * gib) // floor 12.5, target 14.5 (the margin): two deletions
	e.sweepCold()
	if !e.gone("p--old") || !e.gone("p--a") || e.gone("p--b") || cold.getFree() != 19*gib {
		t.Fatalf("cold age cleanup: old %v a %v b %v, cold free %s", e.gone("p--old"), e.gone("p--a"), e.gone("p--b"),
			domain.HumanBytes(cold.getFree()))
	}
	for _, d := range e.autoDeletions() {
		if !strings.Contains(d.Reason, "cold storage low") || strings.Contains(d.Reason, "under pressure") {
			t.Fatalf("cold age reason: %q", d.Reason)
		}
	}

	// Pressure: a hot box to move, cold full, nothing past any age. The
	// move deletes the least recently used cold box past PressureDeleteAge
	// (p--b, used 11h ago), never p--pinned, p--busy (a session) or
	// p--recent (used 30m ago); then a second hot box finds no room and
	// stays: it was used 20m ago, so the hot pressure tier keeps it too.
	e.box("p--h1", instance("p", "h1"), false, true, 6*gib)
	e.box("p--h2", instance("p", "h2"), false, true, 6*gib)
	e.advance(20 * time.Minute)
	e.busy.Store(e.vm("p--busy").ID, true)
	e.svc.mu.Lock()
	e.svc.activity[e.vm("p--recent").ID] = e.clock().Add(-30 * time.Minute)
	e.svc.mu.Unlock()
	cold.setFree(12 * gib)
	hot.setFree(3 * gib)
	e.svc.freeDisk(e.ctx, 0, "")
	if !e.gone("p--b") {
		t.Fatal("pressure did not evict the least recently used cold box")
	}
	for _, n := range []string{"p--pinned", "p--busy", "p--recent", "p--h1", "p--h2"} {
		if e.gone(n) {
			t.Fatalf("%s was deleted under pressure", n)
		}
	}
	moved := tierOf(*e.vm("p--h1")) == domain.TierCold
	if !moved && tierOf(*e.vm("p--h2")) != domain.TierCold {
		t.Fatal("no hot box moved into the room the eviction made")
	}
	ds := e.autoDeletions()
	if last := ds[len(ds)-1]; last.Box != "p/b" || !strings.Contains(last.Reason, "under pressure") ||
		!strings.Contains(last.Reason, "the 12h age limit waived") {
		t.Fatalf("pressure eviction: %+v", last)
	}
}

// TestColdFloor: the cold floor is the larger of 5 GiB and 5% by default,
// and follows coldStorage.minFreeBytes / minFreePercent.
func TestColdFloor(t *testing.T) {
	gib := domain.GiB
	for _, c := range []struct {
		pol         ColdPolicy
		total, want int64
	}{
		{ColdPolicy{}, 250 * gib, 250 * gib / 20},
		{ColdPolicy{}, 50 * gib, 5 * gib},
		{ColdPolicy{MinFreePercent: 10}, 250 * gib, 25 * gib},
		{ColdPolicy{MinFreeBytes: 20 * gib}, 250 * gib, 20 * gib},
		{ColdPolicy{MinFreeBytes: -1}, 250 * gib, 0},
	} {
		s := &Service{cfg: Config{Cold: &c.pol}}
		if got := s.coldFloor(c.total); got != c.want {
			t.Errorf("%+v of %s: floor %s, want %s", c.pol, domain.HumanBytes(c.total), domain.HumanBytes(got), domain.HumanBytes(c.want))
		}
	}
}

// TestColdSweepFullWarnsOnce: a sweep that finds cold storage full with
// nothing it may delete warns once per stuckEvery, not at every sweep.
func TestColdSweepFullWarnsOnce(t *testing.T) {
	gib := domain.GiB
	logs := &lockedBuf{}
	hot, cold := &fakeDisk{free: 60 * gib, total: 100 * gib}, &fakeDisk{free: 200 * gib, total: 250 * gib}
	e := newColdEnv(t, hot, cold, func(c *Config, _ *tierStore) {
		c.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	e.box("p--a", instance("p", "a"), false, true, 6*gib)
	e.advance(DefaultColdAfter + time.Minute)
	cold.setFree(gib) // below the 12.5 GiB floor, and nothing on it to delete
	const full = "cold storage: no room to move unused vms"
	e.sweepCold()
	e.advance(time.Minute)
	e.sweepCold()
	if n := logs.count(full); n != 1 {
		t.Fatalf("%d warnings in two sweeps:\n%s", n, logs.b.String())
	}
	e.advance(stuckEvery)
	e.sweepCold()
	if n := logs.count(full); n != 2 || tierOf(*e.vm("p--a")) != domain.TierHot {
		t.Fatalf("%d warnings after stuckEvery, p--a %s", n, tierOf(*e.vm("p--a")))
	}
}
