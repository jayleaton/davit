//go:build linux

package main_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"davit/internal/domain"
)

// TestImageUpgradeKeepsBoxes: an upgrade that replaces the default image (the
// installer's layout: the old manifest kept in images/retired/<id>.json, its
// rootfs dropped) leaves the boxes made from the old image working: a
// stopped box resumes and its snapshot still clones, while new boxes get the
// new image.
func TestImageUpgradeKeepsBoxes(t *testing.T) {
	h := newHost(t)
	images := filepath.Join(h.dir, "data", "images")
	blob := func(body string) string {
		sum := sha256.Sum256([]byte(body))
		hx := hex.EncodeToString(sum[:])
		os.MkdirAll(filepath.Join(images, "blobs", "sha256"), 0o755)
		if err := os.WriteFile(filepath.Join(images, "blobs", "sha256", hx), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return hx
	}
	manifest := func(gen string) (string, string, string) {
		kernel, rootfs := blob("kernel "+gen), blob("rootfs "+gen)
		sum := sha256.Sum256([]byte("manifest " + gen))
		id := hex.EncodeToString(sum[:])
		return fmt.Sprintf(`{"id":"sha256:%s","schemaVersion":1,"name":"dev","arch":"x86_64",`+
			`"kernel":{"digest":"sha256:%s","size":%d,"bootArgs":"console=ttyS0"},"rootfs":{"digest":"sha256:%s","size":%d}}`,
			id, kernel, len("kernel "+gen), rootfs, len("rootfs "+gen)), id, rootfs
	}
	install := func(path, body string) {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	current := filepath.Join(images, "manifests", "dev.json")
	oldBody, oldID, oldRootfs := manifest("1")
	install(current, oldBody)
	h.start(0, 0)
	old := h.waited(h.cli("new", "old"), domain.OpCreate)
	if old.ImageID != domain.ImageID("img_"+oldID[:16]) {
		t.Fatalf("created on %s, want the installed image", old.ImageID)
	}
	h.waited(h.cli("stop", "old"), domain.OpStop)
	h.waited(h.cli("snapshot", "create", "old", "base"), domain.OpSnapshot)
	h.kill()

	// The upgrade, as install.sh does it.
	install(filepath.Join(images, "retired", oldID+".json"), oldBody)
	newBody, newID, _ := manifest("2")
	install(current, newBody)
	os.Remove(filepath.Join(images, "blobs", "sha256", oldRootfs))
	h.start(0, 0)

	if vm := h.waited(h.cli("resume", "old"), domain.OpResume); vm.State != domain.StateReady {
		t.Fatalf("resume after the upgrade: %s", vm.State)
	}
	if vm := h.waited(h.cli("new", "clone", "--from", "base"), domain.OpCreate); vm.ImageID != old.ImageID {
		t.Fatalf("clone of an old snapshot is on %s, want %s", vm.ImageID, old.ImageID)
	}
	if vm := h.waited(h.cli("new", "fresh"), domain.OpCreate); vm.ImageID != domain.ImageID("img_"+newID[:16]) {
		t.Fatalf("a new box is on %s, want the upgraded image", vm.ImageID)
	}
}
