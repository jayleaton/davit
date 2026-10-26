package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"davit/api"
	"davit/internal/domain"
)

// Parity v1 contract records (migration 6). Thin stores for the
// implementers of environments (G3), credential grants (G4), runs (G6) and
// process history (G7); invariants live here, behaviour in their packages.

// PutEnvironment inserts or updates an environment's metadata.
func (t *Tx) PutEnvironment(e *domain.Environment) error {
	doc, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO environments (id, owner, name, latest_revision, created_ns, doc) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, latest_revision=excluded.latest_revision, doc=excluded.doc`,
		e.ID, e.Owner, e.Name, e.LatestRevision, e.CreatedAt.UnixNano(), string(doc))
	if err != nil && isUnique(err) {
		return domain.Errorf(domain.CodeInvalidArgument, "environment name %q is taken", e.Name).WithDetail("field", "name")
	}
	return err
}

// GetEnvironment returns owner's environment by ID or name.
func (t *Tx) GetEnvironment(owner domain.PrincipalID, ref string) (*domain.Environment, error) {
	var doc string
	err := t.row(`SELECT doc FROM environments WHERE owner = ? AND (id = ? OR name = ?)`, owner, ref, ref).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.Errorf(domain.CodeNotFound, "environment %s not found", ref)
	}
	if err != nil {
		return nil, err
	}
	var e domain.Environment
	if err := json.Unmarshal([]byte(doc), &e); err != nil {
		return nil, err
	}
	e.Owner = owner
	return &e, nil
}

// revisionDoc is a stored revision plus the request that minted it, so a
// retried createEnvironmentRevision replays instead of minting another.
type revisionDoc struct {
	domain.EnvironmentRevision
	Key      string `json:"idempotencyKey,omitempty"`
	BodyHash string `json:"bodyHash,omitempty"`
}

// AddEnvironmentRevision appends the next revision of e (LatestRevision+1)
// and returns it; revisions are immutable once written. key and bodyHash
// record the request (both may be empty).
func (t *Tx) AddEnvironmentRevision(e *domain.Environment, m domain.EnvManifest, now time.Time, key, bodyHash string) (*domain.EnvironmentRevision, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	rev := &domain.EnvironmentRevision{EnvironmentID: e.ID, Revision: e.LatestRevision + 1, Manifest: m, CreatedAt: now}
	doc, err := json.Marshal(revisionDoc{EnvironmentRevision: *rev, Key: key, BodyHash: bodyHash})
	if err != nil {
		return nil, err
	}
	if _, err := t.exec(`INSERT INTO environment_revisions (environment_id, revision, created_ns, doc) VALUES (?, ?, ?, ?)`,
		rev.EnvironmentID, rev.Revision, now.UnixNano(), string(doc)); err != nil {
		return nil, err
	}
	e.LatestRevision, e.UpdatedAt = rev.Revision, now
	return rev, t.PutEnvironment(e)
}

// ListEnvironments returns one page of owner's environments by (createdAt, id).
func (t *Tx) ListEnvironments(owner domain.PrincipalID, p Page) ([]*domain.Environment, string, error) {
	return pageDocs(t, p, func(e *domain.Environment) { e.Owner = owner },
		`SELECT doc, created_ns, id FROM environments WHERE owner = ? AND (created_ns, id) > (?, ?) ORDER BY created_ns, id LIMIT ?`, owner)
}

// DeleteEnvironment deletes an environment and its revisions. VMs keep
// their pinned manifest (VMRecord.Manifest).
func (t *Tx) DeleteEnvironment(id domain.EnvironmentID) error {
	_, err := t.exec(`DELETE FROM environments WHERE id = ?`, id)
	return err
}

// ListEnvironmentRevisions returns one page of an environment's revisions,
// oldest first.
func (t *Tx) ListEnvironmentRevisions(id domain.EnvironmentID, p Page) ([]*domain.EnvironmentRevision, string, error) {
	return pageDocs[domain.EnvironmentRevision](t, p, nil,
		`SELECT doc, created_ns, CAST(revision AS TEXT) FROM environment_revisions WHERE environment_id = ? AND (created_ns, CAST(revision AS TEXT)) > (?, ?) ORDER BY created_ns, revision LIMIT ?`, id)
}

// pageDocs runs a keyset page query over (created_ns, id) returning JSON
// docs. q takes the leading args, then the cursor (ns, id) and the limit.
func pageDocs[T any](t *Tx, p Page, fix func(*T), q string, args ...any) ([]*T, string, error) {
	c, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	rows, err := t.query(q, append(args, c.CreatedNS, c.ID, p.Limit+1)...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*T
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
		v := new(T)
		if err := json.Unmarshal([]byte(doc), v); err != nil {
			return nil, "", err
		}
		if fix != nil {
			fix(v)
		}
		out = append(out, v)
		lastNS, lastID = ns, id
	}
	return out, "", rows.Err()
}

// GetEnvironmentRevision returns one revision.
func (t *Tx) GetEnvironmentRevision(id domain.EnvironmentID, revision int64) (*domain.EnvironmentRevision, error) {
	r, _, _, err := t.EnvironmentRevisionRequest(id, revision)
	return r, err
}

// EnvironmentRevisionByKey returns the revision of id minted with
// Idempotency-Key key and that request's body hash (nil if none).
func (t *Tx) EnvironmentRevisionByKey(id domain.EnvironmentID, key string) (*domain.EnvironmentRevision, string, error) {
	var n int64
	err := t.row(`SELECT revision FROM environment_revisions WHERE environment_id = ? AND json_extract(doc, '$.idempotencyKey') = ?`, id, key).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	rev, _, hash, err := t.EnvironmentRevisionRequest(id, n)
	return rev, hash, err
}

// EnvironmentRevisionRequest returns a revision with the Idempotency-Key and
// body hash of the request that minted it.
func (t *Tx) EnvironmentRevisionRequest(id domain.EnvironmentID, revision int64) (rev *domain.EnvironmentRevision, key, bodyHash string, err error) {
	var doc string
	err = t.row(`SELECT doc FROM environment_revisions WHERE environment_id = ? AND revision = ?`, id, revision).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", "", domain.Errorf(domain.CodeNotFound, "environment %s has no revision %d", id, revision)
	}
	if err != nil {
		return nil, "", "", err
	}
	var d revisionDoc
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		return nil, "", "", err
	}
	return &d.EnvironmentRevision, d.Key, d.BodyHash, nil
}

// PutCredentialProfile inserts or updates a profile's metadata (never a
// secret: those live in the host secret store).
func (t *Tx) PutCredentialProfile(p *domain.CredentialProfile) error {
	doc, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO credential_profiles (id, owner, name, created_ns, doc) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, doc=excluded.doc`, p.ID, p.Owner, p.Name, p.CreatedAt.UnixNano(), string(doc))
	if err != nil && isUnique(err) {
		return domain.Errorf(domain.CodeInvalidArgument, "credential profile name %q is taken", p.Name).WithDetail("field", "name")
	}
	return err
}

// PutGrant inserts or updates a credential grant of owner's VM.
func (t *Tx) PutGrant(owner domain.PrincipalID, g *domain.CredentialGrant) error {
	doc, err := json.Marshal(g)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO credential_grants (id, vm_id, owner, profile_id, state, placeholder, created_ns, doc)
VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET state=excluded.state, doc=excluded.doc`,
		g.ID, g.VMID, owner, g.ProfileID, g.State, g.Placeholder, g.CreatedAt.UnixNano(), string(doc))
	return err
}

// Grants returns a VM's grants, oldest first.
func (t *Tx) Grants(vm domain.VMID) ([]*domain.CredentialGrant, error) {
	rows, err := t.query(`SELECT doc FROM credential_grants WHERE vm_id = ? ORDER BY created_ns, id`, vm)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.CredentialGrant
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, err
		}
		var g domain.CredentialGrant
		if err := json.Unmarshal([]byte(doc), &g); err != nil {
			return nil, err
		}
		out = append(out, &g)
	}
	return out, rows.Err()
}

// RevokeVMGrants revokes every active grant of vm (GrantAfter's delete
// rule) and returns how many it revoked.
func (t *Tx) RevokeVMGrants(owner domain.PrincipalID, vm domain.VMID, reason domain.RevokeReason, now time.Time) (int, error) {
	gs, err := t.Grants(vm)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, g := range gs {
		if g.State != domain.GrantActive {
			continue
		}
		g.State, g.RevokeReason, g.RevokedAt = domain.GrantRevoked, reason, &now
		if err := t.PutGrant(owner, g); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// PutRun inserts or updates a run; key is its Idempotency-Key.
func (t *Tx) PutRun(owner domain.PrincipalID, key, bodyHash string, r *api.Run) error {
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO runs (id, owner, state, idempotency_key, body_hash, created_ns, doc) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state, doc=excluded.doc`,
		r.ID, owner, r.State, nullString(key), bodyHash, r.CreatedAt.UnixNano(), string(doc))
	return err
}

// GetRun returns owner's run.
func (t *Tx) GetRun(owner domain.PrincipalID, id string) (*api.Run, error) {
	var doc string
	err := t.row(`SELECT doc FROM runs WHERE id = ? AND owner = ?`, id, owner).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.Errorf(domain.CodeNotFound, "run %s not found", id)
	}
	if err != nil {
		return nil, err
	}
	var r api.Run
	return &r, json.Unmarshal([]byte(doc), &r)
}

// OutputChunk is one retained stdout/stderr chunk of a managed process.
type OutputChunk struct {
	Seq    int64
	Stream string // api.StreamStdout | api.StreamStderr
	Data   []byte
}

// AppendOutput retains a process's output chunk; seq must increase. A
// chunk of a VM whose privacy policy keeps no output (G17), or of an
// unknown process, is dropped silently (nil), whichever writer offers it.
func (t *Tx) AppendOutput(process string, c OutputChunk) error {
	if ok, err := t.outputRetained(process); err != nil || !ok {
		if err == nil {
			err = t.markWithheld(process)
		}
		return err
	}
	_, err := t.exec(`INSERT INTO process_output (process_id, seq, stream, data) VALUES (?, ?, ?, ?)`, process, c.Seq, c.Stream, c.Data)
	return err
}

// ReadOutput returns up to limit retained chunks with seq > after.
func (t *Tx) ReadOutput(process string, after int64, limit int) ([]OutputChunk, error) {
	rows, err := t.query(`SELECT seq, stream, data FROM process_output WHERE process_id = ? AND seq > ? AND seq > 0 ORDER BY seq LIMIT ?`, process, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutputChunk
	for rows.Next() {
		var c OutputChunk
		if err := rows.Scan(&c.Seq, &c.Stream, &c.Data); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutGuestSample keeps a VM's latest whole-guest process sample. Nothing
// is stored (nil) for a VM whose privacy policy keeps no samples (G17), or
// for an unknown VM.
func (t *Tx) PutGuestSample(vm domain.VMID, s *api.GuestProcessList) error {
	if ok, err := t.samplesRetained(vm); err != nil || !ok {
		return err
	}
	doc, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO guest_process_samples (vm_id, sampled_ns, doc) VALUES (?, ?, ?)
ON CONFLICT(vm_id) DO UPDATE SET sampled_ns=excluded.sampled_ns, doc=excluded.doc`, vm, s.SampledAt.UnixNano(), string(doc))
	return err
}

// GuestSample returns a VM's latest sample (not_found if none).
func (t *Tx) GuestSample(vm domain.VMID) (*api.GuestProcessList, error) {
	var doc string
	err := t.row(`SELECT doc FROM guest_process_samples WHERE vm_id = ?`, vm).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.Errorf(domain.CodeNotFound, "vm %s has no process sample", vm)
	}
	if err != nil {
		return nil, err
	}
	var s api.GuestProcessList
	return &s, json.Unmarshal([]byte(doc), &s)
}

// ListProcessHistory returns one page of owner's processes on vm across
// generations, newest first (the cursor runs backwards).
func (t *Tx) ListProcessHistory(owner domain.PrincipalID, vm domain.VMID, p Page) ([]*ProcessRecord, string, error) {
	c, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	if c.ID == "" {
		c.CreatedNS, c.ID = 1<<62, "\xff"
	}
	rows, err := t.query(`SELECT `+procCols+`, created_ns, id FROM processes WHERE owner = ? AND vm_id = ? AND (created_ns, id) < (?, ?)
ORDER BY created_ns DESC, id DESC LIMIT ?`, owner, vm, c.CreatedNS, c.ID, p.Limit+1)
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

// droppedMarkerSeq is the process_output row recording that retention cut
// a process's oldest output (guest output seqs start at 1).
const droppedMarkerSeq = 0

// withheldMarkerSeq is the process_output row recording that its VM's
// privacy policy (G17 noOutputRetention) kept output of the process off the
// host: it ran, or was running, while the policy was on. It is never
// removed, so the record outlives the policy being turned off again.
const withheldMarkerSeq = -1

// markWithheld records withheldMarkerSeq for process (if it exists).
func (t *Tx) markWithheld(process string) error {
	_, err := t.exec(`INSERT OR IGNORE INTO process_output (process_id, seq, stream, data)
SELECT id, ?, 'stdout', x'' FROM processes WHERE id = ?`, withheldMarkerSeq, process)
	return err
}

// TrimOutput deletes a process's oldest chunks until at most keep bytes
// remain, recording the drop, and returns the bytes left.
func (t *Tx) TrimOutput(process string, keep int64) (int64, error) {
	var total int64
	if err := t.row(`SELECT COALESCE(SUM(length(data)), 0) FROM process_output WHERE process_id = ? AND seq > 0`, process).Scan(&total); err != nil {
		return 0, err
	}
	if total <= keep {
		return total, nil
	}
	rows, err := t.query(`SELECT seq, length(data) FROM process_output WHERE process_id = ? AND seq > 0 ORDER BY seq`, process)
	if err != nil {
		return 0, err
	}
	var upTo int64
	for rows.Next() && total > keep {
		var seq, n int64
		if err := rows.Scan(&seq, &n); err != nil {
			rows.Close()
			return 0, err
		}
		upTo, total = seq, total-n
	}
	rows.Close()
	if _, err := t.exec(`DELETE FROM process_output WHERE process_id = ? AND seq > 0 AND seq <= ?`, process, upTo); err != nil {
		return 0, err
	}
	_, err = t.exec(`INSERT OR IGNORE INTO process_output (process_id, seq, stream, data) VALUES (?, ?, 'stdout', x'')`, process, droppedMarkerSeq)
	return total, err
}

// OutputStat is what the host kept of a process's output.
type OutputStat struct {
	Stdout, Stderr int64
	LastSeq        int64 // the last retained seq
	// Dropped: retention cut the oldest output (internal/guestexec keeps
	// the newest up to its bound), or the process ended with guest output
	// past the last retained seq (the guest evicted it before the host
	// recorded it). Output the privacy policy withheld is not "dropped".
	Dropped bool
	// Withheld: the VM's privacy policy kept (some of) the output off the
	// host.
	Withheld bool
}

// OutputStats reports a process's retained output.
func (t *Tx) OutputStats(process string) (OutputStat, error) {
	var st OutputStat
	rows, err := t.query(`SELECT stream, COALESCE(SUM(length(data)), 0), MAX(seq), SUM(seq = ?), SUM(seq = ?)
FROM process_output WHERE process_id = ? GROUP BY stream`, droppedMarkerSeq, withheldMarkerSeq, process)
	if err != nil {
		return st, err
	}
	trimmed := false
	for rows.Next() {
		var stream string
		var n, maxSeq, dropMarks, withheldMarks int64
		if err := rows.Scan(&stream, &n, &maxSeq, &dropMarks, &withheldMarks); err != nil {
			rows.Close()
			return OutputStat{}, err
		}
		trimmed = trimmed || dropMarks > 0
		st.Withheld = st.Withheld || withheldMarks > 0
		st.LastSeq = max(st.LastSeq, maxSeq)
		if stream == "stderr" {
			st.Stderr += n
		} else {
			st.Stdout += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return OutputStat{}, err
	}
	var status string
	var cursor int64
	err = t.row(`SELECT COALESCE(json_extract(doc, '$.status'), ''), COALESCE(json_extract(doc, '$.outputCursor'), 0) FROM processes WHERE id = ?`, process).Scan(&status, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		st.Dropped = trimmed
		return st, nil
	}
	ended := status != "starting" && status != "running" && status != "unknown"
	st.Dropped = trimmed || !st.Withheld && ended && cursor > st.LastSeq
	return st, err
}
