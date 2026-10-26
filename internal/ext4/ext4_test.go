package ext4_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"davit/internal/ext4"
	"davit/internal/ext4/ext4test"
)

// tree builds the source tree shared by the tests and returns its root.
func tree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	w := func(p string, b []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, p), b, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	w("hello.txt", []byte("hello\n"))
	w("empty", nil)
	w("a/b/c/deep.txt", []byte("deep"))
	big := make([]byte, 3<<20+123) // multi-block, not block aligned
	for i := range big {
		big[i] = byte(i*7 + i>>9)
	}
	w("a/big.bin", big)
	// Sparse: data islands between holes, so the file needs many extents
	// (an extent tree deeper than the inode's four slots).
	sp, err := os.Create(filepath.Join(src, "sparse.bin"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		if _, err := sp.WriteAt(bytes.Repeat([]byte{byte('A' + i)}, 5000), int64(i)*(1<<20)); err != nil {
			t.Fatal(err)
		}
	}
	sp.Truncate(16 << 20)
	sp.Close()
	for _, l := range [][2]string{
		{"rel", "hello.txt"},
		{"a/up", "../hello.txt"},
		{"a/abs", "/a/b/c/deep.txt"},
		{"a/escape", "../../../../../../etc/passwd"},
		{"a/dirlink", "b/c"},
		{"loop1", "loop2"},
		{"loop2", "loop1"},
		{"long", "/" + string(bytes.Repeat([]byte("x"), 100))},
	} {
		if err := os.Symlink(l[1], filepath.Join(src, l[0])); err != nil {
			t.Fatal(err)
		}
	}
	os.Mkdir(filepath.Join(src, "x"+string(bytes.Repeat([]byte("x"), 99))), 0o755)
	if err := os.Mkdir(filepath.Join(src, "many"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 3000 { // several blocks: a hashed (dir_index) directory
		w(fmt.Sprintf("many/entry-with-a-longish-name-%05d", i), []byte(fmt.Sprint(i)))
	}
	return src
}

func open(t *testing.T, img string) *ext4.FS {
	t.Helper()
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	st, _ := f.Stat()
	fsys, err := ext4.Open(f, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

func read(t *testing.T, fsys *ext4.FS, p string) []byte {
	t.Helper()
	in, _, err := fsys.Resolve(p, true)
	if err != nil {
		t.Fatalf("resolve %s: %v", p, err)
	}
	b, err := io.ReadAll(fsys.NewReader(in))
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return b
}

func TestReadsWhatMkfsWrote(t *testing.T) {
	src := tree(t)
	for _, c := range []struct{ name, mkfs string }{
		{"ext4", "mkfs.ext4"},
		{"ext4-1k", "mkfs.ext4 -b 1024"},
		{"ext2-blockmap", "mkfs.ext2 -b 1024"}, // indirect blocks, double indirect for big.bin
	} {
		t.Run(c.name, func(t *testing.T) {
			fsys := open(t, ext4test.Image(t, src, 64, c.mkfs))
			// Every regular file and symlink matches the source.
			err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
				if err != nil || p == src {
					return err
				}
				rel := "/" + filepath.ToSlash(p[len(src)+1:])
				in, canon, err := fsys.Resolve(rel, false)
				if err != nil {
					return fmt.Errorf("resolve %s: %w", rel, err)
				}
				if canon != rel {
					return fmt.Errorf("canonical %q, want %q", canon, rel)
				}
				switch {
				case d.Type()&fs.ModeSymlink != 0:
					want, _ := os.Readlink(p)
					if got, err := fsys.Readlink(in); err != nil || got != want {
						return fmt.Errorf("readlink %s = %q %v, want %q", rel, got, err, want)
					}
				case d.Type().IsRegular():
					want, _ := os.ReadFile(p)
					got, err := io.ReadAll(fsys.NewReader(in))
					if err != nil || !bytes.Equal(got, want) || in.Size != int64(len(want)) {
						return fmt.Errorf("%s: %d bytes %v, want %d (sha %x vs %x)", rel, len(got), err, len(want), sha256.Sum256(got), sha256.Sum256(want))
					}
					st, _ := d.Info()
					if in.FileMode().Perm() != st.Mode().Perm() || in.Type() != ext4.TypeFile {
						return fmt.Errorf("%s: mode %v", rel, in.FileMode())
					}
				case d.IsDir():
					if in.Type() != ext4.TypeDir {
						return fmt.Errorf("%s: type %v", rel, in.Type())
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolveStaysInside(t *testing.T) {
	fsys := open(t, ext4test.Image(t, tree(t), 64, "mkfs.ext4"))
	for p, want := range map[string]string{
		"/rel":                       "hello\n",
		"/a/up":                      "hello\n",
		"/a/abs":                     "deep",
		"/a/dirlink/deep.txt":        "deep",
		"/../../hello.txt":           "hello\n", // ".." stops at the root
		"a/../a/./b/../b/c/deep.txt": "deep",
	} {
		if got := string(read(t, fsys, p)); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	// An escaping symlink resolves inside the image, where /etc/passwd does
	// not exist: never the host's file.
	if _, _, err := fsys.Resolve("/a/escape", true); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("escape: %v, want not exist", err)
	}
	if _, canon, err := fsys.Resolve("/a/dirlink/../c", true); err != nil || canon != "/a/b/c" {
		t.Errorf("dirlink/..: %q %v", canon, err)
	}
	if _, _, err := fsys.Resolve("/loop1", true); !errors.Is(err, ext4.ErrLoop) {
		t.Errorf("loop: %v", err)
	}
	if in, canon, err := fsys.Resolve("/loop1", false); err != nil || in.Type() != ext4.TypeSymlink || canon != "/loop1" {
		t.Errorf("lstat loop1: %v %q %v", in, canon, err)
	}
	if _, _, err := fsys.Resolve("/hello.txt/x", true); !errors.Is(err, ext4.ErrNotDir) {
		t.Errorf("file as dir: %v", err)
	}
	if _, _, err := fsys.Resolve("/nope", true); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
}

func TestReadDirPages(t *testing.T) {
	fsys := open(t, ext4test.Image(t, tree(t), 64, "mkfs.ext4"))
	dir, _, err := fsys.Resolve("/many", true)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	off := int64(0)
	for pages := 0; off >= 0; pages++ {
		if pages > 100 {
			t.Fatal("no end")
		}
		es, next, err := fsys.ReadDir(dir, off, 97)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			names = append(names, e.Name)
		}
		off = next
	}
	if len(names) != 3000 {
		t.Fatalf("%d entries, want 3000", len(names))
	}
	sort.Strings(names)
	for i, n := range names {
		if n != fmt.Sprintf("entry-with-a-longish-name-%05d", i) {
			t.Fatalf("entry %d = %q", i, n)
		}
	}
	if _, _, err := fsys.ReadDir(dir, 5, 10); !errors.Is(err, ext4.ErrBadOffset) {
		t.Errorf("offset inside an entry: %v", err)
	}
}

// TestCorruptImagesFailCleanly flips bytes in the metadata of a valid image:
// every operation either works or returns an error, never panics or hangs.
func TestCorruptImagesFailCleanly(t *testing.T) {
	good, err := os.ReadFile(ext4test.Image(t, tree(t), 16, "mkfs.ext4 -b 1024"))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 120 {
		img := bytes.Clone(good)
		for range 1 + rng.IntN(20) {
			// Superblock, descriptors, inode tables and early directory
			// blocks live in the first few hundred KiB.
			img[rng.IntN(512<<10)] ^= byte(1 + rng.IntN(255))
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d: panic %v", i, r)
				}
			}()
			fsys, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				return
			}
			walk(fsys, "/", 0)
		}()
	}
}

// walk reads everything reachable, ignoring errors, bounded in depth.
func walk(fsys *ext4.FS, p string, depth int) {
	in, _, err := fsys.Resolve(p, false)
	if err != nil || depth > 8 {
		return
	}
	switch in.Type() {
	case ext4.TypeDir:
		es, _, _ := fsys.ReadDir(in, 0, 1000)
		for _, e := range es {
			walk(fsys, p+"/"+e.Name, depth+1)
		}
	case ext4.TypeFile:
		io.Copy(io.Discard, io.LimitReader(fsys.NewReader(in), 64<<20))
	case ext4.TypeSymlink:
		fsys.Readlink(in)
	}
}

func TestNotExt(t *testing.T) {
	if _, err := ext4.Open(bytes.NewReader(make([]byte, 1<<20)), 1<<20); !errors.Is(err, ext4.ErrCorrupt) {
		t.Fatalf("zeros: %v", err)
	}
}
