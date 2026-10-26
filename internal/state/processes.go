package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"davit/internal/domain"
)

// ProcessRecord is a guest process started through the API: who started
// it, on which instance, and the last observed outcome. Live session
// handles (stdin, streams) are not durable.
type ProcessRecord struct {
	ID         string             `json:"id"`
	VMID       domain.VMID        `json:"vmId"`
	Owner      domain.PrincipalID `json:"owner"`
	Generation int64              `json:"generation"`
	Argv       []string           `json:"argv"`
	Detached   bool               `json:"detached"`
	CreatedAt  time.Time          `json:"createdAt"`
	// Handle is the durable job handle the guest agent knows the process
	// by (guestproto feature "jobs"; the reservation ID), "" without jobs.
	// Stored in the processes.handle column too.
	Handle string `json:"handle,omitempty"`
	// IdempotencyKey and BodyHash dedupe execProcess (owner, VM, key).
	IdempotencyKey string `json:"-"`
	BodyHash       string `json:"-"`

	Status       string     `json:"status"`
	ExitCode     *int       `json:"exitCode,omitempty"`
	Signal       string     `json:"signal,omitempty"`
	TimedOut     bool       `json:"timedOut,omitempty"`
	OutputCursor int64      `json:"outputCursor"`
	ExitedAt     *time.Time `json:"exitedAt,omitempty"`
}

// PutProcess inserts or updates p. A second process with the same
// (owner, VM, idempotency key) is an idempotency conflict.
func (t *Tx) PutProcess(p *ProcessRecord) error {
	doc, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = t.exec(`
INSERT INTO processes (id, vm_id, owner, generation, created_ns, idempotency_key, body_hash, handle, doc)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET handle=excluded.handle, doc=excluded.doc`,
		p.ID, p.VMID, p.Owner, p.Generation, p.CreatedAt.UnixNano(), nullString(p.IdempotencyKey), p.BodyHash, nullString(p.Handle), string(doc))
	if err != nil && isUnique(err) {
		return domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key was already used for another process")
	}
	if err != nil {
		return err
	}
	// A process that runs under noOutputRetention has its output withheld
	// even if no writer ever offers any (guestexec does not collect it).
	if ok, err := t.outputRetained(p.ID); err != nil || ok {
		return err
	}
	return t.markWithheld(p.ID)
}

func isUnique(err error) bool { return strings.Contains(err.Error(), "UNIQUE constraint failed") }

const procCols = `doc, idempotency_key, body_hash`

func scanProcess(s interface{ Scan(...any) error }) (*ProcessRecord, error) {
	var doc, hash string
	var key sql.NullString
	if err := s.Scan(&doc, &key, &hash); err != nil {
		return nil, err
	}
	return decodeProcess(doc, key, hash)
}

func decodeProcess(doc string, key sql.NullString, hash string) (*ProcessRecord, error) {
	p := new(ProcessRecord)
	if err := json.Unmarshal([]byte(doc), p); err != nil {
		return nil, err
	}
	p.IdempotencyKey, p.BodyHash = key.String, hash
	return p, nil
}

// GetProcess returns process id.
func (t *Tx) GetProcess(id string) (*ProcessRecord, error) {
	p, err := scanProcess(t.row(`SELECT `+procCols+` FROM processes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("process", id)
	}
	return p, err
}

// ProcessByKey returns the process owner started on vm with key, or nil.
func (t *Tx) ProcessByKey(owner domain.PrincipalID, vm domain.VMID, key string) (*ProcessRecord, error) {
	p, err := scanProcess(t.row(`SELECT `+procCols+` FROM processes WHERE owner = ? AND vm_id = ? AND idempotency_key = ?`, owner, vm, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// ListProcesses returns one page of owner's processes on vm in stable
// (createdAt, id) order, like every other list.
func (t *Tx) ListProcesses(owner domain.PrincipalID, vm domain.VMID, p Page) ([]*ProcessRecord, string, error) {
	c, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	rows, err := t.query(`SELECT `+procCols+`, created_ns, id FROM processes WHERE owner = ? AND vm_id = ? AND (created_ns, id) > (?, ?)
ORDER BY created_ns, id LIMIT ?`, owner, vm, c.CreatedNS, c.ID, p.Limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []*ProcessRecord{}
	var lastNS int64
	var lastID string
	for rows.Next() {
		var doc, hash, id string
		var key sql.NullString
		var ns int64
		if err := rows.Scan(&doc, &key, &hash, &ns, &id); err != nil {
			return nil, "", err
		}
		if len(out) == p.Limit {
			return out, encodeCursor(lastNS, lastID, p.Filter), rows.Err()
		}
		pr, err := decodeProcess(doc, key, hash)
		if err != nil {
			return nil, "", err
		}
		out = append(out, pr)
		lastNS, lastID = ns, id
	}
	return out, "", rows.Err()
}

// DeleteProcess removes process id's record (no error if absent).
func (t *Tx) DeleteProcess(id string) error {
	_, err := t.exec(`DELETE FROM processes WHERE id = ?`, id)
	return err
}

// PruneProcesses keeps the newest keep processes of vm.
func (t *Tx) PruneProcesses(vm domain.VMID, keep int) error {
	_, err := t.exec(`DELETE FROM processes WHERE vm_id = ? AND id NOT IN
	(SELECT id FROM processes WHERE vm_id = ? ORDER BY created_ns DESC, id DESC LIMIT ?)`, vm, vm, keep)
	return err
}
