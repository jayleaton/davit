//go:build darwin

package storage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// apfsDir returns a temp dir where clonefile works, or skips.
func apfsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := probeClone(dir, platformClone); err != nil {
		t.Skipf("clonefile unsupported on %s (%s): %v", dir, fsName(dir), err)
	}
	return dir
}

func TestStoreContractClonefile(t *testing.T) {
	runContract(t, func(t *testing.T) (Store, string) {
		root := apfsDir(t)
		s, err := Open(root, ModeReflink)
		if err != nil {
			t.Fatalf("Open(reflink): %v", err)
		}
		p, _ := s.Probe(context.Background())
		if p.Mode != ModeReflink || !p.FastClone || !strings.HasPrefix(p.Detail, "clonefile ok on ") {
			t.Fatalf("probe %+v", p)
		}
		return s, root
	})
}

func TestClonefileAbsentDestinationAndIsolation(t *testing.T) {
	dir := apfsDir(t)
	src := filepath.Join(dir, "src")
	want := pattern('a', 5*4096)
	if err := os.WriteFile(src, want, 0o444); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()

	if err := clonefileInto(in, dir, "dst"); err != nil {
		t.Fatalf("clonefile: %v", err)
	}
	// clonefile never overwrites: an existing destination is EEXIST.
	if err := clonefileInto(in, dir, "dst"); !errors.Is(err, unix.EEXIST) {
		t.Fatalf("clone onto existing destination: %v, want EEXIST", err)
	}
	// A missing destination directory fails and creates nothing.
	if err := clonefileInto(in, filepath.Join(dir, "missing"), "dst"); err == nil {
		t.Fatal("clone into missing dir succeeded")
	}
	// The clone inherits 0444; openPrivate makes it 0600, source untouched.
	if perm := filePerm(t, filepath.Join(dir, "dst")); perm != 0o444 {
		t.Logf("clone perm before fix-up %o (APFS normally inherits 0444)", perm)
	}
	out, err := openPrivate(filepath.Join(dir, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if filePerm(t, out.Name()) != 0o600 || filePerm(t, src) != 0o444 {
		t.Fatalf("perms clone %o source %o", filePerm(t, out.Name()), filePerm(t, src))
	}
	if _, err := out.WriteAt([]byte("isolated"), 4096); err != nil {
		t.Fatal(err)
	}
	if err := fullSync(out); err != nil {
		t.Fatalf("F_FULLFSYNC: %v", err)
	}
	if !bytes.Equal(readAll(t, src), want) {
		t.Fatal("write to clone changed the source")
	}
}

func TestClonefileSparseGrowth(t *testing.T) {
	dir := apfsDir(t)
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, pattern('s', 64*1024), 0o444); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := clonefileInto(in, dir, "dst"); err != nil {
		t.Fatal(err)
	}
	out, err := openPrivate(filepath.Join(dir, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	const grown = 1 << 30
	if err := out.Truncate(grown); err != nil {
		t.Fatal(err)
	}
	if err := fullSync(out); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(out.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != grown || st.Blocks*512 >= grown/2 {
		t.Fatalf("grown clone size %d allocated %d: not sparse", st.Size, st.Blocks*512)
	}
}

// A clone of a file with an owner immutable flag inherits it; openPrivate
// clears it on the clone only.
func TestClonefileClearsInheritedUserFlags(t *testing.T) {
	dir := apfsDir(t)
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, pattern('f', 8192), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chflags(src, unix.UF_IMMUTABLE); err != nil {
		t.Skipf("chflags: %v", err)
	}
	t.Cleanup(func() { unix.Chflags(src, 0) })
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := clonefileInto(in, dir, "dst"); err != nil {
		t.Fatal(err)
	}
	out, err := openPrivate(filepath.Join(dir, "dst"))
	if err != nil {
		t.Fatalf("openPrivate with inherited UF_IMMUTABLE: %v", err)
	}
	defer out.Close()
	if _, err := out.WriteAt([]byte("w"), 0); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Stat(src, &st); err != nil || st.Flags&unix.UF_IMMUTABLE == 0 {
		t.Fatalf("source lost UF_IMMUTABLE: flags 0x%x %v", st.Flags, err)
	}
}

func TestFullSyncDirectory(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir: %v", err)
	}
}
