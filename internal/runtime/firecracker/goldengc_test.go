//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestGCGoldens: the sweep removes exactly the goldens that can never be
// restored again (root or kernel gone, other engine build, CPU or layout,
// incomplete), reports them first with dryRun, and keeps the rest.
func TestGCGoldens(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{opts: Options{Dir: dir}, version: "v1.17.0", binSHA: "abc", cpu: "cpu0",
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), gLock: map[string]*sync.Mutex{}, gFail: map[string]error{}}
	touch := func(p string) string {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	kernel, image, commit := touch(filepath.Join(dir, "vmlinux")), touch(filepath.Join(dir, "rootfs")), touch(filepath.Join(dir, "disk.img"))
	ok := goldenInputs{Layout: goldenLayout, Engine: Engine + " v1.17.0", EngineSHA: "abc", HostCPU: "cpu0", Kernel: kernel, Rootfs: image, Network: r.opts.NIC.layout()}
	golden := func(id string, in goldenInputs, meta bool) {
		d := filepath.Join(dir, "golden", id)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		touch(filepath.Join(d, "mem"))
		if meta {
			b, _ := json.Marshal(goldenMeta{ID: id, Inputs: in})
			if err := os.WriteFile(filepath.Join(d, "golden.json"), b, 0o400); err != nil {
				t.Fatal(err)
			}
		}
	}
	snap := ok
	snap.Rootfs = commit // a project image's golden
	gone := ok
	gone.Rootfs = filepath.Join(dir, "deleted-snapshot", "disk.img")
	engine := ok
	engine.EngineSHA = "old"
	cpu := ok
	cpu.HostCPU = "other"
	golden("gimage", ok, true)
	golden("gsnap", snap, true)
	golden("ggone", gone, true)
	golden("gengine", engine, true)
	golden("gcpu", cpu, true)
	golden("gpartial", ok, false)
	if err := os.MkdirAll(filepath.Join(dir, "golden", ".prep-gx"), 0o700); err != nil { // a capture in progress: not ours
		t.Fatal(err)
	}

	want := map[string]bool{"ggone": true, "gengine": true, "gcpu": true, "gpartial": true}
	check := func(rep GoldenGCReport, dry bool) {
		t.Helper()
		got := map[string]bool{}
		for _, it := range rep.Removed {
			got[it.ID] = true
			if it.Reason == "" {
				t.Errorf("%s removed without a reason", it.ID)
			}
		}
		if len(got) != len(want) || rep.Kept != 2 || rep.DryRun != dry {
			t.Fatalf("removed %v kept %d dry %v, want %v kept 2", got, rep.Kept, rep.DryRun, want)
		}
		for id := range want {
			if !got[id] {
				t.Fatalf("%s not swept: %+v", id, rep)
			}
		}
	}
	rep, err := r.GCGoldens(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	check(rep, true)
	if _, err := os.Stat(filepath.Join(dir, "golden", "ggone")); err != nil {
		t.Fatal("dry run removed a golden")
	}
	if rep, err = r.GCGoldens(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	check(rep, false)
	left, _ := filepath.Glob(filepath.Join(dir, "golden", "g*"))
	var names []string
	for _, p := range left {
		names = append(names, filepath.Base(p))
	}
	if len(names) != 2 || names[0] != "gimage" || names[1] != "gsnap" {
		t.Fatalf("left %v", names)
	}
	if _, err := os.Stat(filepath.Join(dir, "golden", ".prep-gx")); err != nil {
		t.Fatal("the sweep touched a capture in progress")
	}
	// Removing the snapshot's commit makes its golden garbage.
	os.Remove(commit)
	if rep, _ = r.GCGoldens(context.Background(), false); len(rep.Removed) != 1 || rep.Removed[0].ID != "gsnap" {
		t.Fatalf("after the snapshot's commit went: %+v", rep)
	}
}
