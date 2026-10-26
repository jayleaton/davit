package lifecycle

import (
	"context"
	"encoding/json"
	"slices"

	"davit/internal/domain"
	"davit/internal/network"
	"davit/internal/state"
)

// egressOr normalizes e, or returns def when e is nil.
func egressOr(e *domain.Egress, def domain.Egress) (domain.Egress, error) {
	if e == nil {
		return def, nil
	}
	return e.Normalize()
}

// egressChange is a new egress policy already applied to a VM's live network
// (if it has one) and not yet recorded.
type egressChange struct {
	vm      domain.VMID
	gen     int64
	policy  domain.Egress
	prev    domain.Egress
	applied *network.Handle // live network now enforcing policy, or nil
	handle  json.RawMessage // applied, encoded for the record
}

// applyEgress validates e and applies it to ref's live network. The live
// network is updated before the policy is recorded (Update), so the record
// never claims a restriction that is not in force. It is refused while a
// lifecycle operation runs (the operation may be creating or tearing down
// the network).
func (s *Service) applyEgress(ctx context.Context, req Request, ref string, e domain.Egress) (*egressChange, error) {
	e, err := e.Normalize()
	if err != nil {
		return nil, err
	}
	var rec *state.VMRecord
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		if rec, err = lookupVM(tx, req.Principal, ref); err != nil {
			return err
		}
		if rec.VM.State == domain.StateDeleting {
			return domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
		}
		return checkMutable(rec, req)
	}); err != nil {
		return nil, err
	}
	c := &egressChange{vm: rec.VM.ID, gen: rec.VM.InstanceGeneration, policy: e, prev: rec.VM.Egress}
	if h := decodeNetwork(rec.Network); h != nil && h.Generation == c.gen && rec.VM.State.Live() && !sameEgress(h.Policy, e) {
		nh, err := s.cfg.Network.SetEgress(ctx, *h, e)
		if err != nil {
			s.cfg.Log.Warn("set egress", "vm", c.vm, "generation", c.gen, "err", err)
			de := domain.Errorf(domain.CodeUnavailable, "could not apply the egress policy to the running VM: %v", err)
			de.Retryable = true
			return nil, de
		}
		c.applied = &nh
		c.handle, _ = json.Marshal(nh)
	}
	return c, nil
}

// record stores c in rec (inside Update's transaction) and reports whether
// anything changed. A lifecycle operation that started after applyEgress
// would have read the old policy (and may have built a network with it), so
// the caller is told to retry.
func (c *egressChange) record(rec *state.VMRecord) (bool, error) {
	if rec.VM.ID != c.vm || rec.VM.InstanceGeneration != c.gen || rec.VM.ActiveOperationID != "" {
		e := domain.Errorf(domain.CodeOperationInProgress, "vm %s changed while its egress was being set; retry", rec.VM.ID)
		e.Retryable = true
		return false, e
	}
	if c.handle != nil {
		rec.Network = c.handle
	}
	if sameEgress(rec.VM.Egress, c.policy) {
		return c.handle != nil, nil
	}
	rec.VM.Egress = c.policy
	return true, nil
}

// undo restores the previous policy on the live network after the record
// could not be written (best effort: the record still holds the old policy
// and handle).
func (s *Service) undoEgress(ctx context.Context, c *egressChange) {
	if c == nil || c.applied == nil {
		return
	}
	if _, err := s.cfg.Network.SetEgress(context.WithoutCancel(ctx), *c.applied, c.prev); err != nil {
		s.cfg.Log.Error("restore egress after a failed update", "vm", c.vm, "err", err)
	}
}

func sameEgress(a, b domain.Egress) bool {
	return a.Mode == b.Mode && slices.Equal(a.Allow, b.Allow)
}
