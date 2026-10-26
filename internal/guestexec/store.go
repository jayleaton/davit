package guestexec

import (
	"context"

	"davit/internal/domain"
	"davit/internal/state"
)

// Store keeps process records. GetProcess returns a CodeNotFound error for
// an unknown ID; ProcessByKey returns nil. ReplaceProcess atomically swaps
// the record oldID for p (a reservation for the started process).
type Store interface {
	PutProcess(ctx context.Context, p *state.ProcessRecord) error
	ReplaceProcess(ctx context.Context, oldID string, p *state.ProcessRecord) error
	DeleteProcess(ctx context.Context, id string) error
	GetProcess(ctx context.Context, id string) (*state.ProcessRecord, error)
	ProcessByKey(ctx context.Context, owner domain.PrincipalID, vm domain.VMID, key string) (*state.ProcessRecord, error)
	ListProcesses(ctx context.Context, owner domain.PrincipalID, vm domain.VMID, p state.Page) ([]*state.ProcessRecord, string, error)
	PruneProcesses(ctx context.Context, vm domain.VMID, keep int) error
}

// DBStore is the SQLite Store.
type DBStore struct{ DB *state.DB }

func (s DBStore) PutProcess(ctx context.Context, p *state.ProcessRecord) error {
	return s.DB.Tx(ctx, func(tx *state.Tx) error { return tx.PutProcess(p) })
}

func (s DBStore) ReplaceProcess(ctx context.Context, oldID string, p *state.ProcessRecord) error {
	return s.DB.Tx(ctx, func(tx *state.Tx) error {
		if err := tx.DeleteProcess(oldID); err != nil {
			return err
		}
		return tx.PutProcess(p)
	})
}

func (s DBStore) DeleteProcess(ctx context.Context, id string) error {
	return s.DB.Tx(ctx, func(tx *state.Tx) error { return tx.DeleteProcess(id) })
}

func (s DBStore) GetProcess(ctx context.Context, id string) (p *state.ProcessRecord, err error) {
	err = s.DB.Tx(ctx, func(tx *state.Tx) error { p, err = tx.GetProcess(id); return err })
	return p, err
}

func (s DBStore) ProcessByKey(ctx context.Context, owner domain.PrincipalID, vm domain.VMID, key string) (p *state.ProcessRecord, err error) {
	err = s.DB.Tx(ctx, func(tx *state.Tx) error { p, err = tx.ProcessByKey(owner, vm, key); return err })
	return p, err
}

func (s DBStore) ListProcesses(ctx context.Context, owner domain.PrincipalID, vm domain.VMID, p state.Page) (ps []*state.ProcessRecord, next string, err error) {
	err = s.DB.Tx(ctx, func(tx *state.Tx) error { ps, next, err = tx.ListProcesses(owner, vm, p); return err })
	return ps, next, err
}

func (s DBStore) PruneProcesses(ctx context.Context, vm domain.VMID, keep int) error {
	return s.DB.Tx(ctx, func(tx *state.Tx) error { return tx.PruneProcesses(vm, keep) })
}
