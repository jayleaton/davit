package state

import (
	"database/sql"
	"encoding/json"
	"errors"

	"davit/api"
	"davit/internal/domain"
)

// Run orchestration records (G6). The runs table keeps one JSON document per
// run; its orchestrator (internal/runs) stores the api.Run fields plus what
// it needs to resume after a restart, and GetRun reads the api.Run part.

// RunRef names one run.
type RunRef struct {
	Owner domain.PrincipalID
	ID    string
}

// PutRunDoc inserts or updates run r with doc as its stored document. doc
// must marshal to a JSON object holding every api.Run field (for example a
// struct embedding api.Run). A second run with the same (owner, key) is an
// idempotency conflict.
func (t *Tx) PutRunDoc(owner domain.PrincipalID, key, bodyHash string, r *api.Run, doc any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO runs (id, owner, state, idempotency_key, body_hash, created_ns, doc) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state, doc=excluded.doc`,
		r.ID, owner, r.State, nullString(key), bodyHash, r.CreatedAt.UnixNano(), string(raw))
	if err != nil && isUnique(err) {
		return domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key was already used for another run")
	}
	return err
}

// GetRunDoc decodes owner's run document into doc.
func (t *Tx) GetRunDoc(owner domain.PrincipalID, id string, doc any) error {
	var raw string
	err := t.row(`SELECT doc FROM runs WHERE id = ? AND owner = ?`, id, owner).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Errorf(domain.CodeNotFound, "run %s not found", id)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), doc)
}

// RunByKey returns the ID and body hash of owner's run created with
// Idempotency-Key key, or "" when there is none.
func (t *Tx) RunByKey(owner domain.PrincipalID, key string) (id, bodyHash string, err error) {
	err = t.row(`SELECT id, body_hash FROM runs WHERE owner = ? AND idempotency_key = ?`, owner, key).Scan(&id, &bodyHash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return id, bodyHash, err
}

// UnfinishedRuns returns every run in state creating, waiting or running,
// oldest first.
func (t *Tx) UnfinishedRuns() ([]RunRef, error) {
	rows, err := t.query(`SELECT owner, id FROM runs WHERE state IN ('creating', 'waiting', 'running') ORDER BY created_ns, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RunRef
	for rows.Next() {
		var r RunRef
		if err := rows.Scan(&r.Owner, &r.ID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRuns returns one page of owner's runs by (createdAt, id).
func (t *Tx) ListRuns(owner domain.PrincipalID, p Page) ([]*api.Run, string, error) {
	return pageDocs[api.Run](t, p, nil,
		`SELECT doc, created_ns, id FROM runs WHERE owner = ? AND (created_ns, id) > (?, ?) ORDER BY created_ns, id LIMIT ?`, owner)
}
