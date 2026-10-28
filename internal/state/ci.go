package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"davit/internal/domain"
)

// CI runner job records (internal/ci). The ci_jobs table keeps one JSON
// document per job, keyed by the controller's ID and unique per provider
// job ("github:<job id>"), so a job seen twice (webhook and poll) is
// recorded once.

// CIJobUnfinished are the states whose box may still exist.
var CIJobUnfinished = []string{"queued", "starting", "running", "stopping"}

// PutCIJob inserts or updates job id. A second job with the same
// providerJob is invalid_state.
func (t *Tx) PutCIJob(id, providerJob, st string, createdNS int64, doc any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO ci_jobs (id, provider_job, state, created_ns, doc) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state, doc=excluded.doc`, id, providerJob, st, createdNS, string(raw))
	if err != nil && isUnique(err) {
		return domain.Errorf(domain.CodeInvalidState, "CI job %s is already recorded", providerJob)
	}
	return err
}

// GetCIJob decodes job id into doc.
func (t *Tx) GetCIJob(id string, doc any) error {
	var raw string
	err := t.row(`SELECT doc FROM ci_jobs WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Errorf(domain.CodeNotFound, "CI job %s not found", id)
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), doc)
}

// CIJobByProvider returns the ID of the job recorded for providerJob, or
// "" when there is none.
func (t *Tx) CIJobByProvider(providerJob string) (string, error) {
	var id string
	err := t.row(`SELECT id FROM ci_jobs WHERE provider_job = ?`, providerJob).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CIJobDocs returns the documents of jobs in states (all when empty), by
// creation, oldest first, or newest first with newest; at most limit
// (none when limit <= 0).
func (t *Tx) CIJobDocs(states []string, newest bool, limit int) ([]json.RawMessage, error) {
	q := `SELECT doc FROM ci_jobs`
	var args []any
	if len(states) > 0 {
		q += ` WHERE state IN (?` + strings.Repeat(`, ?`, len(states)-1) + `)`
		for _, s := range states {
			args = append(args, s)
		}
	}
	if newest {
		q += ` ORDER BY created_ns DESC, id DESC`
	} else {
		q += ` ORDER BY created_ns, id`
	}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := t.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(raw))
	}
	return out, rows.Err()
}

// PruneCIJobs deletes finished jobs created before ns and returns how many.
func (t *Tx) PruneCIJobs(ns int64) (int64, error) {
	res, err := t.exec(`DELETE FROM ci_jobs WHERE created_ns < ? AND state NOT IN ('queued', 'starting', 'running', 'stopping')`, ns)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
