package lifecycle

import (
	"context"

	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/state"
)

// BootInfo is what the restore barrier needs to know about the boot in
// progress for an instance key.
type BootInfo struct {
	// Name is the VM's name, or "" when unnamed.
	Name string
	// Kind is the operation booting the instance (create, fork, resume), or
	// "" when none is active (e.g. a re-probe after daemon restart).
	Kind domain.OperationKind
}

// BootInfo describes the VM booting as key. It is for host-internal
// wiring (the readiness prober's plan), not for principals.
func (s *Service) BootInfo(ctx context.Context, key runtime.Key) (BootInfo, error) {
	var info BootInfo
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec, err := tx.GetVM(key.VMID)
		if err != nil {
			return err
		}
		info.Name = rec.VM.Name
		if rec.VM.ActiveOperationID != "" && rec.VM.InstanceGeneration == key.Generation {
			op, err := tx.GetOperation(rec.VM.ActiveOperationID)
			if err != nil {
				return err
			}
			info.Kind = op.Kind
		}
		return nil
	})
	return info, err
}

// GuestTarget is an authorized route to a ready VM's guest agent.
type GuestTarget struct {
	Key    runtime.Key
	Dialer runtime.Dialer
	// Privacy is the VM's privacy policy when the target was resolved, so
	// guest access can skip work the policy discards (G17). It is advisory:
	// the state store enforces the policy on every write.
	Privacy domain.PrivacyPolicy
}

// GuestTarget resolves ref for principal and returns its current
// instance's guest transport. The VM must be ready or running: guest
// access is admitted only after the restore barrier published readiness.
func (s *Service) GuestTarget(ctx context.Context, principal domain.PrincipalID, ref string) (GuestTarget, error) {
	var rec *state.VMRecord
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		rec, err = lookupVM(tx, principal, ref)
		return err
	}); err != nil {
		return GuestTarget{}, err
	}
	vm := rec.VM
	if vm.State == domain.StateDeleted {
		return GuestTarget{}, domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
	}
	if vm.State != domain.StateReady && vm.State != domain.StateRunning {
		return GuestTarget{}, domain.Errorf(domain.CodeInvalidState, "vm %s is %s; guest access needs ready or running", vm.ID, vm.State).
			WithDetail("state", vm.State)
	}
	h, err := decodeHandle(rec.Handle)
	if err != nil {
		return GuestTarget{}, err
	}
	if h == nil || h.Generation != vm.InstanceGeneration {
		return GuestTarget{}, domain.Errorf(domain.CodeUnavailable, "vm %s has no live instance", vm.ID)
	}
	d, err := s.cfg.Runtime.GuestTransport(ctx, *h)
	if err != nil {
		return GuestTarget{}, err
	}
	return GuestTarget{Key: h.Key, Dialer: d, Privacy: vm.Privacy}, nil
}

// DefaultUser is the default login account of vm's image ("" if unknown).
// Host-internal wiring (gateway SSH), not for principals.
func (s *Service) DefaultUser(ctx context.Context, vm domain.VMID) string {
	rec, err := s.loadVM(ctx, vm)
	if err != nil {
		return ""
	}
	img, err := s.cfg.Images.Get(ctx, rec.VM.ImageID)
	if err != nil {
		return ""
	}
	return img.DefaultUser
}
