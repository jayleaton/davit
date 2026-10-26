package snapfiles_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/ext4/ext4test"
	"davit/internal/snapfiles"
	"davit/internal/storage"
	"davit/internal/tarx"
)

const owner = domain.PrincipalID("owner")

// snaps is a snapshot table that can lose entries (DeleteSnapshot).
type snaps struct {
	mu sync.Mutex
	m  map[string]*domain.Snapshot
}

func (s *snaps) GetSnapshot(_ context.Context, p domain.PrincipalID, ref string) (*domain.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sn := s.m[ref]; sn != nil && sn.Owner == p {
		return sn, nil
	}
	return nil, domain.Errorf(domain.CodeNotFound, "snapshot %s not found", ref)
}

func (s *snaps) delete(ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, ref)
}

type fixture struct {
	svc   *snapfiles.Service
	snaps *snaps
	store storage.Store
	src   string // the tree the snapshot's filesystem was made from
	img   string
}

// setup commits an ext4 image of a small tree (plus big, a file of that
// many bytes) as snapshot "snap".
func setup(t *testing.T, big int64) *fixture {
	t.Helper()
	src := t.TempDir()
	w := func(p string, b []byte, mode os.FileMode) {
		os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o755)
		if err := os.WriteFile(filepath.Join(src, p), b, mode); err != nil {
			t.Fatal(err)
		}
	}
	w("home/u/hello.txt", []byte("hello\n"), 0o644)
	w("home/u/proj/main.go", []byte("package main\n"), 0o600)
	w("home/u/proj/sub/x.bin", bytes.Repeat([]byte{7}, 70000), 0o755)
	w("etc/hostname", []byte("snap\n"), 0o644)
	for l, target := range map[string]string{
		"home/u/proj/escape": "../../../../../../../etc/hostname",
		"home/u/proj/abs":    "/etc/hostname",
		"home/u/link":        "proj",
	} {
		if err := os.Symlink(target, filepath.Join(src, l)); err != nil {
			t.Fatal(err)
		}
	}
	size := 64
	if big > 0 {
		// Sparse with data at both ends: large, but cheap to build.
		f, err := os.Create(filepath.Join(src, "big.bin"))
		if err != nil {
			t.Fatal(err)
		}
		f.WriteAt(bytes.Repeat([]byte("head"), 1024), 0)
		f.WriteAt(bytes.Repeat([]byte("tail"), 1024), big-4096)
		f.Close()
		size += int(big >> 20)
	}
	img := ext4test.Image(t, src, size, "mkfs.ext4")
	ctx := context.Background()
	store, err := storage.Open(t.TempDir(), storage.ModeCopy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWritable(ctx, "d1", img, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(ctx, "d1", "c1"); err != nil {
		t.Fatal(err)
	}
	sn := &snaps{m: map[string]*domain.Snapshot{
		"snap": {ID: "snap", Owner: owner, Kind: domain.SnapshotKindDisk, CommitID: "c1"},
	}}
	return &fixture{svc: snapfiles.New(sn, store), snaps: sn, store: store, src: src, img: img}
}

// read reads p (or archives it) into memory, checking the digest.
func (f *fixture) read(t *testing.T, p string, archive bool) ([]byte, snapfiles.Head, error) {
	t.Helper()
	var buf bytes.Buffer
	var head snapfiles.Head
	sum, err := f.svc.ReadFile(context.Background(), owner, "snap", p, archive, func(h snapfiles.Head) (io.Writer, error) {
		head = h
		return &buf, nil
	})
	if err == nil {
		got := sha256.Sum256(buf.Bytes())
		if sum != hex.EncodeToString(got[:]) || (head.SHA256 != "" && head.SHA256 != sum) {
			t.Fatalf("%s: digest %s / head %s, body %x", p, sum, head.SHA256, got)
		}
	}
	return buf.Bytes(), head, err
}

func code(err error) domain.Code {
	if de := domain.AsError(err); de != nil {
		return de.Code
	}
	return ""
}

func TestTreePagesAndBindsCursor(t *testing.T) {
	f := setup(t, 0)
	ctx := context.Background()
	var all []api.SnapshotEntry
	cur := ""
	for {
		page, err := f.svc.Tree(ctx, owner, "snap", "/home/u/link", 2, cur) // a symlink to proj: followed
		if err != nil {
			t.Fatal(err)
		}
		if page.Path != "/home/u/proj" || len(page.Items) > 2 {
			t.Fatalf("page %+v", page)
		}
		all = append(all, page.Items...)
		if cur = page.NextCursor; cur == "" {
			break
		}
	}
	byName := map[string]api.SnapshotEntry{}
	for _, e := range all {
		byName[filepath.Base(e.Path)] = e
	}
	if len(all) != 4 || byName["main.go"].Mode != "0600" || byName["main.go"].SizeBytes != 13 || byName["main.go"].Type != "file" ||
		byName["sub"].Type != "dir" || byName["escape"].Type != "symlink" || byName["escape"].LinkTarget != "../../../../../../../etc/hostname" ||
		byName["abs"].Path != "/home/u/proj/abs" || byName["main.go"].ModifiedAt.IsZero() {
		t.Fatalf("entries %+v", all)
	}
	// A cursor belongs to its snapshot and directory.
	page, _ := f.svc.Tree(ctx, owner, "snap", "/home/u/proj", 1, "")
	if _, err := f.svc.Tree(ctx, owner, "snap", "/home/u", 1, page.NextCursor); code(err) != domain.CodeInvalidArgument {
		t.Fatalf("cursor on another path: %v", err)
	}
	if _, err := f.svc.Tree(ctx, owner, "snap", "/home/u/proj", 1, "garbage"); code(err) != domain.CodeInvalidArgument {
		t.Fatalf("garbage cursor: %v", err)
	}
	for p, want := range map[string]domain.Code{
		"/home/u/hello.txt": domain.CodeInvalidArgument, // not a directory
		"/nope":             domain.CodeNotFound,
		"relative":          domain.CodeInvalidArgument,
	} {
		if _, err := f.svc.Tree(ctx, owner, "snap", p, 0, ""); code(err) != want {
			t.Errorf("%s: %v, want %s", p, err, want)
		}
	}
	if _, err := f.svc.Tree(ctx, "mallory", "snap", "/", 0, ""); code(err) != domain.CodeNotFound {
		t.Errorf("other principal: %v", err)
	}
}

func TestReadFileStaysInsideTheSnapshot(t *testing.T) {
	f := setup(t, 0)
	for p, want := range map[string]string{
		"/home/u/hello.txt":         "hello\n",
		"/../../home/u/hello.txt":   "hello\n",
		"/home/u/proj/escape":       "snap\n", // the snapshot's /etc/hostname, never the host's
		"/home/u/proj/abs":          "snap\n",
		"/home/u/link/../hello.txt": "hello\n",
	} {
		b, head, err := f.read(t, p, false)
		if err != nil || string(b) != want || head.Size != int64(len(want)) || head.Mode == "" {
			t.Errorf("%s: %q %+v %v, want %q", p, b, head, err, want)
		}
	}
	if _, _, err := f.read(t, "/home/u/proj", false); code(err) != domain.CodeInvalidArgument {
		t.Errorf("directory without archive: %v", err)
	}
	if _, _, err := f.read(t, "/home/u/missing", false); code(err) != domain.CodeNotFound {
		t.Errorf("missing: %v", err)
	}
}

func TestArchiveExtractsSafely(t *testing.T) {
	f := setup(t, 0)
	b, head, err := f.read(t, "/home/u/proj", true)
	if err != nil || head.ContentType != api.ContentTypeTar {
		t.Fatalf("archive: %+v %v", head, err)
	}
	dest := t.TempDir()
	if err := tarx.Extract(bytes.NewReader(b), dest, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"main.go", "sub/x.bin"} {
		want, _ := os.ReadFile(filepath.Join(f.src, "home/u/proj", p))
		got, err := os.ReadFile(filepath.Join(dest, p))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if st, _ := os.Stat(filepath.Join(dest, "sub/x.bin")); st == nil || st.Mode().Perm() != 0o755 {
		t.Errorf("mode not kept: %v", st)
	}
	// Symlinks come back as symlinks, never followed on the host.
	if l, err := os.Readlink(filepath.Join(dest, "escape")); err != nil || l != "../../../../../../../etc/hostname" {
		t.Errorf("escape link: %q %v", l, err)
	}
	// A single file archives under its base name.
	b, _, err = f.read(t, "/home/u/hello.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	one := t.TempDir()
	if err := tarx.Extract(bytes.NewReader(b), one, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(one, "hello.txt")); string(got) != "hello\n" {
		t.Errorf("single file archive: %q", got)
	}
}

// sampler is a writer that records the peak heap in use while it receives
// data, and can run a hook after the first chunk.
type sampler struct {
	n, next  int64
	peak     uint64
	afterOne func()
	hash     interface {
		io.Writer
		Sum([]byte) []byte
	}
}

func (s *sampler) Write(p []byte) (int, error) {
	if s.n == 0 && s.afterOne != nil {
		defer s.afterOne()
	}
	s.n += int64(len(p))
	s.hash.Write(p)
	if s.n >= s.next {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		s.peak = max(s.peak, m.HeapInuse)
		s.next += 16 << 20
	}
	return len(p), nil
}

func TestLargeFileStreamsInBoundedMemory(t *testing.T) {
	const big = 256 << 20
	f := setup(t, big)
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	s := &sampler{hash: sha256.New()}
	sum, err := f.svc.ReadFile(context.Background(), owner, "snap", "/big.bin", false, func(h snapfiles.Head) (io.Writer, error) {
		if h.Size != big {
			t.Errorf("head size %d", h.Size)
		}
		return s, nil
	})
	if err != nil || s.n != big || hex.EncodeToString(s.hash.Sum(nil)) != sum {
		t.Fatalf("read %d bytes, sum %s, err %v", s.n, sum, err)
	}
	if grew := int64(s.peak) - int64(base.HeapInuse); grew > 16<<20 {
		t.Fatalf("heap grew %d MiB while streaming a %d MiB file", grew>>20, big>>20)
	}
}

func TestSnapshotDeletedMidRead(t *testing.T) {
	const big = 64 << 20
	f := setup(t, big)
	want, err := os.ReadFile(filepath.Join(f.src, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := &sampler{hash: sha256.New(), afterOne: func() {
		// Delete the snapshot and collect its data while the read runs.
		f.snaps.delete("snap")
		got, err := f.store.Collect(ctx, storage.KeepSet{})
		if err != nil || len(got.Commits) != 1 {
			t.Errorf("collect: %+v %v", got, err)
		}
	}}
	if _, err := f.svc.ReadFile(ctx, owner, "snap", "/big.bin", false, func(snapfiles.Head) (io.Writer, error) { return s, nil }); err != nil {
		t.Fatalf("read across delete: %v", err)
	}
	wantSum := sha256.Sum256(want)
	if s.n != big || !bytes.Equal(s.hash.Sum(nil), wantSum[:]) {
		t.Fatalf("read %d bytes with another digest after the delete", s.n)
	}
	if _, _, err := f.read(t, "/big.bin", false); code(err) != domain.CodeNotFound {
		t.Fatalf("read after delete: %v", err)
	}
}

func TestExport(t *testing.T) {
	f := setup(t, 0)
	ctx := context.Background()
	var raw bytes.Buffer
	sum, err := f.svc.Export(ctx, owner, "snap", api.ExportRaw, func(h snapfiles.Head) (io.Writer, error) {
		if st, _ := os.Stat(f.img); h.Size != st.Size() {
			t.Errorf("raw size %d", h.Size)
		}
		return &raw, nil
	})
	img, _ := os.ReadFile(f.img)
	imgSum := sha256.Sum256(img)
	if err != nil || !bytes.Equal(raw.Bytes(), img) || sum != hex.EncodeToString(imgSum[:]) {
		t.Fatalf("raw export: %d bytes, %v", raw.Len(), err)
	}
	var tb bytes.Buffer
	if _, err := f.svc.Export(ctx, owner, "snap", "", func(snapfiles.Head) (io.Writer, error) { return &tb, nil }); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := tarx.Extract(&tb, dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "home/u/hello.txt")); string(got) != "hello\n" {
		t.Fatalf("tar export: %q", got)
	}
	if _, err := f.svc.Export(ctx, owner, "snap", "zip", nil); code(err) != domain.CodeInvalidArgument {
		t.Fatalf("bad format: %v", err)
	}
}

func TestNotAnExtDisk(t *testing.T) {
	ctx := context.Background()
	zeros := filepath.Join(t.TempDir(), "zeros.img")
	if err := os.WriteFile(zeros, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(t.TempDir(), storage.ModeCopy)
	if err != nil {
		t.Fatal(err)
	}
	store.CreateWritable(ctx, "d1", zeros, 0)
	store.Commit(ctx, "d1", "c1")
	svc := snapfiles.New(&snaps{m: map[string]*domain.Snapshot{"s": {ID: "s", Owner: owner, Kind: domain.SnapshotKindDisk, CommitID: "c1"}}}, store)
	if _, err := svc.Tree(ctx, owner, "s", "/", 0, ""); code(err) != domain.CodeUnsupportedCapability {
		t.Fatalf("zeros: %v", err)
	}
}
