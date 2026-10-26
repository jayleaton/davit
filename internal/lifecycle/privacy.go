package lifecycle

import (
	"context"
	"time"

	"davit/internal/domain"
	"davit/internal/state"
)

// Privacy policy enforcement (G17). The stop decision (discard, delete) is
// journaled in the stop operation when it is accepted, so a stop replayed
// after a daemon crash ends the same way; output and samples are refused
// by the state store itself (state.Tx.AppendOutput, PutGuestSample).

// keepsDisk refuses an operation that needs a committed disk of a VM whose
// policy keeps none (ephemeral or noSnapshot).
func keepsDisk(vm domain.VM, what string) error {
	if vm.Privacy.KeepsDisk() {
		return nil
	}
	reason := "noSnapshot"
	if vm.Privacy.Ephemeral {
		reason = "ephemeral"
	}
	return domain.Errorf(domain.CodeInvalidState, "vm %s is %s: it keeps no committed disk, so %s is not possible (create a new VM; a VM in error can be recovered)", vm.ID, reason, what).
		WithDetail("privacy", reason)
}

// finishDiscard ends a stop that commits nothing (the VMM is already gone):
// the working disk and the VM's automatic snapshots are dropped, and an
// ephemeral VM is deleted. The record is updated before anything is
// collected, so a crash in between leaves only unreferenced data that the
// next collection (at the latest the one at daemon start) removes.
func (s *Service) finishDiscard(ctx context.Context, op *domain.Operation, rec *state.VMRecord) (*domain.OpResult, error) {
	if err := s.step(ctx, op, "storage.discard", 0.6); err != nil {
		return nil, err
	}
	msg := "noSnapshot: nothing is committed; the working disk is discarded"
	if op.Params.Delete {
		msg = "ephemeral: the VM is deleted on stop; nothing is committed"
	}
	s.event(ctx, op, "storage.discard", msg)
	if op.Params.Delete {
		// Deleting first, in its own write, so the host event stream (G16
		// triggers on vms) reports stopping -> deleting -> deleted, exactly
		// as an explicit delete does. A replay finds it deleting already.
		if err := s.updateVM(ctx, rec.VM.ID, func(r *state.VMRecord, now time.Time) error {
			if r.VM.State == domain.StateDeleting {
				return nil
			}
			return r.VM.Transition(domain.StateDeleting, now)
		}); err != nil {
			return nil, err
		}
	}
	res := &domain.OpResult{}
	err := s.finish(ctx, op, res, func(r *state.VMRecord, now time.Time) error {
		r.Handle, r.DiskID, r.RetainedDiskID = nil, "", ""
		r.VM.BootMode, r.VM.ExpiresAt, r.VM.LastSnapshotID, r.VM.LastError = domain.BootModeNone, nil, "", nil
		r.VM.Readiness = domain.Readiness{Services: map[string]bool{}}
		if !op.Params.Delete {
			return r.VM.Transition(domain.StateStopped, now)
		}
		r.Network = nil
		return r.VM.Transition(domain.StateDeleted, now)
	})
	if err != nil {
		return nil, err
	}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error { return pruneAutomatic(tx, rec.VM.ID, true) })
	if err == nil {
		err = s.collect(ctx)
	}
	if err != nil {
		s.cfg.Log.Warn("collect a discarded disk", "vm", rec.VM.ID, "err", err)
	}
	if op.Params.Delete && s.cfg.OnDeleted != nil {
		s.cfg.OnDeleted(ctx, op.VMID)
	}
	return res, nil
}

// setPrivacy replaces rec's policy inside Update's transaction. A policy
// change is refused while a lifecycle operation runs (a stop has already
// journaled its decision). Turning on noOutputRetention or noProcessSamples
// drops what the host already kept for the VM.
func setPrivacy(tx *state.Tx, rec *state.VMRecord, p domain.PrivacyPolicy) error {
	if id := rec.VM.ActiveOperationID; id != "" {
		e := domain.Errorf(domain.CodeOperationInProgress, "vm %s has operation %s in progress; change its privacy policy after it", rec.VM.ID, id)
		e.OperationID, e.Retryable = id, true
		return e.WithDetail("operationId", id)
	}
	if p.NoOutputRetention && !rec.VM.Privacy.NoOutputRetention {
		if _, err := tx.PurgeVMOutput(rec.VM.ID); err != nil {
			return err
		}
	}
	if p.NoProcessSamples && !rec.VM.Privacy.NoProcessSamples {
		if err := tx.DeleteGuestSample(rec.VM.ID); err != nil {
			return err
		}
	}
	rec.VM.Privacy = p
	return nil
}
