package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"davit/internal/domain"
	"davit/internal/state"
)

// Host capacity (G9, feature hostCapacity): the live-VM limit and the host
// memory floor at admission, and the HostCapacity report.
//
// Memory is measured (Config.FreeMemory) when admission runs, but a VM that
// was accepted and is still provisioning has not touched its memory yet: its
// shape is subtracted from the measurement so that a burst of creates (a
// batch) cannot all be admitted against the same free memory.

// usage is what admission counts in the journal.
type usage struct {
	live    int   // VMs holding a slot: provisioning to stopping
	pending int64 // memory of provisioning VMs, not yet in the measurement
}

func liveUsage(tx *state.Tx) (usage, error) {
	vms, err := tx.AllVMs()
	if err != nil {
		return usage{}, err
	}
	var u usage
	for _, r := range vms {
		if r.VM.State.Live() {
			u.live++
		}
		if r.VM.State == domain.StateProvisioning {
			u.pending += r.VM.Shape.MemoryBytes
		}
	}
	return u, nil
}

// freeMemory measures available memory; ok is false when it is not
// measured (no probe, or unsupported on this OS).
func (s *Service) freeMemory() (free int64, ok bool, err error) {
	if s.cfg.FreeMemory == nil {
		return 0, false, nil
	}
	free, err = s.cfg.FreeMemory()
	if errors.Is(err, errors.ErrUnsupported) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("measure free memory: %w", err)
	}
	return free, true, nil
}

// admitHost refuses one more live VM of shape want beyond Limits.VMs, or
// one whose memory would leave less than Limits.MemoryFloor() available.
func (s *Service) admitHost(tx *state.Tx, want domain.Shape) error {
	u, err := liveUsage(tx)
	if err != nil {
		return err
	}
	if err := s.cfg.Limits.AdmitVMs(u.live); err != nil {
		return err
	}
	free, ok, err := s.freeMemory()
	if err != nil || !ok {
		return err
	}
	return s.cfg.Limits.AdmitMemory(free-u.pending, want.MemoryBytes)
}

// capacity is the HostCapacity report; nil if the journal cannot be read.
func (s *Service) capacity(ctx context.Context) *domain.HostCapacity {
	c := &domain.HostCapacity{Limits: s.cfg.Limits}
	var u usage
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		if c.Allocated, err = tx.Allocated(); err != nil {
			return err
		}
		u, err = liveUsage(tx)
		return err
	}); err != nil {
		s.cfg.Log.Warn("capacity report", "err", err)
		return nil
	}
	c.LiveVMs = u.live
	memory, disk := int64(-1), int64(-1)
	if free, ok, err := s.freeMemory(); err != nil {
		s.cfg.Log.Warn("capacity report", "err", err)
	} else if ok {
		c.FreeMemoryBytes, memory = free, max(0, free-u.pending)
	}
	if s.cfg.FreeSpace != nil {
		if free, err := s.cfg.FreeSpace(); err != nil {
			s.cfg.Log.Warn("capacity report: measure free space", "err", err)
		} else {
			c.FreeDiskBytes, disk = free, free
		}
	}
	c.Slots = s.cfg.Limits.Slots(c.Allocated, c.LiveVMs, memory, disk, domain.DefaultShape)
	c.MemoryBudgetBytes = s.cfg.Limits.MemoryBudget()
	if s.cfg.TotalMemory != nil {
		if total, err := s.cfg.TotalMemory(); err == nil {
			c.TotalMemoryBytes = total
		} else if !errors.Is(err, errors.ErrUnsupported) {
			s.cfg.Log.Warn("capacity report: measure total memory", "err", err)
		}
	}
	if after := s.cfg.Limits.ReclaimIdleAfter(); after > 0 {
		if vms, err := s.reclaimable(ctx, reclaimQuery{idle: after, needTimeout: true}); err != nil {
			s.cfg.Log.Warn("capacity report: reclaimable VMs", "err", err)
		} else {
			c.Reclaimable = len(vms)
		}
	}
	c.Waiting = s.waiting()
	return c
}
