package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type tarEntry struct{ name, body string }

func fakeTarball(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fixture serves tgz and pins it (and binBody) for x86_64; it returns the
// request counter.
func fixture(t *testing.T, tgz []byte, binBody string) (FetchOptions, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.URL.Path != "/"+FirecrackerVersion+"/firecracker-"+FirecrackerVersion+"-x86_64.tgz" {
			http.NotFound(w, r)
			return
		}
		w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	old := pins
	pins = map[string]pin{"x86_64": {tarball: hexSum(tgz), binary: hexSum([]byte(binBody))}, "aarch64": {tarball: "00"}}
	t.Cleanup(func() { pins = old })
	return FetchOptions{BaseURL: srv.URL, Client: srv.Client(), Arch: "x86_64"}, &n
}

const (
	fakeBin  = "#!/bin/sh\necho firecracker\n"
	relDir   = "release-" + FirecrackerVersion + "-x86_64/"
	fakeName = "firecracker-" + FirecrackerVersion + "-x86_64"
)

func goodTarball(t *testing.T, extra ...tarEntry) []byte {
	entries := append(extra,
		tarEntry{relDir + "NOTICE", "notice"},
		tarEntry{relDir + fakeName, fakeBin},
		tarEntry{relDir + "SHA256SUMS", "0f44  ./NOTICE\n" + hexSum([]byte(fakeBin)) + "  ./" + fakeName + "\n"},
	)
	return fakeTarball(t, entries...)
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestEnsureBinaryInstallsOnceAndRepairs(t *testing.T) {
	opt, reqs := fixture(t, goodTarball(t), fakeBin)
	dir := filepath.Join(t.TempDir(), "bin")
	ctx := context.Background()

	b, err := EnsureBinary(ctx, dir, opt)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, fakeName)
	if b.Path != want || b.Version != FirecrackerVersion || b.SHA256 != hexSum([]byte(fakeBin)) {
		t.Fatalf("binary = %+v", b)
	}
	if fi, err := os.Stat(want); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("stat = %v, %v", fi, err)
	}
	if rec, _ := os.ReadFile(want + ".sha256"); strings.TrimSpace(string(rec)) != b.SHA256 {
		t.Fatalf("sha record = %q", rec)
	}
	if got := dirNames(t, dir); len(got) != 2 {
		t.Fatalf("dir = %v, want binary + record only", got)
	}

	if _, err := EnsureBinary(ctx, dir, opt); err != nil || reqs.Load() != 1 {
		t.Fatalf("second call: err=%v requests=%d, want no download", err, reqs.Load())
	}

	if err := os.WriteFile(want, []byte("corrupt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if b2, err := EnsureBinary(ctx, dir, opt); err != nil || b2 != b || reqs.Load() != 2 {
		t.Fatalf("repair: %+v err=%v requests=%d", b2, err, reqs.Load())
	}
	if got, _ := os.ReadFile(want); string(got) != fakeBin {
		t.Fatalf("binary not restored: %q", got)
	}
}

func TestEnsureBinaryTarballMismatchLeavesNothing(t *testing.T) {
	opt, _ := fixture(t, goodTarball(t), fakeBin)
	pins["x86_64"] = pin{tarball: hexSum([]byte("other")), binary: hexSum([]byte(fakeBin))}
	dir := t.TempDir()
	if _, err := EnsureBinary(context.Background(), dir, opt); err == nil || !strings.Contains(err.Error(), "tarball sha256") {
		t.Fatalf("err = %v, want tarball mismatch", err)
	}
	if got := dirNames(t, dir); len(got) != 0 {
		t.Fatalf("dir = %v, want empty", got)
	}
}

func TestEnsureBinaryRejectsBadArchives(t *testing.T) {
	for name, tc := range map[string]struct {
		tgz  []byte
		want string
	}{
		"traversal": {goodTarball(t, tarEntry{"../evil", "x"}), "escapes"},
		"absolute":  {goodTarball(t, tarEntry{"/evil", "x"}), "escapes"},
		"sums mismatch": {fakeTarball(t,
			tarEntry{relDir + fakeName, fakeBin},
			tarEntry{relDir + "SHA256SUMS", hexSum([]byte("x")) + "  ./" + fakeName + "\n"}), "SHA256SUMS says"},
		"no sums": {fakeTarball(t, tarEntry{relDir + fakeName, fakeBin}), "no release"},
	} {
		t.Run(name, func(t *testing.T) {
			opt, _ := fixture(t, tc.tgz, fakeBin)
			root := t.TempDir()
			dir := filepath.Join(root, "bin")
			_, err := EnsureBinary(context.Background(), dir, opt)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if got := dirNames(t, dir); len(got) != 0 {
				t.Fatalf("dir = %v, want empty", got)
			}
			if got := dirNames(t, root); len(got) != 1 {
				t.Fatalf("root = %v, want only bin", got)
			}
		})
	}
}

func TestEnsureBinaryPinnedBinaryMismatch(t *testing.T) {
	opt, _ := fixture(t, goodTarball(t), fakeBin)
	p := pins["x86_64"]
	p.binary = hexSum([]byte("other"))
	pins["x86_64"] = p
	if _, err := EnsureBinary(context.Background(), t.TempDir(), opt); err == nil || !strings.Contains(err.Error(), "want pinned") {
		t.Fatalf("err = %v, want pinned mismatch", err)
	}
}

func TestVerifyBinary(t *testing.T) {
	fixture(t, goodTarball(t), fakeBin)
	dir := t.TempDir()
	good := filepath.Join(dir, "fc")
	if err := os.WriteFile(good, []byte(fakeBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := VerifyBinary(good, "x86_64"); err != nil || b.SHA256 != hexSum([]byte(fakeBin)) {
		t.Fatalf("good: %+v %v", b, err)
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("nope"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBinary(bad, "x86_64"); err == nil {
		t.Fatal("bad binary accepted")
	}
	if _, err := VerifyBinary(good, "aarch64"); err == nil || !strings.Contains(err.Error(), "download-verified") {
		t.Fatalf("aarch64: %v", err)
	}
	if _, err := VerifyBinary(good, "mips"); err == nil {
		t.Fatal("unknown arch accepted")
	}
}
