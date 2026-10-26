package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"davit/internal/domain"
	"davit/internal/guestclient"
)

const maxManifestSize = 16 << 20

// ManifestSource is one image manifest and the blob store holding its
// content-addressed files (<BlobDir>/sha256/<hex>).
type ManifestSource struct {
	Manifest string
	BlobDir  string
}

// ManifestCatalog serves images from verified manifests; the first source is
// the default image.
type ManifestCatalog struct {
	*StaticCatalog
	warnings []string
}

// Warnings lists non-fatal problems found while loading, e.g. a baked guest
// agent missing features this host uses ("minor" protocol skew). Fatal ones
// (no common protocol version) fail LoadManifestCatalog instead.
func (c *ManifestCatalog) Warnings() []string { return slices.Clone(c.warnings) }

// imageManifest is the subset of the images workstream manifest we use.
type imageManifest struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Arch   string `json:"arch"`
	Kernel struct {
		Digest   string `json:"digest"`
		Size     int64  `json:"size"`
		BootArgs string `json:"bootArgs"`
	} `json:"kernel"`
	Rootfs struct {
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"rootfs"`
	Guest struct {
		User struct {
			Name string `json:"name"`
		} `json:"user"`
		Agent struct {
			Binary *struct {
				Digest string `json:"digest"`
			} `json:"binary"`
			Version  string                     `json:"version"`
			Protocol *guestclient.AgentProtocol `json:"protocol"`
		} `json:"agent"`
	} `json:"guest"`
}

// LoadManifestCatalog reads every manifest and verifies its kernel and rootfs
// blobs (sha256 and size) before returning. Verifications are cached in
// <cacheDir>/verified.json against the blob's stat identity.
func LoadManifestCatalog(ctx context.Context, cacheDir string, entries ...ManifestSource) (*ManifestCatalog, error) {
	if len(entries) == 0 {
		return nil, errors.New("image catalog: no manifests")
	}
	cache := loadVerifyCache(cacheDir)
	var images []domain.Image
	var warnings []string
	seen := map[string]string{}
	for _, src := range entries {
		img, warn, err := loadManifest(ctx, src, cache)
		if err != nil {
			return nil, fmt.Errorf("image manifest %s: %w", src.Manifest, err)
		}
		if warn != "" {
			warnings = append(warnings, fmt.Sprintf("image %s: %s", img.Ref, warn))
		}
		for _, k := range []string{string(img.ID), img.Ref, img.Digest} {
			if prev, dup := seen[k]; dup {
				return nil, fmt.Errorf("image manifest %s: %q already defined by %s", src.Manifest, k, prev)
			}
			seen[k] = src.Manifest
		}
		images = append(images, img)
	}
	if err := cache.save(); err != nil {
		return nil, fmt.Errorf("image catalog: write verification cache: %w", err)
	}
	return &ManifestCatalog{StaticCatalog: NewStaticCatalog(images...), warnings: warnings}, nil
}

// loadManifest returns the image and a non-fatal warning. A baked guest agent
// whose protocol cannot talk to this host is an error: every boot and golden
// preparation would fail at the restore barrier, so refuse it up front.
func loadManifest(ctx context.Context, src ManifestSource, cache *verifyCache) (img domain.Image, warning string, err error) {
	f, err := os.Open(src.Manifest)
	if err != nil {
		return domain.Image{}, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return domain.Image{}, "", err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxManifestSize+1))
	if err != nil {
		return domain.Image{}, "", err
	}
	if len(raw) > maxManifestSize {
		return domain.Image{}, "", fmt.Errorf("exceeds %d bytes", maxManifestSize)
	}
	var m imageManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return domain.Image{}, "", err
	}
	id, err := digestHex(m.ID)
	if err != nil {
		return domain.Image{}, "", fmt.Errorf("id: %w", err)
	}
	if m.Name == "" {
		return domain.Image{}, "", errors.New("missing name")
	}
	var arch domain.Arch
	switch m.Arch {
	case "x86_64", "amd64":
		arch = domain.ArchAMD64
	case "aarch64", "arm64":
		arch = domain.ArchARM64
	default:
		return domain.Image{}, "", fmt.Errorf("unsupported arch %q", m.Arch)
	}
	kernel, err := cache.verify(ctx, src.BlobDir, "kernel", m.Kernel.Digest, m.Kernel.Size)
	if err != nil {
		return domain.Image{}, "", err
	}
	rootfs, err := cache.verify(ctx, src.BlobDir, "rootfs", m.Rootfs.Digest, m.Rootfs.Size)
	if err != nil {
		return domain.Image{}, "", err
	}
	// Images without a recorded agent (dev images, agents injected later) are
	// left to the handshake; preflight warns about them separately.
	if m.Guest.Agent.Binary != nil {
		switch c, msg := guestclient.CheckAgent(m.Guest.Agent.Protocol); c {
		case guestclient.CompatIncompatible:
			return domain.Image{}, "", fmt.Errorf("%s (agent %s): %s", m.Name, m.Guest.Agent.Version, msg)
		case guestclient.CompatDegraded:
			warning = msg
		}
	}
	return domain.Image{
		ID:       domain.ImageID("img_" + id[:16]),
		Ref:      m.Name,
		Digest:   m.ID,
		Arch:     arch,
		Kernel:   kernel,
		Rootfs:   rootfs,
		BootArgs: m.Kernel.BootArgs, DefaultUser: m.Guest.User.Name,
		RootfsSize: m.Rootfs.Size,
		CreatedAt:  st.ModTime().UTC().Truncate(time.Second),
	}, warning, nil
}

// digestHex returns the lowercase hex of a "sha256:<64 hex>" digest.
func digestHex(d string) (string, error) {
	h, ok := strings.CutPrefix(d, "sha256:")
	if b, err := hex.DecodeString(h); !ok || err != nil || len(b) != sha256.Size || h != strings.ToLower(h) {
		return "", fmt.Errorf("bad digest %q", d)
	}
	return h, nil
}

// blobStat is the identity a cached verification is bound to.
type blobStat struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"` // ns
	Inode uint64 `json:"inode"`
	Dev   uint64 `json:"dev"`
}

func statOf(path string, fi fs.FileInfo) blobStat {
	s := blobStat{Path: path, Size: fi.Size(), Mtime: fi.ModTime().UnixNano()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		s.Inode, s.Dev = uint64(st.Ino), uint64(st.Dev)
	}
	return s
}

// verifyCache maps blob digest to the stat of the file last verified for it.
type verifyCache struct {
	path    string
	entries map[string]blobStat
	dirty   bool
}

// hashBlob is swapped by tests to count hashes.
var hashBlob = sha256Reader

func loadVerifyCache(dir string) *verifyCache {
	c := &verifyCache{path: filepath.Join(dir, "verified.json"), entries: map[string]blobStat{}}
	if raw, err := os.ReadFile(c.path); err == nil {
		// A corrupt cache is only a lost optimization; it is rewritten.
		if json.Unmarshal(raw, &c.entries) != nil {
			c.entries, c.dirty = map[string]blobStat{}, true
		}
	}
	return c
}

// verify checks <blobDir>/sha256/<hex> against digest and size and returns
// its path.
func (c *verifyCache) verify(ctx context.Context, blobDir, what, digest string, size int64) (string, error) {
	h, err := digestHex(digest)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	if size <= 0 {
		return "", fmt.Errorf("%s blob %s: missing size", what, digest)
	}
	path, err := filepath.Abs(filepath.Join(blobDir, "sha256", h))
	if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s blob %s: %w", what, digest, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s blob %s: %s is not a regular file", what, digest, path)
	}
	if fi.Size() != size {
		return "", fmt.Errorf("%s blob %s: size %d, manifest says %d", what, digest, fi.Size(), size)
	}
	before := statOf(path, fi)
	if c.entries[digest] == before {
		return path, nil
	}
	got, err := hashBlob(ctx, f)
	if err != nil {
		return "", fmt.Errorf("%s blob %s: %w", what, digest, err)
	}
	if got != h {
		if _, ok := c.entries[digest]; ok {
			delete(c.entries, digest)
			c.dirty = true
		}
		return "", fmt.Errorf("%s blob %s (%s): content sha256 is %s", what, digest, path, got)
	}
	// Cache only if the file did not change while it was hashed.
	if fi2, err := f.Stat(); err == nil && statOf(path, fi2) == before {
		c.entries[digest] = before
		c.dirty = true
	}
	return path, nil
}

func (c *verifyCache) save() error {
	if !c.dirty {
		return nil
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".verified-*.json")
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), c.path)
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	c.dirty = false
	return nil
}

// sha256Reader hashes r, honoring ctx between chunks.
func sha256Reader(ctx context.Context, r io.Reader) (string, error) {
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := r.Read(buf)
		h.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			return "", err
		}
	}
}
