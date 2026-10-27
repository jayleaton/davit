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

	"davit/internal/domain"
)

const mib = 1 << 20

// newStoreFunc opens a fresh store and returns it with its root directory so
// tests can plant partial work.
type newStoreFunc func(t *testing.T) (Store, string)

func TestStoreContract(t *testing.T) {
	for _, mode := range []Mode{ModeCopy, ""} {
		name := string(mode)
		if mode == "" {
			name = "auto"
		}
		t.Run(name, func(t *testing.T) {
			runContract(t, func(t *testing.T) (Store, string) {
				root := t.TempDir()
				s, err := Open(root, mode)
				if err != nil {
					t.Fatalf("Open(%q): %v", mode, err)
				}
				p, _ := s.Probe(context.Background())
				t.Logf("probe: mode=%s fastClone=%v detail=%q", p.Mode, p.FastClone, p.Detail)
				return s, root
			})
		})
	}
	// A cold tier (tier.go) changes nothing about the contract.
	t.Run("with cold tier", func(t *testing.T) {
		runContract(t, func(t *testing.T) (Store, string) {
			s, root, _ := tiered(t)
			return s, root
		})
	})
}

// runContract checks the Store contract against any implementation.
func runContract(t *testing.T, newStore newStoreFunc) {
	ctx := context.Background()

	t.Run("probe", func(t *testing.T) {
		s, _ := newStore(t)
		p, err := s.Probe(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if p.Mode != ModeReflink && p.Mode != ModeCopy {
			t.Fatalf("mode %q", p.Mode)
		}
		if p.FastClone != (p.Mode == ModeReflink) || p.Detail == "" {
			t.Fatalf("inconsistent probe %+v", p)
		}
	})

	t.Run("create idempotent and grow not shrink", func(t *testing.T) {
		s, _ := newStore(t)
		base := writeBase(t, pattern('b', 8192))

		d, err := s.CreateWritable(ctx, "d1", base, 2*mib)
		if err != nil {
			t.Fatal(err)
		}
		if d.SizeBytes != 2*mib || fileSize(t, d.Path) != 2*mib {
			t.Fatalf("size %d, want %d", d.SizeBytes, 2*mib)
		}
		if got := readN(t, d.Path, 8192); !bytes.Equal(got, pattern('b', 8192)) {
			t.Fatal("disk does not start with base contents")
		}
		writeAt(t, d.Path, 0, []byte("mutated"))
		again, err := s.CreateWritable(ctx, "d1", base, 4*mib)
		if err != nil {
			t.Fatal(err)
		}
		if again != d || !bytes.HasPrefix(readN(t, d.Path, 7), []byte("mutated")) {
			t.Fatalf("re-create changed disk: %+v vs %+v", again, d)
		}

		// Base larger than requested: keep base size.
		small, err := s.CreateWritable(ctx, "d2", base, 100)
		if err != nil {
			t.Fatal(err)
		}
		if small.SizeBytes != 8192 {
			t.Fatalf("shrunk to %d", small.SizeBytes)
		}

		empty, err := s.CreateWritable(ctx, "empty", "", 3*mib)
		if err != nil {
			t.Fatal(err)
		}
		if empty.SizeBytes != 3*mib || !bytes.Equal(readN(t, empty.Path, 4096), make([]byte, 4096)) {
			t.Fatalf("empty disk %+v", empty)
		}
	})

	t.Run("commit immutability and clone isolation", func(t *testing.T) {
		s, _ := newStore(t)
		base := writeBase(t, pattern('x', 16384))
		d, err := s.CreateWritable(ctx, "w", base, mib)
		if err != nil {
			t.Fatal(err)
		}
		writeAt(t, d.Path, 512*1024, []byte("deep-write"))
		want := readAll(t, d.Path)

		c, err := s.Commit(ctx, "w", "c1")
		if err != nil {
			t.Fatal(err)
		}
		if c.SizeBytes != mib || !strings.HasPrefix(c.Digest, "sha256:") || c.CreatedAt.IsZero() {
			t.Fatalf("commit %+v", c)
		}
		// Writing the source disk after commit must not change the commit.
		writeAt(t, d.Path, 0, []byte("after-commit"))
		if !bytes.Equal(readAll(t, c.Path), want) {
			t.Fatal("commit changed when source disk was written")
		}
		if fi, err := os.Stat(c.Path); err != nil || fi.Mode().Perm()&0o222 != 0 {
			t.Fatalf("commit data is writable: %v %v", fi.Mode(), err)
		}

		// Idempotent: same commit back, even though the disk has changed.
		c2, err := s.Commit(ctx, "w", "c1")
		if err != nil {
			t.Fatal(err)
		}
		if c2 != c || !bytes.Equal(readAll(t, c.Path), want) {
			t.Fatalf("re-commit changed commit: %+v vs %+v", c2, c)
		}
		got, err := s.GetCommit(ctx, "c1")
		if err != nil || got != c {
			t.Fatalf("GetCommit = %+v, %v", got, err)
		}

		a, err := s.CloneCommitted(ctx, "c1", "a", 2*mib)
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.CloneCommitted(ctx, "c1", "b", 0)
		if err != nil {
			t.Fatal(err)
		}
		if a.SizeBytes != 2*mib || b.SizeBytes != mib {
			t.Fatalf("clone sizes %d %d", a.SizeBytes, b.SizeBytes)
		}
		if !bytes.Equal(readN(t, a.Path, mib), want) || !bytes.Equal(readAll(t, b.Path), want) {
			t.Fatal("clone does not see commit bytes")
		}
		writeAt(t, a.Path, 100, []byte("only-in-a"))
		if !bytes.Equal(readAll(t, b.Path), want) || !bytes.Equal(readAll(t, c.Path), want) {
			t.Fatal("write to one clone leaked into sibling or commit")
		}
		a2, err := s.CloneCommitted(ctx, "c1", "a", 4*mib)
		if err != nil || a2 != a || !bytes.Equal(readN(t, a.Path, 109)[100:], []byte("only-in-a")) {
			t.Fatalf("re-clone changed disk: %+v %v", a2, err)
		}
	})

	t.Run("not found", func(t *testing.T) {
		s, _ := newStore(t)
		checks := map[string]error{}
		_, checks["GetCommit"] = s.GetCommit(ctx, "nope")
		_, checks["CloneCommitted"] = s.CloneCommitted(ctx, "nope", "d", mib)
		_, checks["Commit"] = s.Commit(ctx, "nope", "c")
		_, checks["OpenForRuntime"] = s.OpenForRuntime(ctx, "nope")
		_, checks["CreateWritable"] = s.CreateWritable(ctx, "d", filepath.Join(t.TempDir(), "missing"), mib)
		for op, err := range checks {
			var de *domain.Error
			if !asDomain(err, &de) || de.Code != domain.CodeNotFound {
				t.Errorf("%s: got %v, want *domain.Error not_found", op, err)
			}
		}
		if err := s.Release(ctx, Lease{ID: "unknown"}); err != nil {
			t.Errorf("Release unknown: %v", err)
		}
	})

	t.Run("open commit survives collect", func(t *testing.T) {
		s, _ := newStore(t)
		base := writeBase(t, pattern('o', 3*mib))
		if _, err := s.CreateWritable(ctx, "d1", base, 3*mib); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Commit(ctx, "d1", "c1"); err != nil {
			t.Fatal(err)
		}
		cf, err := s.OpenCommit(ctx, "c1")
		if err != nil {
			t.Fatal(err)
		}
		defer cf.Close()
		if cf.Commit.ID != "c1" || cf.Commit.SizeBytes != 3*mib {
			t.Fatalf("commit %+v", cf.Commit)
		}
		head := make([]byte, mib)
		if _, err := cf.ReadAt(head, 0); err != nil || !bytes.Equal(head, pattern('o', mib)) {
			t.Fatalf("read before collect: %v", err)
		}
		// Nothing keeps c1: collect removes it while it is open.
		if got, err := s.Collect(ctx, KeepSet{}); err != nil || !slices.Contains(got.Commits, "c1") {
			t.Fatalf("collect: %+v %v", got, err)
		}
		if _, err := s.GetCommit(ctx, "c1"); !domain.IsCode(err, domain.CodeNotFound) {
			t.Fatalf("collected commit still published: %v", err)
		}
		rest := make([]byte, 2*mib)
		if _, err := cf.ReadAt(rest, mib); err != nil || !bytes.Equal(rest, pattern('o', 3*mib)[mib:]) {
			t.Fatalf("open commit unreadable after collect: %v", err)
		}
		if _, err := s.OpenCommit(ctx, "c1"); !domain.IsCode(err, domain.CodeNotFound) {
			t.Fatalf("open of a collected commit: %v", err)
		}
	})

	t.Run("invalid ids", func(t *testing.T) {
		s, _ := newStore(t)
		for _, id := range []string{"", ".", "..", "a/b", `a\b`, "../x", "a..b", "sp ace", "ü", strings.Repeat("a", 129)} {
			_, e1 := s.CreateWritable(ctx, DiskID(id), "", mib)
			_, e2 := s.GetCommit(ctx, CommitID(id))
			_, e3 := s.Commit(ctx, "ok", CommitID(id))
			_, e4 := s.CloneCommitted(ctx, CommitID(id), "ok", mib)
			for i, err := range []error{e1, e2, e3, e4} {
				if !domain.IsCode(err, domain.CodeInvalidArgument) {
					t.Errorf("id %q call %d: got %v, want invalid_argument", id, i, err)
				}
			}
		}
		if _, err := s.CreateWritable(ctx, DiskID(strings.Repeat("a", 128)), "", mib); err != nil {
			t.Errorf("128-char id rejected: %v", err)
		}
	})

	t.Run("collect", func(t *testing.T) {
		s, root := newStore(t)
		for _, id := range []DiskID{"keep", "leased", "drop"} {
			if _, err := s.CreateWritable(ctx, id, "", mib); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range []CommitID{"ckeep", "cdrop"} {
			if _, err := s.Commit(ctx, "keep", id); err != nil {
				t.Fatal(err)
			}
		}
		lease, err := s.OpenForRuntime(ctx, "leased")
		if err != nil || lease.Disk != "leased" || lease.Path == "" {
			t.Fatalf("lease %+v %v", lease, err)
		}
		// Partial work: a stray tmp file and a commit dir with no manifest.
		if err := os.WriteFile(filepath.Join(root, "tmp", "half.img"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "commits", "half"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetCommit(ctx, "half"); !domain.IsCode(err, domain.CodeNotFound) {
			t.Fatalf("partial commit visible: %v", err)
		}

		got, err := s.Collect(ctx, KeepSet{Disks: []DiskID{"keep"}, Commits: []CommitID{"ckeep"}})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Disks, []DiskID{"drop"}) || !slices.Equal(got.Commits, []CommitID{"cdrop"}) || got.Partial != 2 {
			t.Fatalf("collected %+v", got)
		}
		if _, err := s.GetCommit(ctx, "ckeep"); err != nil {
			t.Fatalf("kept commit gone: %v", err)
		}
		if _, err := s.GetCommit(ctx, "cdrop"); !domain.IsCode(err, domain.CodeNotFound) {
			t.Fatalf("dropped commit still there: %v", err)
		}
		if _, err := s.OpenForRuntime(ctx, "drop"); !domain.IsCode(err, domain.CodeNotFound) {
			t.Fatalf("dropped disk still there: %v", err)
		}

		// Once released, the leased disk is collectable.
		if err := s.Release(ctx, lease); err != nil {
			t.Fatal(err)
		}
		got, err = s.Collect(ctx, KeepSet{Disks: []DiskID{"keep"}, Commits: []CommitID{"ckeep"}})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Disks, []DiskID{"leased"}) || len(got.Commits) != 0 || got.Partial != 0 {
			t.Fatalf("second collect %+v", got)
		}
	})
}

func TestOpenModes(t *testing.T) {
	if _, err := Open(t.TempDir(), "zfs"); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("unknown mode: %v", err)
	}
	s, err := Open(t.TempDir(), ModeCopy)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Probe(context.Background()); p.Mode != ModeCopy || p.FastClone || !strings.Contains(p.Detail, "forced") {
		t.Fatalf("forced copy probe %+v", p)
	}

	t.Run("explicit reflink unsupported", func(t *testing.T) {
		dir := t.TempDir()
		auto, err := Open(dir, "")
		if err != nil {
			t.Fatal(err)
		}
		p, _ := auto.Probe(context.Background())
		t.Logf("probe on %s: %+v", dir, p)
		if p.FastClone {
			t.Skip("filesystem supports reflink; cannot test the unsupported path here")
		}
		if _, err := Open(dir, ModeReflink); !domain.IsCode(err, domain.CodeUnsupportedCapability) {
			t.Fatalf("Open(reflink) on non-reflink fs: %v, want unsupported_capability", err)
		}
	})
}

func TestSparseCopyKeepsLengthAndBytes(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	// Data, a hole, data, then a trailing hole that must survive as length.
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("head"), 0)
	f.WriteAt([]byte("tail"), 300*1024+7)
	f.Truncate(mib + 3)
	f.Close()

	in, _ := os.Open(src)
	defer in.Close()
	out, _ := os.Create(filepath.Join(dir, "dst"))
	if err := sparseCopy(context.Background(), out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
	if !bytes.Equal(readAll(t, src), readAll(t, filepath.Join(dir, "dst"))) {
		t.Fatal("copy differs")
	}
}

func asDomain(err error, de **domain.Error) bool { return errors.As(err, de) }

func pattern(c byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = c + byte(i%7)
	}
	return b
}

func writeBase(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rootfs.img")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeAt(t *testing.T, path string, off int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readN(t *testing.T, path string, n int) []byte {
	t.Helper()
	b := readAll(t, path)
	if len(b) < n {
		t.Fatalf("%s: %d bytes, want >= %d", path, len(b), n)
	}
	return b[:n]
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
