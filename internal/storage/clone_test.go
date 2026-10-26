package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"davit/internal/domain"
)

// apfsLike is a portable stand-in for APFS clonefile(2) that runs on any
// filesystem: it refuses an existing destination, copies src into a new file
// and, like APFS, makes the clone inherit src's permissions (so a clone of a
// 0444 commit starts read-only). failAfterProbe, when set, makes every clone
// after the first (the probe) fail with that error and leave a partial file,
// to check that the store neither silently copies nor leaks partial work.
type apfsLike struct {
	t              *testing.T
	calls          atomic.Int32
	failAfterProbe atomic.Pointer[error]
}

func (a *apfsLike) strategy() cloneStrategy {
	return cloneStrategy{name: "test-clonefile", clone: a.clone}
}

func (a *apfsLike) clone(src *os.File, dir, name string) error {
	path := filepath.Join(dir, name)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		a.t.Errorf("clone destination %s exists before clone (%v)", path, err)
		return os.ErrExist
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm()&0o077 != 0 {
		a.t.Errorf("clone dir %s is not private: %v %v", dir, fi.Mode(), err)
	}
	if n := a.calls.Add(1); n > 1 {
		if e := a.failAfterProbe.Load(); e != nil {
			os.WriteFile(path, []byte("partial"), 0o600)
			return *e
		}
	}
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, io.NewSectionReader(src, 0, fi.Size())); err != nil {
		dst.Close()
		os.Remove(path)
		return err
	}
	if err := dst.Chmod(fi.Mode().Perm()); err != nil { // inherit, as APFS does
		dst.Close()
		os.Remove(path)
		return err
	}
	return dst.Close()
}

func (a *apfsLike) failNext(err error) { a.failAfterProbe.Store(&err) }

func openAPFSLike(t *testing.T, mode Mode) (*FS, *apfsLike, string) {
	t.Helper()
	a := &apfsLike{t: t}
	root := t.TempDir()
	s, err := openWith(root, mode, a.strategy())
	if err != nil {
		t.Fatalf("openWith(%q): %v", mode, err)
	}
	return s, a, root
}

// The whole Store contract over the absent-destination clone path.
func TestStoreContractAbsentDestinationClone(t *testing.T) {
	for _, mode := range []Mode{ModeReflink, ""} {
		name := string(mode)
		if mode == "" {
			name = "auto"
		}
		t.Run(name, func(t *testing.T) {
			runContract(t, func(t *testing.T) (Store, string) {
				s, _, root := openAPFSLike(t, mode)
				p, _ := s.Probe(context.Background())
				if p.Mode != ModeReflink || !p.FastClone || p.Detail != "test-clonefile ok on "+fsName(filepath.Join(root, tmpDir)) {
					t.Fatalf("probe %+v", p)
				}
				return s, root
			})
		})
	}
}

func TestCloneReadOnlyCommitIntoPrivateWritable(t *testing.T) {
	ctx := context.Background()
	s, a, root := openAPFSLike(t, ModeReflink)
	base := writeBase(t, pattern('r', 64*1024))
	if _, err := s.CreateWritable(ctx, "w", base, mib); err != nil {
		t.Fatal(err)
	}
	c, err := s.Commit(ctx, "w", "c1")
	if err != nil {
		t.Fatal(err)
	}
	want := readAll(t, c.Path)
	if perm := filePerm(t, c.Path); perm != 0o444 {
		t.Fatalf("commit perm %o, want 0444", perm)
	}
	before := a.calls.Load()

	d, err := s.CloneCommitted(ctx, "c1", "fork", 8*mib)
	if err != nil {
		t.Fatal(err)
	}
	if a.calls.Load() != before+1 {
		t.Fatalf("CloneCommitted used the clone strategy %d times", a.calls.Load()-before)
	}
	if perm := filePerm(t, d.Path); perm != 0o600 {
		t.Fatalf("clone of a read-only commit has perm %o, want 0600", perm)
	}
	if d.SizeBytes != 8*mib || fileSize(t, d.Path) != 8*mib {
		t.Fatalf("clone not grown: %+v", d)
	}
	writeAt(t, d.Path, 0, []byte("fork-head"))
	writeAt(t, d.Path, 8*mib-4, []byte("tail"))
	if !bytes.Equal(readAll(t, c.Path), want) {
		t.Fatal("write to the private clone changed the commit")
	}
	if perm := filePerm(t, c.Path); perm != 0o444 {
		t.Fatalf("commit perm changed to %o", perm)
	}
	if got := readAll(t, d.Path); !bytes.Equal(got[9:mib], want[9:]) || !bytes.Equal(got[8*mib-4:], []byte("tail")) {
		t.Fatal("clone lost commit bytes or the grown tail")
	}
	// A commit of the fork ends read-only and leaves the fork writable.
	c2, err := s.Commit(ctx, "fork", "c2")
	if err != nil {
		t.Fatal(err)
	}
	if filePerm(t, c2.Path) != 0o444 || filePerm(t, d.Path) != 0o600 {
		t.Fatalf("perms after commit: commit %o disk %o", filePerm(t, c2.Path), filePerm(t, d.Path))
	}
	assertNoPartial(t, root)
}

// After a successful probe, a clone error fails the operation: no silent copy,
// no published object and no partial work left behind.
func TestCloneErrorAfterProbeFailsOperation(t *testing.T) {
	ctx := context.Background()
	s, a, root := openAPFSLike(t, ModeReflink)
	base := writeBase(t, pattern('e', 8192))
	if _, err := s.CreateWritable(ctx, "w", base, mib); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, "w", "c1"); err != nil {
		t.Fatal(err)
	}
	a.failNext(syscall.ENOSPC)

	_, e1 := s.CreateWritable(ctx, "d2", base, mib)
	_, e2 := s.CloneCommitted(ctx, "c1", "d3", mib)
	_, e3 := s.Commit(ctx, "w", "c2")
	for i, err := range []error{e1, e2, e3} {
		if !errors.Is(err, syscall.ENOSPC) || !strings.Contains(err.Error(), "test-clonefile") {
			t.Errorf("op %d: got %v, want test-clonefile ENOSPC", i, err)
		}
	}
	for _, id := range []DiskID{"d2", "d3"} {
		if _, err := s.OpenForRuntime(ctx, id); !domain.IsCode(err, domain.CodeNotFound) {
			t.Errorf("disk %s published after failed clone: %v", id, err)
		}
	}
	if _, err := s.GetCommit(ctx, "c2"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Errorf("commit published after failed clone: %v", err)
	}
	assertNoPartial(t, root)
}

// Only a cross-filesystem base import may copy; clones inside the store never.
func TestCrossFSOnlyBaseImportCopies(t *testing.T) {
	ctx := context.Background()
	s, a, root := openAPFSLike(t, ModeReflink)
	base := writeBase(t, pattern('x', 8192))
	if _, err := s.CreateWritable(ctx, "w", base, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(ctx, "w", "c1"); err != nil {
		t.Fatal(err)
	}
	a.failNext(syscall.EXDEV)

	d, err := s.CreateWritable(ctx, "imported", base, mib)
	if err != nil {
		t.Fatalf("cross-volume base import: %v", err)
	}
	if !bytes.Equal(readN(t, d.Path, 8192), pattern('x', 8192)) || d.SizeBytes != mib || filePerm(t, d.Path) != 0o600 {
		t.Fatalf("imported disk wrong: %+v perm %o", d, filePerm(t, d.Path))
	}
	if _, err := s.CloneCommitted(ctx, "c1", "d2", 0); !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("CloneCommitted EXDEV: got %v, want failure", err)
	}
	if _, err := s.Commit(ctx, "w", "c2"); !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("Commit EXDEV: got %v, want failure", err)
	}
	assertNoPartial(t, root)
}

func TestProbeWithStrategy(t *testing.T) {
	failing := cloneStrategy{name: "test-clonefile", clone: func(*os.File, string, string) error { return syscall.ENOTSUP }}
	dir := t.TempDir()

	p, err := probeDir(dir, "", failing)
	if err != nil || p.Mode != ModeCopy || p.FastClone || !strings.Contains(p.Detail, "test-clonefile failed on") || !strings.Contains(p.Detail, "using copy") {
		t.Fatalf("auto with failing clone: %+v %v", p, err)
	}
	if _, err := probeDir(dir, ModeReflink, failing); !domain.IsCode(err, domain.CodeUnsupportedCapability) || !strings.Contains(err.Error(), "test-clonefile") {
		t.Fatalf("forced reflink with failing clone: %v", err)
	}
	called := false
	spy := cloneStrategy{name: "spy", clone: func(*os.File, string, string) error { called = true; return nil }}
	if p, err := probeDir(dir, ModeCopy, spy); err != nil || p.Mode != ModeCopy || called {
		t.Fatalf("forced copy: %+v %v called=%v", p, err, called)
	}

	// A "clone" that shares the source inode is not independent.
	shared := cloneStrategy{name: "hardlink", clone: func(src *os.File, dir, name string) error {
		return os.Link(src.Name(), filepath.Join(dir, name))
	}}
	if p, _ := probeDir(dir, "", shared); p.FastClone || !strings.Contains(p.Detail, "changed the source") {
		t.Fatalf("shared-inode clone passed the probe: %+v", p)
	}
	// A symlink in place of the clone is refused.
	link := cloneStrategy{name: "symlink", clone: func(src *os.File, dir, name string) error {
		return os.Symlink(src.Name(), filepath.Join(dir, name))
	}}
	if p, _ := probeDir(dir, "", link); p.FastClone || !strings.Contains(p.Detail, "not a regular file") {
		t.Fatalf("symlink clone passed the probe: %+v", p)
	}
	if ents, err := os.ReadDir(dir); err != nil || len(ents) != 0 {
		t.Fatalf("probe left files: %v %v", ents, err)
	}
}

// The real platform strategy honours the absent-destination contract where
// the host filesystem can clone (skipped elsewhere, e.g. ext4/tmpfs).
func TestPlatformCloneAbsentDestination(t *testing.T) {
	dir := t.TempDir()
	if err := probeClone(dir, platformClone); err != nil {
		t.Skipf("%s unsupported on %s: %v", platformClone.name, fsName(dir), err)
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, pattern('p', 3*4096), 0o444); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := platformClone.clone(in, dir, "dst"); err != nil {
		t.Fatal(err)
	}
	if err := platformClone.clone(in, dir, "dst"); err == nil {
		t.Fatal("clone onto an existing destination succeeded")
	}
	out, err := openPrivate(filepath.Join(dir, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := out.Truncate(64 * mib); err != nil {
		t.Fatal(err)
	}
	if _, err := out.WriteAt([]byte("mine"), 4096); err != nil {
		t.Fatal(err)
	}
	if err := fullSync(out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readAll(t, src), pattern('p', 3*4096)) || filePerm(t, src) != 0o444 {
		t.Fatal("source changed")
	}
	if filePerm(t, out.Name()) != 0o600 || fileSize(t, out.Name()) != 64*mib {
		t.Fatalf("clone perm %o size %d", filePerm(t, out.Name()), fileSize(t, out.Name()))
	}
}

func filePerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func assertNoPartial(t *testing.T, root string) {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(root, tmpDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("partial work left in tmp: %v", ents)
	}
}
