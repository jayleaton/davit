package lifecycle

import (
	"context"

	"davit/internal/domain"
	"davit/internal/state"
	"davit/internal/storage"
)

// goldenBase is what a create's golden is captured from: the image itself,
// or, for a create from a snapshot (a project image's instances), the image
// with the snapshot's committed disk as its root. Commits are immutable, and
// the runtime's golden fingerprint covers the root path, so each snapshot
// (each project-image generation) gets its own golden and every instance
// created from it restores that golden and shares its memory pages.
func (s *Service) goldenBase(ctx context.Context, img domain.Image, snapshot domain.SnapshotID) (domain.Image, error) {
	if snapshot == "" {
		return img, nil
	}
	var snap *domain.Snapshot
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) { snap, err = tx.GetSnapshot(snapshot); return err }); err != nil {
		return img, err
	}
	c, err := s.cfg.Storage.GetCommit(ctx, storage.CommitID(snap.CommitID))
	if err != nil {
		return img, err
	}
	img.Rootfs = c.Path
	return img, nil
}
