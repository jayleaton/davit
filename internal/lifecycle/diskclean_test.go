package lifecycle

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
	"davit/internal/state"
	"davit/internal/storage"
)

// fakeDisk models the data dir's filesystem: free space the test sets
// (guests writing), a save reserve that takes its size while armed, and
// box disks whose space comes back when their VM's storage is collected
// after a delete.
type fakeDisk struct {
	mu          sync.Mutex
	free, total int64
	reserve     int64 // save reserve size (0: none)
	armed       bool
	commitNeed  int64                 // a commit fails with ENOSPC below this
	cost        map[domain.VMID]int64 // freed when the VM's storage is collected
	db          *state.DB
}

func (d *fakeDisk) setFree(n int64) { d.mu.Lock(); d.free = n; d.mu.Unlock() }
func (d *fakeDisk) getFree() int64  { d.mu.Lock(); defer d.mu.Unlock(); return d.free }
func (d *fakeDisk) setCost(vm domain.VMID, n int64) {
	d.mu.Lock()
	d.cost[vm] = n
	d.mu.Unlock()
}

func (d *fakeDisk) usage() (domain.DiskUsage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return domain.DiskUsage{FreeBytes: d.free, TotalBytes: d.total}, nil
}

func (d *fakeDisk) Size() int64 { return d.reserve }
func (d *fakeDisk) Armed() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.armed }
func (d *fakeDisk) Arm(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.armed {
		d.armed, d.free = true, d.free-d.reserve
	}
	return nil
}
func (d *fakeDisk) Release() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.armed {
		return false
	}
	d.armed, d.free = false, d.free+d.reserve
	return true
}

// diskStore charges commits against fakeDisk and gives a deleted VM's
// space back when its storage is collected.
type diskStore struct {
	storage.Store
	d *fakeDisk
}

func (s diskStore) Commit(ctx context.Context, disk storage.DiskID, id storage.CommitID) (storage.Commit, error) {
	if s.d.getFree() < s.d.commitNeed {
		return storage.Commit{}, enospc("write")
	}
	return s.Store.Commit(ctx, disk, id)
}

func (s diskStore) Collect(ctx context.Context, keep storage.KeepSet) (storage.Collected, error) {
	_ = s.d.db.Tx(ctx, func(tx *state.Tx) error {
		all, err := tx.AllVMs()
		for _, r := range all {
			if r.VM.State != domain.StateDeleting && r.VM.State != domain.StateDeleted {
				continue
			}
			s.d.mu.Lock()
			s.d.free += s.d.cost[r.VM.ID]
			delete(s.d.cost, r.VM.ID)
			s.d.mu.Unlock()
		}
		return err
	})
	return s.Store.Collect(ctx, keep)
}

// diskEnv is an env on a fakeDisk with a settable clock.
type diskEnv struct {
	*env
	d    *fakeDisk
	mu   sync.Mutex
	now  time.Time
	busy sync.Map
	ctx  context.Context
}

func newDiskEnv(t *testing.T, d *fakeDisk, opts fake.Options, adjust func(*Config)) *diskEnv {
	de := &diskEnv{d: d, now: time.Now(), ctx: context.Background()}
	if d.cost == nil {
		d.cost = map[domain.VMID]int64{}
	}
	de.env = newEnv(t, opts, func(c *Config) {
		d.db = c.DB
		c.Now, c.SweepInterval = de.clock, time.Hour
		c.DiskSpace, c.Storage = d.usage, diskStore{Store: c.Storage, d: d}
		if d.reserve > 0 {
			c.SaveReserve = d
		}
		c.Limits.MinFreeDiskBytes = 5 * domain.GiB
		c.Busy = func(vm domain.VMID, _ int64) bool { _, ok := de.busy.Load(vm); return ok }
		if adjust != nil {
			adjust(c)
		}
	})
	return de
}

func (e *diskEnv) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *diskEnv) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

// box creates (and with stop, stops) a box VM; cost is the space its
// deletion gives back.
func (e *diskEnv) box(name string, labels map[string]string, keep, stop bool, cost int64) *domain.VM {
	e.t.Helper()
	no := false
	e.mustSucceed(e.svc.Create(e.ctx, e.req(e.newKey()), api.CreateVMRequest{Name: name, AutoStop: &no, Labels: labels, Keep: keep}))
	if stop {
		e.mustSucceed(e.svc.Stop(e.ctx, e.req(""), name, false))
	}
	vm := e.vm(name)
	e.d.setCost(vm.ID, cost)
	return vm
}

func instance(project, inst string) map[string]string {
	return map[string]string{domain.LabelBox: project, domain.LabelBoxInstance: inst, domain.LabelBoxGeneration: "1"}
}

func builder(project string) map[string]string {
	return map[string]string{domain.LabelBox: project, domain.LabelBoxRole: domain.BoxRoleImage}
}

// gone reports whether ref has been deleted.
func (e *diskEnv) gone(ref string) bool {
	e.t.Helper()
	vm, err := e.svc.GetVM(e.ctx, owner, ref)
	if domain.IsCode(err, domain.CodeNotFound) {
		return true
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return vm.State == domain.StateDeleted
}

func (e *diskEnv) autoDeletions() []domain.AutoDeletion {
	e.t.Helper()
	var out []domain.AutoDeletion
	if err := e.db.Tx(e.ctx, func(tx *state.Tx) error {
		rows, err := tx.ListEvents(owner, 0, 1000, state.EventFilter{Types: []string{api.EventVMAutoDeleted}})
		for _, r := range rows {
			var d domain.AutoDeletion
			if err := json.Unmarshal(r.Data, &d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// TestDiskAdmissionQueues: a create that would leave less than the floor
// after its growth headroom is refused at once with the disk message, or
// with a wait queues (resource disk, the space it needs); the queued
// request runs the low-disk cleanup for what it needs, which deletes an
// old unused box, and is then admitted.
func TestDiskAdmissionQueues(t *testing.T) {
	d := &fakeDisk{free: 50 * domain.GiB}
	e := newDiskEnv(t, d, fake.Options{}, func(c *Config) { c.AutoDelete.InstanceMinAge = time.Hour })
	e.box("p--old", instance("p", "old"), false, true, 3*domain.GiB)
	e.advance(2 * time.Hour)

	// 6 GiB free: the default shape's headroom (2 GiB) plus the 5 GiB
	// floor needs 7.
	d.setFree(6 * domain.GiB)
	_, err := e.svc.Create(e.ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "web"})
	de := domain.AsError(err)
	if !domain.IsDiskSpaceError(err) || !domain.IsRoomError(err) || !de.Retryable ||
		!strings.Contains(de.Message, "host disk low: 6.0 GiB free, need 7.0 GiB") ||
		!strings.Contains(de.Message, "box gc") || !strings.Contains(de.Message, "box rm") || de.Details["needBytes"] != 7*domain.GiB {
		t.Fatalf("refusal without a wait: %v %+v", err, de.Details)
	}
	if e.gone("p--old") {
		t.Fatal("a request that may not wait must not start the cleanup")
	}

	// Nothing can be freed yet (the box is pinned): the request queues.
	if _, err := e.svc.Update(e.ctx, e.req(""), "p--old", api.UpdateVMRequest{Keep: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	e.advance(2 * time.Hour) // pinning is a use
	req := e.req(e.newKey())
	req.CapacityWait = 20 * time.Second
	done := make(chan error, 1)
	go func() {
		a, err := e.svc.Create(e.ctx, req, api.CreateVMRequest{Name: "web"})
		if err == nil {
			e.wait(a.Operation.ID)
		}
		done <- err
	}()
	var w domain.CapacityWaiter
	for deadline := time.Now().Add(10 * time.Second); ; {
		if q := e.svc.Capabilities().Capacity.Waiting; len(q) == 1 && q[0].Resource == "disk" {
			w = q[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the create did not queue for disk")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if w.DiskBytes != 7*domain.GiB || w.Ticket != domain.CapacityTicket(req.IdempotencyKey) {
		t.Fatalf("waiter: %+v", w)
	}
	time.Sleep(3 * waitPoll)
	if e.gone("p--old") {
		t.Fatal("a pinned box was auto-deleted")
	}

	// Unpinned, the queued request's cleanup deletes it: 6+3 = 9 GiB is
	// the 7 needed plus the 2 GiB margin.
	if _, err := e.svc.Update(e.ctx, e.req(""), "p--old", api.UpdateVMRequest{Keep: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	e.advance(2 * time.Hour)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("queued create: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("queued create was not admitted")
	}
	if !e.gone("p--old") || len(e.autoDeletions()) != 1 {
		t.Fatalf("the old box was not auto-deleted for the queued create: %+v", e.autoDeletions())
	}
}

// TestDiskAdmissionReservesStartingVMs: VMs accepted but still
// provisioning have not written yet; their headroom counts as reserved, so
// a burst cannot all be admitted against the same free space.
func TestDiskAdmissionReservesStartingVMs(t *testing.T) {
	d := &fakeDisk{free: 9 * domain.GiB}
	e := newDiskEnv(t, d, fake.Options{BootDelay: time.Minute}, nil) // accepted creates stay provisioning
	if _, err := e.svc.Create(e.ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	// 9 GiB free - 2 reserved for a - 2 for b = 5, the floor: b fits; c not.
	if _, err := e.svc.Create(e.ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "b"}); err != nil {
		t.Fatalf("second create: %v", err)
	}
	_, err := e.svc.Create(e.ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c"})
	if de := domain.AsError(err); !domain.IsDiskSpaceError(err) || de.Details["reservedBytes"] != 4*domain.GiB {
		t.Fatalf("third create with two starting: %v %+v", err, de.Details)
	}
	if c := e.svc.Capabilities().Capacity; c.DiskReservedBytes != 4*domain.GiB || c.DiskFloorBytes != 5*domain.GiB || c.Slots != 0 {
		t.Fatalf("capacity: %+v", c)
	}
}

// TestSaveReserve: the reserve is armed at start; a stop that begins with
// less free space than the reserve releases it, and so does a commit that
// runs out of space (then it is retried), so the stop succeeds; the
// sweeper arms it again once twice its size is free.
func TestSaveReserve(t *testing.T) {
	d := &fakeDisk{free: 30 * domain.GiB, reserve: 2 * domain.GiB, commitNeed: 3 * domain.GiB}
	e := newDiskEnv(t, d, fake.Options{}, nil)
	for deadline := time.Now().Add(5 * time.Second); !d.Armed(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("save reserve not armed at start")
		}
	}
	if d.getFree() != 28*domain.GiB {
		t.Fatalf("free after arming: %d", d.getFree())
	}
	no := false
	for _, n := range []string{"a", "b"} {
		e.mustSucceed(e.svc.Create(e.ctx, e.req(e.newKey()), api.CreateVMRequest{Name: n, AutoStop: &no}))
	}

	// Guests filled the disk to 1 GiB: the stop releases the reserve first.
	d.setFree(domain.GiB)
	if op := e.mustSucceed(e.svc.Stop(e.ctx, e.req(""), "a", false)); op.State != domain.OpSucceeded {
		t.Fatalf("stop on a full disk: %+v", op)
	}
	if d.Armed() || d.getFree() != 3*domain.GiB {
		t.Fatalf("reserve after the stop: armed %v, free %d", d.Armed(), d.getFree())
	}
	if c := e.svc.Capabilities().Capacity; c.SaveReserveBytes != 2*domain.GiB || c.SaveReserveArmed {
		t.Fatalf("capacity: %+v", c)
	}

	// Not re-armed below twice its size; at 10 GiB it is.
	if err := e.svc.Sweep(e.ctx); err != nil || d.Armed() {
		t.Fatalf("re-armed with 3 GiB free: %v", err)
	}
	d.setFree(10 * domain.GiB)
	// The stop's operation ends before it has finished collecting; a sweep
	// while it still runs leaves the reserve to the next one.
	for deadline := time.Now().Add(5 * time.Second); !d.Armed(); time.Sleep(10 * time.Millisecond) {
		if err := e.svc.Sweep(e.ctx); err != nil || time.Now().After(deadline) {
			t.Fatalf("not re-armed with 10 GiB free: %v", err)
		}
	}

	// 2.5 GiB free is above the reserve, but the commit needs 3: it fails
	// with ENOSPC, releases the reserve and succeeds on the retry.
	d.setFree(5 * domain.GiB / 2)
	if op := e.mustSucceed(e.svc.Stop(e.ctx, e.req(""), "b", false)); op.State != domain.OpSucceeded {
		t.Fatalf("stop whose commit ran out of space: %+v", op)
	}
	if d.Armed() {
		t.Fatal("the reserve was not released for the commit")
	}
}

// TestAutoDelete: below the floor the cleanup deletes unused boxes,
// project images without instances before stopped instances, each least
// recently used first, and stops once free space is the floor plus the
// margin. It never deletes a pinned box, one used within the minimum age,
// a running one, a busy one (open session), an image that still has
// instances or a VM that is not a box. Each deletion is recorded: warn
// log, deleteReason, operation reason auto-delete and a vm.autodeleted
// event.
func TestAutoDelete(t *testing.T) {
	d := &fakeDisk{free: 100 * domain.GiB, total: 100 * domain.GiB}
	e := newDiskEnv(t, d, fake.Options{}, nil)
	gib := domain.GiB

	// An image without instances, with a generation.
	img := e.box("img--image", builder("img"), false, true, 5*gib)
	if _, err := e.svc.Snapshot(e.ctx, e.req(e.newKey()), "img--image", api.SnapshotVMRequest{Name: "img-image-g1"}); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Hour)
	// An image that still has instances.
	e.box("p--image", builder("p"), false, true, 5*gib)
	e.box("p--a", instance("p", "a"), false, true, 5*gib) // oldest instance
	e.advance(time.Hour)
	e.box("p--b", instance("p", "b"), false, true, 5*gib)
	e.box("p--pinned", instance("p", "pinned"), true, true, 5*gib)
	busy := e.box("p--busy", instance("p", "busy"), false, true, 5*gib)
	e.busy.Store(busy.ID, true)
	e.box("plain", nil, false, true, 5*gib)
	e.advance(4 * 24 * time.Hour)
	e.box("p--running", instance("p", "running"), false, false, 5*gib)
	e.advance(time.Hour)
	e.box("p--recent", instance("p", "recent"), false, true, 5*gib) // within PressureDeleteAge
	e.advance(30 * time.Minute)

	// The floor is 10% of 100 GiB; the target 10 + 2 margin. 3 GiB free:
	// the image (+5) and then the oldest instance (+5) reach 13.
	d.setFree(3 * gib)
	e.svc.freeDisk(e.ctx, 0, "")
	if !e.gone("img--image") || !e.gone("p--a") {
		t.Fatalf("deleted: img--image %v, p--a %v", e.gone("img--image"), e.gone("p--a"))
	}
	if snaps := e.snapshotsOf(img.ID); len(snaps) != 0 {
		t.Fatalf("the image's generations were kept: %+v", snaps)
	}
	if e.gone("p--b") {
		t.Fatal("the cleanup went on past the floor plus the margin")
	}
	got := e.autoDeletions()
	if len(got) != 2 || got[0].Box != "img" || got[0].Kind != "image" || got[1].Box != "p/a" || got[1].Kind != "instance" ||
		got[1].FreedBytes != 5*gib || got[1].UnusedSeconds < int64(4*24*3600) || got[1].LastUsedAt.IsZero() {
		t.Fatalf("audit events (LRU, images first): %+v", got)
	}
	a := e.vm(string(got[1].VMID))
	if !strings.HasPrefix(a.DeleteReason, "auto-deleted: unused for ") || !strings.Contains(a.DeleteReason, "host disk low") {
		t.Fatalf("deleteReason: %q", a.DeleteReason)
	}
	var reasons []string
	if err := e.db.Tx(e.ctx, func(tx *state.Tx) error {
		rows, err := tx.ListEvents(owner, 0, 1000, state.EventFilter{Types: []string{"operation.created"}, VMID: a.ID})
		for _, r := range rows {
			reasons = append(reasons, string(r.Data))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(reasons, func(s string) bool { return strings.Contains(s, `"reason":"auto-delete"`) }) {
		t.Fatalf("the delete operation has no reason auto-delete: %v", reasons)
	}

	// Still low: the next instance goes, then nothing else may.
	d.setFree(3 * gib)
	e.svc.freeDisk(e.ctx, 0, "")
	if !e.gone("p--b") {
		t.Fatal("p--b was not deleted")
	}
	for _, n := range []string{"p--image", "p--pinned", "p--busy", "plain", "p--recent", "p--running"} {
		if e.gone(n) {
			t.Fatalf("%s was auto-deleted", n)
		}
	}
	if d.getFree() != 8*gib || len(e.autoDeletions()) != 3 {
		t.Fatalf("free %d, deletions %d", d.getFree(), len(e.autoDeletions()))
	}

	// Above the floor nothing runs; off, nothing is deleted.
	d.setFree(50 * gib)
	e.svc.freeDisk(e.ctx, 0, "")
	if len(e.autoDeletions()) != 3 {
		t.Fatal("the cleanup ran above the floor")
	}
}

// TestAutoDeleteDisabled: with auto-delete off the cleanup deletes no box.
func TestAutoDeleteDisabled(t *testing.T) {
	d := &fakeDisk{free: 100 * domain.GiB}
	e := newDiskEnv(t, d, fake.Options{}, func(c *Config) { c.AutoDelete.Disabled = true })
	e.box("p--a", instance("p", "a"), false, true, 5*domain.GiB)
	e.advance(10 * 24 * time.Hour)
	d.setFree(domain.GiB)
	e.svc.freeDisk(e.ctx, 0, "")
	if e.gone("p--a") {
		t.Fatal("auto-delete is off")
	}
}

// TestBlobPrune: only a blob no manifest names (anywhere in it), no served
// image uses and that has not changed for the minimum age is removed;
// files that are not sha256-named blobs are never touched, a dry run
// removes nothing, and a manifest that cannot be read stops the prune.
func TestBlobPrune(t *testing.T) {
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o755); err != nil {
		t.Fatal(err)
	}
	h := func(c byte) string { return strings.Repeat(string(c), 64) }
	kernel, agent, served, orphan := h('a'), h('b'), h('c'), h('d')
	for _, n := range []string{kernel, agent, served, orphan, "notes.txt", orphan + ".partial", strings.ToUpper(h('e'))} {
		if err := os.WriteFile(filepath.Join(blobs, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m1, m2 := filepath.Join(dir, "m1.json"), filepath.Join(dir, "m2.json")
	os.WriteFile(m1, []byte(`{"kernel":{"digest":"sha256:`+kernel+`"}}`), 0o644)
	os.WriteFile(m2, []byte(`{"guest":{"agent":{"binary":{"digest":"sha256:`+agent+`"}}}}`), 0o644)
	manifests := []string{m1, m2}
	b := &BlobStore{Dir: filepath.Join(dir, "blobs"), Manifests: func() ([]string, error) { return manifests, nil }}
	inUse := []string{filepath.Join(blobs, served)}
	ctx := context.Background()
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(blobs, n)); return err == nil }

	// Everything was just written: nothing is old enough.
	if res, err := b.prune(ctx, inUse, false, time.Now()); err != nil || len(res.Removed) != 0 {
		t.Fatalf("fresh blobs: %+v %v", res, err)
	}
	later := time.Now().Add(2 * time.Hour)
	if res, err := b.prune(ctx, inUse, true, later); err != nil || len(res.Removed) != 1 || res.Removed[0].Digest != "sha256:"+orphan || !exists(orphan) {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	// An unreadable manifest might name the orphan: stop.
	manifests = append(manifests, filepath.Join(dir, "missing.json"))
	if _, err := b.prune(ctx, inUse, false, later); err == nil || !exists(orphan) {
		t.Fatalf("prune with an unreadable manifest: %v", err)
	}
	manifests = manifests[:2]
	res, err := b.prune(ctx, inUse, false, later)
	if err != nil || len(res.Removed) != 1 || res.Kept != 3 || exists(orphan) {
		t.Fatalf("prune: %+v %v", res, err)
	}
	for _, n := range []string{kernel, agent, served, "notes.txt", orphan + ".partial", strings.ToUpper(h('e'))} {
		if !exists(n) {
			t.Fatalf("%s was removed", n)
		}
	}
	// Without a manifest list nothing can be proven unreferenced.
	if _, err := (&BlobStore{Dir: b.Dir}).prune(ctx, nil, false, later); err == nil {
		t.Fatal("prune without manifests must refuse")
	}
}

// TestPruneImagesOwnerOnly: only the host owner may prune image blobs.
func TestPruneImagesOwnerOnly(t *testing.T) {
	e := newEnv(t, fake.Options{}, func(c *Config) { c.HostOwner = owner })
	if _, err := e.svc.PruneImages(context.Background(), "someone-else", api.PruneImagesRequest{}); !domain.IsCode(err, domain.CodePermissionDenied) {
		t.Fatalf("prune by a non-owner: %v", err)
	}
	if res, err := e.svc.PruneImages(context.Background(), owner, api.PruneImagesRequest{DryRun: true}); err != nil || !res.DryRun {
		t.Fatalf("owner dry run: %+v %v", res, err)
	}
}

// TestFileReserve: the reserve file takes its size when armed (a no-op when
// it is), and releasing it removes it.
func TestFileReserve(t *testing.T) {
	r := &FileReserve{Path: filepath.Join(t.TempDir(), "save-reserve"), Bytes: 3<<20 + 5}
	if r.Armed() || r.Release() {
		t.Fatal("a new reserve is not armed")
	}
	for range 2 {
		if err := r.Arm(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(r.Path); err != nil || fi.Size() != r.Bytes || !r.Armed() {
		t.Fatalf("armed reserve: %v %v", fi, err)
	}
	if !r.Release() || r.Armed() {
		t.Fatal("release")
	}
	if _, err := os.Stat(r.Path + ".tmp"); err == nil {
		t.Fatal("a temporary file was left behind")
	}
}

// TestAutoDeleteForQueuedRequest: above the floor, a queued request's
// cleanup deletes only the requester's unused boxes, never another
// owner's, and never the image whose generation a queued create boots.
func TestAutoDeleteForQueuedRequest(t *testing.T) {
	d := &fakeDisk{free: 50 * domain.GiB}
	e := newDiskEnv(t, d, fake.Options{}, nil)
	gib := domain.GiB
	no := false
	// Another owner's box, stopped.
	other := Request{Principal: "local:other", Method: "POST", Path: "/test"}
	theirState := func() domain.VMState {
		var st domain.VMState
		e.db.Tx(e.ctx, func(tx *state.Tx) error {
			r, err := tx.FindVMByName("local:other", "q--theirs")
			if err == nil {
				st = r.VM.State
			}
			return err
		})
		return st
	}
	settle := func(want domain.VMState) {
		for deadline := time.Now().Add(10 * time.Second); theirState() != want; time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("their box is %s, want %s", theirState(), want)
			}
		}
	}
	other.IdempotencyKey = e.newKey()
	if _, err := e.svc.Create(e.ctx, other, api.CreateVMRequest{Name: "q--theirs", AutoStop: &no, Labels: instance("q", "theirs")}); err != nil {
		t.Fatal(err)
	}
	settle(domain.StateReady)
	other.IdempotencyKey = ""
	if _, err := e.svc.Stop(e.ctx, other, "q--theirs", false); err != nil {
		t.Fatal(err)
	}
	settle(domain.StateStopped)
	img := e.box("img--image", builder("img"), false, true, 5*gib)
	gen, err := e.svc.Snapshot(e.ctx, e.req(e.newKey()), "img--image", api.SnapshotVMRequest{Name: "img-image-g1"})
	if err != nil {
		t.Fatal(err)
	}
	e.box("p--mine", instance("p", "mine"), false, true, 3*gib)
	var theirs domain.VMID
	if err := e.db.Tx(e.ctx, func(tx *state.Tx) error {
		r, err := tx.FindVMByName("local:other", "q--theirs")
		if err == nil {
			theirs = r.VM.ID
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	d.setCost(theirs, 5*gib)
	e.advance(10 * 24 * time.Hour)

	// 6 GiB free is above the 5 GiB floor; the queued create needs 7, plus
	// the 2 GiB margin: 9. It boots img's generation 1.
	d.setFree(6 * gib)
	req := e.req(e.newKey())
	req.CapacityWait = 20 * time.Second
	done := make(chan error, 1)
	go func() {
		a, err := e.svc.Create(e.ctx, req, api.CreateVMRequest{Name: "p--new", SnapshotID: gen.Operation.Result.SnapshotID})
		if err == nil {
			e.wait(a.Operation.ID)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("queued create: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("queued create was not admitted")
	}
	if !e.gone("p--mine") {
		t.Fatal("the requester's unused box was not deleted")
	}
	if theirState() != domain.StateStopped || e.gone(string(img.ID)) {
		t.Fatalf("deleted another owner's box (%s) or the image the create boots (%v)", theirState(), e.gone(string(img.ID)))
	}
}

// retiredCatalog serves one retired image besides the static ones.
type retiredCatalog struct {
	*StaticCatalog
	retired domain.Image
}

func (c retiredCatalog) RetiredImages() []domain.Image { return []domain.Image{c.retired} }

// TestPruneKeepsRetiredImages: the blobs of an image an upgrade retired
// stay, even with no manifest naming them: boxes made before the upgrade
// still boot its kernel.
func TestPruneKeepsRetiredImages(t *testing.T) {
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs", "sha256")
	os.MkdirAll(blobs, 0o755)
	kernel := filepath.Join(blobs, strings.Repeat("f", 64))
	os.WriteFile(kernel, []byte("k"), 0o644)
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.Images = retiredCatalog{StaticCatalog: c.Images.(*StaticCatalog), retired: domain.Image{ID: "img_old", Kernel: kernel}}
		c.ImageBlobs = &BlobStore{Dir: filepath.Join(dir, "blobs"), MinAge: time.Nanosecond, Manifests: func() ([]string, error) { return nil, nil }}
	})
	time.Sleep(time.Millisecond)
	res, err := e.svc.pruneBlobs(context.Background(), false)
	if err != nil || len(res.Removed) != 0 {
		t.Fatalf("prune: %+v %v", res, err)
	}
	if _, err := os.Stat(kernel); err != nil {
		t.Fatal("a retired image's kernel was pruned")
	}
}

// TestAutoDeleteUnderPressure: below the floor with nothing old enough,
// the cleanup waives the instance age down to PressureDeleteAge, least
// recently used first, and says so in the reason; never a pinned, busy or
// running box, nor one used within PressureDeleteAge. A request's cleanup
// above the floor (its own need) never escalates.
func TestAutoDeleteUnderPressure(t *testing.T) {
	gib := domain.GiB
	d := &fakeDisk{free: 100 * gib, total: 100 * gib}
	e := newDiskEnv(t, d, fake.Options{}, nil)
	e.box("p--b", instance("p", "b"), false, true, 5*gib)
	e.box("p--pinned", instance("p", "pinned"), true, true, 5*gib)
	busy := e.box("p--busy", instance("p", "busy"), false, true, 5*gib)
	e.busy.Store(busy.ID, true)
	e.advance(time.Hour)
	e.box("p--a", instance("p", "a"), false, true, gib)
	e.box("p--c", instance("p", "c"), false, true, gib)
	e.box("p--running", instance("p", "running"), false, false, 5*gib)
	e.advance(2 * time.Hour)
	e.box("p--recent", instance("p", "recent"), false, true, 5*gib)
	e.advance(30 * time.Minute)

	// Above the floor (10 GiB) for a request needing 15: no box is old
	// enough at the usual age and it does not escalate.
	d.setFree(11 * gib)
	e.svc.freeDisk(e.ctx, 15*gib, owner)
	if n := len(e.autoDeletions()); n != 0 {
		t.Fatalf("a request's cleanup above the floor deleted %d boxes", n)
	}

	// Below: 3 GiB free, target 12: p--b (oldest, 5), then p--a and p--c
	// (1 each) leave it at 10, unmet; p--recent would be next but for
	// PressureDeleteAge.
	d.setFree(3 * gib)
	e.svc.freeDisk(e.ctx, 0, "")
	got := e.autoDeletions()
	if len(got) != 3 || got[0].Box != "p/b" || d.getFree() != 10*gib {
		t.Fatalf("pressure deletions (LRU): %+v, free %s", got, domain.HumanBytes(d.getFree()))
	}
	for _, g := range got {
		if !strings.Contains(g.Reason, "under pressure") || !strings.Contains(g.Reason, "the 72h age limit waived") {
			t.Fatalf("reason: %q", g.Reason)
		}
	}
	for _, n := range []string{"p--pinned", "p--busy", "p--running", "p--recent"} {
		if e.gone(n) {
			t.Fatalf("%s was auto-deleted under pressure", n)
		}
	}
}

// lockedBuf is a log sink tests read while the service writes.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// TestCleanupStuck: a cleanup that frees nothing is not repeated until
// the journal changes or stuckEvery passes, and logs what blocks it (at
// warn, at most every stuckEvery), never a per-minute no-op.
func TestCleanupStuck(t *testing.T) {
	gib := domain.GiB
	logs := &lockedBuf{}
	d := &fakeDisk{free: 100 * gib, total: 100 * gib}
	e := newDiskEnv(t, d, fake.Options{}, func(c *Config) {
		c.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	e.box("p--pinned", instance("p", "pinned"), true, true, 5*gib)
	e.box("p--running", instance("p", "running"), false, false, 5*gib)
	e.advance(4 * 24 * time.Hour)
	d.setFree(3 * gib)
	const stuck, started = "cleanup freed nothing", "host disk low: cleaning up"

	e.svc.freeDisk(e.ctx, 0, "")
	if logs.count(stuck) != 1 || logs.count("1 pinned (p--pinned)") != 1 || logs.count("1 running (p--running)") != 1 {
		t.Fatalf("stuck log:\n%s", logs.b.String())
	}
	e.advance(2 * time.Minute)
	e.svc.freeDisk(e.ctx, 0, "")
	if logs.count(started) != 1 {
		t.Fatal("a stuck cleanup ran again within stuckEvery with nothing changed")
	}
	// A journal change wakes it, but the log stays quiet.
	if _, err := e.svc.Update(e.ctx, e.req(""), "p--running", api.UpdateVMRequest{Keep: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	e.svc.freeDisk(e.ctx, 0, "")
	if logs.count(stuck) != 1 || logs.count(started) != 1 {
		t.Fatalf("repeated stuck log within stuckEvery:\n%s", logs.b.String())
	}
	e.advance(stuckEvery)
	e.svc.freeDisk(e.ctx, 0, "")
	if logs.count(stuck) != 2 {
		t.Fatalf("no stuck log after stuckEvery:\n%s", logs.b.String())
	}
}
