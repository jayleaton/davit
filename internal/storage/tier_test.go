package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
)

// tiered opens a store with a cold tier in two temp dirs (one filesystem
// here; the moves do not care).
func tiered(t *testing.T) (*FS, string, string) {
	t.Helper()
	hot, cold := t.TempDir(), t.TempDir()
	s, err := Open(hot, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AttachCold(cold); err != nil {
		t.Fatal(err)
	}
	return s, hot, cold
}

// reopen is a daemon restart: a new store on the same directories, with
// no moves in flight.
func reopen(t *testing.T, hot, cold string) *FS {
	t.Helper()
	s, err := Open(hot, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AttachCold(cold); err != nil {
		t.Fatal(err)
	}
	return s
}

// sparseDisk is a 4 MiB disk with data at a few offsets and holes between.
func sparseDisk(t *testing.T) (string, []byte) {
	t.Helper()
	want := make([]byte, 4*mib)
	copy(want[0:], pattern('a', 70000))
	copy(want[mib:], pattern('b', 5))
	copy(want[3*mib+100:], pattern('c', 200000))
	p := filepath.Join(t.TempDir(), "disk.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, off := range []int64{0, mib, 3*mib + 100} {
		n := min(int64(len(want))-off, 200000)
		if _, err := f.WriteAt(want[off:off+n], off); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Truncate(4 * mib); err != nil {
		t.Fatal(err)
	}
	return p, want
}

// committed publishes a commit c1 of sparseDisk in s.
func committed(t *testing.T, s *FS) []byte {
	t.Helper()
	ctx := context.Background()
	base, want := sparseDisk(t)
	if _, err := s.CreateWritable(ctx, "d1", base, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, "d1", "c1"); err != nil {
		t.Fatal(err)
	}
	return want
}

func published(root string, id CommitID) bool {
	_, err := os.Stat(filepath.Join(root, commitsDir, string(id), manifestName))
	return err == nil
}

func TestDemoteRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, hot, cold := tiered(t)
	want := committed(t, s)

	d, err := s.Demote(ctx, "c1", MoveOptions{IdleIO: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Commit.Tier != domain.TierCold || d.Bytes == 0 || d.Bytes > 4*mib {
		t.Fatalf("demotion %+v", d)
	}
	if !published(hot, "c1") || !published(cold, "c1") {
		t.Fatal("both copies must exist until the journal switches")
	}
	d.Release()
	keep := KeepSet{Commits: []CommitID{"c1"}, Cold: []CommitID{"c1"}}
	out, err := s.Collect(ctx, keep)
	if err != nil {
		t.Fatal(err)
	}
	if published(hot, "c1") || !published(cold, "c1") || out.Moved != 1 {
		t.Fatalf("after collect: hot %v cold %v %+v", published(hot, "c1"), published(cold, "c1"), out)
	}
	c, err := s.GetCommit(ctx, "c1")
	if err != nil || c.Tier != domain.TierCold {
		t.Fatalf("GetCommit: %+v %v", c, err)
	}
	if got := readAll(t, c.Path); !bytes.Equal(got, want) {
		t.Fatal("cold copy differs")
	}
	// Demote of a commit already cold is a no-op.
	if d, err := s.Demote(ctx, "c1", MoveOptions{}); err != nil || d.Commit.Tier != domain.TierCold {
		t.Fatalf("demote again: %v", err)
	} else {
		d.Release()
	}

	var calls []int64
	disk, err := s.Restore(ctx, "c1", "d2", 8*mib, RestoreOptions{Progress: func(done, total int64) { calls = append(calls, done) }})
	if err != nil {
		t.Fatal(err)
	}
	if disk.SizeBytes != 8*mib || !bytes.Equal(readN(t, disk.Path, 4*mib), want) || len(calls) == 0 {
		t.Fatalf("restored disk: size %d progress %v", disk.SizeBytes, calls)
	}
	// A plain clone of a cold commit restores too, and is idempotent.
	d3, err := s.CloneCommitted(ctx, "c1", "d3", 0)
	if err != nil || !bytes.Equal(readAll(t, d3.Path), want) {
		t.Fatalf("CloneCommitted from cold: %v", err)
	}
	if again, err := s.Restore(ctx, "c1", "d3", 0, RestoreOptions{}); err != nil || again != d3 {
		t.Fatalf("restore existing disk: %+v %v", again, err)
	}
	assertNoPartial(t, hot)
	assertNoPartial(t, cold)
}

func TestRestoreOntoBase(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []Mode{"", ModeCopy} {
		t.Run(string(mode)+"mode", func(t *testing.T) {
			s, a, hot := openAPFSLike(t, mode)
			if err := s.AttachCold(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			base, _ := sparseDisk(t)
			if _, err := s.CreateWritable(ctx, "d1", base, 0); err != nil {
				t.Fatal(err)
			}
			// The guest rewrote a block, zeroed another and grew the disk.
			writeAt(t, filepath.Join(hot, disksDir, "d1.img"), 2*mib, []byte("new data"))
			writeAt(t, filepath.Join(hot, disksDir, "d1.img"), mib, make([]byte, 5))
			if err := os.Truncate(filepath.Join(hot, disksDir, "d1.img"), 6*mib); err != nil {
				t.Fatal(err)
			}
			want := readAll(t, filepath.Join(hot, disksDir, "d1.img"))
			if _, err := s.Commit(ctx, "d1", "c1"); err != nil {
				t.Fatal(err)
			}
			d, err := s.Demote(ctx, "c1", MoveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			d.Release()
			if _, err := s.Collect(ctx, KeepSet{Commits: []CommitID{"c1"}, Cold: []CommitID{"c1"}}); err != nil {
				t.Fatal(err)
			}
			before := a.calls.Load()
			disk, err := s.Restore(ctx, "c1", "d2", 0, RestoreOptions{Base: base})
			if err != nil {
				t.Fatal(err)
			}
			if got := readAll(t, disk.Path); !bytes.Equal(got, want) {
				t.Fatal("disk restored onto the base differs from the commit")
			}
			if cloned := a.calls.Load() > before; cloned != (mode == "") {
				t.Fatalf("base cloned %v in mode %q", cloned, mode)
			}
			assertNoPartial(t, hot)
		})
	}
}

// TestDemoteCrashPoints interrupts a move at each step, restarts and
// checks that collection recovers a single good copy.
func TestDemoteCrashPoints(t *testing.T) {
	ctx := context.Background()

	t.Run("copy interrupted", func(t *testing.T) {
		s, hot, cold := tiered(t)
		want := committed(t, s)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Demote(cctx, "c1", MoveOptions{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("demote with canceled ctx: %v", err)
		}
		// A crash, unlike a cancel, leaves the partial copy behind.
		if err := os.MkdirAll(filepath.Join(cold, tmpDir, "demote-123", ""), 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(cold, tmpDir, "demote-123", commitData), []byte("part"), 0o600)
		s = reopen(t, hot, cold)
		out, err := s.Collect(ctx, KeepSet{Commits: []CommitID{"c1"}})
		if err != nil || out.Partial != 1 {
			t.Fatalf("collect: %+v %v", out, err)
		}
		assertNoPartial(t, cold)
		if c, err := s.GetCommit(ctx, "c1"); err != nil || c.Tier != domain.TierHot || !bytes.Equal(readAll(t, c.Path), want) {
			t.Fatalf("hot commit after interrupted copy: %+v %v", c, err)
		}
	})

	t.Run("published, journal not switched", func(t *testing.T) {
		s, hot, cold := tiered(t)
		want := committed(t, s)
		if _, err := s.Demote(ctx, "c1", MoveOptions{}); err != nil {
			t.Fatal(err)
		}
		s = reopen(t, hot, cold) // never released: the daemon died
		out, err := s.Collect(ctx, KeepSet{Commits: []CommitID{"c1"}})
		if err != nil || out.Moved != 1 || published(cold, "c1") || !published(hot, "c1") {
			t.Fatalf("collect: %+v %v", out, err)
		}
		if c, err := s.GetCommit(ctx, "c1"); err != nil || !bytes.Equal(readAll(t, c.Path), want) {
			t.Fatal(err)
		}
	})

	t.Run("journal switched, hot not removed", func(t *testing.T) {
		s, hot, cold := tiered(t)
		want := committed(t, s)
		if _, err := s.Demote(ctx, "c1", MoveOptions{}); err != nil {
			t.Fatal(err)
		}
		s = reopen(t, hot, cold)
		out, err := s.Collect(ctx, KeepSet{Commits: []CommitID{"c1"}, Cold: []CommitID{"c1"}})
		if err != nil || out.Moved != 1 || !published(cold, "c1") || published(hot, "c1") {
			t.Fatalf("collect: %+v %v", out, err)
		}
		if c, err := s.GetCommit(ctx, "c1"); err != nil || c.Tier != domain.TierCold || !bytes.Equal(readAll(t, c.Path), want) {
			t.Fatal(err)
		}
	})

	t.Run("cold copy lost: the only copy stays", func(t *testing.T) {
		s, hot, _ := tiered(t)
		committed(t, s)
		out, err := s.Collect(ctx, KeepSet{Commits: []CommitID{"c1"}, Cold: []CommitID{"c1"}})
		if err != nil || out.Moved != 0 || !published(hot, "c1") {
			t.Fatalf("collect: %+v %v", out, err)
		}
	})

	t.Run("manifest without digest is unpublished", func(t *testing.T) {
		s, _, cold := tiered(t)
		committed(t, s)
		d, err := s.Demote(ctx, "c1", MoveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		d.Release()
		mp := filepath.Join(cold, commitsDir, "c1", manifestName)
		mb, _ := os.ReadFile(mp)
		os.Chmod(mp, 0o644)
		os.WriteFile(mp, bytes.Replace(mb, []byte(`"dataDigest"`), []byte(`"x"`), 1), 0o444)
		out, err := s.Collect(ctx, KeepSet{Commits: []CommitID{"c1"}, Cold: []CommitID{"c1"}})
		if err != nil || out.Partial != 1 || published(cold, "c1") {
			t.Fatalf("collect: %+v %v", out, err)
		}
	})

	t.Run("in flight moves are left alone", func(t *testing.T) {
		s, hot, cold := tiered(t)
		committed(t, s)
		d, err := s.Demote(ctx, "c1", MoveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// Unreferenced, and no journal says cold: still both stay.
		if _, err := s.Collect(ctx, KeepSet{}); err != nil || !published(hot, "c1") || !published(cold, "c1") {
			t.Fatalf("collect during move: %v", err)
		}
		d.Release()
		out, err := s.Collect(ctx, KeepSet{})
		if err != nil || published(hot, "c1") || published(cold, "c1") || !slices.Contains(out.Commits, "c1") {
			t.Fatalf("collect after move: %+v %v", out, err)
		}
	})
}

func TestRestoreDetectsDamage(t *testing.T) {
	ctx := context.Background()
	s, hot, cold := tiered(t)
	committed(t, s)
	d, err := s.Demote(ctx, "c1", MoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Release()
	if _, err := s.Collect(ctx, KeepSet{Commits: []CommitID{"c1"}, Cold: []CommitID{"c1"}}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cold, commitsDir, "c1", commitData)
	os.Chmod(p, 0o644)
	writeAt(t, p, 3*mib+200, []byte("bit rot"))
	_, err = s.Restore(ctx, "c1", "d2", 0, RestoreOptions{})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("damaged")) {
		t.Fatalf("restore of a damaged copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hot, disksDir, "d2.img")); !os.IsNotExist(err) {
		t.Fatalf("damaged restore published a disk: %v", err)
	}
	assertNoPartial(t, hot)
}

func TestDigestIgnoresLayout(t *testing.T) {
	ctx := context.Background()
	sparse, want := sparseDisk(t)
	dense := filepath.Join(t.TempDir(), "dense.img")
	if err := os.WriteFile(dense, want, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := func(p string) string {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		d, err := fileDigest(ctx, f, int64(len(want)), nil)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if a, b := sum(sparse), sum(dense); a != b {
		t.Fatalf("digest depends on holes: %s vs %s", a, b)
	}
	writeAt(t, dense, 2*mib, []byte{1})
	if sum(sparse) == sum(dense) {
		t.Fatal("digest missed a changed byte")
	}
}

func TestPacerLimitsRate(t *testing.T) {
	p := newPacer(8 * mib)
	start := time.Now()
	for range 32 {
		if err := p.wait(context.Background(), mib/16); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("2 MiB at 8 MiB/s took %v", d)
	}
}

func TestDeltaBytesCountsOnlyUnsharedData(t *testing.T) {
	ctx := context.Background()
	s, hot, _ := tiered(t)
	base := filepath.Join(hot, "base.img") // on the store's filesystem, like an image
	if err := os.WriteFile(base, pattern('x', 2*mib), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateWritable(ctx, "d1", base, 0); err != nil {
		t.Fatal(err)
	}
	writeAt(t, filepath.Join(hot, disksDir, "d1.img"), mib, pattern('y', 4096))
	if _, err := s.Commit(ctx, "d1", "c1"); err != nil {
		t.Fatal(err)
	}
	all, err := s.DeltaBytes(ctx, "c1", "")
	if err != nil || all < 2*mib {
		t.Fatalf("delta without base: %d %v", all, err)
	}
	delta, err := s.DeltaBytes(ctx, "c1", base)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Probe(ctx); p.FastClone && delta > 256<<10 {
		t.Fatalf("delta %d after a 4 KiB write to a reflink clone", delta)
	}
	t.Logf("delta %d of %d", delta, all)
}

// TestAttachColdChecksIdentity: an unmounted cold disk (an empty
// directory where the tier was), or another store's tier, is refused; the
// same tier under a new path is accepted.
func TestAttachColdChecksIdentity(t *testing.T) {
	_, hot, cold := tiered(t)
	if dir, ok := ColdRecorded(hot); !ok || dir != cold {
		t.Fatalf("ColdRecorded = %q %v", dir, ok)
	}
	fresh, err := Open(hot, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.AttachCold(t.TempDir()); err == nil || !strings.Contains(err.Error(), "is the cold disk mounted") {
		t.Fatalf("attach of an empty dir in place of the tier: %v", err)
	}
	if err := fresh.AttachCold(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("attached a missing directory")
	}
	other, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.AttachCold(cold); err == nil || !strings.Contains(err.Error(), "another davit store") {
		t.Fatalf("another store attached this tier: %v", err)
	}
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(cold, moved); err != nil {
		t.Fatal(err)
	}
	if err := fresh.AttachCold(moved); err != nil {
		t.Fatalf("the same tier at a new path: %v", err)
	}
	if dir, _ := ColdRecorded(hot); dir != moved {
		t.Fatalf("record not updated: %q", dir)
	}
}

// TestPromoteReversesDemote: a promoted commit is hot again, and once
// the journal no longer lists it cold, collection drops the cold copy.
func TestPromoteReversesDemote(t *testing.T) {
	ctx := context.Background()
	s, hot, cold := tiered(t)
	want := committed(t, s)
	d, err := s.Demote(ctx, "c1", MoveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Release()
	if _, err := s.Collect(ctx, KeepSet{Commits: []CommitID{"c1"}, Cold: []CommitID{"c1"}}); err != nil {
		t.Fatal(err)
	}
	m, err := s.Promote(ctx, "c1", RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if m.Commit.Tier != domain.TierHot || !published(hot, "c1") || !published(cold, "c1") {
		t.Fatalf("after promote: %+v", m.Commit)
	}
	// Still listed cold and in flight: both stay.
	if _, err := s.Collect(ctx, KeepSet{Commits: []CommitID{"c1"}, Cold: []CommitID{"c1"}}); err != nil || !published(hot, "c1") {
		t.Fatalf("collect during the promote: %v", err)
	}
	m.Release()
	out, err := s.Collect(ctx, KeepSet{Commits: []CommitID{"c1"}})
	if err != nil || out.Moved != 1 || published(cold, "c1") {
		t.Fatalf("collect after the promote: %+v %v", out, err)
	}
	c, err := s.GetCommit(ctx, "c1")
	if err != nil || c.Tier != domain.TierHot || !bytes.Equal(readAll(t, c.Path), want) {
		t.Fatalf("promoted commit: %+v %v", c, err)
	}
	if again, err := s.Promote(ctx, "c1", RestoreOptions{}); err != nil || again.Commit.Tier != domain.TierHot {
		t.Fatalf("promote of a hot commit: %v", err)
	} else {
		again.Release()
	}
	assertNoPartial(t, hot)
}
