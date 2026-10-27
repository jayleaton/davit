package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"time"

	"davit/api"
	"davit/internal/domain"
)

// Pruning image blobs (pruneImages, `davit box image prune`, and the
// low-disk cleanup). The daemon's blob store (<dataDir>/images/blobs/
// sha256/<hex>) keeps every file an install ever put there; an image
// replaced by a newer one leaves its kernel and agent blobs (install.sh only
// drops a replaced rootfs). A blob is removed only when davit can prove
// nothing uses it:
//
//   - no manifest names its digest: every manifest in the manifests
//     directory and every configured one is read in full at each prune, and
//     any "sha256:<hex>" anywhere in it (kernel, rootfs, agent binary,
//     future fields) counts; a manifest that cannot be read stops the prune;
//   - no image the running daemon serves uses its path, current or retired
//     (an image whose manifest was removed after the daemon started is
//     still served; a retired one boots the boxes made before an upgrade);
//   - it has not changed for BlobStore.MinAge: an install moves blobs into
//     place before the manifest that names them.
//
// Only regular files named by a sha256 hex digest are candidates; nothing
// else in the directory is touched. Goldens captured from a removed blob
// can never be restored again; the golden sweep that follows frees them.

// DefaultBlobMinAge is BlobStore.MinAge when it is zero.
const DefaultBlobMinAge = time.Hour

// BlobStore is the image blob directory the daemon owns.
type BlobStore struct {
	// Dir holds the blobs as sha256/<hex>.
	Dir string
	// Manifests lists every image manifest whose blobs must stay; it is
	// called at each prune.
	Manifests func() ([]string, error)
	// MinAge: blobs changed more recently are kept (0: DefaultBlobMinAge).
	MinAge time.Duration
}

var (
	digestRef = regexp.MustCompile(`sha256:([0-9a-f]{64})`)
	blobName  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// referenced returns the digests the manifests name.
func (b *BlobStore) referenced() (map[string]bool, error) {
	keep := map[string]bool{}
	if b.Manifests == nil {
		return nil, errors.New("no manifest list: cannot prove a blob unreferenced")
	}
	paths, err := b.Manifests()
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("read manifest %s: %w", p, err)
		}
		raw, err := io.ReadAll(io.LimitReader(f, maxManifestSize+1))
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("read manifest %s: %w", p, err)
		}
		if len(raw) > maxManifestSize {
			return nil, fmt.Errorf("manifest %s exceeds %d bytes", p, maxManifestSize)
		}
		for _, m := range digestRef.FindAllSubmatch(raw, -1) {
			keep[string(m[1])] = true
		}
	}
	return keep, nil
}

// prune removes (dryRun: lists) the unreferenced blobs. inUse are blob
// paths the daemon's images use.
func (b *BlobStore) prune(ctx context.Context, inUse []string, dryRun bool, now time.Time) (*api.PruneImagesResult, error) {
	keep, err := b.referenced()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(b.Dir, "sha256")
	for _, p := range inUse {
		if filepath.Dir(p) == dir {
			keep[filepath.Base(p)] = true
		}
	}
	minAge := b.MinAge
	if minAge == 0 {
		minAge = DefaultBlobMinAge
	}
	res := &api.PruneImagesResult{DryRun: dryRun, Removed: []api.PrunedBlob{}}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := e.Name()
		if !blobName.MatchString(name) || !e.Type().IsRegular() {
			continue
		}
		if keep[name] {
			res.Kept++
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue // gone meanwhile
		}
		if now.Sub(changedAt(fi)) < minAge {
			res.Kept++
			continue
		}
		bytes := fi.Size()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			bytes = st.Blocks * 512
		}
		if !dryRun {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, err
			}
		}
		res.Removed = append(res.Removed, api.PrunedBlob{Digest: "sha256:" + name, Bytes: bytes, ChangedAt: changedAt(fi).UTC()})
		res.Bytes += bytes
	}
	slices.SortFunc(res.Removed, func(a, b api.PrunedBlob) int { return a.ChangedAt.Compare(b.ChangedAt) })
	return res, nil
}

// pruneBlobs prunes the image blob store against the served images, then
// sweeps the goldens a removed blob made stale.
func (s *Service) pruneBlobs(ctx context.Context, dryRun bool) (*api.PruneImagesResult, error) {
	if s.cfg.ImageBlobs == nil {
		return &api.PruneImagesResult{DryRun: dryRun, Removed: []api.PrunedBlob{}}, nil
	}
	imgs, err := s.cfg.Images.List(ctx)
	if err != nil {
		return nil, err
	}
	if r, ok := s.cfg.Images.(interface{ RetiredImages() []domain.Image }); ok {
		imgs = append(imgs, r.RetiredImages()...)
	}
	var inUse []string
	for _, img := range imgs {
		inUse = append(inUse, img.Kernel, img.Rootfs)
	}
	res, err := s.cfg.ImageBlobs.prune(ctx, inUse, dryRun, s.now())
	if err != nil {
		return nil, err
	}
	if !dryRun && len(res.Removed) > 0 {
		for _, r := range res.Removed {
			s.cfg.Log.Warn("pruned unreferenced image blob", "digest", r.Digest, "bytes", r.Bytes, "changedAt", r.ChangedAt.Format(time.RFC3339))
		}
		if err := s.collect(ctx); err != nil {
			s.cfg.Log.Warn("golden sweep after blob prune", "err", err)
		}
	}
	return res, nil
}

// PruneImages removes the image blobs no manifest or served image uses
// (pruneImages, `davit box image prune`). Only the host owner may.
func (s *Service) PruneImages(ctx context.Context, principal domain.PrincipalID, body api.PruneImagesRequest) (*api.PruneImagesResult, error) {
	if principal == "" {
		return nil, domain.Errorf(domain.CodeUnauthenticated, "no principal")
	}
	if s.cfg.HostOwner != "" && principal != s.cfg.HostOwner {
		return nil, domain.Errorf(domain.CodePermissionDenied, "only the host owner may prune image blobs")
	}
	return s.pruneBlobs(ctx, body.DryRun)
}
