//go:build linux

package firecracker_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
	"davit/internal/lifecycle"
	"davit/internal/network"
	"davit/internal/network/nethelper"
	"davit/internal/runtime"
	"davit/internal/runtime/firecracker"
	"davit/internal/runtime/firecracker/fctest"
	"davit/internal/runtime/runtimetest"
)

func TestMain(m *testing.M) {
	fctest.FakeVMM() // supervise_test.go runs this binary as the VMM
	// Rootless networks re-exec this binary as their egress helper.
	nethelper.Serve()
	os.Exit(m.Run())
}

// shape keeps every test well under the 4 GiB guest RAM budget: at most
// three VMs of 512 MiB run at once.
func shape(img domain.Image) domain.Shape {
	return domain.Shape{CPUs: 1, MemoryBytes: 512 * domain.MiB, DiskBytes: img.RootfsSize}
}

func open(t *testing.T, fx fctest.Fixture, dir string) *firecracker.Runtime {
	t.Helper()
	rt, err := firecracker.New(firecracker.Options{Dir: dir, Binary: fx.Binary.Path, GuestShutdown: fctest.Shutdown})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// disk returns a private reflink of the test rootfs on the scratch fs.
func disk(t *testing.T, fx fctest.Fixture) string {
	t.Helper()
	dir, err := os.MkdirTemp(fx.Scratch, "disk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	p := filepath.Join(dir, "root.img")
	if err := fctest.Clone(fx.Image.Rootfs, p, 0); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConformance(t *testing.T) {
	fx := fctest.Setup(t)
	o := func(t *testing.T, dir string) runtime.Runtime {
		rec := fctest.Record(open(t, fx, dir))
		fctest.Guard(t, dir, rec) // no VMM may outlive the subtest
		return rec
	}
	runtimetest.Run(t, runtimetest.Harness{
		New: o, Reopen: o, Image: fx.Image, Shape: shape(fx.Image),
		Disk: func(t *testing.T, _ runtime.Key) string { return disk(t, fx) },
		CloneDisk: func(t *testing.T, src string) string {
			p := filepath.Join(filepath.Dir(src), "clone-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".img")
			if err := fctest.Clone(src, p, 0); err != nil {
				t.Fatal(err)
			}
			return p
		},
	})
}

// TestGoldenRestore covers what the shared suite cannot: golden capture,
// restore into private disks, isolation between restores, guest network.
func TestGoldenRestore(t *testing.T) {
	fx := fctest.Setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	// Instances and captures get rootless per-VM namespaces from
	// internal/network, as lifecycle provisions them in the daemon.
	ncfg := network.DefaultConfig()
	prov, err := network.NewRootless(ncfg)
	if err != nil {
		t.Fatal(err)
	}
	netFor := func(vm domain.VMID, gen int64) (runtime.NetworkHandle, func()) {
		h, err := prov.Create(ctx, vm, gen, domain.Egress{Mode: domain.EgressFiltered})
		if err != nil {
			t.Fatal(err)
		}
		enter, err := prov.Enter(h)
		if err != nil {
			t.Fatal(err)
		}
		return lifecycle.RuntimeNetwork(h, enter), func() { prov.Destroy(context.Background(), h) }
	}
	dir := t.TempDir()
	fctest.Guard(t, dir, nil)
	rt, err := firecracker.New(firecracker.Options{Dir: dir, Binary: fx.Binary.Path, GuestShutdown: fctest.Shutdown,
		NIC: firecracker.NIC{Tap: ncfg.TapName, MAC: ncfg.GuestMAC},
		CaptureNetwork: func(ctx context.Context) (runtime.NetworkHandle, func(), error) {
			nh, release := netFor("golden", time.Now().UnixNano())
			return nh, release, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	sh := shape(fx.Image)
	prep, err := rt.Prepare(ctx, fx.Image, sh)
	if err != nil {
		t.Fatal(err)
	}
	if prep.Golden == nil || prep.Golden.Disk == "" {
		t.Fatalf("no golden: %+v", prep)
	}
	again, err := rt.Prepare(ctx, fx.Image, sh)
	if err != nil || again.Golden.ID != prep.Golden.ID {
		t.Fatalf("second Prepare not cached: %+v %v", again.Golden, err)
	}

	var hs []runtime.Handle
	for i, vm := range []string{"vm-a", "vm-b"} {
		root := filepath.Join(t.TempDir(), "root.img")
		if err := fctest.Clone(prep.Golden.Disk, root, 0); err != nil {
			t.Fatal(err)
		}
		inst := runtime.Instance{Key: runtime.Key{VMID: domain.VMID(vm), Generation: 1}, Shape: sh, Image: fx.Image}
		t0 := time.Now()
		nh, release := netFor(domain.VMID(vm), 1)
		h, err := rt.RestoreGolden(ctx, inst, *prep.Golden, runtime.DiskSet{Root: root}, nh)
		if err != nil {
			release()
			t.Fatal(err)
		}
		t.Cleanup(func() { rt.Destroy(context.Background(), h); release() })
		if h.BootMode != domain.BootModeGoldenRestore {
			t.Fatalf("boot mode %s", h.BootMode)
		}
		out, err := fctest.Sh(ctx, rt, h, "echo "+vm+" > /root/who; cat /root/who")
		if err != nil || out != vm {
			t.Fatalf("restore %d: %q %v", i, out, err)
		}
		t.Logf("%s: restore -> barrier -> first command %s", vm, time.Since(t0).Round(time.Millisecond))
		// Guest NIC on the per-VM tap: ARP resolves the namespace side (the
		// network's ruleset drops guest-to-namespace IP traffic by design).
		if out, err := fctest.Sh(ctx, rt, h, "busybox ping -c1 -W1 172.16.0.1 >/dev/null 2>&1; ip -4 -o addr show eth0; ip neigh show 172.16.0.1"); err != nil || !strings.Contains(out, "172.16.0.2/") || !strings.Contains(out, "lladdr") {
			t.Fatalf("guest network: %q %v", out, err)
		}
		hs = append(hs, h)
	}
	if out, err := fctest.Sh(ctx, rt, hs[0], "cat /root/who"); err != nil || out != "vm-a" {
		t.Fatalf("restores share a disk: vm-a sees %q %v", out, err)
	}
	// Restores of one golden get distinct identities from the barrier.
	a, _ := fctest.Sh(ctx, rt, hs[0], "cat /etc/machine-id")
	b, _ := fctest.Sh(ctx, rt, hs[1], "cat /etc/machine-id")
	if a == "" || a == b {
		t.Fatalf("restores share machine-id %q", a)
	}
	// Stop through the agent: the guest powers off and the VMM is reaped.
	if err := rt.Stop(ctx, hs[1], time.Minute); err != nil {
		t.Fatalf("agent stop: %v", err)
	}

	// A golden refuses an instance whose disk is not its generation's size.
	bad := runtime.Instance{Key: runtime.Key{VMID: "vm-c", Generation: 1}, Shape: sh, Image: fx.Image}
	small := filepath.Join(t.TempDir(), "small.img")
	if err := os.WriteFile(small, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RestoreGolden(ctx, bad, *prep.Golden, runtime.DiskSet{Root: small}, runtime.NetworkHandle{}); err == nil {
		t.Fatal("restored a golden onto a mismatched disk")
	}
	// And one captured for another shape.
	other := sh
	other.MemoryBytes = 256 * domain.MiB
	bad.Shape = other
	if _, err := rt.RestoreGolden(ctx, bad, *prep.Golden, runtime.DiskSet{Root: small}, runtime.NetworkHandle{}); !domain.IsCode(err, domain.CodeUnsupportedCapability) {
		t.Fatalf("incompatible golden: %v", err)
	}
}
