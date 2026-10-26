package state

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"davit/internal/domain"
)

// VMRecord is a VM plus its host-internal fields.
type VMRecord struct {
	VM             domain.VM
	Env            []string
	DiskID         string // current private writable disk
	RetainedDiskID string // working disk kept after a failed commit, for recovery
	Handle         json.RawMessage
	// Network is the network provider's handle for the current instance
	// generation (internal/network), persisted before any VMM uses it so a
	// crashed daemon can still release it.
	Network json.RawMessage
	// Manifest is a copy of the pinned environment revision's manifest
	// (VM.Environment), so preparation never depends on the environment
	// still existing.
	Manifest *domain.EnvManifest
}

type vmDoc struct {
	domain.VM
	Env      []string            `json:"env,omitempty"`
	Network  json.RawMessage     `json:"network,omitempty"`
	Manifest *domain.EnvManifest `json:"workspaceManifest,omitempty"`
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// PutVM inserts or replaces rec after checking domain invariants.
func (t *Tx) PutVM(rec *VMRecord) error {
	vm := &rec.VM
	if err := vm.CheckInvariants(); err != nil {
		return err
	}
	doc, err := json.Marshal(vmDoc{VM: *vm, Env: rec.Env, Network: rec.Network, Manifest: rec.Manifest})
	if err != nil {
		return err
	}
	var handle any
	if len(rec.Handle) > 0 {
		handle = string(rec.Handle)
	}
	_, err = t.exec(`
INSERT INTO vms (id, owner, name, state, generation, active_operation_id, created_ns, disk_id, retained_disk_id, runtime_handle, doc)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, state=excluded.state, generation=excluded.generation,
	active_operation_id=excluded.active_operation_id, disk_id=excluded.disk_id,
	retained_disk_id=excluded.retained_disk_id, runtime_handle=excluded.runtime_handle, doc=excluded.doc`,
		vm.ID, vm.Owner, vm.Name, vm.State, vm.InstanceGeneration, nullString(string(vm.ActiveOperationID)),
		vm.CreatedAt.UnixNano(), rec.DiskID, rec.RetainedDiskID, handle, string(doc))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: vms.owner, vms.name") {
		return domain.Errorf(domain.CodeInvalidArgument, "a VM named %q already exists", vm.Name).WithDetail("field", "name")
	}
	return err
}

const vmCols = `doc, disk_id, retained_disk_id, runtime_handle`

func scanVM(s interface{ Scan(...any) error }) (*VMRecord, error) {
	var doc, disk, retained string
	var handle sql.NullString
	if err := s.Scan(&doc, &disk, &retained, &handle); err != nil {
		return nil, err
	}
	var d vmDoc
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		return nil, err
	}
	rec := &VMRecord{VM: d.VM, Env: d.Env, DiskID: disk, RetainedDiskID: retained, Network: d.Network, Manifest: d.Manifest}
	if rec.VM.Egress.Mode == "" { // records from before egress modes
		rec.VM.Egress = domain.DefaultEgress()
	}
	if handle.Valid {
		rec.Handle = json.RawMessage(handle.String)
	}
	return rec, nil
}

// GetVM returns the VM with id, including tombstones.
func (t *Tx) GetVM(id domain.VMID) (*VMRecord, error) {
	rec, err := scanVM(t.row(`SELECT `+vmCols+` FROM vms WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("vm", id)
	}
	return rec, err
}

// FindVMByName returns owner's non-deleted VM called name.
func (t *Tx) FindVMByName(owner domain.PrincipalID, name string) (*VMRecord, error) {
	rec, err := scanVM(t.row(`SELECT `+vmCols+` FROM vms WHERE owner = ? AND name = ? AND state != 'deleted'`, owner, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("vm", name)
	}
	return rec, err
}

// AllVMs returns every VM record including tombstones (reconciliation, GC).
func (t *Tx) AllVMs() ([]*VMRecord, error) {
	rows, err := t.query(`SELECT ` + vmCols + ` FROM vms ORDER BY created_ns, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*VMRecord
	for rows.Next() {
		rec, err := scanVM(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Page is a keyset pagination request. Filter is an opaque string the cursor
// is bound to: a cursor issued for one filter is rejected for another.
type Page struct {
	Limit  int
	Cursor string
	Filter string
	// Labels keeps only VMs carrying every one of these labels (ListVMs).
	Labels map[string]string
}

type cursor struct {
	CreatedNS int64  `json:"t"`
	ID        string `json:"i"`
	Filter    string `json:"f"`
}

func (p Page) decode() (cursor, error) {
	if p.Cursor == "" {
		return cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(p.Cursor)
	var c cursor
	if err == nil {
		err = json.Unmarshal(raw, &c)
	}
	if err != nil || c.Filter != p.Filter {
		return cursor{}, domain.Errorf(domain.CodeInvalidArgument, "invalid cursor").WithDetail("field", "cursor")
	}
	return c, nil
}

// Cursor is a decoded page position.
type Cursor struct {
	CreatedNS int64
	ID        string
}

// Decode validates p's cursor (bound to p.Filter) for callers that page
// non-SQL collections the same way.
func (p Page) Decode() (Cursor, error) {
	c, err := p.decode()
	return Cursor{CreatedNS: c.CreatedNS, ID: c.ID}, err
}

// EncodeCursor makes the cursor for the page position after (createdNS, id).
func EncodeCursor(createdNS int64, id, filter string) string {
	return encodeCursor(createdNS, id, filter)
}

func encodeCursor(createdNS int64, id, filter string) string {
	raw, _ := json.Marshal(cursor{CreatedNS: createdNS, ID: id, Filter: filter})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// ListVMs returns one page of owner's VMs in states, ordered by (createdAt, id).
func (t *Tx) ListVMs(owner domain.PrincipalID, states []domain.VMState, p Page) ([]*VMRecord, string, error) {
	c, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	q := `SELECT ` + vmCols + `, created_ns, id FROM vms WHERE owner = ? AND (created_ns, id) > (?, ?)`
	args := []any{owner, c.CreatedNS, c.ID}
	if len(states) > 0 {
		q += ` AND state IN (?` + strings.Repeat(`, ?`, len(states)-1) + `)`
		for _, s := range states {
			args = append(args, s)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(p.Labels)) {
		// Keys are validated (domain.ValidateLabels): no quote can end the path.
		q += ` AND json_extract(doc, ?) = ?`
		args = append(args, `$.labels."`+k+`"`, p.Labels[k])
	}
	q += ` ORDER BY created_ns, id LIMIT ?`
	args = append(args, p.Limit+1)
	rows, err := t.query(q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*VMRecord
	var lastNS int64
	var lastID string
	for rows.Next() {
		var doc, disk, retained string
		var handle sql.NullString
		var ns int64
		var id string
		if err := rows.Scan(&doc, &disk, &retained, &handle, &ns, &id); err != nil {
			return nil, "", err
		}
		if len(out) == p.Limit {
			return out, encodeCursor(lastNS, lastID, p.Filter), rows.Err()
		}
		var d vmDoc
		if err := json.Unmarshal([]byte(doc), &d); err != nil {
			return nil, "", err
		}
		out = append(out, &VMRecord{VM: d.VM, Env: d.Env, DiskID: disk, RetainedDiskID: retained, Network: d.Network})
		lastNS, lastID = ns, id
	}
	return out, "", rows.Err()
}

// Allocated sums the shapes that currently hold host resources: CPU/RAM for
// live VMs, disk for every non-deleted VM.
func (t *Tx) Allocated() (domain.Shape, error) {
	vms, err := t.AllVMs()
	if err != nil {
		return domain.Shape{}, err
	}
	var s domain.Shape
	for _, r := range vms {
		if r.VM.State == domain.StateDeleted {
			continue
		}
		s.DiskBytes += r.VM.Shape.DiskBytes
		if r.VM.State.Live() {
			s.CPUs += r.VM.Shape.CPUs
			s.MemoryBytes += r.VM.Shape.MemoryBytes
		}
	}
	return s, nil
}

// PurgeTombstones removes deleted VMs (and their operations) whose deletion
// is older than before. Returns the number removed.
func (t *Tx) PurgeTombstones(before time.Time) (int, error) {
	rows, err := t.query(`SELECT id, doc FROM vms WHERE state = 'deleted'`)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id, doc string
		if err := rows.Scan(&id, &doc); err != nil {
			rows.Close()
			return 0, err
		}
		var d vmDoc
		if json.Unmarshal([]byte(doc), &d) == nil && d.DeletedAt != nil && d.DeletedAt.Before(before) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		for _, q := range []string{
			`DELETE FROM idempotency WHERE operation_id IN (SELECT id FROM operations WHERE vm_id = ?)`,
			`DELETE FROM operations WHERE vm_id = ?`,
			`DELETE FROM credential_grants WHERE vm_id = ?`,
			`DELETE FROM vms WHERE id = ?`,
		} {
			if _, err := t.exec(q, id); err != nil {
				return 0, err
			}
		}
	}
	return len(ids), nil
}

type opDoc struct {
	domain.Operation
	Params domain.OpParams `json:"params"`
}

// PutOperation inserts or replaces op. State changes must follow
// domain.CanTransitionOp; a terminal operation is immutable.
func (t *Tx) PutOperation(op *domain.Operation) error {
	var prev string
	err := t.row(`SELECT state FROM operations WHERE id = ?`, op.ID).Scan(&prev)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case prev != string(op.State) && !domain.CanTransitionOp(domain.OperationState(prev), op.State),
		domain.OperationState(prev).Terminal():
		return domain.Errorf(domain.CodeInternal, "operation %s: illegal state change %s -> %s", op.ID, prev, op.State)
	}
	doc, err := json.Marshal(opDoc{Operation: *op, Params: op.Params})
	if err != nil {
		return err
	}
	_, err = t.exec(`
INSERT INTO operations (id, vm_id, owner, kind, state, created_ns, doc) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state, doc=excluded.doc`,
		op.ID, op.VMID, op.Owner, op.Kind, op.State, op.CreatedAt.UnixNano(), string(doc))
	return err
}

func scanOp(s interface{ Scan(...any) error }) (*domain.Operation, error) {
	var doc string
	if err := s.Scan(&doc); err != nil {
		return nil, err
	}
	var d opDoc
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		return nil, err
	}
	op := d.Operation
	op.Params = d.Params
	return &op, nil
}

// GetOperation returns the operation with id.
func (t *Tx) GetOperation(id domain.OperationID) (*domain.Operation, error) {
	op, err := scanOp(t.row(`SELECT doc FROM operations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("operation", id)
	}
	return op, err
}

// UnfinishedOperations returns queued/running operations in creation order:
// the journal the daemon replays after a restart.
func (t *Tx) UnfinishedOperations() ([]*domain.Operation, error) {
	rows, err := t.query(`SELECT doc FROM operations WHERE state IN ('queued', 'running') ORDER BY created_ns, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Operation
	for rows.Next() {
		op, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// AppendEvent appends ev to its operation's stream, assigning the next seq.
func (t *Tx) AppendEvent(ev *domain.OperationEvent) error {
	if err := t.row(`SELECT COALESCE(MAX(seq), 0) + 1 FROM operation_events WHERE operation_id = ?`, ev.OperationID).Scan(&ev.Seq); err != nil {
		return err
	}
	doc, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO operation_events (operation_id, seq, doc) VALUES (?, ?, ?)`, ev.OperationID, ev.Seq, string(doc))
	return err
}

// Events returns up to limit events of op with seq > after.
func (t *Tx) Events(op domain.OperationID, after int64, limit int) ([]domain.OperationEvent, error) {
	rows, err := t.query(`SELECT doc FROM operation_events WHERE operation_id = ? AND seq > ? ORDER BY seq LIMIT ?`, op, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.OperationEvent
	for rows.Next() {
		var doc string
		var ev domain.OperationEvent
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(doc), &ev); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

type snapDoc struct {
	domain.Snapshot
	CommitID string `json:"commitId"`
}

// PutSnapshot inserts snap. Snapshots are immutable except for their name.
func (t *Tx) PutSnapshot(s *domain.Snapshot) error {
	doc, err := json.Marshal(snapDoc{Snapshot: *s, CommitID: s.CommitID})
	if err != nil {
		return err
	}
	_, err = t.exec(`
INSERT INTO snapshots (id, owner, source_vm_id, name, commit_id, created_ns, doc) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, doc=excluded.doc`,
		s.ID, s.Owner, s.SourceVMID, s.Name, s.CommitID, s.CreatedAt.UnixNano(), string(doc))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return domain.Errorf(domain.CodeInvalidArgument, "a snapshot named %q already exists", s.Name)
	}
	return err
}

func scanSnap(s interface{ Scan(...any) error }) (*domain.Snapshot, error) {
	var doc string
	if err := s.Scan(&doc); err != nil {
		return nil, err
	}
	var d snapDoc
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		return nil, err
	}
	snap := d.Snapshot
	snap.CommitID = d.CommitID
	return &snap, nil
}

// GetSnapshot returns the snapshot with id.
func (t *Tx) GetSnapshot(id domain.SnapshotID) (*domain.Snapshot, error) {
	s, err := scanSnap(t.row(`SELECT doc FROM snapshots WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("snapshot", id)
	}
	return s, err
}

// FindSnapshotByName returns owner's snapshot called name.
func (t *Tx) FindSnapshotByName(owner domain.PrincipalID, name string) (*domain.Snapshot, error) {
	s, err := scanSnap(t.row(`SELECT doc FROM snapshots WHERE owner = ? AND name = ?`, owner, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("snapshot", name)
	}
	return s, err
}

// DeleteSnapshot removes the snapshot row; data is removed by storage GC.
func (t *Tx) DeleteSnapshot(id domain.SnapshotID) error {
	_, err := t.exec(`DELETE FROM snapshots WHERE id = ?`, id)
	return err
}

// ListSnapshots returns one page of owner's snapshots, optionally only those
// taken from vm.
func (t *Tx) ListSnapshots(owner domain.PrincipalID, vm domain.VMID, p Page) ([]*domain.Snapshot, string, error) {
	c, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	q := `SELECT doc, created_ns, id FROM snapshots WHERE owner = ? AND (created_ns, id) > (?, ?)`
	args := []any{owner, c.CreatedNS, c.ID}
	if vm != "" {
		q += ` AND source_vm_id = ?`
		args = append(args, vm)
	}
	q += ` ORDER BY created_ns, id LIMIT ?`
	args = append(args, p.Limit+1)
	rows, err := t.query(q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*domain.Snapshot
	var lastNS int64
	var lastID string
	for rows.Next() {
		var doc, id string
		var ns int64
		if err := rows.Scan(&doc, &ns, &id); err != nil {
			return nil, "", err
		}
		if len(out) == p.Limit {
			return out, encodeCursor(lastNS, lastID, p.Filter), rows.Err()
		}
		s, err := scanSnap(staticRow{doc})
		if err != nil {
			return nil, "", err
		}
		out = append(out, s)
		lastNS, lastID = ns, id
	}
	return out, "", rows.Err()
}

type staticRow struct{ doc string }

func (r staticRow) Scan(dst ...any) error {
	if len(dst) != 1 {
		return fmt.Errorf("staticRow: want 1 dest, got %d", len(dst))
	}
	*(dst[0].(*string)) = r.doc
	return nil
}

// AllCommitIDs returns the storage commit of every snapshot row.
func (t *Tx) AllCommitIDs() ([]string, error) {
	rows, err := t.query(`SELECT commit_id FROM snapshots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SnapshotsOfVM returns every snapshot taken from vm.
func (t *Tx) SnapshotsOfVM(vm domain.VMID) ([]*domain.Snapshot, error) {
	rows, err := t.query(`SELECT doc FROM snapshots WHERE source_vm_id = ? ORDER BY created_ns, id`, vm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.Snapshot
	for rows.Next() {
		s, err := scanSnap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Idempotency is a reservation of (scope -> operation).
type Idempotency struct {
	Scope       string
	BodyHash    string
	OperationID domain.OperationID
	CreatedAt   time.Time
}

// GetIdempotency returns the record for scope, or nil if none.
func (t *Tx) GetIdempotency(scope string) (*Idempotency, error) {
	var r Idempotency
	var ns int64
	err := t.row(`SELECT scope, body_hash, operation_id, created_ns FROM idempotency WHERE scope = ?`, scope).
		Scan(&r.Scope, &r.BodyHash, &r.OperationID, &ns)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	r.CreatedAt = time.Unix(0, ns).UTC()
	return &r, err
}

// PutIdempotency records a reservation. It must be written in the same
// transaction as the operation it points to.
func (t *Tx) PutIdempotency(r *Idempotency) error {
	_, err := t.exec(`INSERT INTO idempotency (scope, body_hash, operation_id, created_ns) VALUES (?, ?, ?, ?)`,
		r.Scope, r.BodyHash, r.OperationID, r.CreatedAt.UnixNano())
	return err
}

// ExpireIdempotency starts the retention clock for op's keys once it is terminal.
func (t *Tx) ExpireIdempotency(op domain.OperationID, at time.Time) error {
	_, err := t.exec(`UPDATE idempotency SET expires_ns = ? WHERE operation_id = ? AND expires_ns IS NULL`, at.UnixNano(), op)
	return err
}

// PurgeIdempotency removes keys whose retention ended before now.
func (t *Tx) PurgeIdempotency(now time.Time) (int64, error) {
	res, err := t.exec(`DELETE FROM idempotency WHERE expires_ns IS NOT NULL AND expires_ns < ?`, now.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SnapshotByCommit returns a snapshot of commit id, or nil if none. Several
// may share one (a stop's automatic snapshot and a named one): a named
// snapshot wins, then the oldest.
func (t *Tx) SnapshotByCommit(commit string) (*domain.Snapshot, error) {
	s, err := scanSnap(t.row(`SELECT doc FROM snapshots WHERE commit_id = ? ORDER BY name = '', created_ns, id LIMIT 1`, commit))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}
