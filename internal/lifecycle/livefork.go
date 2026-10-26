package lifecycle

import (
	"context"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/state"
	"davit/internal/storage"
)

// Live fork (SPEC §6 "live fork"): the running source is paused, its RAM and
// device state checkpointed and its disk committed while paused, then
// unpaused; the child restores that checkpoint onto a clone of exactly that
// commit, so RAM and disk come from one instant. The child is a new VM
// (new ID, identity rotated by the restore barrier); processes running in
// the source are running in the child too.

// planLiveFork accepts a live fork of src: the child keeps the source's
// exact shape (a RAM image restores only into the same CPUs, memory and
// disk size) and no snapshot is involved.
func (s *Service) planLiveFork(tx *state.Tx, now time.Time, req Request, src *state.VMRecord, body api.ForkVMRequest) (*domain.Operation, error) {
	if !s.caps.LiveFork {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "%s runtime cannot fork a running VM", s.caps.Engine).
			WithDetail("capability", "liveFork")
	}
	if body.SnapshotID != "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "live and snapshotId are mutually exclusive")
	}
	if src.VM.State != domain.StateReady && src.VM.State != domain.StateRunning {
		return nil, domain.Errorf(domain.CodeInvalidState, "vm %s is %s; a live fork needs it ready or running", src.VM.ID, src.VM.State).
			WithDetail("state", src.VM.State)
	}
	base, err := s.profileShape(body.Type, src.VM.Shape)
	if err != nil {
		return nil, err
	}
	shape, err := s.resolveShape(body.Shape, base, src.VM.Shape.DiskBytes)
	if err != nil {
		return nil, err
	}
	if shape != src.VM.Shape {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "a live fork keeps the source's shape (cpus, memory, disk)").
			WithDetail("field", "shape")
	}
	ttl, auto, err := s.ttl(body.TTLSeconds, body.AutoStop, int64(s.cfg.DefaultTTL/time.Second), true)
	if err != nil {
		return nil, err
	}
	op := &domain.Operation{ID: domain.NewOperationID(), Kind: domain.OpFork, Phase: "queued",
		Params: domain.OpParams{SourceVMID: src.VM.ID, Live: true}}
	// A fork never widens its source's egress unless asked to.
	egress, err := egressOr(body.Egress, src.VM.Egress)
	if err != nil {
		return nil, err
	}
	rec, err := s.newVM(tx, now, req.Principal, body.Name, shape, src.VM.ImageID, "", nil, ttl, auto, egress, op)
	if err == nil {
		rec.VM.Privacy = src.VM.Privacy // forks inherit the privacy policy
		if err = s.forkWorkspace(tx, rec, src, body.Environment); err == nil {
			err = tx.PutVM(rec)
		}
	}
	return op, err
}

// pauseGuards serialize everything that pauses or unpauses a VM's instance
// (live fork checkpoints, stop), so a concurrent stop cannot resume a
// source between its checkpoint and its disk commit.
var pauseGuards sync.Map // domain.VMID -> *sync.Mutex

func pauseGuard(vm domain.VMID) func() {
	m, _ := pauseGuards.LoadOrStore(vm, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// liveCheckpoint returns the checkpoint (and published matching disk
// commit) a live fork restores from. Journal order makes every replay safe:
//  1. the commit ID is journaled before the source pauses, so garbage
//     collection keeps it from the moment it can exist;
//  2. the checkpoint is journaled before the child's disk is cloned from the
//     commit, so a journaled checkpoint is never retaken (the child's disk may
//     already hold that instant) and an unjournaled one has no disk yet.
func (s *Service) liveCheckpoint(ctx context.Context, op *domain.Operation) (runtime.Checkpoint, error) {
	if id := op.Params.CheckpointID; id != "" {
		if _, err := s.cfg.Storage.GetCommit(ctx, storage.CommitID(op.Params.LiveCommitID)); err != nil {
			return runtime.Checkpoint{}, err
		}
		return runtime.Checkpoint{ID: id, Fingerprint: op.Params.CheckpointFingerprint}, nil
	}
	src, err := s.loadVM(ctx, op.Params.SourceVMID)
	if err != nil {
		return runtime.Checkpoint{}, err
	}
	h, err := decodeHandle(src.Handle)
	if err != nil {
		return runtime.Checkpoint{}, err
	}
	if h == nil || (src.VM.State != domain.StateReady && src.VM.State != domain.StateRunning) {
		return runtime.Checkpoint{}, domain.Errorf(domain.CodeInvalidState, "source vm %s is %s; a live fork needs it ready or running", src.VM.ID, src.VM.State)
	}
	commit := storage.CommitID("c-" + domain.NewID("live"))
	if err := s.journalLive(ctx, op, func(p *domain.OpParams) { p.LiveCommitID = string(commit) }); err != nil {
		return runtime.Checkpoint{}, err
	}
	if err := s.step(ctx, op, "source.checkpoint", 0.1); err != nil {
		return runtime.Checkpoint{}, err
	}
	unlock := pauseGuard(src.VM.ID)
	defer unlock()
	t0 := time.Now()
	if err := s.cfg.Runtime.Pause(ctx, *h); err != nil {
		return runtime.Checkpoint{}, err
	}
	paused := true
	resume := func() error {
		if !paused {
			return nil
		}
		paused = false
		return s.resumeSource(ctx, src.VM.ID, *h)
	}
	defer resume()
	t1 := time.Now()
	cp, err := s.cfg.Runtime.Checkpoint(ctx, *h, runtime.Quiescence{})
	if err != nil {
		return runtime.Checkpoint{}, err
	}
	t2 := time.Now()
	if _, err := s.cfg.Storage.Commit(ctx, storage.DiskID(src.DiskID), commit); err != nil {
		s.cfg.Runtime.DiscardCheckpoint(context.WithoutCancel(ctx), cp)
		return runtime.Checkpoint{}, err
	}
	t3 := time.Now()
	if err := resume(); err != nil {
		s.cfg.Runtime.DiscardCheckpoint(context.WithoutCancel(ctx), cp)
		return runtime.Checkpoint{}, err
	}
	ms := func(d time.Duration) string { return d.Round(time.Millisecond).String() }
	s.event(ctx, op, "source.checkpoint", "source paused "+ms(time.Since(t0))+
		" (pause "+ms(t1.Sub(t0))+", ram "+ms(t2.Sub(t1))+", disk "+ms(t3.Sub(t2))+")")
	err = s.journalLive(ctx, op, func(p *domain.OpParams) {
		p.CheckpointID, p.CheckpointFingerprint, p.BootPlan = cp.ID, cp.Fingerprint, "ram:"+cp.ID
	})
	if err != nil {
		s.cfg.Runtime.DiscardCheckpoint(context.WithoutCancel(ctx), cp)
		return runtime.Checkpoint{}, err
	}
	return cp, nil
}

func (s *Service) journalLive(ctx context.Context, op *domain.Operation, set func(*domain.OpParams)) error {
	return s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		cur, err := tx.GetOperation(op.ID)
		if err != nil {
			return err
		}
		set(&cur.Params)
		if err := tx.PutOperation(cur); err != nil {
			return err
		}
		*op = *cur
		return nil
	})
}

// resumeSource unpauses a live fork's source, retrying a slow VMM API. A
// source that cannot be resumed is never left silently paused: it is killed
// and put in error (its disk is intact; resume restarts it).
func (s *Service) resumeSource(ctx context.Context, vm domain.VMID, h runtime.Handle) error {
	ctx = context.WithoutCancel(ctx)
	var err error
	for range 3 {
		uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = s.cfg.Runtime.Unpause(uctx, h)
		cancel()
		if err == nil {
			return nil
		}
	}
	s.cfg.Log.Error("cannot resume live fork source; stopping it", "vm", vm, "err", err)
	kctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.cfg.Runtime.Kill(kctx, h)
	s.updateVM(ctx, vm, func(r *state.VMRecord, now time.Time) error {
		if !domain.CanTransition(r.VM.State, domain.StateError) {
			return nil
		}
		r.VM.LastError = domain.Errorf(domain.CodeUnavailable, "could not resume after a live fork checkpoint: %v", err)
		r.VM.BootMode, r.VM.Readiness = domain.BootModeNone, domain.Readiness{Services: map[string]bool{}}
		return r.VM.Transition(domain.StateError, now)
	})
	return domain.Errorf(domain.CodeUnavailable, "live fork source %s could not be resumed: %v", vm, err)
}

// discardLive releases a live fork's checkpoint once it is no longer
// needed (restored, or the operation failed). The disk commit is left to
// garbage collection, which keeps it only while the operation is
// unfinished.
func (s *Service) discardLive(ctx context.Context, op *domain.Operation) {
	if op.Params.CheckpointID == "" {
		return
	}
	cp := runtime.Checkpoint{ID: op.Params.CheckpointID, Fingerprint: op.Params.CheckpointFingerprint}
	if err := s.cfg.Runtime.DiscardCheckpoint(context.WithoutCancel(ctx), cp); err != nil {
		s.cfg.Log.Warn("discard checkpoint", "op", op.ID, "checkpoint", cp.ID, "err", err)
	}
}
