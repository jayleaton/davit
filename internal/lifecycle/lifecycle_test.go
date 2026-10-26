package lifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
	"davit/internal/state"
	"davit/internal/storage"
)

const owner = domain.PrincipalID("local:test")

type env struct {
	t    *testing.T
	dir  string
	svc  *Service
	db   *state.DB
	rt   *fake.Runtime
	opts fake.Options
	cfg  func(*Config)
	key  int
	mu   sync.Mutex
}

func newEnv(t *testing.T, opts fake.Options, cfg func(*Config)) *env {
	e := &env{t: t, dir: t.TempDir(), opts: opts, cfg: cfg}
	e.start()
	t.Cleanup(e.stop)
	return e
}

func (e *env) start() {
	t := e.t
	db, err := state.Open(context.Background(), filepath.Join(e.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	o := e.opts
	o.Dir = filepath.Join(e.dir, "runtime")
	rt, err := fake.New(o)
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.Open(filepath.Join(e.dir, "storage"), storage.ModeCopy)
	if err != nil {
		t.Fatal(err)
	}
	img, err := DevImage(filepath.Join(e.dir, "images"))
	if err != nil {
		t.Fatal(err)
	}
	c := Config{DB: db, Runtime: rt, Storage: st, Images: NewStaticCatalog(img),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Prober: RuntimeProber{Poll: time.Millisecond}}
	if e.cfg != nil {
		e.cfg(&c)
	}
	svc, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.svc, e.db, e.rt = svc, db, rt
}

// stop models a daemon exit: in-flight operations stop at a safe point and
// stay journaled.
func (e *env) stop() {
	if e.svc != nil {
		e.svc.Close()
		e.db.Close()
		e.svc = nil
	}
}

func (e *env) req(key string) Request {
	return Request{Principal: owner, IdempotencyKey: key, Method: "POST", Path: "/test"}
}

func (e *env) newKey() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.key++
	return "k" + string(rune('a'+e.key%26)) + time.Now().Format("150405.000000000")
}

func (e *env) wait(id domain.OperationID) *domain.Operation {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var after int64
	for {
		op, evs, err := e.svc.Events(ctx, owner, id, after, 2*time.Second)
		if err != nil {
			e.t.Fatalf("wait %s: %v", id, err)
		}
		if n := len(evs); n > 0 {
			after = evs[n-1].Seq
		}
		if op.State.Terminal() && len(evs) == 0 {
			return op
		}
	}
}

func (e *env) mustSucceed(a *Accepted, err error) *domain.Operation {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpSucceeded {
		e.t.Fatalf("operation %s %s: %+v", op.Kind, op.State, op.Error)
	}
	return op
}

func (e *env) vm(ref string) *domain.VM {
	e.t.Helper()
	vm, err := e.svc.GetVM(context.Background(), owner, ref)
	if err != nil {
		e.t.Fatal(err)
	}
	return vm
}

func TestFullLifecycle(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()

	a, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "web", Env: map[string]string{"TOKEN": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.VM.State != domain.StateProvisioning || a.Operation.State != domain.OpQueued {
		t.Fatalf("accepted %s/%s", a.VM.State, a.Operation.State)
	}
	op := e.mustSucceed(a, nil)
	vm := e.vm("web")
	if vm.State != domain.StateReady || vm.BootMode != domain.BootModeColdBoot || vm.InstanceGeneration != 1 || vm.ExpiresAt == nil {
		t.Fatalf("after create: %+v", vm)
	}
	if vm.Readiness.Agent {
		t.Fatal("RuntimeProber must not claim agent readiness")
	}
	if op.Result.BootMode != domain.BootModeColdBoot {
		t.Fatalf("result %+v", op.Result)
	}

	if _, err := e.svc.Fork(ctx, e.req(e.newKey()), "web", api.ForkVMRequest{}); !domain.IsCode(err, domain.CodeStopRequired) {
		t.Fatalf("fork without snapshot: %v", err)
	}

	stop := e.mustSucceed(e.svc.Stop(ctx, e.req(""), "web", false))
	vm = e.vm("web")
	if vm.State != domain.StateStopped || vm.LastSnapshotID != stop.Result.SnapshotID || vm.LastSnapshotID == "" {
		t.Fatalf("after stop: %+v", vm)
	}
	snap, err := e.svc.GetSnapshot(ctx, owner, string(vm.LastSnapshotID))
	if err != nil || snap.Consistency != domain.ConsistencyClean || snap.Kind != domain.SnapshotKindDisk {
		t.Fatalf("snapshot %+v %v", snap, err)
	}

	again, err := e.svc.Stop(ctx, e.req(""), "web", false)
	if err != nil || !again.Operation.Result.NoOp || again.Operation.State != domain.OpSucceeded {
		t.Fatalf("stop on stopped must be a no-op: %+v %v", again, err)
	}

	forkOp := e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "web", api.ForkVMRequest{Name: "web-fork"}))
	fk := e.vm("web-fork")
	if fk.State != domain.StateReady || fk.SourceSnapshotID != snap.ID || forkOp.Result.SnapshotID != snap.ID || fk.ID == vm.ID {
		t.Fatalf("fork: %+v", fk)
	}

	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "web", api.ResumeVMRequest{}))
	vm = e.vm("web")
	if vm.State != domain.StateReady || vm.InstanceGeneration != 2 {
		t.Fatalf("after resume: state %s gen %d", vm.State, vm.InstanceGeneration)
	}
	noop, err := e.svc.Resume(ctx, e.req(""), "web", api.ResumeVMRequest{})
	if err != nil || !noop.Operation.Result.NoOp {
		t.Fatalf("resume on ready must be a no-op: %v", err)
	}

	e.mustSucceed(e.svc.Delete(ctx, e.req(""), "web"))
	if _, err := e.svc.GetVM(ctx, owner, "web"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("deleted VM still resolvable by name: %v", err)
	}
	tomb := e.vm(string(vm.ID))
	if tomb.State != domain.StateDeleted || tomb.DeletedAt == nil {
		t.Fatalf("tombstone %+v", tomb)
	}
	if _, err := e.svc.GetSnapshot(ctx, owner, string(snap.ID)); err != nil {
		t.Fatalf("snapshot a live fork came from must survive source deletion: %v", err)
	}
	hs, _ := e.rt.Discover(ctx)
	for _, h := range hs {
		if h.VMID == vm.ID {
			t.Fatalf("deleted VM left runtime instance %+v", h)
		}
	}
	list, _, err := e.svc.ListVMs(ctx, owner, nil, false, Page{})
	if err != nil || len(list) != 1 || list[0].Name != "web-fork" {
		t.Fatalf("list: %+v %v", list, err)
	}
	all, _, _ := e.svc.ListVMs(ctx, owner, nil, true, Page{})
	if len(all) != 2 {
		t.Fatalf("list --all: %d", len(all))
	}
}

func TestIdempotencyAndConflicts(t *testing.T) {
	e := newEnv(t, fake.Options{BootDelay: 300 * time.Millisecond}, nil)
	ctx := context.Background()
	body := api.CreateVMRequest{Name: "a"}
	a1, err := e.svc.Create(ctx, e.req("same"), body)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := e.svc.Create(ctx, e.req("same"), body)
	if err != nil || a2.Operation.ID != a1.Operation.ID || !a2.Replayed {
		t.Fatalf("replay: %+v %v", a2, err)
	}
	if _, err := e.svc.Create(ctx, e.req("same"), api.CreateVMRequest{Name: "b"}); !domain.IsCode(err, domain.CodeIdempotencyConflict) {
		t.Fatalf("different body: %v", err)
	}
	other := e.req("same")
	other.Principal = "someone-else"
	a3, err := e.svc.Create(ctx, other, body)
	if err != nil || a3.Operation.ID == a1.Operation.ID {
		t.Fatalf("key scope must include principal: %v", err)
	}
	if _, err := e.svc.Create(ctx, e.req(""), body); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("create without key: %v", err)
	}

	_, err = e.svc.Stop(ctx, e.req(""), "a", false)
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeOperationInProgress || de.OperationID != a1.Operation.ID {
		t.Fatalf("stop during provisioning: %v", err)
	}
	e.wait(a1.Operation.ID)

	stale := int64(1)
	r := e.req("")
	r.IfMatch = &stale
	if _, err := e.svc.Stop(ctx, r, "a", false); !domain.IsCode(err, domain.CodeRevisionMismatch) {
		t.Fatalf("If-Match stale: %v", err)
	}
	cur := e.vm("a").StateRevision
	r.IfMatch = &cur
	e.mustSucceed(e.svc.Stop(ctx, r, "a", false))

	if _, err := e.svc.GetVM(ctx, "someone-else", string(a1.VM.ID)); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("other owner's VM must be invisible: %v", err)
	}
}

func TestDaemonRestartMidBootReplaysJournal(t *testing.T) {
	e := newEnv(t, fake.Options{BootDelay: 400 * time.Millisecond}, nil)
	ctx := context.Background()
	a, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, a.Operation.ID, "runtime.boot")
	e.stop() // daemon dies while the VMM is booting

	e.opts.BootDelay = 0
	e.start()
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpSucceeded {
		t.Fatalf("replayed operation %s: %+v", op.State, op.Error)
	}
	if vm := e.vm("slow"); vm.State != domain.StateReady || vm.InstanceGeneration != 1 {
		t.Fatalf("after replay: %+v", vm)
	}
	hs, _ := e.rt.Discover(ctx)
	if len(hs) != 1 {
		t.Fatalf("replay spawned %d instances, want the original one", len(hs))
	}
	_, evs, _ := e.svc.Events(ctx, owner, op.ID, 0, 0)
	var recovered bool
	for _, ev := range evs {
		recovered = recovered || ev.Phase == "recovered"
	}
	if !recovered {
		t.Fatal("journal has no recovery event")
	}
}

func TestDaemonRestartMidStopCommits(t *testing.T) {
	e := newEnv(t, fake.Options{StopDelay: 400 * time.Millisecond}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "s"}))
	a, err := e.svc.Stop(ctx, e.req(""), "s", false)
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, a.Operation.ID, "guest.shutdown")
	e.stop()
	e.opts.StopDelay = 0
	e.start()
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpSucceeded || op.Result.SnapshotID != op.Params.SnapshotID {
		t.Fatalf("replayed stop: %+v", op)
	}
	if vm := e.vm("s"); vm.State != domain.StateStopped || vm.LastSnapshotID != op.Params.SnapshotID {
		t.Fatalf("after replayed stop: %+v", vm)
	}
}

func TestLostInstanceGoesToErrorAndResumes(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"}))
	vm := e.vm("c")
	e.stop()
	if err := e.rt.Crash(runtime.Key{VMID: vm.ID, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	e.start()
	vm = e.vm("c")
	if vm.State != domain.StateError || vm.LastError == nil || vm.LastError.Code != domain.CodeUnavailable {
		t.Fatalf("crashed while daemon down: %+v", vm)
	}
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "c", api.ResumeVMRequest{}))
	vm = e.vm("c")
	if vm.State != domain.StateReady || vm.InstanceGeneration != 2 || vm.LastError != nil {
		t.Fatalf("resume from error: %+v", vm)
	}
	hs, _ := e.rt.Discover(ctx)
	if len(hs) != 1 || hs[0].Generation != 2 {
		t.Fatalf("old generation not cleaned up: %+v", hs)
	}
}

func TestFailedStopKeepsLastSnapshot(t *testing.T) {
	// One graceful attempt and no forced kill: the stop fails (stopretry.go).
	e := newEnv(t, fake.Options{}, func(c *Config) { c.StopAttempts, c.NoStopForce = 1, true })
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "f"}))
	first := e.mustSucceed(e.svc.Stop(ctx, e.req(""), "f", false))
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "f", api.ResumeVMRequest{}))

	e.rt.InjectFault("Stop", domain.Errorf(domain.CodeTimeout, "guest hung"))
	a, err := e.svc.Stop(ctx, e.req(""), "f", false)
	if err != nil {
		t.Fatal(err)
	}
	op := e.wait(a.Operation.ID)
	vm := e.vm("f")
	if op.State != domain.OpFailed || vm.State != domain.StateError || vm.LastSnapshotID != first.Result.SnapshotID {
		t.Fatalf("failed stop: op %s vm %s last %s", op.State, vm.State, vm.LastSnapshotID)
	}
	forced := e.mustSucceed(e.svc.Resume(ctx, e.req(""), "f", api.ResumeVMRequest{}))
	_ = forced
	fo := e.mustSucceed(e.svc.Stop(ctx, e.req(""), "f", true))
	snap, _ := e.svc.GetSnapshot(ctx, owner, string(fo.Result.SnapshotID))
	if snap.Consistency != domain.ConsistencyCrash {
		t.Fatalf("forced stop must record crash consistency, got %s", snap.Consistency)
	}
}

func TestTTLSweepStops(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Now = clock; c.SweepInterval = time.Hour })
	ctx := context.Background()
	ttl := int64(60)
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "ttl", TTLSeconds: &ttl}))
	no := false
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "keep", TTLSeconds: &ttl, AutoStop: &no}))
	mu.Lock()
	now = now.Add(61 * time.Second)
	mu.Unlock()
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if id := e.vm("ttl").ActiveOperationID; id != "" {
		e.wait(id)
	}
	if s := e.vm("ttl").State; s != domain.StateStopped {
		t.Fatalf("expired VM is %s", s)
	}
	if s := e.vm("keep").State; s != domain.StateReady {
		t.Fatalf("no-auto-stop VM is %s", s)
	}
}

// TestTurnLeaseHoldsAutoStop: an active prompt turn's lease defers the TTL
// stop until it ends; then the expired VM stops on the next sweep.
func TestTurnLeaseHoldsAutoStop(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	var leased domain.VMID
	var until time.Time
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.Now, c.SweepInterval = clock, time.Hour
		c.TurnLease = func(_ context.Context, vm domain.VMID) (time.Time, error) {
			mu.Lock()
			defer mu.Unlock()
			if vm == leased {
				return until, nil
			}
			return time.Time{}, nil
		}
	})
	ctx := context.Background()
	ttl := int64(60)
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "turn", TTLSeconds: &ttl}))
	mu.Lock()
	leased, until = e.vm("turn").ID, now.Add(5*time.Minute)
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if v := e.vm("turn"); v.State != domain.StateReady || v.ActiveOperationID != "" {
		t.Fatalf("leased VM was stopped: %s", v.State)
	}
	mu.Lock()
	now = until
	mu.Unlock()
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if id := e.vm("turn").ActiveOperationID; id != "" {
		e.wait(id)
	}
	if s := e.vm("turn").State; s != domain.StateStopped {
		t.Fatalf("VM after its lease ended is %s", s)
	}
}

func TestAdmissionAndValidation(t *testing.T) {
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Limits = domain.Limits{CPUs: 3} })
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "one"}))
	if _, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "two"}); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("over CPU limit: %v", err)
	}
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "one", false))
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "two"}))
	if _, err := e.svc.Resume(ctx, e.req(""), "one", api.ResumeVMRequest{}); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("resume over CPU limit: %v", err)
	}
	for name, body := range map[string]api.CreateVMRequest{
		"bad name":     {Name: "Bad_Name"},
		"dup name":     {Name: "two"},
		"image+snap":   {Image: "dev", SnapshotID: "snap_x"},
		"bad env":      {Env: map[string]string{"A=B": "c"}},
		"golden":       {RequireGolden: true},
		"no image":     {Image: "nope"},
		"shrink disk":  {Shape: &api.ShapeOverride{DiskBytes: 1}},
		"huge cpus":    {Shape: &api.ShapeOverride{CPUs: 1000}},
		"negative ttl": {TTLSeconds: new(int64(-1))},
	} {
		if _, err := e.svc.Create(ctx, e.req(e.newKey()), body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOrphanInstancesDestroyedOnStart(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	disk := filepath.Join(e.dir, "orphan.img")
	if err := writeFile(disk); err != nil {
		t.Fatal(err)
	}
	e.stop()
	if _, err := e.rt.ColdBoot(ctx, runtime.Instance{Key: runtime.Key{VMID: "vm_ghost", Generation: 1}}, runtime.DiskSet{Root: disk}, runtime.NetworkHandle{}); err != nil {
		t.Fatal(err)
	}
	e.start()
	if hs, _ := e.rt.Discover(ctx); len(hs) != 0 {
		t.Fatalf("orphan survived reconciliation: %+v", hs)
	}
}

func waitPhase(t *testing.T, e *env, id domain.OperationID, phase string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, err := e.svc.GetOperation(context.Background(), owner, id)
		if err != nil {
			t.Fatal(err)
		}
		if op.Phase == phase {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("operation never reached phase %s", phase)
}

func writeFile(p string) error { return os.WriteFile(p, []byte("disk"), 0o600) }

func TestNamedSnapshotsPruningAndGC(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	store := e.svc.cfg.Storage
	commitOf := func(ref string) storage.CommitID {
		t.Helper()
		sn, err := e.svc.GetSnapshot(ctx, owner, ref)
		if err != nil {
			t.Fatal(err)
		}
		return storage.CommitID(sn.CommitID)
	}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "src"}))
	if _, err := e.svc.Snapshot(ctx, e.req(e.newKey()), "src", api.SnapshotVMRequest{Name: "base"}); !domain.IsCode(err, domain.CodeStopRequired) {
		t.Fatalf("snapshot of a ready VM: %v", err)
	}
	first := e.mustSucceed(e.svc.Stop(ctx, e.req(""), "src", false)).Result.SnapshotID

	a, err := e.svc.Snapshot(ctx, e.req(e.newKey()), "src", api.SnapshotVMRequest{Name: "base"})
	if err != nil || a.Operation.State != domain.OpSucceeded || a.Operation.Result.SnapshotID == first {
		t.Fatalf("snapshot create: %+v %v", a, err)
	}
	base, _ := e.svc.GetSnapshot(ctx, owner, "base")
	if base.Name != "base" || base.CommitID != string(commitOf(string(first))) {
		t.Fatalf("named snapshot must share the stop commit: %+v", base)
	}
	if _, err := e.svc.Snapshot(ctx, e.req(e.newKey()), "src", api.SnapshotVMRequest{Name: "base"}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("duplicate name: %v", err)
	}

	// A second stop supersedes the first automatic snapshot; the named one stays.
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "src", api.ResumeVMRequest{}))
	second := e.mustSucceed(e.svc.Stop(ctx, e.req(""), "src", false)).Result.SnapshotID
	if _, err := e.svc.GetSnapshot(ctx, owner, string(first)); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("superseded automatic snapshot not pruned: %v", err)
	}
	if _, err := store.GetCommit(ctx, storage.CommitID(base.CommitID)); err != nil {
		t.Fatalf("commit still named by %q was collected: %v", base.Name, err)
	}

	if _, err := e.svc.DeleteSnapshot(ctx, owner, string(second)); !domain.IsCode(err, domain.CodeInvalidState) {
		t.Fatalf("deleting a VM's last committed disk: %v", err)
	}
	e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "src", api.ForkVMRequest{Name: "child", SnapshotID: "base"}))
	if _, err := e.svc.DeleteSnapshot(ctx, owner, "base"); !domain.IsCode(err, domain.CodeInvalidState) {
		t.Fatalf("deleting the only resume source of a fork: %v", err)
	}
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "child", false))
	if _, err := e.svc.DeleteSnapshot(ctx, owner, "base"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetCommit(ctx, storage.CommitID(base.CommitID)); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("unreferenced commit not collected: %v", err)
	}
	if _, err := e.svc.DeleteSnapshot(ctx, owner, "base"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("repeat delete: %v", err)
	}

	// Named snapshots outlive their VM; automatic ones go with it.
	e.mustSucceed(e.svc.Snapshot(ctx, e.req(e.newKey()), "src", api.SnapshotVMRequest{Name: "keep"}))
	e.mustSucceed(e.svc.Delete(ctx, e.req(""), "src"))
	if _, err := e.svc.GetSnapshot(ctx, owner, "keep"); err != nil {
		t.Fatalf("named snapshot deleted with its VM: %v", err)
	}
	if _, err := e.svc.GetSnapshot(ctx, owner, string(second)); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("automatic snapshot survived VM deletion: %v", err)
	}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "reborn", SnapshotID: "keep"}))
}

func TestCollectKeepsInFlightStopCommit(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "v"}))
	opID := domain.OperationID("op_inflight")
	rec := e.vm("v")
	// Simulate a stop that has published its commit but not yet its snapshot row.
	err := e.db.Tx(ctx, func(tx *state.Tx) error {
		return tx.PutOperation(&domain.Operation{ID: opID, Kind: domain.OpStop, VMID: rec.ID, Owner: owner, State: domain.OpRunning, CreatedAt: time.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	vmRec, _ := func() (r *state.VMRecord, err error) {
		err = e.db.Tx(ctx, func(tx *state.Tx) error { r, err = tx.GetVM(rec.ID); return err })
		return
	}()
	if _, err := e.svc.cfg.Storage.Commit(ctx, storage.DiskID(vmRec.DiskID), stopCommitID(opID)); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.collect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.cfg.Storage.GetCommit(ctx, stopCommitID(opID)); err != nil {
		t.Fatalf("collection removed an in-flight stop's commit: %v", err)
	}
}

func TestGoldenNew(t *testing.T) {
	e := newEnv(t, fake.Options{Golden: true}, nil)
	ctx := context.Background()
	op := e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "g"}))
	if vm := e.vm("g"); vm.BootMode != domain.BootModeGoldenRestore || op.Result.BootMode != domain.BootModeGoldenRestore {
		t.Fatalf("new on a golden-capable runtime: vm %s, result %s", vm.BootMode, op.Result.BootMode)
	}
	// Disk-only resume and fork never restore golden RAM (SPEC §6).
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "g", false))
	if op := e.mustSucceed(e.svc.Resume(ctx, e.req(""), "g", api.ResumeVMRequest{})); op.Result.BootMode != domain.BootModeColdBoot {
		t.Fatalf("resume %s", op.Result.BootMode)
	}
	if op := e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "g", api.ForkVMRequest{Name: "f"})); op.Result.BootMode != domain.BootModeColdBoot {
		t.Fatalf("fork %s", op.Result.BootMode)
	}

	// A failed capture falls back to an honestly reported cold boot...
	e.rt.InjectFault("Prepare", errors.New("capture failed"))
	if op := e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"})); op.Result.BootMode != domain.BootModeColdBoot {
		t.Fatalf("fallback %s", op.Result.BootMode)
	}
	// ...unless the caller required golden.
	e.rt.InjectFault("Prepare", errors.New("capture failed"))
	a, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "r", RequireGolden: true})
	if err != nil {
		t.Fatal(err)
	}
	if op := e.wait(a.Operation.ID); op.State != domain.OpFailed {
		t.Fatalf("require-golden without a golden: %s", op.State)
	}
}

func TestGoldenReplayAfterBootColdBoots(t *testing.T) {
	e := newEnv(t, fake.Options{Golden: true, BootDelay: 400 * time.Millisecond}, nil)
	ctx := context.Background()
	a, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "g"})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, a.Operation.ID, "runtime.boot")
	e.stop()
	// The restored VMM ran (and may have written its disk) before dying.
	if err := e.rt.Crash(runtime.Key{VMID: a.VM.ID, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	e.opts.BootDelay = 0
	e.start()
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpSucceeded || op.Result.BootMode != domain.BootModeColdBoot {
		t.Fatalf("replay after a journaled golden boot must cold boot: %s %s %+v", op.State, op.Result.BootMode, op.Error)
	}
}

func TestColdBootPlanSurvivesReplay(t *testing.T) {
	e := newEnv(t, fake.Options{Golden: true, BootDelay: 400 * time.Millisecond}, nil)
	ctx := context.Background()
	// Attempt 1 has no golden: the disk comes from the image rootfs.
	e.rt.InjectFault("Prepare", errors.New("capture failed"))
	a, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "p"})
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, a.Operation.ID, "runtime.boot")
	e.stop()
	if err := e.rt.Crash(runtime.Key{VMID: a.VM.ID, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	// Restarts (each rewriting the phase) now find a golden; the journaled
	// plan, not the phase, must keep them from restoring it onto that disk.
	for range 2 {
		e.opts.BootDelay = 0
		e.start()
		e.stop()
	}
	e.start()
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpSucceeded || op.Result.BootMode != domain.BootModeColdBoot || op.Params.BootPlan != "cold" {
		t.Fatalf("replay: %s %s plan %q %+v", op.State, op.Result.BootMode, op.Params.BootPlan, op.Error)
	}
}

func TestLiveFork(t *testing.T) {
	e := newEnv(t, fake.Options{LiveFork: true}, nil)
	ctx := context.Background()
	src := e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "src"}))
	op := e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "src", api.ForkVMRequest{Name: "kid", Live: true}))
	if op.Result.BootMode != domain.BootModeRAMRestore || op.Params.CheckpointID == "" {
		t.Fatalf("live fork: mode %s checkpoint %q", op.Result.BootMode, op.Params.CheckpointID)
	}
	if kid := e.vm("kid"); kid.State != domain.StateReady || kid.SourceSnapshotID != "" {
		t.Fatalf("child %+v", kid)
	}
	// The source keeps running (unpaused) and no snapshot was required.
	hs, _ := e.rt.Discover(ctx)
	for _, h := range hs {
		if h.VMID == src.VMID {
			if st, _ := e.rt.Inspect(ctx, h); st.Phase != runtime.PhaseRunning {
				t.Fatalf("source after live fork: %s", st.Phase)
			}
		}
	}
	if m, _ := filepath.Glob(filepath.Join(e.dir, "runtime", "*.cp.json")); len(m) != 0 {
		t.Fatalf("checkpoint not discarded: %v", m)
	}

	// A failed checkpoint fails the fork and never leaves the source paused.
	e.rt.InjectFault("Checkpoint", errors.New("snapshot failed"))
	a, err := e.svc.Fork(ctx, e.req(e.newKey()), "src", api.ForkVMRequest{Name: "bad", Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if op := e.wait(a.Operation.ID); op.State != domain.OpFailed {
		t.Fatalf("fork with a failing checkpoint: %s", op.State)
	}
	hs, _ = e.rt.Discover(ctx)
	for _, h := range hs {
		if h.VMID == src.VMID {
			if st, _ := e.rt.Inspect(ctx, h); st.Phase != runtime.PhaseRunning {
				t.Fatalf("source after a failed live fork: %s", st.Phase)
			}
		}
	}

	// Rejections are explicit and create nothing.
	for name, body := range map[string]api.ForkVMRequest{
		"shape":    {Name: "x1", Live: true, Shape: &api.ShapeOverride{CPUs: 4}},
		"snapshot": {Name: "x2", Live: true, SnapshotID: "snap_x"},
	} {
		if _, err := e.svc.Fork(ctx, e.req(e.newKey()), "src", body); !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "kid", false))
	if _, err := e.svc.Fork(ctx, e.req(e.newKey()), "kid", api.ForkVMRequest{Name: "x3", Live: true}); !domain.IsCode(err, domain.CodeInvalidState) {
		t.Errorf("live fork of a stopped vm: %v", err)
	}

	// A daemon that died with the source paused (mid-checkpoint) unpauses it.
	var sh runtime.Handle
	hs, _ = e.rt.Discover(ctx)
	for _, h := range hs {
		if h.VMID == src.VMID {
			sh = h
		}
	}
	if err := e.rt.Pause(ctx, sh); err != nil {
		t.Fatal(err)
	}
	e.stop()
	e.start()
	if st, _ := e.rt.Inspect(ctx, sh); st.Phase != runtime.PhaseRunning {
		t.Fatalf("recovery left the source %s", st.Phase)
	}
}

func TestLiveForkNeedsCapability(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "src"}))
	if _, err := e.svc.Fork(ctx, e.req(e.newKey()), "src", api.ForkVMRequest{Name: "kid", Live: true}); !domain.IsCode(err, domain.CodeUnsupportedCapability) {
		t.Fatalf("live fork without capability: %v", err)
	}
}
