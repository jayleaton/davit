package lifecycle

import (
	"context"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/state"
	"davit/internal/storage"
)

// Recover (G9) is a disk-preserving power cycle for a wedged guest: the VMM
// of the current generation is killed without a RAM or disk commit, and the
// VM's current working disk cold boots as is in a new generation. Nothing
// rolls back: what the guest had written survives exactly as a power loss
// leaves it (crash-consistent), and no snapshot is taken.

// powerCycleMessage is the journal event recover writes before it boots;
// killed reports whether a VMM was still there to kill.
func powerCycleMessage(killed bool) string {
	what := "the VMM was killed without a RAM or disk commit"
	if !killed {
		what = "no VMM was left to kill; nothing was committed"
	}
	return "power cycle: " + what + "; the current disk boots as is (crash-consistent, like a power loss); no snapshot is taken"
}

// Recover accepts a power cycle of ref on its current disk. It is accepted
// for ready, running and error VMs that still have a working disk; a stopped
// VM is resumed instead, and a VM without a disk resumes from its last
// snapshot. Idempotency-Key is required.
func (s *Service) Recover(ctx context.Context, req Request, ref string, body api.RecoverVMRequest) (*Accepted, error) {
	req.VMRef = ref
	disk := s.workingDisk(ctx, req.Principal, ref)
	return s.mutate(ctx, req, body, true, func(tx *state.Tx, now time.Time) (*domain.Operation, error) {
		rec, err := lookupVM(tx, req.Principal, ref)
		if err != nil {
			return nil, err
		}
		if err := checkMutable(rec, req); err != nil {
			return nil, err
		}
		vm := &rec.VM
		switch vm.State {
		case domain.StateReady, domain.StateRunning, domain.StateError:
		case domain.StateStopped:
			return nil, domain.Errorf(domain.CodeInvalidState,
				"vm %s is stopped: its disk is committed and nothing runs; use resume", vm.ID).
				WithDetail("state", vm.State).WithDetail("hint", "resume")
		default:
			return nil, domain.Errorf(domain.CodeInvalidState, "cannot recover vm in state %s", vm.State).
				WithDetail("state", vm.State)
		}
		// A ready or running VM booted its disk, so it exists. An error VM may
		// have failed before its disk was made (create, fork, resume).
		if rec.DiskID == "" || (vm.State == domain.StateError && disk != storage.DiskID(rec.DiskID)) {
			return nil, domain.Errorf(domain.CodeInvalidState,
				"vm %s has no working disk to power-cycle; resume it from its last snapshot", vm.ID).
				WithDetail("state", vm.State).WithDetail("hint", "resume")
		}
		// A ready or running VM already holds its CPUs and memory, and its
		// VMM is gone before the new one starts; an error VM holds none.
		if !vm.State.Live() {
			if err := s.admit(tx, domain.Shape{CPUs: vm.Shape.CPUs, MemoryBytes: vm.Shape.MemoryBytes}, vm.Shape); err != nil {
				return nil, err
			}
		}
		op := &domain.Operation{ID: domain.NewOperationID(), Kind: domain.OpRecover, VMID: vm.ID, Phase: "queued",
			Params: domain.OpParams{Generation: vm.InstanceGeneration + 1, ReuseDisk: true}}
		if err := vm.Transition(domain.StateProvisioning, now); err != nil {
			return nil, err
		}
		vm.InstanceGeneration++
		vm.ActiveOperationID, vm.LastError, vm.BootMode = op.ID, nil, domain.BootModeNone
		vm.Readiness, vm.ExpiresAt = domain.Readiness{Services: map[string]bool{}}, nil
		// The disk stays: rec.DiskID is the one the new generation boots.
		rec.Handle = nil
		return op, tx.PutVM(rec)
	})
}

// workingDisk returns the disk ID of ref's working disk if it exists in
// storage, else "". It runs before the mutation transaction (storage is not
// called inside one) and never fails: the plan decides with what it found.
func (s *Service) workingDisk(ctx context.Context, principal domain.PrincipalID, ref string) storage.DiskID {
	var id string
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec, err := lookupVM(tx, principal, ref)
		if err == nil {
			id = rec.DiskID
		}
		return err
	}); err != nil || id == "" {
		return ""
	}
	l, err := s.cfg.Storage.OpenForRuntime(ctx, storage.DiskID(id))
	if err != nil {
		return ""
	}
	s.cfg.Storage.Release(ctx, l)
	return storage.DiskID(id)
}

// powerCycle ends every other generation of op's VM before recover boots
// its disk again: the VMM is killed (Destroy), never stopped or
// checkpointed, and the old generation's disk lease is released so the same
// disk can be opened for the new one.
func (s *Service) powerCycle(ctx context.Context, op *domain.Operation) error {
	unlock := pauseGuard(op.VMID) // not mid live-fork checkpoint of this VM
	killed, err := s.killOtherGenerations(ctx, op.VMID, op.Params.Generation)
	unlock()
	if err != nil {
		return err
	}
	s.releaseLease(ctx, op.VMID)
	s.event(ctx, op, "runtime.power-cycle", powerCycleMessage(killed))
	return nil
}

// killOtherGenerations destroys vm's instances of every generation but gen
// and reports whether one of them was still live.
func (s *Service) killOtherGenerations(ctx context.Context, vm domain.VMID, gen int64) (bool, error) {
	hs, err := s.cfg.Runtime.Discover(ctx)
	if err != nil {
		return false, err
	}
	killed := false
	for _, h := range hs {
		if h.VMID != vm || h.Generation == gen {
			continue
		}
		if st, err := s.cfg.Runtime.Inspect(ctx, h); err == nil && st.Phase != runtime.PhaseExited && st.Phase != runtime.PhaseGone {
			killed = true
		}
		if err := s.cfg.Runtime.Destroy(ctx, h); err != nil {
			return killed, err
		}
	}
	return killed, nil
}
