package lifecycle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

// Making room for a VM (feature capacityReclaim). When admission refuses a
// create, fork or resume for memory, the live VM count or CPU
// (domain.IsRoomError; disk never), the host first stops idle VMs, oldest
// activity first, and retries after each stop. A VM is reclaimable when it
// has an idle timeout (it agreed to be stopped when unused), is ready with
// no operation, runs no process, has no stream or tunnel open, holds no
// prompt-turn lease and has had no activity for Limits.ReclaimIdleAfter().
// Ephemeral and no-snapshot VMs are never reclaimed: their stop would lose
// the disk. Each reclaim is a stop with Operation.reason "reclaim", logged
// and visible as operation.created in the event stream.
//
// If there is still no room, a request with a capacity wait (createVM and
// resumeVM waitSeconds) joins a FIFO queue. Only the head retries admission
// (reclaiming again, since boxes go idle meanwhile), at least every
// waitPoll, until it is admitted or its wait ends; then the last refusal is
// returned. While anyone waits, a new request queues behind them, so a
// stream of small VMs cannot starve a large one; one that may not wait
// fails at once.

// waitPoll is how often the head of the queue retries admission.
const waitPoll = 500 * time.Millisecond

// maxReclaims bounds the VMs stopped for one admission attempt.
const maxReclaims = 32

type waiter struct{ domain.CapacityWaiter }

// withRoom runs try (an admission) and makes room or waits up to wait as
// described above.
func (s *Service) withRoom(ctx context.Context, req Request, wait time.Duration, try func() (*Accepted, error)) (*Accepted, error) {
	ahead := s.queueLen()
	var last error
	if ahead == 0 {
		acc, err := s.reclaimFor(ctx, try)
		if wait <= 0 || !domain.IsRoomError(err) {
			return acc, err
		}
		last = err
	} else if wait <= 0 {
		e := domain.Errorf(domain.CodeResourceExhausted,
			"%d requests are waiting for host capacity ahead of this one; retry with a wait (waitSeconds) to queue behind them", ahead)
		e.Retryable = true
		return nil, e.WithDetail("resource", "queue").WithDetail("waiting", ahead)
	}
	w := s.enqueue(req, last)
	defer s.dequeue(w)
	s.cfg.Log.Info("waiting for host capacity", "ticket", w.Ticket, "ahead", ahead, "memoryBytes", w.MemoryBytes, "wait", wait)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(waitPoll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			return nil, domain.Errorf(domain.CodeUnavailable, "daemon is shutting down")
		case <-deadline.C:
			return nil, waitedOut(last, wait, s.queuePos(w))
		case <-poll.C:
		}
		if s.queuePos(w) > 0 {
			continue
		}
		acc, err := s.reclaimFor(ctx, try)
		if !domain.IsRoomError(err) {
			if err == nil {
				s.cfg.Log.Info("admitted after waiting for host capacity", "ticket", w.Ticket, "waited", s.now().Sub(w.Since).Round(time.Millisecond))
			}
			return acc, err
		}
		last = err
	}
}

// waitedOut is the refusal after a capacity wait ran out.
func waitedOut(last error, wait time.Duration, ahead int) error {
	msg := "no host capacity"
	e := &domain.Error{Code: domain.CodeResourceExhausted}
	if le, ok := last.(*domain.Error); ok && le != nil {
		msg, e.Details = le.Message, maps.Clone(le.Details)
	}
	if ahead > 0 {
		msg = fmt.Sprintf("%d requests were still ahead in the queue", ahead)
	}
	e.Message, e.Retryable = fmt.Sprintf("waited %s for host capacity: %s", wait, msg), true
	if e.Details["resource"] == nil {
		e = e.WithDetail("resource", "queue")
	}
	return e.WithDetail("waitedSeconds", int64(wait/time.Second))
}

// enqueue appends a waiter for req; last (an admission refusal, or nil)
// carries the memory it needs.
func (s *Service) enqueue(req Request, last error) *waiter {
	key := req.IdempotencyKey
	if key == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		key = hex.EncodeToString(b)
	}
	w := &waiter{domain.CapacityWaiter{Ticket: domain.CapacityTicket(key), MemoryBytes: vmMemory(last), Since: s.now()}}
	s.mu.Lock()
	s.queue = append(s.queue, w)
	s.mu.Unlock()
	return w
}

func (s *Service) dequeue(w *waiter) {
	s.mu.Lock()
	if i := slices.Index(s.queue, w); i >= 0 {
		s.queue = slices.Delete(s.queue, i, i+1)
	}
	s.mu.Unlock()
}

func (s *Service) queueLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// queuePos is how many waiters are ahead of w.
func (s *Service) queuePos(w *waiter) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return max(0, slices.Index(s.queue, w))
}

func (s *Service) waiting() []domain.CapacityWaiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.CapacityWaiter, len(s.queue))
	for i, w := range s.queue {
		out[i] = w.CapacityWaiter
	}
	return out
}

// vmMemory is the memory an admission refusal says the VM needs (0 if it
// does not say).
func vmMemory(err error) int64 {
	if e, ok := err.(*domain.Error); ok && e != nil {
		if n, ok := e.Details["vmMemoryBytes"].(int64); ok {
			return n
		}
	}
	return 0
}

// reclaimFor runs try, and while it is refused for room stops one more
// reclaimable VM and retries. Concurrent admissions reclaim one at a time,
// so two of them never stop two VMs for one shortfall.
func (s *Service) reclaimFor(ctx context.Context, try func() (*Accepted, error)) (*Accepted, error) {
	acc, err := try()
	if !domain.IsRoomError(err) || s.cfg.Limits.ReclaimIdleAfter() == 0 {
		return acc, err
	}
	s.reclaimMu.Lock()
	defer s.reclaimMu.Unlock()
	acc, err = try() // another admission may have made room meanwhile
	for n := 0; domain.IsRoomError(err) && n < maxReclaims; n++ {
		vms, rerr := s.reclaimable(ctx, reclaimQuery{idle: s.cfg.Limits.ReclaimIdleAfter(), needTimeout: true})
		if rerr != nil {
			s.cfg.Log.Warn("reclaim: list idle VMs", "err", rerr)
			break
		}
		if len(vms) == 0 {
			break
		}
		if rerr := s.reclaimStop(ctx, vms[0]); rerr != nil {
			s.cfg.Log.Warn("reclaim: stop", "vm", vms[0].ID, "err", rerr)
			break
		}
		acc, err = try()
	}
	return acc, err
}

// reclaimQuery selects reclaimable VMs.
type reclaimQuery struct {
	idle        time.Duration      // at least this long without activity
	needTimeout bool               // only VMs with an idle timeout
	owner       domain.PrincipalID // only this owner's ("": any)
	labels      map[string]string  // only VMs carrying all of these
}

// reclaimable returns the VMs q selects that may be stopped now, oldest
// activity first.
func (s *Service) reclaimable(ctx context.Context, q reclaimQuery) ([]domain.VM, error) {
	var vms []domain.VM
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		all, err := tx.AllVMs()
		if err != nil {
			return err
		}
		vms = vms[:0]
		for _, r := range all {
			vms = append(vms, r.VM)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	now := s.now()
	out := vms[:0]
	for _, v := range vms {
		if (v.State != domain.StateReady && v.State != domain.StateRunning) || v.ActiveOperationID != "" ||
			v.Privacy.Ephemeral || !v.Privacy.KeepsDisk() ||
			(q.needTimeout && v.IdleTimeoutSeconds <= 0) || (q.owner != "" && v.Owner != q.owner) ||
			now.Sub(s.lastActivity(v)) < q.idle || !hasLabels(v.Labels, q.labels) {
			continue
		}
		if s.cfg.Busy != nil && s.cfg.Busy(v.ID, v.InstanceGeneration) {
			continue
		}
		if s.leased(ctx, v.ID, now) {
			continue
		}
		out = append(out, v)
	}
	slices.SortStableFunc(out, func(a, b domain.VM) int { return s.lastActivity(a).Compare(s.lastActivity(b)) })
	return out, nil
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// reclaimStop stops vm to make room and waits until the stop has finished.
func (s *Service) reclaimStop(ctx context.Context, vm domain.VM) error {
	idle := s.now().Sub(s.lastActivity(vm)).Round(time.Second)
	req := Request{
		Principal:      vm.Owner,
		IdempotencyKey: fmt.Sprintf("reclaim:%s:%d:%d", vm.ID, vm.InstanceGeneration, vm.StateRevision),
		Method:         "POST", Path: "internal/reclaim-stop",
	}
	acc, err := s.stop(ctx, req, string(vm.ID), false, "reclaim")
	if err != nil {
		return err
	}
	s.cfg.Log.Info("stopping idle VM to make room", "vm", vm.ID, "name", vm.Name, "idle", idle,
		"memoryBytes", vm.Shape.MemoryBytes, "operation", acc.Operation.ID)
	return s.waitOperation(ctx, acc.Operation.ID)
}

// waitOperation returns once operation id is terminal; its failure is an
// error.
func (s *Service) waitOperation(ctx context.Context, id domain.OperationID) error {
	for {
		changed := s.watch()
		var op *domain.Operation
		if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
			op, err = tx.GetOperation(id)
			return err
		}); err != nil {
			return err
		}
		if op.State.Terminal() {
			if op.Error != nil {
				return op.Error
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			return domain.Errorf(domain.CodeUnavailable, "daemon is shutting down")
		case <-changed:
		}
	}
}

// Reclaim stops the caller's VMs that have been idle at least
// body.IdleSeconds (reclaimVMs; `davit box gc`), oldest activity first,
// with the same exclusions as making room, except that it needs no idle
// timeout. It returns at once with the stop operations.
func (s *Service) Reclaim(ctx context.Context, principal domain.PrincipalID, body api.ReclaimVMsRequest) (*api.ReclaimVMsResult, error) {
	if principal == "" {
		return nil, domain.Errorf(domain.CodeUnauthenticated, "no principal")
	}
	if body.IdleSeconds < 0 {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "idleSeconds must not be negative").WithDetail("field", "idleSeconds")
	}
	if err := domain.ValidateLabels(body.Labels); err != nil {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "labels: %s", domain.AsError(err).Message).WithDetail("field", "labels")
	}
	vms, err := s.reclaimable(ctx, reclaimQuery{idle: time.Duration(body.IdleSeconds) * time.Second, owner: principal, labels: body.Labels})
	if err != nil {
		return nil, err
	}
	res := &api.ReclaimVMsResult{Items: []api.ReclaimedVM{}}
	now := s.now()
	for _, v := range vms {
		it := api.ReclaimedVM{VMID: v.ID, Name: v.Name, IdleSeconds: int64(now.Sub(s.lastActivity(v)) / time.Second), MemoryBytes: v.Shape.MemoryBytes}
		if !body.DryRun {
			req := Request{
				Principal:      v.Owner,
				IdempotencyKey: fmt.Sprintf("gc:%s:%d:%d", v.ID, v.InstanceGeneration, v.StateRevision),
				Method:         "POST", Path: "internal/gc-stop",
			}
			acc, err := s.stop(ctx, req, string(v.ID), false, "gc")
			if err != nil {
				if domain.IsCode(err, domain.CodeOperationInProgress) || domain.IsCode(err, domain.CodeInvalidState) {
					continue // it changed since it was listed
				}
				return nil, err
			}
			s.cfg.Log.Info("stopping idle VM (reclaimVMs)", "vm", v.ID, "name", v.Name, "idleSeconds", it.IdleSeconds, "operation", acc.Operation.ID)
			it.OperationID = acc.Operation.ID
		}
		res.Items = append(res.Items, it)
	}
	return res, nil
}
