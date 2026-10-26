package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/state"
	"davit/internal/storage"
)

func (s *Service) dispatch(id domain.OperationID) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.execute(s.ctx, id)
	}()
}

// bootSlot waits for one of the Limits.BootSlots boot slots, so a batch
// (or many clients) cannot start more boots at once; the operation stays
// queued, with one event saying why, until a slot frees up. Stop and
// delete never wait for one.
func (s *Service) bootSlot(ctx context.Context, op *domain.Operation) (func(), error) {
	if s.boots == nil {
		return func() {}, nil
	}
	select {
	case s.boots <- struct{}{}:
	default:
		s.event(ctx, op, "queued", fmt.Sprintf("waiting for a boot slot (%d boots in flight, limits.concurrentBoots)", cap(s.boots)))
		select {
		case s.boots <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return func() { <-s.boots }, nil
}

func (s *Service) execute(ctx context.Context, id domain.OperationID) {
	var op *domain.Operation
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		op, err = tx.GetOperation(id)
		return err
	}); err != nil {
		s.cfg.Log.Error("load operation", "op", id, "err", err)
		return
	}
	if op.State.Terminal() {
		return
	}
	var result *domain.OpResult
	var err error
	switch op.Kind {
	case domain.OpCreate, domain.OpFork, domain.OpResume, domain.OpRecover:
		var release func()
		if release, err = s.bootSlot(ctx, op); err == nil {
			result, err = s.runBoot(ctx, op)
			release()
		}
	case domain.OpStop:
		result, err = s.runStop(ctx, op)
	case domain.OpDelete:
		result, err = s.runDelete(ctx, op)
	default:
		err = domain.Errorf(domain.CodeNotImplemented, "operation kind %s", op.Kind)
	}
	if ctx.Err() != nil {
		// Daemon shutdown: leave the operation journaled; recovery resumes it.
		s.cfg.Log.Info("operation interrupted by shutdown", "op", op.ID, "phase", op.Phase)
		return
	}
	if err != nil {
		s.cfg.Log.Warn("operation failed", "op", op.ID, "kind", op.Kind, "err", err)
		s.fail(context.WithoutCancel(ctx), op, err)
		return
	}
	s.grantEvent(context.WithoutCancel(ctx), op)
	s.cfg.Log.Info("operation succeeded", "op", op.ID, "kind", op.Kind, "vm", op.VMID, "ms", result.DurationMs)
}

// step journals the start of a phase. Progress is the fraction complete.
func (s *Service) step(ctx context.Context, op *domain.Operation, phase string, progress float64) error {
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		cur, err := tx.GetOperation(op.ID)
		if err != nil {
			return err
		}
		cur.State, cur.Phase, cur.Progress, cur.UpdatedAt = domain.OpRunning, phase, progress, s.now()
		if err := tx.PutOperation(cur); err != nil {
			return err
		}
		*op = *cur
		return tx.AppendEvent(&domain.OperationEvent{OperationID: op.ID, Time: cur.UpdatedAt, State: cur.State, Phase: phase, Progress: progress})
	})
	if err == nil {
		s.notify()
	}
	return err
}

// event appends an informational event without changing the operation.
func (s *Service) event(ctx context.Context, op *domain.Operation, phase, msg string) {
	s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		return tx.AppendEvent(&domain.OperationEvent{OperationID: op.ID, Time: s.now(), State: op.State, Phase: phase, Progress: op.Progress, Message: msg})
	})
	s.notify()
}

// finish commits the terminal observation: fn updates the VM, then the
// operation succeeds, in one transaction.
func (s *Service) finish(ctx context.Context, op *domain.Operation, result *domain.OpResult, fn func(rec *state.VMRecord, now time.Time) error) error {
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		now := s.now()
		rec, err := tx.GetVM(op.VMID)
		if err != nil {
			return err
		}
		if err := fn(rec, now); err != nil {
			return err
		}
		rec.VM.ActiveOperationID = ""
		if err := tx.PutVM(rec); err != nil {
			return err
		}
		cur, err := tx.GetOperation(op.ID)
		if err != nil {
			return err
		}
		result.VMID = op.VMID
		result.DurationMs = now.Sub(cur.CreatedAt).Milliseconds()
		cur.State, cur.Phase, cur.Progress, cur.Result, cur.UpdatedAt, cur.CompletedAt = domain.OpSucceeded, "done", 1, result, now, &now
		if err := tx.PutOperation(cur); err != nil {
			return err
		}
		*op = *cur
		if err := tx.ExpireIdempotency(op.ID, now.Add(s.cfg.IdempotencyRetention)); err != nil {
			return err
		}
		return tx.AppendEvent(&domain.OperationEvent{OperationID: op.ID, Time: now, State: cur.State, Phase: "done", Progress: 1})
	})
	if err == nil {
		s.notify()
		if op.Kind == domain.OpCreate || op.Kind == domain.OpFork || op.Kind == domain.OpResume {
			s.kickWorkspaces(s.ctx, op.VMID)
		}
	}
	return err
}

// fail records a terminal failure. The VM goes to error (or stays deleting
// for a failed delete, which may be retried) and keeps its last committed
// snapshot; nothing is overwritten.
func (s *Service) fail(ctx context.Context, op *domain.Operation, cause error) {
	de := domain.AsError(cause)
	if errors.Is(cause, context.DeadlineExceeded) {
		de = domain.Errorf(domain.CodeTimeout, "%s timed out in phase %s", op.Kind, op.Phase)
		de.Retryable = true
	}
	de = &domain.Error{Code: de.Code, Message: de.Message, Retryable: de.Retryable, Details: de.Details, OperationID: op.ID}
	if op.Kind == domain.OpCreate || op.Kind == domain.OpFork || op.Kind == domain.OpResume || op.Kind == domain.OpRecover {
		// Never leave a half-booted instance behind.
		if s.destroyInstances(ctx, op.VMID, func(int64) bool { return true }) == nil {
			s.releaseNetwork(ctx, op.VMID)
		}
		s.discardLive(ctx, op)
		s.releaseLease(ctx, op.VMID)
	}
	// The journal keeps a space reserve, so recording a failure normally
	// succeeds even on a full disk; retry briefly before giving up (the
	// unfinished operation is then replayed on the next start).
	err := retry(ctx, 5, s.cfg.FailureRetry, func() error {
		return s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
			now := s.now()
			rec, err := tx.GetVM(op.VMID)
			if err != nil {
				return err
			}
			rec.VM.LastError = de
			rec.VM.ActiveOperationID = ""
			switch {
			case rec.VM.State == domain.StateDeleting:
				rec.VM.UpdatedAt = now
			case domain.CanTransition(rec.VM.State, domain.StateError):
				if op.Kind != domain.OpStop {
					rec.Handle = nil
				}
				rec.VM.BootMode, rec.VM.Readiness = domain.BootModeNone, domain.Readiness{Services: map[string]bool{}}
				if err := rec.VM.Transition(domain.StateError, now); err != nil {
					return err
				}
			}
			if err := tx.PutVM(rec); err != nil {
				return err
			}
			cur, err := tx.GetOperation(op.ID)
			if err != nil {
				return err
			}
			cur.State, cur.Error, cur.UpdatedAt, cur.CompletedAt = domain.OpFailed, de, now, &now
			if err := tx.PutOperation(cur); err != nil {
				return err
			}
			if err := tx.ExpireIdempotency(op.ID, now.Add(s.cfg.IdempotencyRetention)); err != nil {
				return err
			}
			return tx.AppendEvent(&domain.OperationEvent{OperationID: op.ID, Time: now, State: domain.OpFailed, Phase: cur.Phase, Progress: cur.Progress, Message: de.Message})
		})
	})
	if err != nil {
		s.cfg.Log.Error("record operation failure", "op", op.ID, "err", err)
	}
	s.notify()
}

// retry runs f up to n times with doubling backoff while it fails.
func retry(ctx context.Context, n int, backoff time.Duration, f func() error) error {
	var err error
	for i := 0; i < n; i++ {
		if err = f(); err == nil {
			return nil
		}
		select {
		case <-time.After(backoff << i):
		case <-ctx.Done():
			return err
		}
	}
	return err
}

func (s *Service) loadVM(ctx context.Context, id domain.VMID) (rec *state.VMRecord, err error) {
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec, err = tx.GetVM(id)
		return err
	})
	return rec, err
}

func (s *Service) updateVM(ctx context.Context, id domain.VMID, fn func(rec *state.VMRecord, now time.Time) error) error {
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec, err := tx.GetVM(id)
		if err != nil {
			return err
		}
		if err := fn(rec, s.now()); err != nil {
			return err
		}
		return tx.PutVM(rec)
	})
	if err == nil {
		s.notify()
	}
	return err
}

// destroyInstances destroys every runtime instance of vm whose generation
// matches; it proves prior generations are gone before a new one starts.
func (s *Service) destroyInstances(ctx context.Context, vm domain.VMID, match func(gen int64) bool) error {
	hs, err := s.cfg.Runtime.Discover(ctx)
	if err != nil {
		return err
	}
	for _, h := range hs {
		if h.VMID == vm && match(h.Generation) {
			if err := s.cfg.Runtime.Destroy(ctx, h); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) runBoot(ctx context.Context, op *domain.Operation) (*domain.OpResult, error) {
	rec, err := s.loadVM(ctx, op.VMID)
	if err != nil {
		return nil, err
	}
	gen := op.Params.Generation
	if rec.VM.State != domain.StateProvisioning || rec.VM.InstanceGeneration != gen {
		return nil, domain.Errorf(domain.CodeInternal, "vm %s is %s gen %d, operation expects provisioning gen %d", rec.VM.ID, rec.VM.State, rec.VM.InstanceGeneration, gen)
	}
	img, err := s.cfg.Images.Get(ctx, rec.VM.ImageID)
	if err != nil {
		return nil, err
	}

	if err := s.step(ctx, op, "runtime.reconcile", 0.05); err != nil {
		return nil, err
	}
	if op.Params.ReuseDisk {
		err = s.powerCycle(ctx, op) // recover: kill, no commit (recover.go)
	} else {
		err = s.destroyInstances(ctx, rec.VM.ID, func(g int64) bool { return g != gen })
	}
	if err != nil {
		return nil, err
	}

	// The boot plan is journaled before the disk exists and is never inferred
	// from the phase (which every attempt rewrites): "golden:<id>" means the
	// disk is a clone of that golden that no VMM has opened, anything else
	// cold boots. It turns "cold" before the disk reaches a VMM, so a replay
	// never restores golden RAM onto a disk that may have changed (SPEC §6).
	// A live fork restores the source's checkpoint ("ram:<id>") until the
	// child's disk may have reached a VMM; then, like golden, it cold boots.
	var cp *runtime.Checkpoint
	if op.Kind == domain.OpFork && op.Params.Live {
		if plan := op.Params.BootPlan; plan == "" || strings.HasPrefix(plan, "ram:") {
			c, err := s.liveCheckpoint(ctx, op)
			if err != nil {
				return nil, err
			}
			cp = &c
		}
	}
	var golden *runtime.Golden
	goldenImg := img // what the golden was captured from (a snapshot's disk as root)
	if plan := op.Params.BootPlan; plan == "" || strings.HasPrefix(plan, "golden:") {
		if op.Kind == domain.OpCreate && s.caps.GoldenRestore {
			if err := s.step(ctx, op, "image.prepare", 0.1); err != nil {
				return nil, err
			}
			base, err := s.goldenBase(ctx, img, op.Params.SourceSnapshotID)
			var prep runtime.Prepared
			if err == nil {
				prep, err = s.cfg.Runtime.Prepare(ctx, base, rec.VM.Shape)
			}
			switch {
			case err == nil && prep.Golden != nil && (plan == "" || plan == "golden:"+prep.Golden.ID):
				golden, goldenImg = prep.Golden, base
			case op.Params.RequireGolden:
				if err == nil {
					err = domain.Errorf(domain.CodeUnsupportedCapability, "no golden checkpoint for this image, shape and host")
				}
				return nil, err
			case ctx.Err() != nil:
				return nil, ctx.Err()
			default:
				s.event(ctx, op, "image.prepare", "golden unavailable, cold booting: "+errString(err))
			}
		}
		next := "cold"
		if golden != nil {
			next = "golden:" + golden.ID
		}
		if next != plan {
			if err := s.setBootPlan(ctx, op, next); err != nil {
				return nil, err
			}
		}
	}

	if err := s.step(ctx, op, "storage.prepare", 0.2); err != nil {
		return nil, err
	}
	disk := storage.DiskID(rec.DiskID)
	if op.Params.ReuseDisk {
		err = nil // recover boots the working disk as it is: nothing is created
	} else if cp != nil {
		_, err = s.cfg.Storage.CloneCommitted(ctx, storage.CommitID(op.Params.LiveCommitID), disk, rec.VM.Shape.DiskBytes)
	} else if golden != nil {
		// Also for a create from a snapshot: its golden's disk is that snapshot booted.
		_, err = s.cfg.Storage.CreateWritable(ctx, disk, golden.Disk, rec.VM.Shape.DiskBytes)
	} else if src := op.Params.SourceSnapshotID; src != "" {
		var snap *domain.Snapshot
		if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) { snap, err = tx.GetSnapshot(src); return err }); err != nil {
			return nil, err
		}
		_, err = s.cfg.Storage.CloneCommitted(ctx, storage.CommitID(snap.CommitID), disk, rec.VM.Shape.DiskBytes)
	} else {
		_, err = s.cfg.Storage.CreateWritable(ctx, disk, img.Rootfs, rec.VM.Shape.DiskBytes)
	}
	if err != nil {
		return nil, err
	}

	nh, err := s.provisionNetwork(ctx, rec.VM.ID, gen)
	if err != nil {
		return nil, err
	}
	if golden != nil || cp != nil {
		// From here the disk belongs to a VMM: a replay adopts it or cold boots.
		if err := s.setBootPlan(ctx, op, "cold"); err != nil {
			return nil, err
		}
	}
	if err := s.step(ctx, op, "runtime.boot", 0.3); err != nil {
		return nil, err
	}
	lease, err := s.cfg.Storage.OpenForRuntime(ctx, disk)
	if err != nil {
		return nil, err
	}
	s.setLease(rec.VM.ID, lease)
	inst := runtime.Instance{Key: runtime.Key{VMID: rec.VM.ID, Generation: gen}, Shape: rec.VM.Shape, Image: img}
	// ColdBoot and RestoreGolden of an already-live key adopt it, so a
	// replay after a crash never spawns a second VMM for the same generation.
	var h runtime.Handle
	if cp != nil {
		h, err = s.cfg.Runtime.RestoreCheckpoint(ctx, inst, *cp, runtime.DiskSet{Root: lease.Path}, nh)
	} else if golden != nil {
		gi := inst
		gi.Image = goldenImg
		h, err = s.cfg.Runtime.RestoreGolden(ctx, gi, *golden, runtime.DiskSet{Root: lease.Path}, nh)
	} else {
		h, err = s.cfg.Runtime.ColdBoot(ctx, inst, runtime.DiskSet{Root: lease.Path}, nh)
	}
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(h)
	if err := s.updateVM(ctx, rec.VM.ID, func(r *state.VMRecord, _ time.Time) error { r.Handle = raw; return nil }); err != nil {
		return nil, err
	}
	if cp != nil {
		s.discardLive(ctx, op) // the child maps its RAM image privately now
	}

	if err := s.step(ctx, op, "readiness", 0.7); err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, s.cfg.ReadyTimeout)
	defer cancel()
	rd, err := s.cfg.Prober.Probe(rctx, s.cfg.Runtime, h)
	if err != nil {
		return nil, err
	}
	if rd.Services == nil {
		rd.Services = map[string]bool{}
	}

	res := &domain.OpResult{BootMode: h.BootMode, SnapshotID: op.Params.SourceSnapshotID}
	return res, s.finish(ctx, op, res, func(r *state.VMRecord, now time.Time) error {
		if err := r.VM.Transition(domain.StateReady, now); err != nil {
			return err
		}
		r.VM.BootMode, r.VM.Readiness, r.VM.LastError = h.BootMode, rd, nil
		r.VM.Endpoint.SSHHostKeyFingerprints = nil
		if s.cfg.Identity != nil {
			r.VM.Endpoint.SSHHostKeyFingerprints = s.cfg.Identity.SSHHostKeyFingerprints(runtime.Key{VMID: rec.VM.ID, Generation: gen})
		}
		r.VM.ExpiresAt = nil
		if r.VM.AutoStop {
			exp := now.Add(time.Duration(r.VM.TTLSeconds) * time.Second)
			r.VM.ExpiresAt = &exp
		}
		return nil
	})
}

func (s *Service) runStop(ctx context.Context, op *domain.Operation) (*domain.OpResult, error) {
	rec, err := s.loadVM(ctx, op.VMID)
	if err != nil {
		return nil, err
	}
	h, err := decodeHandle(rec.Handle)
	if err != nil {
		return nil, err
	}

	if err := s.step(ctx, op, "guest.shutdown", 0.1); err != nil {
		return nil, err
	}
	forced := op.Params.Force
	if h != nil {
		// Not while a live fork holds this VM paused for its checkpoint.
		unlock := pauseGuard(rec.VM.ID)
		if op.Params.Force {
			err = s.cfg.Runtime.Kill(ctx, *h)
		} else {
			forced, err = s.gracefulStop(ctx, op, *h) // stopretry.go
		}
		unlock()
		if err != nil {
			return nil, err
		}
		if err := s.step(ctx, op, "runtime.verify", 0.4); err != nil {
			return nil, err
		}
		st, err := s.cfg.Runtime.Inspect(ctx, *h)
		if err != nil {
			return nil, err
		}
		if st.Phase != runtime.PhaseExited && st.Phase != runtime.PhaseGone {
			return nil, domain.Errorf(domain.CodeUnavailable, "runtime still %s after shutdown", st.Phase)
		}
		if err := s.cfg.Runtime.Destroy(ctx, *h); err != nil {
			return nil, err
		}
	}
	s.releaseNetwork(ctx, rec.VM.ID)
	s.releaseLease(ctx, rec.VM.ID)
	if op.Params.Discard {
		return s.finishDiscard(ctx, op, rec) // privacy.go
	}

	if err := s.step(ctx, op, "storage.commit", 0.6); err != nil {
		return nil, err
	}
	commitID := stopCommitID(op.ID)
	c, err := s.cfg.Storage.Commit(ctx, storage.DiskID(rec.DiskID), commitID)
	if err != nil {
		// Keep the working disk for recovery; the last good snapshot stays.
		s.updateVM(ctx, rec.VM.ID, func(r *state.VMRecord, _ time.Time) error { r.RetainedDiskID = r.DiskID; return nil })
		return nil, err
	}

	consistency := domain.ConsistencyClean
	if forced {
		consistency = domain.ConsistencyCrash
	}
	img, _ := s.cfg.Images.Get(ctx, rec.VM.ImageID)
	snap := &domain.Snapshot{
		ID: op.Params.SnapshotID, Owner: rec.VM.Owner, SourceVMID: rec.VM.ID, Generation: op.Params.Generation,
		ImageID: rec.VM.ImageID, Arch: img.Arch, Kind: domain.SnapshotKindDisk, Consistency: consistency,
		SizeBytes: c.SizeBytes, Digest: c.Digest, CommitID: string(c.ID), CreatedAt: c.CreatedAt.UTC(),
	}
	res := &domain.OpResult{SnapshotID: snap.ID}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		if _, err := tx.GetSnapshot(snap.ID); err == nil {
			return nil // replayed after a crash
		}
		return tx.PutSnapshot(snap)
	})
	if err != nil {
		return nil, err
	}
	err = s.finish(ctx, op, res, func(r *state.VMRecord, now time.Time) error {
		if err := r.VM.Transition(domain.StateStopped, now); err != nil {
			return err
		}
		r.VM.LastSnapshotID, r.VM.BootMode, r.VM.ExpiresAt, r.Handle = snap.ID, domain.BootModeNone, nil, nil
		r.VM.Readiness = domain.Readiness{Services: map[string]bool{}}
		r.VM.LastError = nil
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Superseded automatic snapshots would otherwise pin a full disk per
	// stop (in copy mode). Best effort: leftovers are collected later.
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error { return pruneAutomatic(tx, rec.VM.ID, false) })
	if err == nil {
		err = s.collect(ctx)
	}
	if err != nil {
		s.cfg.Log.Warn("prune snapshots after stop", "vm", rec.VM.ID, "err", err)
	}
	return res, nil
}

func (s *Service) runDelete(ctx context.Context, op *domain.Operation) (*domain.OpResult, error) {
	if err := s.step(ctx, op, "runtime.destroy", 0.2); err != nil {
		return nil, err
	}
	unlock := pauseGuard(op.VMID) // not mid live-fork checkpoint of this VM
	err := s.destroyInstances(ctx, op.VMID, func(int64) bool { return true })
	unlock()
	if err != nil {
		return nil, err
	}
	// A deleted VM is never reconciled again: its network must be gone first.
	if err := s.releaseNetwork(ctx, op.VMID); err != nil {
		return nil, err
	}
	s.releaseLease(ctx, op.VMID)

	if err := s.step(ctx, op, "storage.collect", 0.6); err != nil {
		return nil, err
	}
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error { return pruneAutomatic(tx, op.VMID, true) }); err != nil {
		return nil, err
	}
	if err := s.collect(ctx); err != nil {
		return nil, err
	}

	res := &domain.OpResult{}
	err = s.finish(ctx, op, res, func(r *state.VMRecord, now time.Time) error {
		r.Handle, r.DiskID, r.RetainedDiskID, r.Network = nil, "", "", nil
		r.VM.LastError = nil
		return r.VM.Transition(domain.StateDeleted, now)
	})
	if err == nil && s.cfg.OnDeleted != nil {
		s.cfg.OnDeleted(ctx, op.VMID)
	}
	return res, err
}

// setBootPlan journals op's boot plan.
func (s *Service) setBootPlan(ctx context.Context, op *domain.Operation, plan string) error {
	return s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		cur, err := tx.GetOperation(op.ID)
		if err != nil {
			return err
		}
		cur.Params.BootPlan = plan
		if err := tx.PutOperation(cur); err != nil {
			return err
		}
		*op = *cur
		return nil
	})
}

func errString(err error) string {
	if err == nil {
		return "no golden"
	}
	return err.Error()
}
