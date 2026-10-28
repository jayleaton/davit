package ci

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"davit/internal/ci/github"
	"davit/internal/domain"
	"davit/internal/lifecycle"
	"davit/internal/state"
)

// Reconcile, at start and every ReconcileEvery: delete runner boxes no
// unfinished job record accounts for (a record pruned, or a box made by a
// create whose record was lost), and remove this host's idle runner
// registrations that no job will start (an attempt replaced after a
// restart, or a registration whose box never ran it). Busy runners are
// left alone: GitHub refuses to remove them, and their job ends them.

func (s *Service) reconcileLoop() {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		next := s.cfg.ReconcileEvery
		if !s.reconcile(s.ctx) {
			// Something could not be removed yet (a box still finishing an
			// operation after the restart, GitHub unreachable): soon again.
			next = min(next, 30*s.cfg.Retry)
		}
		t.Reset(next)
	}
}

// reconcile reports whether it got through everything.
func (s *Service) reconcile(ctx context.Context) bool {
	live := map[string]record{}
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		recs, err := loadRecords(tx, state.CIJobUnfinished, false, 0)
		for _, r := range recs {
			live[r.ID] = r
		}
		if err == nil {
			_, err = tx.PruneCIJobs(s.cfg.Clock.Now().Add(-s.cfg.Retention).UnixNano())
		}
		return err
	}); err != nil {
		s.cfg.Log.Warn("ci: reconcile", "err", err)
		return false
	}
	boxes := s.deleteOrphans(ctx)
	return s.removeIdleRunners(ctx, live) && boxes
}

// deleteOrphans deletes runner boxes that no unfinished record owns. Each
// box's record is read when the box is looked at: a job records itself
// before it creates its box, so a box made during the reconcile is never
// taken for an orphan.
func (s *Service) deleteOrphans(ctx context.Context) bool {
	cursor, ok := "", true
	for {
		vms, next, err := s.cfg.VMs.ListVMs(ctx, s.cfg.Owner, nil, true, lifecycle.Page{Limit: 200, Cursor: cursor, Labels: map[string]string{LabelCI: "github"}})
		if err != nil {
			s.cfg.Log.Warn("ci: reconcile: list runner boxes", "err", err)
			return false
		}
		for _, vm := range vms {
			if vm.State == domain.StateDeleted || vm.State == domain.StateDeleting {
				continue
			}
			owned, err := s.owns(ctx, vm.Labels[LabelJob], vm.ID)
			if err != nil {
				s.cfg.Log.Warn("ci: reconcile: read the box's job", "vm", vm.ID, "err", err)
				ok = false
				continue
			}
			if owned {
				continue
			}
			s.cfg.Log.Warn("ci: deleting an orphan runner box", "vm", vm.ID, "name", vm.Name, "job", vm.Labels[LabelJob])
			req := lifecycle.Request{Principal: s.cfg.Owner, IdempotencyKey: fmt.Sprintf("ci:orphan:%s:%d", vm.ID, vm.StateRevision),
				Method: http.MethodDelete, Path: "internal/ci/orphan/" + string(vm.ID)}
			if _, err := s.cfg.VMs.Delete(ctx, req, string(vm.ID)); err != nil && !domain.IsCode(err, domain.CodeNotFound) {
				s.cfg.Log.Warn("ci: reconcile: delete orphan box", "vm", vm.ID, "err", err)
				ok = false
			}
		}
		if next == "" || ctx.Err() != nil {
			return ok
		}
		cursor = next
	}
}

// owns reports whether the unfinished job id has (or is about to make) box vm.
func (s *Service) owns(ctx context.Context, id string, vm domain.VMID) (bool, error) {
	if id == "" {
		return false, nil
	}
	var r record
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error { return tx.GetCIJob(id, &r) })
	switch {
	case domain.IsCode(err, domain.CodeNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return r.Phase != phaseDone && (r.VMID == "" || r.VMID == vm), nil
}

// removeIdleRunners removes this host's idle runner registrations that no
// unfinished job owns: a registration of a finished or unknown job, or an
// attempt older than its job's current one.
func (s *Service) removeIdleRunners(ctx context.Context, live map[string]record) bool {
	ok := true
	type scoped struct {
		inst  int64
		scope github.Scope
	}
	scopes := map[string]scoped{}
	for name, p := range s.set.repos {
		sc := s.scope(name)
		scopes[sc.String()] = scoped{p.installation, sc}
	}
	for _, sc := range scopes {
		in := s.cfg.App.Installation(sc.inst)
		runners, err := in.Runners(ctx, sc.scope)
		if err != nil {
			s.cfg.Log.Warn("ci: reconcile: list runners", "scope", sc.scope.String(), "err", err)
			ok = false
			continue
		}
		for _, rn := range runners {
			id, attempt, mine := s.parseRunner(rn.Name)
			if !mine || rn.Busy {
				continue
			}
			if r, owned := live[id]; owned && (attempt >= r.Attempt || rn.ID == r.RunnerID) {
				continue
			}
			err := in.DeleteRunner(ctx, sc.scope, rn.ID)
			switch {
			case github.IsStatus(err, http.StatusUnprocessableEntity):
				// It just took a job.
			case err != nil:
				s.cfg.Log.Warn("ci: reconcile: remove idle runner", "runner", rn.Name, "err", err)
				ok = false
			default:
				s.cfg.Log.Info("ci: removed an idle runner registration no job owns", "runner", rn.Name, "scope", sc.scope.String())
			}
		}
	}
	return ok
}
