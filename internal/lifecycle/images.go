package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sync"
	"time"

	"davit/internal/domain"
)

// ImageCatalog resolves image references. The images workstream supplies the
// verified-manifest implementation; StaticCatalog serves M0 and tests.
type ImageCatalog interface {
	// Resolve maps a ref, digest or ID to an image; "" is the default image.
	Resolve(ctx context.Context, ref string) (domain.Image, error)
	Get(ctx context.Context, id domain.ImageID) (domain.Image, error)
	List(ctx context.Context) ([]domain.Image, error)
}

// StaticCatalog is a fixed in-memory catalog; the first image is the default.
type StaticCatalog struct {
	mu     sync.RWMutex
	images []domain.Image
}

// NewStaticCatalog returns a catalog of images.
func NewStaticCatalog(images ...domain.Image) *StaticCatalog {
	return &StaticCatalog{images: images}
}

func (c *StaticCatalog) Resolve(_ context.Context, ref string) (domain.Image, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i, img := range c.images {
		if (ref == "" && i == 0) || ref == string(img.ID) || ref == img.Ref || ref == img.Digest {
			return img, nil
		}
	}
	return domain.Image{}, domain.Errorf(domain.CodeNotFound, "image %q not found", ref).WithDetail("field", "image")
}

func (c *StaticCatalog) Get(ctx context.Context, id domain.ImageID) (domain.Image, error) {
	if id == "" {
		return domain.Image{}, domain.Errorf(domain.CodeNotFound, "image not found")
	}
	return c.Resolve(ctx, string(id))
}

func (c *StaticCatalog) List(context.Context) ([]domain.Image, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.images), nil
}

// DevImage materializes a small placeholder rootfs under dir for runtimes
// that do not read the disk (the fake). It is not bootable; real images come
// from the images workstream. Its ID is derived from the content digest.
func DevImage(dir string) (domain.Image, error) {
	path := filepath.Join(dir, "dev-rootfs.img")
	if _, err := os.Stat(path); err != nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return domain.Image{}, err
		}
		const size = 4 << 20
		f, err := os.CreateTemp(dir, ".dev-rootfs-*")
		if err != nil {
			return domain.Image{}, err
		}
		_, err = f.WriteString("davit dev placeholder rootfs\n")
		if err == nil {
			err = f.Truncate(size)
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(f.Name(), path)
		}
		if err != nil {
			os.Remove(f.Name())
			return domain.Image{}, err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return domain.Image{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return domain.Image{}, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	st, _ := f.Stat()
	return domain.Image{
		ID: domain.ImageID("img_" + sum[:16]), Ref: "dev", Digest: "sha256:" + sum,
		Arch: domain.Arch(goruntime.GOARCH), Rootfs: path, RootfsSize: n, CreatedAt: st.ModTime().UTC().Truncate(time.Second),
	}, nil
}
