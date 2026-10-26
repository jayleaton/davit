package lifecycle

import (
	"context"
	"path/filepath"

	"davit/api"
	"davit/internal/runtime"
	"davit/internal/state"
)

// watchGoldens hooks an engine that evicts goldens to keep free space
// (runtime.GoldenEvictor): each eviction becomes a golden.evicted event,
// owned by the snapshot's owner for a project image's golden, else by the
// host owner (Config.HostOwner). It enforces the floor once at start; the
// sweeper enforces it on every tick.
func (s *Service) watchGoldens(ctx context.Context) {
	ev, ok := s.cfg.Runtime.(runtime.GoldenEvictor)
	if !ok {
		return
	}
	ev.OnGoldenEvicted(func(e runtime.GoldenEviction) {
		data := map[string]any{"goldenId": e.ID, "imageDigest": e.ImageDigest, "bytes": e.Bytes, "reason": e.Reason}
		owner, subject := s.cfg.HostOwner, e.ID
		err := s.cfg.DB.Tx(context.WithoutCancel(ctx), func(tx *state.Tx) error {
			// A project image's golden is a boot of its snapshot's commit
			// (storage/commits/<commit>/disk.img).
			if snap, err := tx.SnapshotByCommit(filepath.Base(filepath.Dir(e.Root))); err != nil {
				return err
			} else if snap != nil {
				owner, subject = snap.Owner, string(snap.ID)
				data["snapshotId"] = string(snap.ID)
			}
			if owner == "" {
				return nil // no one to tell; the engine logged it
			}
			return tx.AppendHostEvent(owner, api.EventGoldenEvicted, "", subject, data)
		})
		if err != nil {
			s.cfg.Log.Warn("golden.evicted event", "golden", e.ID, "err", err)
		}
		s.notify()
	})
	s.enforceGoldenFloor(ctx)
}

// enforceGoldenFloor asks the engine to evict goldens below its free-space
// floor (a no-op for engines without goldens).
func (s *Service) enforceGoldenFloor(ctx context.Context) {
	if ev, ok := s.cfg.Runtime.(runtime.GoldenEvictor); ok {
		if err := ev.EnforceGoldenFloor(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Log.Warn("golden floor", "err", err)
		}
	}
}
