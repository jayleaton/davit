// Package runtimetest is the reusable runtime adapter conformance suite.
// Every adapter (fake, Firecracker, Cloud Hypervisor, Mac) runs it:
//
//	func TestConformance(t *testing.T) {
//		runtimetest.Run(t, runtimetest.Harness{New: ..., Reopen: ..., Disk: ...})
//	}
//
// Real adapters run it in the integration tier on a host that can boot VMs.
package runtimetest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"davit/internal/domain"
	"davit/internal/runtime"
)

// Harness adapts one engine to the suite.
type Harness struct {
	// New returns a fresh adapter whose state lives under dir.
	New func(t *testing.T, dir string) runtime.Runtime
	// Reopen returns a second adapter over the same dir, modelling a daemon
	// restart. Nil skips restart tests (only allowed for adapters whose
	// instances cannot outlive the daemon, which must then say so).
	Reopen func(t *testing.T, dir string) runtime.Runtime
	// Image is the bootable image to use; zero value for fakes.
	Image domain.Image
	// Disk returns a bootable private root disk path for key.
	Disk func(t *testing.T, key runtime.Key) string
	// CloneDisk returns a private copy of disk src (a checkpoint's matching
	// disk generation). Required when the adapter reports ramSnapshot.
	CloneDisk func(t *testing.T, src string) string
	// Shape to boot; zero uses a small default.
	Shape domain.Shape
	// Timeout bounds each lifecycle call; zero means 2 minutes.
	Timeout time.Duration
}

func (h Harness) ctx(t *testing.T) context.Context {
	d := h.Timeout
	if d == 0 {
		d = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func (h Harness) instance(vm string, gen int64) runtime.Instance {
	s := h.Shape
	if s == (domain.Shape{}) {
		s = domain.Shape{CPUs: 1, MemoryBytes: 256 * domain.MiB, DiskBytes: 64 * domain.MiB}
	}
	return runtime.Instance{Key: runtime.Key{VMID: domain.VMID(vm), Generation: gen}, Shape: s, Image: h.Image}
}

func (h Harness) boot(t *testing.T, rt runtime.Runtime, vm string, gen int64) runtime.Handle {
	t.Helper()
	inst := h.instance(vm, gen)
	hd, err := rt.ColdBoot(h.ctx(t), inst, runtime.DiskSet{Root: h.Disk(t, inst.Key)}, runtime.NetworkHandle{})
	if err != nil {
		t.Fatalf("ColdBoot %v: %v", inst.Key, err)
	}
	t.Cleanup(func() { rt.Destroy(context.Background(), hd) })
	return hd
}

func phase(t *testing.T, rt runtime.Runtime, h runtime.Handle) runtime.Phase {
	t.Helper()
	st, err := rt.Inspect(context.Background(), h)
	if err != nil {
		t.Fatalf("Inspect %v: %v", h.Key, err)
	}
	return st.Phase
}

// TempDisk is a Harness.Disk helper for adapters that do not read the disk.
func TempDisk(t *testing.T, key runtime.Key) string {
	p := filepath.Join(t.TempDir(), "root.img")
	if err := os.WriteFile(p, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// CopyDisk is a Harness.CloneDisk helper that copies the file.
func CopyDisk(t *testing.T, src string) string {
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "clone.img")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Run executes the conformance suite.
func Run(t *testing.T, h Harness) {
	t.Run("Capabilities", func(t *testing.T) {
		rt := h.New(t, t.TempDir())
		c := rt.Capabilities()
		if c.Engine == "" || len(c.Arch) == 0 || c.GuestTransport == "" || c.Shape.MaxCPUs < 1 {
			t.Fatalf("incomplete capabilities: %+v", c)
		}
		if !c.ColdBoot {
			t.Fatal("cold boot is the mandatory fallback; every adapter must support it")
		}
		if !slices.Contains(c.BootModes(), domain.BootModeColdBoot) {
			t.Fatal("BootModes must include cold_boot")
		}
	})

	t.Run("BootInspectStop", func(t *testing.T) {
		rt := h.New(t, t.TempDir())
		hd := h.boot(t, rt, "vm-a", 1)
		if hd.Generation != 1 || hd.VMID != "vm-a" || hd.ProcessIdentity == "" || hd.BootMode != domain.BootModeColdBoot {
			t.Fatalf("handle %+v", hd)
		}
		if p := phase(t, rt, hd); p != runtime.PhaseRunning {
			t.Fatalf("after ColdBoot returned: %s, want running", p)
		}
		if err := rt.Stop(h.ctx(t), hd, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		if p := phase(t, rt, hd); p != runtime.PhaseExited && p != runtime.PhaseGone {
			t.Fatalf("after Stop: %s", p)
		}
		if err := rt.Stop(h.ctx(t), hd, time.Second); err != nil {
			t.Fatalf("Stop must be idempotent: %v", err)
		}
		if err := rt.Kill(h.ctx(t), hd); err != nil {
			t.Fatalf("Kill after exit must be idempotent: %v", err)
		}
	})

	t.Run("ColdBootIsIdempotentPerKey", func(t *testing.T) {
		rt := h.New(t, t.TempDir())
		a := h.boot(t, rt, "vm-a", 1)
		b := h.boot(t, rt, "vm-a", 1)
		if a.ProcessIdentity != b.ProcessIdentity {
			t.Fatalf("second ColdBoot of a live key spawned a new VMM: %s vs %s", a.ProcessIdentity, b.ProcessIdentity)
		}
	})

	t.Run("KillAndDestroy", func(t *testing.T) {
		rt := h.New(t, t.TempDir())
		hd := h.boot(t, rt, "vm-a", 1)
		if err := rt.Kill(h.ctx(t), hd); err != nil {
			t.Fatal(err)
		}
		if p := phase(t, rt, hd); p != runtime.PhaseExited && p != runtime.PhaseGone {
			t.Fatalf("after Kill: %s", p)
		}
		for range 2 {
			if err := rt.Destroy(h.ctx(t), hd); err != nil {
				t.Fatalf("Destroy must be idempotent: %v", err)
			}
		}
		if p := phase(t, rt, hd); p != runtime.PhaseGone {
			t.Fatalf("after Destroy: %s, want gone", p)
		}
		live := h.boot(t, rt, "vm-b", 1)
		if err := rt.Destroy(h.ctx(t), live); err != nil {
			t.Fatalf("Destroy of a live instance: %v", err)
		}
		if p := phase(t, rt, live); p != runtime.PhaseGone {
			t.Fatalf("Destroy left live instance in %s", p)
		}
	})

	t.Run("StaleHandleCannotTouchReplacement", func(t *testing.T) {
		rt := h.New(t, t.TempDir())
		old := h.boot(t, rt, "vm-a", 1)
		if err := rt.Kill(h.ctx(t), old); err != nil {
			t.Fatal(err)
		}
		// Same key booted again after exit: a new process must get a new
		// identity and the old handle must not be able to stop it.
		repl := h.boot(t, rt, "vm-a", 1)
		if repl.ProcessIdentity == old.ProcessIdentity {
			t.Fatal("replacement reused the old process identity")
		}
		for name, call := range map[string]func() error{
			"Stop":    func() error { return rt.Stop(h.ctx(t), old, time.Second) },
			"Kill":    func() error { return rt.Kill(h.ctx(t), old) },
			"Destroy": func() error { return rt.Destroy(h.ctx(t), old) },
		} {
			if err := call(); !errors.Is(err, runtime.ErrStaleHandle) {
				t.Errorf("%s with stale handle: %v, want ErrStaleHandle", name, err)
			}
		}
		if p := phase(t, rt, repl); p != runtime.PhaseRunning {
			t.Fatalf("replacement disturbed by stale handle: %s", p)
		}
		// A different generation is a different instance entirely.
		gen2 := h.boot(t, rt, "vm-a", 2)
		if p := phase(t, rt, gen2); p != runtime.PhaseRunning || gen2.Generation != 2 {
			t.Fatalf("gen2 %+v %s", gen2, p)
		}
	})

	t.Run("Discover", func(t *testing.T) {
		rt := h.New(t, t.TempDir())
		a := h.boot(t, rt, "vm-a", 1)
		b := h.boot(t, rt, "vm-b", 3)
		hs, err := rt.Discover(h.ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		var keys []runtime.Key
		for _, x := range hs {
			keys = append(keys, x.Key)
		}
		if !slices.Contains(keys, a.Key) || !slices.Contains(keys, b.Key) {
			t.Fatalf("Discover %v missing %v/%v", keys, a.Key, b.Key)
		}
	})

	t.Run("SurvivesDaemonRestart", func(t *testing.T) {
		if h.Reopen == nil {
			t.Skip("adapter instances do not outlive the daemon")
		}
		dir := t.TempDir()
		rt := h.New(t, dir)
		hd := h.boot(t, rt, "vm-a", 1)
		rt2 := h.Reopen(t, dir)
		hs, err := rt2.Discover(h.ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(hs, func(x runtime.Handle) bool { return x.Key == hd.Key && x.ProcessIdentity == hd.ProcessIdentity }) {
			t.Fatalf("restarted adapter did not rediscover %+v in %+v", hd, hs)
		}
		if p := phase(t, rt2, hd); p != runtime.PhaseRunning {
			t.Fatalf("rediscovered instance %s", p)
		}
		again := h.boot(t, rt2, "vm-a", 1)
		if again.ProcessIdentity != hd.ProcessIdentity {
			t.Fatal("replayed ColdBoot after restart spawned a second VMM")
		}
		if err := rt2.Stop(h.ctx(t), hd, 30*time.Second); err != nil {
			t.Fatalf("restarted adapter cannot stop rediscovered instance: %v", err)
		}
	})

	t.Run("CheckpointRestore", func(t *testing.T) {
		rt := h.New(t, t.TempDir())
		if !rt.Capabilities().RAMSnapshot {
			t.Skip("no RAM checkpoints")
		}
		if h.CloneDisk == nil {
			t.Fatal("ramSnapshot adapters need Harness.CloneDisk")
		}
		inst := h.instance("vm-src", 1)
		disk := h.Disk(t, inst.Key)
		src, err := rt.ColdBoot(h.ctx(t), inst, runtime.DiskSet{Root: disk}, runtime.NetworkHandle{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rt.Destroy(context.Background(), src) })
		if _, err := rt.Checkpoint(h.ctx(t), src, runtime.Quiescence{}); err == nil {
			t.Fatal("Checkpoint of a running instance must fail: RAM and disk could diverge")
		}
		if err := rt.Pause(h.ctx(t), src); err != nil {
			t.Fatal(err)
		}
		cp, err := rt.Checkpoint(h.ctx(t), src, runtime.Quiescence{})
		if err != nil {
			t.Fatal(err)
		}
		if p := phase(t, rt, src); p != runtime.PhasePaused {
			t.Fatalf("Checkpoint must leave the source paused, got %s", p)
		}
		clone := h.CloneDisk(t, disk) // the matching disk generation, taken while paused
		if err := rt.Unpause(h.ctx(t), src); err != nil {
			t.Fatal(err)
		}
		child := h.instance("vm-child", 1)
		ch, err := rt.RestoreCheckpoint(h.ctx(t), child, cp, runtime.DiskSet{Root: clone}, runtime.NetworkHandle{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rt.Destroy(context.Background(), ch) })
		if ch.BootMode != domain.BootModeRAMRestore || ch.Key != child.Key {
			t.Fatalf("restored handle %+v", ch)
		}
		for _, x := range []runtime.Handle{src, ch} {
			if p := phase(t, rt, x); p != runtime.PhaseRunning {
				t.Fatalf("%v after checkpoint/restore: %s", x.Key, p)
			}
		}
		for range 2 {
			if err := rt.DiscardCheckpoint(h.ctx(t), cp); err != nil {
				t.Fatalf("DiscardCheckpoint must be idempotent: %v", err)
			}
		}
		if p := phase(t, rt, ch); p != runtime.PhaseRunning {
			t.Fatalf("discarding the checkpoint disturbed its restore: %s", p)
		}
	})

	t.Run("UnsupportedIsExplicit", func(t *testing.T) {
		rt := h.New(t, t.TempDir())
		c := rt.Capabilities()
		inst := h.instance("vm-x", 1)
		disks := runtime.DiskSet{Root: h.Disk(t, inst.Key)}
		if !c.GoldenRestore {
			if _, err := rt.RestoreGolden(h.ctx(t), inst, runtime.Golden{}, disks, runtime.NetworkHandle{}); !domain.IsCode(err, domain.CodeUnsupportedCapability) {
				t.Errorf("RestoreGolden without capability: %v", err)
			}
		}
		if !c.RAMSnapshot {
			if _, err := rt.RestoreCheckpoint(h.ctx(t), inst, runtime.Checkpoint{}, disks, runtime.NetworkHandle{}); !domain.IsCode(err, domain.CodeUnsupportedCapability) {
				t.Errorf("RestoreCheckpoint without capability: %v", err)
			}
		}
		if c.GuestTransport == "none" {
			hd := h.boot(t, rt, "vm-y", 1)
			if _, err := rt.GuestTransport(h.ctx(t), hd); !domain.IsCode(err, domain.CodeUnsupportedCapability) {
				t.Errorf("GuestTransport with transport none: %v", err)
			}
		}
	})
}
