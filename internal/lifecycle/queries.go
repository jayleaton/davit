package lifecycle

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"davit/internal/domain"
	"davit/internal/state"
)

// Page is a list request.
type Page struct {
	Limit  int
	Cursor string
	// Labels filters ListVMs to VMs carrying every one of these labels.
	Labels map[string]string
}

func (p Page) state(filter ...string) state.Page {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	for _, k := range slices.Sorted(maps.Keys(p.Labels)) {
		filter = append(filter, "label:"+k+"="+p.Labels[k]) // a cursor belongs to its filter
	}
	return state.Page{Limit: min(p.Limit, 200), Cursor: p.Cursor, Filter: strings.Join(filter, "|"), Labels: p.Labels}
}

// GetVM returns a VM by ID or name, including tombstones by ID.
func (s *Service) GetVM(ctx context.Context, principal domain.PrincipalID, ref string) (vm *domain.VM, err error) {
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec, err := lookupVM(tx, principal, ref)
		if err == nil {
			vm = &rec.VM
			s.decorate(vm)
		}
		return err
	})
	return vm, err
}

// ListVMs lists principal's VMs in states (default provisioning, ready,
// running); all includes every state.
func (s *Service) ListVMs(ctx context.Context, principal domain.PrincipalID, states []domain.VMState, all bool, p Page) (out []domain.VM, next string, err error) {
	if all {
		states = nil
	} else if len(states) == 0 {
		states = domain.DefaultListStates
	}
	for _, st := range states {
		if !st.Valid() {
			return nil, "", domain.Errorf(domain.CodeInvalidArgument, "unknown state %q", st).WithDetail("field", "state")
		}
	}
	if err := domain.ValidateLabels(p.Labels); err != nil {
		return nil, "", domain.Errorf(domain.CodeInvalidArgument, "label filter: %s", domain.AsError(err).Message).WithDetail("field", "label")
	}
	sorted := slices.Clone(states)
	slices.Sort(sorted)
	filter := []string{string(principal)}
	for _, st := range sorted {
		filter = append(filter, string(st))
	}
	out = []domain.VM{}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		recs, n, err := tx.ListVMs(principal, states, p.state(filter...))
		for _, r := range recs {
			s.decorate(&r.VM)
			out = append(out, r.VM)
		}
		next = n
		return err
	})
	return out, next, err
}

// GetOperation returns an operation visible to principal.
func (s *Service) GetOperation(ctx context.Context, principal domain.PrincipalID, id domain.OperationID) (op *domain.Operation, err error) {
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		op, err = tx.GetOperation(id)
		if err == nil && op.Owner != principal {
			op, err = nil, domain.Errorf(domain.CodeNotFound, "operation %s not found", id)
		}
		return err
	})
	return op, err
}

// Events returns events of op after seq. With wait > 0 it long-polls until
// at least one event exists or the operation is terminal.
func (s *Service) Events(ctx context.Context, principal domain.PrincipalID, id domain.OperationID, after int64, wait time.Duration) (*domain.Operation, []domain.OperationEvent, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		changed := s.watch()
		op, err := s.GetOperation(ctx, principal, id)
		if err != nil {
			return nil, nil, err
		}
		var evs []domain.OperationEvent
		if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
			evs, err = tx.Events(id, after, 500)
			return err
		}); err != nil {
			return nil, nil, err
		}
		if len(evs) > 0 || op.State.Terminal() || wait <= 0 {
			if evs == nil {
				evs = []domain.OperationEvent{}
			}
			return op, evs, nil
		}
		select {
		case <-changed:
		case <-deadline.C:
			return op, []domain.OperationEvent{}, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-s.ctx.Done():
			return op, []domain.OperationEvent{}, nil
		}
	}
}

// GetSnapshot returns a snapshot (ID or name) visible to principal.
func (s *Service) GetSnapshot(ctx context.Context, principal domain.PrincipalID, ref string) (snap *domain.Snapshot, err error) {
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		snap, err = lookupSnapshot(tx, principal, ref)
		return err
	})
	return snap, err
}

// ListSnapshots lists principal's snapshots, optionally of one VM (ID or name).
func (s *Service) ListSnapshots(ctx context.Context, principal domain.PrincipalID, vmRef string, p Page) (out []domain.Snapshot, next string, err error) {
	out = []domain.Snapshot{}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		var vm domain.VMID
		if vmRef != "" {
			rec, err := lookupVM(tx, principal, vmRef)
			if err != nil {
				return err
			}
			vm = rec.VM.ID
		}
		snaps, n, err := tx.ListSnapshots(principal, vm, p.state(string(principal), string(vm)))
		for _, sn := range snaps {
			out = append(out, *sn)
		}
		next = n
		return err
	})
	return out, next, err
}

// ListImages returns one page of images in stable (createdAt, id) order.
func (s *Service) ListImages(ctx context.Context, p Page) ([]domain.Image, string, error) {
	imgs, err := s.cfg.Images.List(ctx)
	if err != nil {
		return nil, "", err
	}
	slices.SortFunc(imgs, func(a, b domain.Image) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
	sp := p.state("images")
	after, err := sp.Decode()
	if err != nil {
		return nil, "", err
	}
	out := []domain.Image{}
	for _, img := range imgs {
		if after.ID != "" && (img.CreatedAt.UnixNano() < after.CreatedNS ||
			img.CreatedAt.UnixNano() == after.CreatedNS && string(img.ID) <= after.ID) {
			continue
		}
		if len(out) == sp.Limit {
			last := out[len(out)-1]
			return out, state.EncodeCursor(last.CreatedAt.UnixNano(), string(last.ID), sp.Filter), nil
		}
		out = append(out, img)
	}
	return out, "", nil
}

// GetImage returns one image.
func (s *Service) GetImage(ctx context.Context, id domain.ImageID) (domain.Image, error) {
	return s.cfg.Images.Get(ctx, id)
}
