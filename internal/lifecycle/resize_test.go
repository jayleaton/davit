package lifecycle

import (
	"context"
	"os"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
)

// stopped creates VM name and stops it.
func (e *env) stopped(name string) *domain.VM {
	e.t.Helper()
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: name}))
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), name, false))
	return e.vm(name)
}

// booted reports the shape the runtime instance of vm's current generation
// was booted with.
func (e *env) booted(vm *domain.VM) domain.Shape {
	e.t.Helper()
	sh, ok := e.rt.InstanceShape(runtime.Key{VMID: vm.ID, Generation: vm.InstanceGeneration})
	if !ok {
		e.t.Fatalf("no runtime instance for %s gen %d", vm.ID, vm.InstanceGeneration)
	}
	return sh
}

// diskSize is the size of the working disk vm's runtime holds open.
func (e *env) diskSize(vm *domain.VM) int64 {
	e.t.Helper()
	e.svc.mu.Lock()
	l, ok := e.svc.leases[vm.ID]
	e.svc.mu.Unlock()
	if !ok {
		e.t.Fatalf("no disk lease for %s", vm.ID)
	}
	fi, err := os.Stat(l.Path)
	if err != nil {
		e.t.Fatal(err)
	}
	return fi.Size()
}

func resizeTo(sh api.ShapeOverride) api.ResumeVMRequest { return api.ResumeVMRequest{Shape: &sh} }

func TestResizeCPUsAndMemoryAtResume(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	before := e.stopped("r")
	a, err := e.svc.Resume(ctx, e.req(""), "r", resizeTo(api.ShapeOverride{CPUs: 4, MemoryBytes: 3 * domain.GiB}))
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Shape{CPUs: 4, MemoryBytes: 3 * domain.GiB, DiskBytes: before.Shape.DiskBytes}
	if a.VM.Shape != want || a.Operation.Params.Shape != want {
		t.Fatalf("accepted shape %+v, journaled %+v, want %+v", a.VM.Shape, a.Operation.Params.Shape, want)
	}
	e.mustSucceed(a, nil)
	vm := e.vm("r")
	if vm.Shape != want || e.booted(vm) != want {
		t.Fatalf("after resize: vm %+v, runtime %+v, want %+v", vm.Shape, e.booted(vm), want)
	}
	// The shape sticks for later boots.
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "r", false))
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "r", api.ResumeVMRequest{}))
	if vm = e.vm("r"); vm.Shape != want || e.booted(vm) != want {
		t.Fatalf("plain resume after resize: %+v", vm.Shape)
	}
	// Out of the runtime's range: refused like create.
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "r", false))
	if _, err := e.svc.Resume(ctx, e.req(""), "r", resizeTo(api.ShapeOverride{CPUs: 999})); !domain.IsCode(err, domain.CodeUnsupportedCapability) {
		t.Fatalf("cpus beyond the runtime: %v", err)
	}
	if _, err := e.svc.Resume(ctx, e.req(""), "r", resizeTo(api.ShapeOverride{CPUs: -1})); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("negative cpus: %v", err)
	}
	if vm = e.vm("r"); vm.State != domain.StateStopped || vm.Shape != want {
		t.Fatalf("a refused resize changed the VM: %s %+v", vm.State, vm.Shape)
	}
}

func TestResizeDiskGrowsOnly(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	before := e.stopped("d")
	cur := before.Shape.DiskBytes

	_, err := e.svc.Resume(ctx, e.req(""), "d", resizeTo(api.ShapeOverride{DiskBytes: cur - domain.GiB}))
	de := domain.AsError(err)
	if de.Code != domain.CodeInvalidArgument || de.Details["field"] != "shape.diskBytes" ||
		de.Details["reason"] != "disk_shrink" || de.Details["currentBytes"] != cur {
		t.Fatalf("shrink: %v %+v", err, de.Details)
	}

	// Same size is not a resize.
	a, err := e.svc.Resume(ctx, e.req(""), "d", resizeTo(api.ShapeOverride{DiskBytes: cur}))
	if err != nil {
		t.Fatal(err)
	}
	if a.Operation.Params.Shape != (domain.Shape{}) {
		t.Fatalf("same size journaled a resize: %+v", a.Operation.Params.Shape)
	}
	e.mustSucceed(a, nil)
	if vm := e.vm("d"); vm.Shape != before.Shape || e.diskSize(vm) != cur {
		t.Fatalf("same size: %+v, disk %d", vm.Shape, e.diskSize(vm))
	}

	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "d", false))
	grown := cur + 4*domain.GiB
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "d", resizeTo(api.ShapeOverride{DiskBytes: grown})))
	vm := e.vm("d")
	if vm.Shape.DiskBytes != grown || e.booted(vm).DiskBytes != grown {
		t.Fatalf("grown shape %+v", vm.Shape)
	}
	if got := e.diskSize(vm); got != grown {
		t.Fatalf("resumed disk clone is %d bytes, want %d", got, grown)
	}
	// The next stop commits the grown disk.
	stop := e.mustSucceed(e.svc.Stop(ctx, e.req(""), "d", false))
	snap, err := e.svc.GetSnapshot(ctx, owner, string(stop.Result.SnapshotID))
	if err != nil || snap.SizeBytes != grown {
		t.Fatalf("snapshot of the grown disk: %+v %v", snap, err)
	}
}

func TestResizeAdmission(t *testing.T) {
	limits := domain.Limits{CPUs: 4, DiskBytes: 10 * domain.GiB}
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Limits = limits })
	ctx := context.Background()
	e.stopped("a") // 2 CPUs, 8 GiB disk

	if _, err := e.svc.Resume(ctx, e.req(""), "a", resizeTo(api.ShapeOverride{CPUs: 6})); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("cpus above the host limit: %v", err)
	}
	// Only the growth is admitted: 8 GiB held + 4 GiB more exceeds 10 GiB...
	if _, err := e.svc.Resume(ctx, e.req(""), "a", resizeTo(api.ShapeOverride{DiskBytes: 12 * domain.GiB})); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("disk growth above the host limit: %v", err)
	}
	if vm := e.vm("a"); vm.State != domain.StateStopped || vm.Shape != domain.DefaultShape {
		t.Fatalf("refused resize changed the VM: %s %+v", vm.State, vm.Shape)
	}
	// ...while 2 GiB more fits exactly, with all 4 CPUs.
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "a", resizeTo(api.ShapeOverride{CPUs: 4, DiskBytes: 10 * domain.GiB})))
	if vm := e.vm("a"); vm.Shape.CPUs != 4 || vm.Shape.DiskBytes != 10*domain.GiB {
		t.Fatalf("admitted resize: %+v", vm.Shape)
	}
}

func TestResizeRunningVMNeedsStop(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "live"}))
	_, err := e.svc.Resume(ctx, e.req(""), "live", resizeTo(api.ShapeOverride{CPUs: 4}))
	if !domain.IsCode(err, domain.CodeStopRequired) {
		t.Fatalf("resize of a ready VM: %v", err)
	}
	// Asking for the shape it already has is the usual no-op.
	a, err := e.svc.Resume(ctx, e.req(""), "live", resizeTo(api.ShapeOverride{CPUs: domain.DefaultShape.CPUs}))
	if err != nil || !a.Operation.Result.NoOp {
		t.Fatalf("same-shape resume of a ready VM: %+v %v", a, err)
	}
	if vm := e.vm("live"); vm.Shape != domain.DefaultShape || vm.InstanceGeneration != 1 {
		t.Fatalf("running VM changed: %+v", vm)
	}
}

func TestResizeByProfile(t *testing.T) {
	profiles := []domain.Profile{
		{Name: "big", Shape: domain.Shape{CPUs: 4, MemoryBytes: 4 * domain.GiB}},
		{Name: "tiny", Shape: domain.Shape{CPUs: 1, MemoryBytes: 512 * domain.MiB, DiskBytes: domain.GiB}},
	}
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Profiles = profiles })
	ctx := context.Background()
	before := e.stopped("p")

	if _, err := e.svc.Resume(ctx, e.req(""), "p", api.ResumeVMRequest{Type: "huge"}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("unknown profile: %v", err)
	}
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "p", api.ResumeVMRequest{Type: "big"}))
	want := domain.Shape{CPUs: 4, MemoryBytes: 4 * domain.GiB, DiskBytes: before.Shape.DiskBytes}
	if vm := e.vm("p"); vm.Shape != want || e.booted(vm) != want {
		t.Fatalf("type big: %+v", vm.Shape)
	}
	// Shape fields override the profile; a profile's smaller disk keeps the
	// current one rather than shrinking it.
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "p", false))
	tiny := api.ShapeOverride{CPUs: 2}
	e.mustSucceed(e.svc.Resume(ctx, e.req(""), "p", api.ResumeVMRequest{Type: "tiny", Shape: &tiny}))
	want = domain.Shape{CPUs: 2, MemoryBytes: 512 * domain.MiB, DiskBytes: before.Shape.DiskBytes}
	if vm := e.vm("p"); vm.Shape != want || e.booted(vm) != want {
		t.Fatalf("type tiny + cpus 2: %+v", vm.Shape)
	}
}

func TestResizeReplay(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.stopped("x")
	body := resizeTo(api.ShapeOverride{CPUs: 3, DiskBytes: 9 * domain.GiB})
	want := domain.Shape{CPUs: 3, MemoryBytes: domain.DefaultShape.MemoryBytes, DiskBytes: 9 * domain.GiB}

	// The daemon dies while the resized VM boots: the replay boots the
	// journaled shape on the disk it cloned.
	e.stop()
	e.opts.BootDelay = 400 * time.Millisecond
	e.start()
	a, err := e.svc.Resume(ctx, e.req("k-resize"), "x", body)
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, a.Operation.ID, "runtime.boot")
	e.stop()
	e.opts.BootDelay = 0
	e.start()
	op := e.wait(a.Operation.ID)
	if op.State != domain.OpSucceeded || op.Params.Shape != want {
		t.Fatalf("replayed resize %s %+v: %+v", op.State, op.Params.Shape, op.Error)
	}
	vm := e.vm("x")
	if vm.Shape != want || e.booted(vm) != want || e.diskSize(vm) != want.DiskBytes || vm.InstanceGeneration != 2 {
		t.Fatalf("after replay: %+v gen %d", vm.Shape, vm.InstanceGeneration)
	}

	// The same key returns the same operation; a different body under it
	// is a conflict.
	again, err := e.svc.Resume(ctx, e.req("k-resize"), "x", body)
	if err != nil || !again.Replayed || again.Operation.ID != a.Operation.ID {
		t.Fatalf("idempotent replay: %+v %v", again, err)
	}
	if _, err := e.svc.Resume(ctx, e.req("k-resize"), "x", resizeTo(api.ShapeOverride{CPUs: 5})); !domain.IsCode(err, domain.CodeIdempotencyConflict) {
		t.Fatalf("same key, other shape: %v", err)
	}
	if vm := e.vm("x"); vm.InstanceGeneration != 2 || vm.Shape != want {
		t.Fatalf("replay changed the VM: %+v", vm)
	}
}
