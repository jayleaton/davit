//go:build linux

package main_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
)

// TestFirecrackerBoxResize: on real VMs, an instance created with 2 GiB is
// resized by `box up --memory 4GiB` and `box run --memory ...` while it
// runs (stopped, disk committed, booted again): the guest's MemTotal
// follows, a file written before survives every resize, and flagless ups,
// also after a down, keep the last shape.
func TestFirecrackerBoxResize(t *testing.T) {
	h := newFCHost(t, nil)
	env := append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=", "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"))
	tree := filepath.Join(h.dir, "proj")
	gitTree(t, tree, "main")
	box := func(args ...string) (string, string) {
		t.Helper()
		start := time.Now()
		so, se, code := human(t, env, tree, append([]string{"box"}, args...)...)
		if code != 0 {
			t.Fatalf("box %v: exit %d\nstdout %s\nstderr %s\ndaemon log tail:\n%s", args, code, so, se, tail(h.log.String(), 4000))
		}
		t.Logf("box %s: %s", strings.Join(args, " "), time.Since(start).Round(time.Millisecond))
		return so, se
	}
	// memTotal runs grep MemTotal in the box (with extra box run flags)
	// and returns it in bytes.
	memTotal := func(flags ...string) int64 {
		t.Helper()
		so, _ := box(append(append([]string{"run", "proj/a", "--no-sync"}, flags...), "--", "grep", "MemTotal", "/proc/meminfo")...)
		f := strings.Fields(so)
		if len(f) != 3 || f[0] != "MemTotal:" || f[2] != "kB" {
			t.Fatalf("MemTotal: %q", so)
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return kb << 10
	}
	// about: the guest sees its memory less what the kernel keeps.
	about := func(got, want int64) {
		t.Helper()
		if got > want || got < want-want/8 {
			t.Fatalf("MemTotal %d bytes (%.2f GiB), want about %d GiB", got, float64(got)/float64(domain.GiB), want/domain.GiB)
		}
		t.Logf("MemTotal %.2f GiB with %d GiB", float64(got)/float64(domain.GiB), want/domain.GiB)
	}
	marker := func() {
		t.Helper()
		if so, _ := box("run", "proj/a", "--no-sync", "--", "cat", "resize-marker"); strings.TrimSpace(so) != "kept" {
			t.Fatalf("marker after the resize: %q", so)
		}
	}

	box("image", "build", "--setup-cmd", "true", "--cpus", "1", "--memory", "2GiB")
	box("up", "proj/a", "--cpus", "1", "--memory", "2GiB")
	about(memTotal(), 2*domain.GiB)
	box("run", "proj/a", "--no-sync", "--", "sh", "-c", "echo kept > resize-marker && sync")

	// box up --memory on the running box.
	so, se := box("up", "proj/a", "--memory", "4GiB")
	if !strings.Contains(so, "proj/a resized") || !strings.Contains(se, "resizing from 1 cpu, 2.0 GiB memory") {
		t.Fatalf("box up --memory 4GiB: stdout %q stderr %q", so, se)
	}
	about(memTotal(), 4*domain.GiB)
	marker()
	if so, _ := box("up", "proj/a"); !strings.Contains(so, "proj/a running") {
		t.Fatalf("flagless up: %q", so)
	}
	box("down", "proj/a")
	if so, _ := box("up", "proj/a"); !strings.Contains(so, "proj/a resumed") {
		t.Fatalf("flagless up after down: %q", so)
	}
	about(memTotal(), 4*domain.GiB)

	// box run --memory on the running box, down and up again.
	about(memTotal("--memory", "2GiB"), 2*domain.GiB)
	about(memTotal("--memory", "4GiB"), 4*domain.GiB)
	marker()
	box("down", "proj/a")
	box("up", "proj/a")
	about(memTotal(), 4*domain.GiB)
	marker()
	if vm := h.info("proj--a"); vm.Shape.CPUs != 1 || vm.Shape.MemoryBytes != 4*domain.GiB {
		t.Fatalf("shape after the resizes: %+v", vm.Shape)
	}
}
