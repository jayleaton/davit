package mac_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/runtime/mac"
	"davit/internal/runtime/mac/fakehelper"
	"davit/internal/runtime/runtimetest"
)

func TestMain(m *testing.M) {
	fakehelper.Main() // this binary is the helper when run as one
	os.Exit(m.Run())
}

// shortDirs maps a test's dir to a short one: helper sockets must fit in a
// 103-byte path, and runtimetest's subtest temp dirs are long. A Reopen of
// the same dir gets the same short dir.
type shortDirs struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *shortDirs) get(t *testing.T, dir string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.m[dir]; ok {
		return d
	}
	d, err := os.MkdirTemp("", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	s.m[dir] = d
	return d
}

func image(t *testing.T) domain.Image {
	kernel := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	return domain.Image{ID: "img", Arch: domain.ArchARM64, Kernel: kernel, BootArgs: "console=hvc0 root=/dev/vda rw"}
}

func open(t *testing.T, dir, helper string, env ...string) *mac.Runtime {
	t.Helper()
	rt, err := mac.New(mac.Options{Dir: dir, Helper: helper, HelperEnv: env})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestConformance(t *testing.T) {
	helper := fakehelper.Binary(t)
	dirs := &shortDirs{m: map[string]string{}}
	o := func(t *testing.T, dir string) runtime.Runtime { return open(t, dirs.get(t, dir), helper) }
	runtimetest.Run(t, runtimetest.Harness{
		New: o, Reopen: o, Image: image(t), Disk: runtimetest.TempDisk, Timeout: 30 * time.Second,
	})
}

func boot(t *testing.T, rt *mac.Runtime, img domain.Image, vm string) runtime.Handle {
	t.Helper()
	inst := runtime.Instance{Key: runtime.Key{VMID: domain.VMID(vm), Generation: 1}, Image: img,
		Shape: domain.Shape{CPUs: 1, MemoryBytes: 256 * domain.MiB, DiskBytes: 64 * domain.MiB}}
	h, err := rt.ColdBoot(context.Background(), inst, runtime.DiskSet{Root: runtimetest.TempDisk(t, inst.Key)}, runtime.NetworkHandle{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Destroy(context.Background(), h) })
	return h
}

func TestCapabilitiesAreHonest(t *testing.T) {
	c := open(t, shortDir(t), fakehelper.Binary(t)).Capabilities()
	if c.Engine != mac.Engine || !c.ColdBoot || !c.DiskFork || c.GuestTransport != "vsock" ||
		c.GoldenRestore || c.RAMSnapshot || c.LiveFork || len(c.RAMCheckpointFormats) != 0 ||
		len(c.Arch) != 1 || c.Arch[0] != domain.ArchARM64 || c.Shape.MaxCPUs != 8 {
		t.Fatalf("capabilities %+v", c)
	}
}

func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestGuestTransportReachesTheAgentPort(t *testing.T) {
	rt := open(t, shortDir(t), fakehelper.Binary(t))
	h := boot(t, rt, image(t), "vm-a")
	d, err := rt.GuestTransport(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := d.DialGuest(ctx, mac.AgentPort)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := []byte("hello guest, " + strings.Repeat("x", 100000)) // larger than any frame
	go c.Write(msg)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != string(msg) {
		t.Fatalf("echo: %v", err)
	}
	if _, err := d.DialGuest(ctx, 9999); !domain.IsCode(err, domain.CodeUnavailable) {
		t.Fatalf("closed port: %v", err)
	}
}

func TestStopTimeoutKeepsRunningUntilKill(t *testing.T) {
	rt := open(t, shortDir(t), fakehelper.Binary(t), "DAVIT_FAKE_VZ=ignore-shutdown")
	h := boot(t, rt, image(t), "vm-a")
	ctx := context.Background()
	err := rt.Stop(ctx, h, 50*time.Millisecond)
	if !domain.IsCode(err, domain.CodeTimeout) {
		t.Fatalf("stop of a guest that ignores it: %v", err)
	}
	if st, _ := rt.Inspect(ctx, h); st.Phase != runtime.PhaseRunning {
		t.Fatalf("after a timed-out stop: %s, want running (never escalate silently)", st.Phase)
	}
	if err := rt.Kill(ctx, h); err != nil {
		t.Fatal(err)
	}
	if st, _ := rt.Inspect(ctx, h); st.Phase != runtime.PhaseExited || st.ExitReason != "killed" {
		t.Fatalf("after kill: %+v", st)
	}
}

func TestHelperCrashIsReportedAndReplaced(t *testing.T) {
	dir := shortDir(t)
	rt := open(t, dir, fakehelper.Binary(t))
	img := image(t)
	h := boot(t, rt, img, "vm-a")
	// The helper process dies without writing its exit record.
	f := strings.Split(h.ProcessIdentity, ":")
	pid, _ := strconv.Atoi(f[1])
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		st, err := rt.Inspect(ctx, h)
		if err == nil && st.Phase == runtime.PhaseExited && st.ExitReason == "crashed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("crashed helper: %+v %v", st, err)
		}
	}
	if err := rt.Stop(ctx, h, time.Second); err != nil {
		t.Fatalf("stop of a crashed instance must be a no-op: %v", err)
	}
	// The key boots again in a new helper; the old handle is stale.
	again := boot(t, rt, img, "vm-a")
	if again.ProcessIdentity == h.ProcessIdentity {
		t.Fatal("replacement reused the crashed helper's identity")
	}
	if err := rt.Kill(ctx, h); !errors.Is(err, runtime.ErrStaleHandle) {
		t.Fatalf("stale kill: %v", err)
	}
}

func TestHandleRoundTripsAndNamesItsHelper(t *testing.T) {
	rt := open(t, shortDir(t), fakehelper.Binary(t))
	h := boot(t, rt, image(t), "vm-a")
	b, _ := json.Marshal(h)
	var back runtime.Handle
	if json.Unmarshal(b, &back) != nil || back.ProcessIdentity != h.ProcessIdentity || string(back.Data) != string(h.Data) {
		t.Fatalf("handle round trip: %s", b)
	}
	if st, err := rt.Inspect(context.Background(), back); err != nil || st.Phase != runtime.PhaseRunning {
		t.Fatalf("inspect via decoded handle: %+v %v", st, err)
	}
}

func TestRefusesWhatItCannotDo(t *testing.T) {
	ctx := context.Background()
	rt := open(t, shortDir(t), fakehelper.Binary(t))
	img := image(t)
	inst := runtime.Instance{Key: runtime.Key{VMID: "vm-a", Generation: 1}, Image: img,
		Shape: domain.Shape{CPUs: 1, MemoryBytes: 256 * domain.MiB, DiskBytes: 64 * domain.MiB}}
	disk := runtime.DiskSet{Root: runtimetest.TempDisk(t, inst.Key)}
	if _, err := rt.ColdBoot(ctx, inst, disk, runtime.NetworkHandle{TapName: "tap0", Namespace: "/proc/1/ns/net"}); !domain.IsCode(err, domain.CodeUnsupportedCapability) {
		t.Errorf("network handle: %v", err)
	}
	x86 := inst
	x86.Image.Arch = domain.ArchAMD64
	if _, err := rt.ColdBoot(ctx, x86, disk, runtime.NetworkHandle{}); !domain.IsCode(err, domain.CodeUnsupportedCapability) {
		t.Errorf("x86 image: %v", err)
	}
	big := inst
	big.Shape.CPUs = 64
	if _, err := rt.ColdBoot(ctx, big, disk, runtime.NetworkHandle{}); !domain.IsCode(err, domain.CodeUnsupportedCapability) {
		t.Errorf("shape beyond VZ limits: %v", err)
	}
	if _, err := rt.Prepare(ctx, img, inst.Shape); err != nil {
		t.Errorf("prepare: %v", err)
	}
	if _, err := mac.New(mac.Options{Dir: shortDir(t), Helper: fakehelper.Binary(t), HelperEnv: []string{"DAVIT_FAKE_VZ=unentitled"}}); err == nil || !strings.Contains(err.Error(), "entitlement") {
		t.Errorf("unentitled helper: %v", err)
	}
	if _, err := mac.New(mac.Options{Dir: shortDir(t), Helper: "/nonexistent/helper"}); err == nil {
		t.Error("missing helper accepted")
	}
	long := filepath.Join(shortDir(t), strings.Repeat("d", 100))
	if _, err := mac.New(mac.Options{Dir: long, Helper: fakehelper.Binary(t)}); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("long runtime dir: %v", err)
	}
}
