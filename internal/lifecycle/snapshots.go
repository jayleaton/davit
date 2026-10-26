package lifecycle

import (
	"context"
	"strings"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/state"
	"davit/internal/storage"
)

// lookupSnapshot resolves a snapshot ID or a named snapshot visible to
// principal. Other owners' snapshots are indistinguishable from absent ones.
func lookupSnapshot(tx *state.Tx, principal domain.PrincipalID, ref string) (*domain.Snapshot, error) {
	if ref == "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "snapshot ID or name is required")
	}
	var snap *domain.Snapshot
	var err error
	if strings.HasPrefix(ref, "snap_") {
		snap, err = tx.GetSnapshot(domain.SnapshotID(ref))
	} else {
		snap, err = tx.FindSnapshotByName(principal, ref)
	}
	if err != nil || snap.Owner != principal {
		return nil, domain.Errorf(domain.CodeNotFound, "snapshot %s not found", ref)
	}
	return snap, nil
}

// snapshotsInUse returns every snapshot something still depends on, with the
// reason: a VM's last committed disk, the only disk a VM can resume from (a
// VM created from a snapshot that has not committed its own yet), or the
// source of an unfinished operation. VM except is treated as already gone.
func snapshotsInUse(tx *state.Tx, except domain.VMID) (map[domain.SnapshotID]string, error) {
	use := map[domain.SnapshotID]string{}
	vms, err := tx.AllVMs()
	if err != nil {
		return nil, err
	}
	for _, r := range vms {
		v := r.VM
		if v.ID == except || v.State == domain.StateDeleted || v.State == domain.StateDeleting {
			continue
		}
		if v.LastSnapshotID != "" {
			use[v.LastSnapshotID] = "last committed disk of vm " + string(v.ID)
		} else if v.SourceSnapshotID != "" {
			use[v.SourceSnapshotID] = "resume source of vm " + string(v.ID)
		}
	}
	ops, err := tx.UnfinishedOperations()
	if err != nil {
		return nil, err
	}
	for _, o := range ops {
		if o.Params.SourceSnapshotID != "" {
			use[o.Params.SourceSnapshotID] = "source of unfinished operation " + string(o.ID)
		}
	}
	return use, nil
}

// pruneAutomatic drops vm's unnamed snapshots nothing depends on any more:
// superseded stop commits, or all of them once the VM is being deleted.
func pruneAutomatic(tx *state.Tx, vm domain.VMID, deleting bool) error {
	except := domain.VMID("")
	if deleting {
		except = vm
	}
	use, err := snapshotsInUse(tx, except)
	if err != nil {
		return err
	}
	snaps, err := tx.SnapshotsOfVM(vm)
	if err != nil {
		return err
	}
	for _, sn := range snaps {
		if _, busy := use[sn.ID]; sn.Name == "" && !busy {
			if err := tx.DeleteSnapshot(sn.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// collect garbage-collects storage against the current journal.
func (s *Service) collect(ctx context.Context) error {
	var keep storage.KeepSet
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		keep, err = keepSet(tx)
		return err
	}); err != nil {
		return err
	}
	if _, err := s.cfg.Storage.Collect(ctx, keep); err != nil {
		return err
	}
	// Goldens captured from a collected disk (a deleted project-image
	// generation) can never be restored again: free them now.
	if sw, ok := s.cfg.Runtime.(runtime.GoldenSweeper); ok {
		if _, _, err := sw.SweepGoldens(ctx); err != nil {
			s.cfg.Log.Warn("golden sweep after collection", "err", err)
		}
	}
	return nil
}

// Snapshot names the committed disk of a stopped VM (SPEC §3 `snapshot
// create`). It copies no data: the named snapshot shares the commit of the
// VM's last snapshot, so the returned operation has already succeeded.
func (s *Service) Snapshot(ctx context.Context, req Request, ref string, body api.SnapshotVMRequest) (*Accepted, error) {
	req.VMRef = ref
	return s.mutate(ctx, req, body, true, func(tx *state.Tx, now time.Time) (*domain.Operation, error) {
		if !domain.ValidName(body.Name) {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "invalid snapshot name %q: use 1-63 lowercase letters, digits and '-'", body.Name).
				WithDetail("field", "name")
		}
		rec, err := lookupVM(tx, req.Principal, ref)
		if err != nil {
			return nil, err
		}
		if err := checkMutable(rec, req); err != nil {
			return nil, err
		}
		vm := &rec.VM
		if err := keepsDisk(*vm, "snapshot"); err != nil {
			return nil, err
		}
		switch {
		case vm.State == domain.StateReady || vm.State == domain.StateRunning:
			return nil, domain.Errorf(domain.CodeStopRequired, "vm %s is %s; stop it first (live snapshots are not supported yet)", vm.ID, vm.State)
		case vm.State != domain.StateStopped:
			return nil, domain.Errorf(domain.CodeInvalidState, "cannot snapshot vm in state %s", vm.State).WithDetail("state", vm.State)
		case vm.LastSnapshotID == "":
			return nil, domain.Errorf(domain.CodeInternal, "stopped vm %s has no committed snapshot", vm.ID)
		}
		last, err := tx.GetSnapshot(vm.LastSnapshotID)
		if err != nil {
			return nil, err
		}
		named := *last
		named.ID, named.Name = domain.NewSnapshotID(), body.Name
		if err := tx.PutSnapshot(&named); err != nil {
			return nil, err
		}
		return &domain.Operation{
			ID: domain.NewOperationID(), Kind: domain.OpSnapshot, VMID: vm.ID, State: domain.OpSucceeded,
			Phase: "done", Progress: 1, Params: domain.OpParams{SnapshotName: body.Name, SnapshotID: named.ID},
			Result: &domain.OpResult{VMID: vm.ID, SnapshotID: named.ID},
		}, nil
	})
}

// DeleteSnapshot removes a snapshot record and collects its data once
// nothing references it. Snapshots something depends on are refused.
func (s *Service) DeleteSnapshot(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.Snapshot, error) {
	if principal == "" {
		return nil, domain.Errorf(domain.CodeUnauthenticated, "no principal")
	}
	var snap *domain.Snapshot
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		if snap, err = lookupSnapshot(tx, principal, ref); err != nil {
			return err
		}
		use, err := snapshotsInUse(tx, "")
		if err != nil {
			return err
		}
		if why, busy := use[snap.ID]; busy {
			return domain.Errorf(domain.CodeInvalidState, "snapshot %s is in use: %s", snap.ID, why).WithDetail("usedBy", why)
		}
		return tx.DeleteSnapshot(snap.ID)
	})
	if err != nil {
		return nil, err
	}
	s.notify()
	// Data shared with another snapshot stays; collection failure leaves only
	// unreferenced garbage that the next collection removes.
	if err := s.collect(ctx); err != nil {
		s.cfg.Log.Warn("collect after snapshot delete", "snapshot", snap.ID, "err", err)
	}
	return snap, nil
}
