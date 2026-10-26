package lifecycle

import (
	"context"
	"fmt"
	"time"

	"davit/internal/domain"
	"davit/internal/state"
)

// Idle stops (VM.IdleTimeoutSeconds): a ready VM nobody uses is stopped,
// keeping its disk, so a forgotten project box does not hold host RAM.
// "Use" is exec, an attached process stream or an open tunnel (ssh): the
// daemon calls Touch when one starts or ends and Busy reports one still
// open. Activity lives in memory; after a daemon restart a VM counts from
// its last state change (UpdatedAt), so a restart never stops a box early
// before a full idle period.

// Touch records activity on vm now.
func (s *Service) Touch(vm domain.VMID) { s.touch(vm, s.now()) }

func (s *Service) touch(vm domain.VMID, at time.Time) {
	s.mu.Lock()
	if at.After(s.activity[vm]) {
		s.activity[vm] = at
	}
	s.mu.Unlock()
}

// lastActivity is the later of vm's last Touch and its last state change.
func (s *Service) lastActivity(vm domain.VM) time.Time {
	s.mu.Lock()
	at := s.activity[vm.ID]
	s.mu.Unlock()
	if vm.UpdatedAt.After(at) {
		at = vm.UpdatedAt
	}
	return at
}

// idleDue reports whether v's idle timeout has run out (Busy aside).
func (s *Service) idleDue(v domain.VM, now time.Time) bool {
	if v.IdleTimeoutSeconds <= 0 || v.ActiveOperationID != "" || (v.State != domain.StateReady && v.State != domain.StateRunning) {
		return false
	}
	return !now.Before(s.lastActivity(v).Add(time.Duration(v.IdleTimeoutSeconds) * time.Second))
}

// stopIdle stops the idle VMs that are not busy.
func (s *Service) stopIdle(ctx context.Context, idle []*state.VMRecord, now time.Time) {
	for _, r := range idle {
		if s.cfg.Busy != nil && s.cfg.Busy(r.VM.ID, r.VM.InstanceGeneration) {
			s.touch(r.VM.ID, now)
			continue
		}
		if s.leased(ctx, r.VM.ID, now) {
			continue
		}
		req := Request{
			Principal:      r.VM.Owner,
			IdempotencyKey: fmt.Sprintf("idle:%s:%d:%d", r.VM.ID, r.VM.InstanceGeneration, r.VM.StateRevision),
			Method:         "POST", Path: "internal/idle-stop",
		}
		if _, err := s.stop(ctx, req, string(r.VM.ID), false, "idle"); err != nil && !domain.IsCode(err, domain.CodeOperationInProgress) {
			s.cfg.Log.Warn("idle stop", "vm", r.VM.ID, "err", err)
		} else {
			s.cfg.Log.Info("idle; stopping", "vm", r.VM.ID, "name", r.VM.Name, "idleTimeoutSeconds", r.VM.IdleTimeoutSeconds)
		}
	}
}
