package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
)

// writeBlob stores body under blobDir and returns its digest.
func writeBlob(t *testing.T, blobDir, body string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	h := hex.EncodeToString(sum[:])
	if err := os.MkdirAll(filepath.Join(blobDir, "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, "sha256", h), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return "sha256:" + h
}

// writeManifest writes a manifest named name with fresh blobs and returns
// its path and id.
func writeManifest(t *testing.T, dir, blobDir, name, arch, kernel, rootfs string) (string, string) {
	t.Helper()
	idSum := sha256.Sum256([]byte(name + arch))
	id := "sha256:" + hex.EncodeToString(idSum[:])
	m := map[string]any{
		"id": id, "schemaVersion": 1, "name": name, "arch": arch,
		"kernel": map[string]any{"digest": writeBlob(t, blobDir, kernel), "size": len(kernel), "bootArgs": "console=ttyS0 root=/dev/vda rw"},
		"rootfs": map[string]any{"digest": writeBlob(t, blobDir, rootfs), "size": len(rootfs), "fsType": "ext4"},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, id
}

func countHashes(t *testing.T) *int {
	n := new(int)
	old := hashBlob
	hashBlob = func(ctx context.Context, r io.Reader) (string, error) { *n++; return sha256Reader(ctx, r) }
	t.Cleanup(func() { hashBlob = old })
	return n
}

func TestManifestCatalogResolve(t *testing.T) {
	ctx := context.Background()
	dir, blobs := t.TempDir(), t.TempDir()
	basePath, baseID := writeManifest(t, dir, blobs, "ubuntu-base", "x86_64", "kernel-a", "rootfs-a")
	armPath, armID := writeManifest(t, dir, blobs, "ubuntu-arm", "aarch64", "kernel-b", "rootfs-b")
	cat, err := LoadManifestCatalog(ctx, filepath.Join(dir, "cache"),
		ManifestSource{basePath, blobs}, ManifestSource{armPath, blobs})
	if err != nil {
		t.Fatal(err)
	}
	var _ ImageCatalog = cat

	def, err := cat.Resolve(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(basePath)
	kSum := sha256.Sum256([]byte("kernel-a"))
	want := domain.Image{
		ID: domain.ImageID("img_" + baseID[7:23]), Ref: "ubuntu-base", Digest: baseID, Arch: domain.ArchAMD64,
		Kernel:     filepath.Join(blobs, "sha256", hex.EncodeToString(kSum[:])),
		Rootfs:     def.Rootfs,
		BootArgs:   "console=ttyS0 root=/dev/vda rw",
		RootfsSize: int64(len("rootfs-a")),
		CreatedAt:  st.ModTime().UTC().Truncate(time.Second),
	}
	if def != want {
		t.Fatalf("default = %+v\nwant      %+v", def, want)
	}
	if b, _ := os.ReadFile(def.Rootfs); string(b) != "rootfs-a" {
		t.Fatalf("rootfs path %s holds %q", def.Rootfs, b)
	}

	for _, ref := range []string{"ubuntu-arm", armID, "img_" + armID[7:23]} {
		img, err := cat.Resolve(ctx, ref)
		if err != nil || img.Ref != "ubuntu-arm" || img.Arch != domain.ArchARM64 {
			t.Fatalf("Resolve(%q) = %+v, %v", ref, img, err)
		}
	}
	if img, err := cat.Get(ctx, def.ID); err != nil || img != def {
		t.Fatalf("Get = %+v, %v", img, err)
	}
	if _, err := cat.Resolve(ctx, "nope"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("missing ref: %v", err)
	}
	if list, _ := cat.List(ctx); len(list) != 2 {
		t.Fatalf("list = %v", list)
	}
}

func TestManifestCatalogRejectsCorruptBlob(t *testing.T) {
	ctx := context.Background()
	dir, blobs := t.TempDir(), t.TempDir()
	path, _ := writeManifest(t, dir, blobs, "base", "x86_64", "kernel", "rootfs")
	sum := sha256.Sum256([]byte("rootfs"))
	blob := filepath.Join(blobs, "sha256", hex.EncodeToString(sum[:]))
	if err := os.WriteFile(blob, []byte("rootfX"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadManifestCatalog(ctx, dir, ManifestSource{path, blobs})
	if err == nil || !strings.Contains(err.Error(), "rootfs blob sha256:"+hex.EncodeToString(sum[:])) {
		t.Fatalf("err = %v, want rootfs digest mismatch naming the blob", err)
	}
	if err := os.WriteFile(blob, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifestCatalog(ctx, dir, ManifestSource{path, blobs}); err == nil || !strings.Contains(err.Error(), "size 5") {
		t.Fatalf("err = %v, want size mismatch", err)
	}
}

func TestManifestCatalogVerificationCache(t *testing.T) {
	ctx := context.Background()
	dir, blobs, cache := t.TempDir(), t.TempDir(), t.TempDir()
	path, _ := writeManifest(t, dir, blobs, "base", "x86_64", "kernel", "rootfs")
	hashes := countHashes(t)
	load := func() error {
		_, err := LoadManifestCatalog(ctx, cache, ManifestSource{path, blobs})
		return err
	}

	if err := load(); err != nil || *hashes != 2 {
		t.Fatalf("first load: err=%v hashes=%d, want 2", err, *hashes)
	}
	if _, err := os.Stat(filepath.Join(cache, "verified.json")); err != nil {
		t.Fatal(err)
	}
	if err := load(); err != nil || *hashes != 2 {
		t.Fatalf("cached load: err=%v hashes=%d, want no rehash", err, *hashes)
	}

	// Same size, different content and mtime: the cache must not vouch for it.
	sum := sha256.Sum256([]byte("rootfs"))
	blob := filepath.Join(blobs, "sha256", hex.EncodeToString(sum[:]))
	if err := os.WriteFile(blob, []byte("ROOTFS"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(blob, future, future); err != nil {
		t.Fatal(err)
	}
	if err := load(); err == nil || *hashes != 3 {
		t.Fatalf("changed blob: err=%v hashes=%d, want rehash and mismatch", err, *hashes)
	}

	// Restored content with a new mtime is rehashed once, then cached again.
	if err := os.WriteFile(blob, []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := load(); err != nil || *hashes != 4 {
		t.Fatalf("restored blob: err=%v hashes=%d", err, *hashes)
	}
	if err := load(); err != nil || *hashes != 4 {
		t.Fatalf("recached: err=%v hashes=%d", err, *hashes)
	}
}

func TestManifestCatalogRejectsBadManifest(t *testing.T) {
	ctx := context.Background()
	dir, blobs := t.TempDir(), t.TempDir()
	path, _ := writeManifest(t, dir, blobs, "base", "riscv64", "k", "r")
	if _, err := LoadManifestCatalog(ctx, dir, ManifestSource{path, blobs}); err == nil || !strings.Contains(err.Error(), "arch") {
		t.Fatalf("err = %v, want unsupported arch", err)
	}
	ok, _ := writeManifest(t, dir, blobs, "ok", "arm64", "k", "r")
	if _, err := LoadManifestCatalog(ctx, dir, ManifestSource{ok, blobs}, ManifestSource{ok, blobs}); err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("err = %v, want duplicate", err)
	}
	if _, err := LoadManifestCatalog(ctx, dir); err == nil {
		t.Fatal("empty catalog accepted")
	}
}

// withAgent rewrites a manifest to record a baked agent with protocol p
// (nil: none recorded, as manifests built before protocol tracking).
func withAgent(t *testing.T, path string, p *guestclient.AgentProtocol) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	agent := map[string]any{"binary": map[string]any{"digest": "sha256:" + strings.Repeat("c", 64)}, "version": "v-test"}
	if p != nil {
		agent["protocol"] = p
	}
	m["guest"] = map[string]any{"user": map[string]any{"name": "davit"}, "agent": agent}
	if raw, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Version skew between the baked guest agent and this host: no common
// protocol version refuses the image (every boot's barrier would fail);
// missing features or an untracked protocol only warn.
func TestManifestCatalogAgentProtocolSkew(t *testing.T) {
	ctx := context.Background()
	dir, blobs := t.TempDir(), t.TempDir()
	host := &guestclient.AgentProtocol{Min: guestproto.MinProtocolVersion, Max: guestproto.ProtocolVersion, Features: guestclient.Features}

	cur, _ := writeManifest(t, dir, blobs, "current", "x86_64", "k1", "r1")
	withAgent(t, cur, host)
	cat, err := LoadManifestCatalog(ctx, dir, ManifestSource{cur, blobs})
	if err != nil || len(cat.Warnings()) != 0 {
		t.Fatalf("matching agent: err %v warnings %v", err, cat.Warnings())
	}

	major, _ := writeManifest(t, dir, blobs, "future", "x86_64", "k2", "r2")
	withAgent(t, major, &guestclient.AgentProtocol{Min: guestproto.ProtocolVersion + 1, Max: guestproto.ProtocolVersion + 1, Features: guestclient.Features})
	if _, err := LoadManifestCatalog(ctx, dir, ManifestSource{cur, blobs}, ManifestSource{major, blobs}); err == nil ||
		!strings.Contains(err.Error(), "protocol") || !strings.Contains(err.Error(), "images/build.sh") {
		t.Fatalf("major skew: err = %v, want refusal naming the protocol and the fix", err)
	}

	minor, _ := writeManifest(t, dir, blobs, "older", "x86_64", "k3", "r3")
	withAgent(t, minor, &guestclient.AgentProtocol{Min: 1, Max: guestproto.ProtocolVersion, Features: []string{guestproto.FeatureExec}})
	legacy, _ := writeManifest(t, dir, blobs, "legacy", "x86_64", "k4", "r4")
	withAgent(t, legacy, nil)
	cat, err = LoadManifestCatalog(ctx, dir, ManifestSource{minor, blobs}, ManifestSource{legacy, blobs})
	if err != nil {
		t.Fatalf("minor skew must load: %v", err)
	}
	w := strings.Join(cat.Warnings(), "\n")
	if len(cat.Warnings()) != 2 || !strings.Contains(w, "image older") || !strings.Contains(w, guestproto.FeatureTCP) ||
		!strings.Contains(w, "image legacy") {
		t.Fatalf("warnings = %q", w)
	}
}

// TestManifestCatalogRetired: a manifest an upgrade replaced is still found
// by ID (the VMs and snapshots made from it boot with its kernel), even
// without its rootfs, but it is never listed, the default or a name's image;
// one whose kernel is gone is a warning, not a failed start.
func TestManifestCatalogRetired(t *testing.T) {
	ctx := context.Background()
	dir, blobs := t.TempDir(), t.TempDir()
	cur, _ := writeManifest(t, dir, blobs, "dev", "x86_64", "kernel-new", "rootfs-new")
	retiredDir := RetiredDir(t.TempDir())
	os.MkdirAll(retiredDir, 0o755)
	old, oldID := writeManifest(t, retiredDir, blobs, "dev-old", "x86_64", "kernel-old", "rootfs-old")
	// The old manifest keeps the current one's name, as a replaced image does.
	raw, _ := os.ReadFile(old)
	os.WriteFile(old, []byte(strings.Replace(string(raw), `"name":"dev-old"`, `"name":"dev"`, 1)), 0o644)
	var m struct {
		Rootfs struct{ Digest string } `json:"rootfs"`
	}
	json.Unmarshal(raw, &m)
	os.Remove(filepath.Join(blobs, "sha256", strings.TrimPrefix(m.Rootfs.Digest, "sha256:"))) // dropped by the upgrade
	broken, _ := writeManifest(t, retiredDir, t.TempDir(), "gone", "x86_64", "k", "r")

	cat, err := LoadManifestCatalog(ctx, filepath.Join(dir, "cache"), ManifestSource{cur, blobs})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.LoadRetired(ctx, filepath.Join(dir, "cache"), ManifestSource{old, blobs}, ManifestSource{broken, blobs}); err != nil {
		t.Fatal(err)
	}
	id := domain.ImageID("img_" + strings.TrimPrefix(oldID, "sha256:")[:16])
	img, err := cat.Get(ctx, id)
	if err != nil || img.Kernel == "" || img.Rootfs != "" || img.Ref != "dev" {
		t.Fatalf("retired image by ID: %+v %v", img, err)
	}
	if def, _ := cat.Resolve(ctx, ""); def.ID == id {
		t.Fatal("a retired image is the default")
	}
	if byName, _ := cat.Resolve(ctx, "dev"); byName.ID == id {
		t.Fatal("a retired image answers its name")
	}
	if _, err := cat.Resolve(ctx, string(id)); err == nil {
		t.Fatal("a retired image is resolvable for a create")
	}
	if list, _ := cat.List(ctx); len(list) != 1 {
		t.Fatalf("listed %d images, want only the current one", len(list))
	}
	if w := strings.Join(cat.Warnings(), "\n"); !strings.Contains(w, "retired image manifest "+broken) {
		t.Fatalf("no warning for the unloadable retired manifest: %q", w)
	}
}
