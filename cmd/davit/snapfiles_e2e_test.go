//go:build linux

package main_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/ext4/ext4test"
)

// TestSnapshotFilesEndToEnd reads files out of a stopped VM's snapshot
// through the daemon, the API and the CLI, without booting it: the fake
// runtime's image is a real ext4 filesystem, so the committed disk is too.
func TestSnapshotFilesEndToEnd(t *testing.T) {
	src := t.TempDir()
	w := func(p string, b []byte, mode os.FileMode) {
		os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o755)
		if err := os.WriteFile(filepath.Join(src, p), b, mode); err != nil {
			t.Fatal(err)
		}
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<20) // 16 MiB
	w("etc/hostname", []byte("box\n"), 0o644)
	w("home/u/proj/main.go", []byte("package main\n"), 0o600)
	w("home/u/proj/data/big.bin", big, 0o644)
	os.Symlink("../../../../../../etc/hostname", filepath.Join(src, "home/u/proj/escape"))
	img := ext4test.Image(t, src, 64, "mkfs.ext4")

	h := newHost(t)
	installImage(t, filepath.Join(h.dir, "data", "images"), img)
	h.start(0, 0)
	h.waited(h.cli("new", "box", "--disk", "128MiB"), domain.OpCreate)
	h.waited(h.cli("stop", "box"), domain.OpStop)
	r := h.cli("snapshots", "box")
	var snap api.Snapshot
	if r.code != 0 || len(r.raw) < 2 || json.Unmarshal(r.raw[0], &snap) != nil || snap.ID == "" {
		t.Fatalf("snapshots: exit %d %s", r.code, r.raw)
	}
	id := string(snap.ID)

	// tree: entries of a directory, symlinks with their targets.
	r = h.cli("snapshot", "tree", id, "/home/u/proj", "--all")
	got := map[string]api.SnapshotEntry{}
	for _, raw := range r.raw[:len(r.raw)-1] {
		var e api.SnapshotEntry
		json.Unmarshal(raw, &e)
		got[e.Path] = e
	}
	if r.code != 0 || len(got) != 3 || got["/home/u/proj/main.go"].Mode != "0600" || got["/home/u/proj/data"].Type != "dir" ||
		got["/home/u/proj/escape"].LinkTarget != "../../../../../../etc/hostname" {
		t.Fatalf("tree: exit %d %+v", r.code, got)
	}

	// pull a file: bytes, mode and a verified digest.
	out := t.TempDir()
	r = h.cli("snapshot", "pull", id, "/home/u/proj/data/big.bin", out)
	var pr struct {
		Destination string
		Bytes       int64
		SHA256      string
	}
	json.Unmarshal(r.raw[len(r.raw)-1], &pr)
	sum := sha256.Sum256(big)
	if b, _ := os.ReadFile(filepath.Join(out, "big.bin")); r.code != 0 || !bytes.Equal(b, big) || pr.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("pull file: exit %d %+v %v", r.code, pr, r.last().Error)
	}
	// An escaping symlink resolves inside the snapshot, never on this host.
	r = h.cli("snapshot", "pull", id, "/home/u/proj/escape", filepath.Join(out, "esc"))
	if b, _ := os.ReadFile(filepath.Join(out, "esc")); r.code != 0 || string(b) != "box\n" {
		t.Fatalf("pull escape: exit %d %q", r.code, b)
	}

	// pull a directory: extracted, modes kept, symlinks stored as links.
	r = h.cli("snapshot", "pull", id, "/home/u/proj", out)
	pr.SHA256 = ""
	json.Unmarshal(r.raw[len(r.raw)-1], &pr)
	if r.code != 0 || len(pr.SHA256) != 64 { // the archive's digest came as a trailer and matched
		t.Fatalf("pull dir: exit %d %+v %+v", r.code, pr, r.last().Error)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "proj/data/big.bin")); !bytes.Equal(b, big) {
		t.Fatal("pulled directory: big.bin differs")
	}
	if st, err := os.Stat(filepath.Join(out, "proj/main.go")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("pulled directory: main.go %v %v", st, err)
	}
	if l, err := os.Readlink(filepath.Join(out, "proj/escape")); err != nil || l != "../../../../../../etc/hostname" {
		t.Fatalf("pulled directory: escape %q %v", l, err)
	}

	// export raw: the committed disk, byte for byte, written sparse.
	rawFile := filepath.Join(out, "disk.raw")
	r = h.cli("snapshot", "export", id, "-o", rawFile, "--format", "raw")
	var er struct{ SHA256 string }
	json.Unmarshal(r.raw[len(r.raw)-1], &er)
	if r.code != 0 {
		t.Fatalf("export raw: exit %d %+v", r.code, r.last().Error)
	}
	disk, _ := os.ReadFile(rawFile)
	if diskSum := sha256.Sum256(disk); er.SHA256 != hex.EncodeToString(diskSum[:]) {
		t.Fatalf("export digest %q does not match the file", er.SHA256)
	}
	want, _ := os.ReadFile(img)
	if len(disk) != 128<<20 || !bytes.Equal(disk[:len(want)], want) || !bytes.Equal(disk[len(want):], make([]byte, len(disk)-len(want))) {
		t.Fatalf("raw export: %d bytes, image prefix equal %v", len(disk), bytes.Equal(disk[:min(len(disk), len(want))], want))
	}
	var st syscall.Stat_t
	if syscall.Stat(rawFile, &st) == nil && st.Blocks*512 >= int64(len(disk)) {
		t.Errorf("raw export not sparse: %d of %d bytes allocated", st.Blocks*512, len(disk))
	}

	// Errors keep their codes; an unknown path is not_found (exit code set).
	if r := h.cli("snapshot", "pull", id, "/nope", out); r.code == 0 || r.last().Error == nil || r.last().Error.Code != domain.CodeNotFound {
		t.Fatalf("missing path: exit %d %+v", r.code, r.last())
	}
}

// installImage writes a manifest whose rootfs is img (and a dummy kernel)
// under dir/manifests and dir/blobs, where the daemon looks by default.
func installImage(t *testing.T, dir, img string) {
	t.Helper()
	blob := func(b []byte) string {
		s := sha256.Sum256(b)
		h := hex.EncodeToString(s[:])
		os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755)
		if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", h), b, 0o644); err != nil {
			t.Fatal(err)
		}
		return "sha256:" + h
	}
	rootfs, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{
		"id": "sha256:" + fmt.Sprintf("%064x", 1), "schemaVersion": 1, "name": "ext4-test", "arch": "x86_64",
		"kernel": map[string]any{"digest": blob([]byte("kernel")), "size": 6, "bootArgs": "console=ttyS0 root=/dev/vda rw"},
		"rootfs": map[string]any{"digest": blob(rootfs), "size": len(rootfs), "fsType": "ext4"},
	}
	b, _ := json.Marshal(m)
	os.MkdirAll(filepath.Join(dir, "manifests"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "manifests", "ext4-test.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}
