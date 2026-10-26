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
	"time"

	"davit/internal/domain"
	"davit/internal/runtime"
)

// lruFixture is a runtime with a 10 MiB golden floor and these goldens
// (id, base image, minutes since last use), each with a 2 MiB memory file.
func lruFixture(t *testing.T, goldens ...struct {
	id, image string
	age       int
}) (*Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	r := &Runtime{opts: Options{Dir: dir, GoldenMinFree: 10 * domain.MiB},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), gLock: map[string]*sync.Mutex{}, gFail: map[string]error{}}
	base := time.Now().Add(-time.Hour)
	for _, g := range goldens {
		d := filepath.Join(dir, "golden", g.id)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(goldenMeta{ID: g.id, Inputs: goldenInputs{ImageDigest: g.image, Rootfs: "/root/" + g.id}})
		os.WriteFile(filepath.Join(d, "golden.json"), b, 0o400)
		mem := make([]byte, 2*domain.MiB)
		for i := range mem {
			mem[i] = 1 // allocated blocks, not a hole
		}
		os.WriteFile(filepath.Join(d, "mem"), mem, 0o400)
		used := base.Add(-time.Duration(g.age) * time.Minute)
		os.Chtimes(d, used, used)
	}
	return r, dir
}

type lruG = struct {
	id, image string
	age       int
}

// TestGoldenLRU: below the floor, the least recently used goldens go first,
// the most recent golden of each image always stays, a golden a Prepare
// holds is skipped, and every eviction is reported with what it freed.
func TestGoldenLRU(t *testing.T) {
	r, dir := lruFixture(t, lruG{"g1", "dev", 1}, lruG{"g2", "dev", 2}, lruG{"g3", "dev", 3}, lruG{"g4", "dev", 4},
		lruG{"gbase", "base", 50}, lruG{"g5", "dev", 5})
	removed := func() int64 {
		left, _ := filepath.Glob(filepath.Join(dir, "golden", "g*"))
		return int64(6 - len(left))
	}
	free := int64(3 * domain.MiB)
	r.statFree = func() (int64, error) { return free, nil } // the filesystem has not caught up yet
	var got []runtime.GoldenEviction
	r.OnGoldenEvicted(func(e runtime.GoldenEviction) { got = append(got, e) })

	// A Prepare holds g5, the least recently used.
	l := r.goldenLock("g5")
	l.Lock()
	if err := r.EnforceGoldenFloor(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.Unlock()
	// 3 MiB free, 7 short: g5 is held, gbase and g1 are protected, so g4, g3
	// and g2 go (6 MiB) and the floor is not met.
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
		if e.Reason == "" || e.Bytes < 2*domain.MiB || e.ImageDigest != "dev" || e.Root != "/root/"+e.ID {
			t.Fatalf("eviction %+v", e)
		}
	}
	if len(ids) != 3 || ids[0] != "g4" || ids[1] != "g3" || ids[2] != "g2" {
		t.Fatalf("evicted %v, want g4 g3 g2 (least recently used first)", ids)
	}
	for _, keep := range []string{"g1", "gbase", "g5"} {
		if _, err := os.Stat(filepath.Join(dir, "golden", keep)); err != nil {
			t.Fatalf("%s evicted", keep)
		}
	}
	// The filesystem catches up; now g5 is free: it goes, and the floor is
	// met (3 + 4×2 = 11 MiB).
	got = nil
	r.statFree = func() (int64, error) { return 3*domain.MiB + removed()*2*domain.MiB, nil }
	if ok, err := r.enforceFloor(context.Background(), 0); err != nil || !ok || len(got) != 1 || got[0].ID != "g5" {
		t.Fatalf("second pass: ok %v err %v %+v", ok, err, got)
	}
	// A capture needing 2 MiB more cannot be made room for: only protected
	// goldens are left.
	if ok, _ := r.enforceFloor(context.Background(), 2*domain.MiB); ok {
		t.Fatal("capture admitted below the floor")
	}
	// The floor can be disabled.
	r.opts.GoldenMinFree = -1
	if ok, err := r.enforceFloor(context.Background(), 100*domain.GiB); !ok || err != nil {
		t.Fatalf("disabled floor: %v %v", ok, err)
	}
}

// TestGoldenLRUDelayedFree: XFS frees an unlinked file's blocks in the
// background, so statfs right after an eviction can still show the old free
// space. One eviction that is enough must be the only one (on the test host
// the second-oldest golden went too).
func TestGoldenLRUDelayedFree(t *testing.T) {
	r, dir := lruFixture(t, lruG{"gnew", "dev", 1}, lruG{"gmid", "dev", 2}, lruG{"gold", "dev", 3})
	r.statFree = func() (int64, error) { return 9 * domain.MiB, nil } // 1 MiB short, and never updated
	var got []string
	r.OnGoldenEvicted(func(e runtime.GoldenEviction) { got = append(got, e.ID) })
	if ok, err := r.enforceFloor(context.Background(), 0); err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if len(got) != 1 || got[0] != "gold" {
		t.Fatalf("evicted %v, want only gold", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "golden", "gmid")); err != nil {
		t.Fatal("gmid evicted")
	}
}
