package firecracker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// FirecrackerVersion is the pinned release.
const FirecrackerVersion = "v1.17.0"

// DefaultReleaseBaseURL is where release tarballs are downloaded from.
const DefaultReleaseBaseURL = "https://github.com/firecracker-microvm/firecracker/releases/download"

const (
	maxTarballSize = 256 << 20
	maxBinarySize  = 64 << 20
	maxSumsSize    = 64 << 10
)

// pin is the expected digests for one architecture. An empty binary digest
// means the binary is trusted only through the verified tarball's SHA256SUMS.
type pin struct{ tarball, binary string }

// pins is a variable so tests can substitute fixtures.
var pins = map[string]pin{
	"x86_64": {
		tarball: "06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558",
		binary:  "99ad0f5cd0514a88aad0e9ae8cfdb3cc3b4ab9d190e1194602406c786b5de7a5",
	},
	"aarch64": {
		tarball: "e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256",
	},
}

// Binary is a verified Firecracker executable.
type Binary struct {
	Path    string
	Version string
	SHA256  string
}

// FetchOptions configures EnsureBinary.
type FetchOptions struct {
	BaseURL string       // release download base; DefaultReleaseBaseURL if empty
	Client  *http.Client // http.DefaultClient if nil
	Arch    string       // "x86_64" or "aarch64"; derived from GOARCH if empty
}

// EnsureBinary returns the pinned firecracker under dir (e.g. <dataDir>/bin),
// downloading and verifying it on first use. An installed binary whose digest
// no longer matches is replaced.
func EnsureBinary(ctx context.Context, dir string, opt FetchOptions) (Binary, error) {
	arch, p, err := archPin(opt.Arch)
	if err != nil {
		return Binary{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Binary{}, fmt.Errorf("firecracker: %w", err)
	}
	name := binaryName(arch)
	dst := filepath.Join(dir, name)
	if b, ok := installed(dst, p); ok {
		return b, nil
	}
	if err := fetch(ctx, dir, arch, p, opt); err != nil {
		return Binary{}, fmt.Errorf("firecracker %s %s: %w", FirecrackerVersion, arch, err)
	}
	b, ok := installed(dst, p)
	if !ok {
		return Binary{}, fmt.Errorf("firecracker %s %s: installed binary failed verification", FirecrackerVersion, arch)
	}
	return b, nil
}

// VerifyBinary checks an explicitly configured binary: its sha256 must equal
// the pinned binary digest for arch ("" means the host arch).
func VerifyBinary(path, arch string) (Binary, error) {
	arch, p, err := archPin(arch)
	if err != nil {
		return Binary{}, err
	}
	if p.binary == "" {
		return Binary{}, fmt.Errorf("firecracker: no pinned binary digest for %s; only download-verified installs are supported (unset the binary path)", arch)
	}
	if fi, err := os.Stat(path); err != nil {
		return Binary{}, fmt.Errorf("firecracker: %w", err)
	} else if !fi.Mode().IsRegular() {
		return Binary{}, fmt.Errorf("firecracker: %s is not a regular file", path)
	}
	sum, err := hashFile(path)
	if err != nil {
		return Binary{}, fmt.Errorf("firecracker: %w", err)
	}
	if sum != p.binary {
		return Binary{}, fmt.Errorf("firecracker: %s has sha256 %s, want %s (%s %s)", path, sum, p.binary, FirecrackerVersion, arch)
	}
	return Binary{Path: path, Version: FirecrackerVersion, SHA256: sum}, nil
}

func archPin(arch string) (string, pin, error) {
	if arch == "" {
		switch goruntime.GOARCH {
		case "amd64":
			arch = "x86_64"
		case "arm64":
			arch = "aarch64"
		default:
			return "", pin{}, fmt.Errorf("firecracker: unsupported GOARCH %s", goruntime.GOARCH)
		}
	}
	p, ok := pins[arch]
	if !ok {
		return "", pin{}, fmt.Errorf("firecracker: unsupported arch %q", arch)
	}
	return arch, p, nil
}

func binaryName(arch string) string { return "firecracker-" + FirecrackerVersion + "-" + arch }

// installed reports whether dst is executable and hashes to both its recorded
// digest and the pin (when pinned).
func installed(dst string, p pin) (Binary, bool) {
	rec, err := os.ReadFile(dst + ".sha256")
	if err != nil {
		return Binary{}, false
	}
	want := strings.TrimSpace(string(rec))
	fi, err := os.Stat(dst)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return Binary{}, false
	}
	sum, err := hashFile(dst)
	if err != nil || sum != want || (p.binary != "" && sum != p.binary) {
		return Binary{}, false
	}
	return Binary{Path: dst, Version: FirecrackerVersion, SHA256: sum}, true
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetch downloads the release tarball into dir, verifies it, extracts and
// verifies the binary and installs it atomically with its digest record.
func fetch(ctx context.Context, dir, arch string, p pin, opt FetchOptions) error {
	var temps []string
	defer func() {
		for _, t := range temps {
			os.Remove(t)
		}
	}()
	newTemp := func(pattern string) (*os.File, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err == nil {
			temps = append(temps, f.Name())
		}
		return f, err
	}

	tgz, err := newTemp(".firecracker-*.tgz")
	if err != nil {
		return err
	}
	defer tgz.Close()
	if sum, err := download(ctx, tgz, arch, opt); err != nil {
		return err
	} else if sum != p.tarball {
		return fmt.Errorf("tarball sha256 %s, want %s", sum, p.tarball)
	}
	if _, err := tgz.Seek(0, io.SeekStart); err != nil {
		return err
	}

	bin, err := newTemp(".firecracker-*.bin")
	if err != nil {
		return err
	}
	defer bin.Close()
	sum, err := extract(tgz, bin, arch)
	if err != nil {
		return err
	}
	if p.binary != "" && sum != p.binary {
		return fmt.Errorf("binary sha256 %s, want pinned %s", sum, p.binary)
	}
	if err := bin.Chmod(0o755); err != nil {
		return err
	}
	if err := bin.Sync(); err != nil {
		return err
	}
	if err := bin.Close(); err != nil {
		return err
	}

	rec, err := newTemp(".firecracker-*.sha256")
	if err != nil {
		return err
	}
	defer rec.Close()
	if _, err := rec.WriteString(sum + "\n"); err != nil {
		return err
	}
	if err := rec.Chmod(0o644); err != nil {
		return err
	}
	if err := rec.Sync(); err != nil {
		return err
	}
	if err := rec.Close(); err != nil {
		return err
	}

	dst := filepath.Join(dir, binaryName(arch))
	if err := os.Rename(bin.Name(), dst); err != nil {
		return err
	}
	if err := os.Rename(rec.Name(), dst+".sha256"); err != nil {
		return err
	}
	return syncDir(dir)
}

// download streams the tarball into f and returns its sha256, refusing
// anything over maxTarballSize.
func download(ctx context.Context, f *os.File, arch string, opt FetchOptions) (string, error) {
	base := opt.BaseURL
	if base == "" {
		base = DefaultReleaseBaseURL
	}
	client := opt.Client
	if client == nil {
		client = http.DefaultClient
	}
	url := strings.TrimSuffix(base, "/") + "/" + FirecrackerVersion + "/" + binaryName(arch) + ".tgz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxTarballSize+1))
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	if n > maxTarballSize {
		return "", fmt.Errorf("GET %s: tarball exceeds %d bytes", url, maxTarballSize)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extract copies the firecracker binary out of the tarball into bin and
// checks it against the tarball's SHA256SUMS. Only the two needed entries are
// read; any entry with an absolute or escaping path fails the archive.
func extract(tgz io.Reader, bin *os.File, arch string) (string, error) {
	zr, err := gzip.NewReader(tgz)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	name := binaryName(arch)
	prefix := "release-" + FirecrackerVersion + "-" + arch + "/"
	var binSum string
	var sums []byte
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if !localPath(hdr.Name) {
			return "", fmt.Errorf("tarball entry %q escapes the archive", hdr.Name)
		}
		switch path.Clean(hdr.Name) {
		case prefix + name:
			if hdr.Typeflag != tar.TypeReg || binSum != "" {
				return "", fmt.Errorf("tarball entry %q: unexpected type or duplicate", hdr.Name)
			}
			h := sha256.New()
			n, err := io.Copy(io.MultiWriter(bin, h), io.LimitReader(tr, maxBinarySize+1))
			if err != nil {
				return "", err
			}
			if n > maxBinarySize {
				return "", fmt.Errorf("tarball entry %q exceeds %d bytes", hdr.Name, maxBinarySize)
			}
			binSum = hex.EncodeToString(h.Sum(nil))
		case prefix + "SHA256SUMS":
			if hdr.Typeflag != tar.TypeReg || sums != nil {
				return "", fmt.Errorf("tarball entry %q: unexpected type or duplicate", hdr.Name)
			}
			if sums, err = io.ReadAll(io.LimitReader(tr, maxSumsSize+1)); err != nil {
				return "", err
			}
			if len(sums) > maxSumsSize {
				return "", fmt.Errorf("tarball entry %q exceeds %d bytes", hdr.Name, maxSumsSize)
			}
		}
	}
	if binSum == "" {
		return "", fmt.Errorf("tarball has no %s", prefix+name)
	}
	if sums == nil {
		return "", fmt.Errorf("tarball has no %sSHA256SUMS", prefix)
	}
	want, err := sumFor(sums, name)
	if err != nil {
		return "", err
	}
	if binSum != want {
		return "", fmt.Errorf("binary sha256 %s, SHA256SUMS says %s", binSum, want)
	}
	return binSum, nil
}

func localPath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) {
		return false
	}
	c := path.Clean(name)
	return c != ".." && !strings.HasPrefix(c, "../")
}

// sumFor finds name's digest in sha256sum output ("<hex>  ./<name>").
func sumFor(sums []byte, name string) (string, error) {
	var found string
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		if strings.TrimPrefix(strings.TrimPrefix(f[1], "*"), "./") != name {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("SHA256SUMS lists %s twice", name)
		}
		if b, err := hex.DecodeString(f[0]); err != nil || len(b) != sha256.Size {
			return "", fmt.Errorf("SHA256SUMS: bad digest for %s", name)
		}
		found = strings.ToLower(f[0])
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("SHA256SUMS has no entry for %s", name)
	}
	return found, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
