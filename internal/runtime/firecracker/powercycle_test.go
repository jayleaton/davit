//go:build linux

package firecracker_test

import (
	"bufio"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/runtime/firecracker"
	"davit/internal/runtime/firecracker/fctest"
)

// guestDisk sends one disk command to the fake guest of h over its vsock
// and returns the answer line.
func guestDisk(t *testing.T, ctx context.Context, rt runtime.Runtime, h runtime.Handle, cmd string) string {
	t.Helper()
	d, err := rt.GuestTransport(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.DialGuest(ctx, firecracker.AgentPort)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(cmd + "\n")); err != nil {
		t.Fatal(err)
	}
	l, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return strings.TrimSuffix(l, "\n")
}

// TestPowerCycleKeepsDisk is the adapter side of recover (G9), the way
// lifecycle drives it: Destroy kills the running VMM of one generation (no
// shutdown, no RAM or device checkpoint), and ColdBoot of the next
// generation on the same disk path boots what the guest wrote, unsynced
// and uncommitted.
func TestPowerCycleKeepsDisk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	rt := fctest.Record(fakeRuntime(t, dir, fctest.FakeBinary(t)))
	fctest.Guard(t, dir, rt)

	root := filepath.Join(t.TempDir(), "root.img")
	if err := os.WriteFile(root, []byte("committed-image-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	boot := func(gen int64) runtime.Handle {
		t.Helper()
		inst := runtime.Instance{
			Key:   runtime.Key{VMID: "vm-wedged", Generation: gen},
			Shape: domain.Shape{CPUs: 1, MemoryBytes: 128 * domain.MiB, DiskBytes: 64 * domain.MiB},
			// The guest ignores both shutdown requests: it is wedged.
			Image: domain.Image{ID: "fake", Digest: "sha256:fake", Arch: domain.ArchAMD64, Kernel: "/fake/vmlinux",
				BootArgs: "fake.cad=ignore fake.poweroff=ignore"},
		}
		h, err := rt.ColdBoot(ctx, inst, runtime.DiskSet{Root: root}, runtime.NetworkHandle{})
		if err != nil {
			t.Fatal(err)
		}
		if h.BootMode != domain.BootModeColdBoot {
			t.Fatalf("generation %d boot mode %q", gen, h.BootMode)
		}
		return h
	}

	old := boot(1)
	if got := guestDisk(t, ctx, rt, old, "disk-write uncommitted-work"); got != "ok" {
		t.Fatalf("guest write: %q", got)
	}
	pid := pidOf(t, old)
	if err := rt.Destroy(ctx, old); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if running(pid) {
		t.Fatalf("VMM %d survived the power cycle", pid)
	}
	if st, err := rt.Inspect(ctx, old); err != nil || st.Phase != runtime.PhaseGone {
		t.Fatalf("old generation after destroy: %s %v", st.Phase, err)
	}
	// Nothing was committed: no RAM or device state anywhere in the runtime.
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (d.Name() == "vmstate" || d.Name() == "mem") {
			t.Errorf("power cycle wrote %s", p)
		}
		return nil
	})
	if ents, err := os.ReadDir(filepath.Join(dir, "checkpoints")); err == nil && len(ents) > 0 {
		t.Errorf("power cycle left checkpoints: %v", ents)
	}

	cur := boot(2)
	t.Cleanup(func() { rt.Destroy(context.Background(), cur) })
	if pidOf(t, cur) == pid {
		t.Fatal("generation 2 is the old VMM")
	}
	if got := guestDisk(t, ctx, rt, cur, "disk-read 16"); got != "uncommitted-work" {
		t.Fatalf("generation 2 reads %q from its disk, want the write of generation 1", got)
	}
	if hs, err := rt.Discover(ctx); err != nil || len(hs) != 1 || hs[0].Generation != 2 {
		t.Fatalf("instances after the power cycle: %+v %v", hs, err)
	}
	if err := rt.Destroy(ctx, cur); err != nil {
		t.Fatal(err)
	}
}
