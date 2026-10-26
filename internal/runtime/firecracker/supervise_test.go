//go:build linux

package firecracker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/runtime/firecracker"
	"davit/internal/runtime/firecracker/fctest"
)

// These tests drive the adapter's process supervision against the fake VMM
// (fctest.FakeVMM): no guest, no images, so they run in `make check`.

// fakeRuntime opens an adapter over dir whose "Firecracker" is the fake and
// whose guest shutdown is the fake agent's power-off.
func fakeRuntime(t *testing.T, dir, bin string, log ...*slog.Logger) *firecracker.Runtime {
	t.Helper()
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skipf("the adapter requires KVM: %v", err)
	} else {
		f.Close()
	}
	o := firecracker.Options{Dir: dir, Binary: bin,
		GuestShutdown: func(ctx context.Context, _ runtime.Key, d runtime.Dialer) error {
			c, err := d.DialGuest(ctx, firecracker.AgentPort)
			if err != nil {
				return err
			}
			defer c.Close()
			_, err = io.WriteString(c, "poweroff\n")
			return err
		}}
	if len(log) > 0 {
		o.Log = log[0]
	}
	rt, err := firecracker.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// fakeBoot cold boots vm on the fake; behavior is its fake.* boot args.
func fakeBoot(t *testing.T, ctx context.Context, rt runtime.Runtime, vm, behavior string) runtime.Handle {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root.img")
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	inst := runtime.Instance{
		Key:   runtime.Key{VMID: domain.VMID(vm), Generation: 1},
		Shape: domain.Shape{CPUs: 1, MemoryBytes: 128 * domain.MiB, DiskBytes: 64 * domain.MiB},
		Image: domain.Image{ID: "fake", Digest: "sha256:fake", Arch: domain.ArchAMD64, Kernel: "/fake/vmlinux", BootArgs: behavior},
	}
	h, err := rt.ColdBoot(ctx, inst, runtime.DiskSet{Root: root}, runtime.NetworkHandle{})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// pidOf is the VMM pid in a handle's process identity ("fc:<pid>:...").
func pidOf(t *testing.T, h runtime.Handle) int {
	t.Helper()
	f := strings.Split(h.ProcessIdentity, ":")
	if len(f) < 2 {
		t.Fatalf("process identity %q", h.ProcessIdentity)
	}
	pid, err := strconv.Atoi(f[1])
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// running reports whether pid is a live (not zombie) process.
func running(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	return i >= 0 && i+2 < len(b) && b[i+2] != 'Z' && b[i+2] != 'X'
}

// TestStopRacesLeaveNoVMM forces the shutdown overlaps suspected of leaking
// VMMs: Ctrl+Alt+Del and the agent's power-off arriving together with every
// combination of guest reactions and timings, a Stop that times out
// followed by Destroy, and Destroy racing a Stop still waiting. A Stop that
// succeeds must leave no VMM running, and after Destroy none may survive.
func TestStopRacesLeaveNoVMM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	rt := fctest.Record(fakeRuntime(t, dir, fctest.FakeBinary(t)))
	fctest.Guard(t, dir, rt)

	destroyed := func(t *testing.T, h runtime.Handle) {
		t.Helper()
		if err := rt.Destroy(ctx, h); err != nil {
			t.Fatalf("destroy: %v", err)
		}
		if st, err := rt.Inspect(ctx, h); err != nil || st.Phase != runtime.PhaseGone {
			t.Fatalf("after destroy: %s %v", st.Phase, err)
		}
		if running(pidOf(t, h)) {
			t.Fatalf("VMM %d survived Destroy", pidOf(t, h))
		}
	}
	n := 0
	for _, cad := range []string{"reboot", "halt", "ignore"} {
		for _, off := range []string{"halt", "reboot", "ignore"} {
			for _, delay := range []string{"0s", "3ms", "40ms"} {
				name := "cad=" + cad + "/poweroff=" + off + "/delay=" + delay
				t.Run(name, func(t *testing.T) {
					n++
					h := fakeBoot(t, ctx, rt, "vm-"+strconv.Itoa(n), "fake.cad="+cad+" fake.poweroff="+off+" fake.delay="+delay)
					err := rt.Stop(ctx, h, 500*time.Millisecond)
					if cad == "ignore" && off == "ignore" {
						if !domain.IsCode(err, domain.CodeTimeout) {
							t.Fatalf("stop of a guest that never shuts down: %v, want timeout", err)
						}
						if !running(pidOf(t, h)) {
							t.Fatal("timed-out stop must leave the VMM running")
						}
					} else {
						if err != nil {
							t.Fatalf("stop: %v", err)
						}
						if running(pidOf(t, h)) {
							t.Fatal("stop succeeded but the VMM still runs")
						}
						if st, _ := rt.Inspect(ctx, h); st.Phase != runtime.PhaseExited {
							t.Fatalf("after stop: %s", st.Phase)
						}
					}
					destroyed(t, h)
				})
			}
		}
	}

	t.Run("destroy during stop", func(t *testing.T) {
		h := fakeBoot(t, ctx, rt, "vm-busy", "fake.cad=ignore fake.poweroff=ignore")
		stopped := make(chan error, 1)
		go func() { stopped <- rt.Stop(ctx, h, time.Minute) }()
		time.Sleep(50 * time.Millisecond)
		destroyed(t, h)
		select {
		case err := <-stopped:
			if err != nil && !errors.Is(err, runtime.ErrStaleHandle) {
				t.Logf("stop after concurrent destroy: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("stop still waiting for a destroyed VMM")
		}
	})

	t.Run("concurrent stops", func(t *testing.T) {
		h := fakeBoot(t, ctx, rt, "vm-twice", "fake.cad=halt fake.poweroff=halt fake.delay=5ms")
		errs := make(chan error, 2)
		for range 2 {
			go func() { errs <- rt.Stop(ctx, h, 5*time.Second) }()
		}
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("stop: %v", err)
			}
		}
		destroyed(t, h)
	})
}

// TestRestartReapsStrayVMMs checks that a restarted adapter (daemon start)
// kills VMMs no instance record accounts for and leaves everything else.
func TestRestartReapsStrayVMMs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	fctest.Guard(t, dir, nil)
	bin := fctest.FakeBinary(t)
	rt := fakeRuntime(t, dir, bin)
	instDir := func(h runtime.Handle) string {
		return filepath.Join(dir, "instances", string(h.Key.VMID)+".g"+strconv.FormatInt(h.Key.Generation, 10))
	}

	kept := fakeBoot(t, ctx, rt, "vm-kept", "")
	// Its instance directory vanished (the leak this guards against).
	orphan := fakeBoot(t, ctx, rt, "vm-orphan", "")
	if err := os.RemoveAll(instDir(orphan)); err != nil {
		t.Fatal(err)
	}
	// Its record names another process.
	replaced := fakeBoot(t, ctx, rt, "vm-replaced", "")
	recPath := filepath.Join(instDir(replaced), "instance.json")
	var rec map[string]any
	if b, err := os.ReadFile(recPath); err != nil || json.Unmarshal(b, &rec) != nil {
		t.Fatalf("read record: %v", err)
	}
	rec["proc"].(map[string]any)["start"] = 1
	if b, err := json.Marshal(rec); err != nil || os.WriteFile(recPath, b, 0o600) != nil {
		t.Fatal("rewrite record")
	}
	// A VMM outside instances/.
	loose := exec.Command(bin, "--api-sock", "api.sock")
	loose.Dir = filepath.Join(dir, "golden")
	if err := loose.Start(); err != nil {
		t.Fatal(err)
	}
	go loose.Wait()
	// Not a VMM: never touched.
	other := exec.Command("sleep", "60")
	other.Dir = instDir(kept)
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Process.Kill(); other.Wait() })
	for _, p := range []int{pidOf(t, orphan), pidOf(t, replaced), loose.Process.Pid} {
		if !running(p) {
			t.Fatalf("setup: %d not running", p)
		}
	}

	var logs bytes.Buffer
	restarted := fakeRuntime(t, dir, bin, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for what, p := range map[string]int{"orphan": pidOf(t, orphan), "replaced": pidOf(t, replaced), "loose": loose.Process.Pid} {
		if running(p) {
			t.Errorf("%s VMM %d survived the restart", what, p)
		}
	}
	if st, err := restarted.Inspect(ctx, kept); err != nil || st.Phase != runtime.PhaseRunning {
		t.Errorf("recorded instance after restart: %s %v", st.Phase, err)
	}
	if !running(other.Process.Pid) {
		t.Error("restart killed a process that is not a VMM")
	}
	// Every kill is logged with its reason, so a VMM that died without
	// such a line was killed from outside the adapter.
	if n := strings.Count(logs.String(), `msg="SIGKILL firecracker VMM"`); n != 3 || !strings.Contains(logs.String(), "why=\"stray: no instance record") {
		t.Errorf("restart logged %d VMM kills, want 3 strays:\n%s", n, logs.String())
	}
	// Through the adapter that spawned it: it also records the VMM's exit,
	// which a second adapter in this process would race (a restarted
	// daemon has no such twin).
	if err := rt.Destroy(ctx, kept); err != nil {
		t.Fatal(err)
	}
}
