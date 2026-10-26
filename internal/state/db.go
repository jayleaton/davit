// Package state is the durable control-plane store (SPEC §5): VMs,
// operations and their event journal, snapshots and idempotency records in
// SQLite (pure-Go driver, WAL, synchronous=FULL, foreign keys on).
//
// Rules callers rely on:
//   - Every mutation happens inside Tx; a transaction never spans VMM,
//     storage or network calls.
//   - One daemon per store: Open takes an exclusive flock and fails fast if
//     another process holds it.
//   - Bulk data never lives here; only IDs and manifests.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"

	"davit/internal/domain"
)

// DB is the open store.
type DB struct {
	db      *sql.DB
	lock    *os.File
	reserve reserve
	fault   atomic.Pointer[func() error] // write fault injection; unset in production
}

// ErrLocked is returned by Open when another process owns the store.
var ErrLocked = errors.New("state: store is locked by another process")

// Open opens (creating if needed) the database at path, takes the store lock
// and applies pending migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("state: lock: %w", err)
	}
	// A file: URI: '%', '?' and '#' in the path are escaped, or a data
	// directory containing them would open another file and lose the
	// pragmas.
	dsn := "file:" + uriEscaper.Replace(path) + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		lock.Close()
		return nil, err
	}
	// One connection serializes writers and keeps transactions short and
	// deadlock-free; readers are cheap at this scale.
	db.SetMaxOpenConns(1)
	d := &DB{db: db, lock: lock, reserve: reserve{path: path + ".reserve", size: ReserveBytes}}
	// Best effort: a host that is already full starts without a reserve and
	// re-arms it once space returns (Rearm).
	_ = d.reserve.arm()
	if err := d.migrate(ctx); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

var uriEscaper = strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")

// Close closes the database and releases the store lock.
func (d *DB) Close() error {
	err := d.db.Close()
	if d.lock != nil {
		d.lock.Close()
	}
	return err
}

// Tx runs fn in one immediate transaction, committing iff fn returns nil.
//
// If the host disk is full, Tx releases the space reserve and runs fn once
// more, so the journal can still record terminal results (fn must therefore
// tolerate running twice; its effects on Go state are re-assigned). A write
// that still cannot fit returns an error wrapping domain.ErrDiskFull, which
// maps to resource_exhausted.
func (d *DB) Tx(ctx context.Context, fn func(*Tx) error) error {
	err := d.tx(ctx, fn)
	if !isDiskFull(err) {
		return err
	}
	if d.reserve.release() {
		if err = d.tx(ctx, fn); !isDiskFull(err) {
			return err
		}
	}
	if !errors.Is(err, domain.ErrDiskFull) {
		err = fmt.Errorf("%w: %w", domain.ErrDiskFull, err)
	}
	return err
}

func (d *DB) tx(ctx context.Context, fn func(*Tx) error) error {
	sqlTx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: begin: %w", err)
	}
	var fault func() error
	if f := d.fault.Load(); f != nil {
		fault = *f
	}
	if err := fn(&Tx{tx: sqlTx, ctx: ctx, fault: fault}); err != nil {
		sqlTx.Rollback()
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("state: commit: %w", err)
	}
	return nil
}

// Tx is an open transaction.
type Tx struct {
	tx    *sql.Tx
	ctx   context.Context
	fault func() error
}

func (t *Tx) exec(q string, args ...any) (sql.Result, error) {
	if t.fault != nil {
		if err := t.fault(); err != nil {
			return nil, err
		}
	}
	return t.tx.ExecContext(t.ctx, q, args...)
}
func (t *Tx) query(q string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(t.ctx, q, args...)
}
func (t *Tx) row(q string, args ...any) *sql.Row { return t.tx.QueryRowContext(t.ctx, q, args...) }

// migrations are append-only; never edit a released entry.
var migrations = []string{
	1: `
CREATE TABLE vms (
	id                  TEXT PRIMARY KEY,
	owner               TEXT NOT NULL,
	name                TEXT NOT NULL,
	state               TEXT NOT NULL,
	generation          INTEGER NOT NULL,
	active_operation_id TEXT,
	created_ns          INTEGER NOT NULL,
	disk_id             TEXT NOT NULL DEFAULT '',
	retained_disk_id    TEXT NOT NULL DEFAULT '',
	runtime_handle      TEXT,
	doc                 TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX vms_owner_live_name ON vms(owner, name) WHERE state != 'deleted';
CREATE INDEX vms_page ON vms(owner, created_ns, id);

CREATE TABLE operations (
	id         TEXT PRIMARY KEY,
	vm_id      TEXT NOT NULL REFERENCES vms(id),
	owner      TEXT NOT NULL,
	kind       TEXT NOT NULL,
	state      TEXT NOT NULL,
	created_ns INTEGER NOT NULL,
	doc        TEXT NOT NULL
) STRICT;
CREATE INDEX operations_unfinished ON operations(state) WHERE state IN ('queued', 'running');
CREATE INDEX operations_vm ON operations(vm_id, created_ns);

CREATE TABLE operation_events (
	operation_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
	seq          INTEGER NOT NULL,
	doc          TEXT NOT NULL,
	PRIMARY KEY (operation_id, seq)
) STRICT;

CREATE TABLE snapshots (
	id           TEXT PRIMARY KEY,
	owner        TEXT NOT NULL,
	source_vm_id TEXT NOT NULL,
	name         TEXT NOT NULL DEFAULT '',
	commit_id    TEXT NOT NULL,
	created_ns   INTEGER NOT NULL,
	doc          TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX snapshots_owner_name ON snapshots(owner, name) WHERE name != '';
CREATE INDEX snapshots_page ON snapshots(owner, created_ns, id);
CREATE INDEX snapshots_vm ON snapshots(source_vm_id);

CREATE TABLE idempotency (
	scope        TEXT PRIMARY KEY,
	body_hash    TEXT NOT NULL,
	operation_id TEXT NOT NULL REFERENCES operations(id),
	created_ns   INTEGER NOT NULL,
	expires_ns   INTEGER
) STRICT;
`,
	2: `
CREATE TABLE processes (
	id              TEXT PRIMARY KEY,
	vm_id           TEXT NOT NULL REFERENCES vms(id),
	owner           TEXT NOT NULL,
	generation      INTEGER NOT NULL,
	created_ns      INTEGER NOT NULL,
	idempotency_key TEXT,
	body_hash       TEXT NOT NULL DEFAULT '',
	doc             TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX processes_idempotency ON processes(owner, vm_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX processes_vm ON processes(vm_id, created_ns);
`,
	3: `
CREATE TABLE conversations (
	id         TEXT PRIMARY KEY,
	vm_id      TEXT NOT NULL REFERENCES vms(id),
	owner      TEXT NOT NULL,
	created_ns INTEGER NOT NULL,
	updated_ns INTEGER NOT NULL,
	doc        TEXT NOT NULL
) STRICT;
CREATE INDEX conversations_vm ON conversations(owner, vm_id, updated_ns);

CREATE TABLE prompts (
	id              TEXT PRIMARY KEY,
	conversation_id TEXT NOT NULL REFERENCES conversations(id),
	state           TEXT NOT NULL,
	created_ns      INTEGER NOT NULL,
	doc             TEXT NOT NULL
) STRICT;
CREATE INDEX prompts_unfinished ON prompts(state) WHERE state IN ('starting', 'running');
CREATE INDEX prompts_conversation ON prompts(conversation_id, created_ns);

CREATE TABLE prompt_events (
	prompt_id TEXT NOT NULL REFERENCES prompts(id) ON DELETE CASCADE,
	seq       INTEGER NOT NULL,
	doc       TEXT NOT NULL,
	PRIMARY KEY (prompt_id, seq)
) STRICT;
`,
	4: `
ALTER TABLE prompts ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE prompts ADD COLUMN vm_id TEXT NOT NULL DEFAULT '';
ALTER TABLE prompts ADD COLUMN idempotency_key TEXT;
ALTER TABLE prompts ADD COLUMN body_hash TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX prompts_idempotency ON prompts(owner, vm_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX conversations_page ON conversations(owner, vm_id, created_ns, id);
`,
	5: `
CREATE TABLE tokens (
	id              TEXT PRIMARY KEY,
	owner           TEXT NOT NULL,
	hash            BLOB NOT NULL UNIQUE,
	idempotency_key TEXT,
	body_hash       TEXT NOT NULL,
	created_ns      INTEGER NOT NULL,
	expires_ns      INTEGER NOT NULL,
	revoked_ns      INTEGER,
	doc             TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX tokens_idempotency ON tokens(owner, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX tokens_page ON tokens(owner, created_ns, id);
`,
	// 6: parity v1 contract (docs/research/CONTRACT-v1.md). Secret values
	// have no table: credential secrets live in the host secret store.
	6: `
CREATE TABLE environments (
	id              TEXT PRIMARY KEY,
	owner           TEXT NOT NULL,
	name            TEXT NOT NULL,
	latest_revision INTEGER NOT NULL,
	created_ns      INTEGER NOT NULL,
	doc             TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX environments_owner_name ON environments(owner, name);
CREATE INDEX environments_page ON environments(owner, created_ns, id);

CREATE TABLE environment_revisions (
	environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
	revision       INTEGER NOT NULL,
	created_ns     INTEGER NOT NULL,
	doc            TEXT NOT NULL,
	PRIMARY KEY (environment_id, revision)
) STRICT;

CREATE TABLE credential_profiles (
	id         TEXT PRIMARY KEY,
	owner      TEXT NOT NULL,
	name       TEXT NOT NULL,
	created_ns INTEGER NOT NULL,
	doc        TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX credential_profiles_owner_name ON credential_profiles(owner, name);

CREATE TABLE credential_grants (
	id          TEXT PRIMARY KEY,
	vm_id       TEXT NOT NULL REFERENCES vms(id),
	owner       TEXT NOT NULL,
	profile_id  TEXT NOT NULL,
	state       TEXT NOT NULL,
	placeholder TEXT NOT NULL UNIQUE,
	created_ns  INTEGER NOT NULL,
	doc         TEXT NOT NULL
) STRICT;
CREATE INDEX credential_grants_vm ON credential_grants(vm_id, created_ns);
CREATE INDEX credential_grants_profile ON credential_grants(profile_id) WHERE state = 'active';

CREATE TABLE runs (
	id              TEXT PRIMARY KEY,
	owner           TEXT NOT NULL,
	state           TEXT NOT NULL,
	idempotency_key TEXT,
	body_hash       TEXT NOT NULL DEFAULT '',
	created_ns      INTEGER NOT NULL,
	doc             TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX runs_idempotency ON runs(owner, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX runs_unfinished ON runs(state) WHERE state IN ('creating', 'waiting', 'running');

ALTER TABLE processes ADD COLUMN handle TEXT;
CREATE UNIQUE INDEX processes_handle ON processes(vm_id, handle) WHERE handle IS NOT NULL;

CREATE TABLE process_output (
	process_id TEXT NOT NULL REFERENCES processes(id) ON DELETE CASCADE,
	seq        INTEGER NOT NULL,
	stream     TEXT NOT NULL CHECK (stream IN ('stdout', 'stderr')),
	data       BLOB NOT NULL,
	PRIMARY KEY (process_id, seq)
) STRICT;

CREATE TABLE guest_process_samples (
	vm_id      TEXT PRIMARY KEY REFERENCES vms(id) ON DELETE CASCADE,
	sampled_ns INTEGER NOT NULL,
	doc        TEXT NOT NULL
) STRICT;
`,
	// 7: G16 event stream (triggers) and webhook delivery outbox.
	7: `
-- G16: the host event stream. Triggers append an allowlisted summary of
-- every VM, operation, operation event, run and prompt change in the same
-- transaction as the change (lossless, ordered by seq, lifecycle untouched).
-- Only named fields are copied: never env values, prompt text or secrets.
CREATE TABLE events (
	seq     INTEGER PRIMARY KEY AUTOINCREMENT,
	ns      INTEGER NOT NULL,
	owner   TEXT NOT NULL,
	type    TEXT NOT NULL,
	vm_id   TEXT NOT NULL DEFAULT '',
	subject TEXT NOT NULL,
	data    TEXT NOT NULL
) STRICT;
CREATE INDEX events_owner ON events(owner, seq);
CREATE INDEX events_ns ON events(ns);

CREATE TRIGGER events_vm_insert AFTER INSERT ON vms BEGIN
	INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (CAST(ROUND((julianday('now') - 2440587.5) * 86400000.0) AS INTEGER) * 1000000, NEW.owner, 'vm.created', NEW.id, NEW.id, json_object('name', NEW.name, 'state', NEW.state, 'generation', NEW.generation, 'stateRevision', json_extract(NEW.doc, '$.stateRevision')));
END;
CREATE TRIGGER events_vm_state AFTER UPDATE OF state, generation ON vms
WHEN NEW.state IS NOT OLD.state OR NEW.generation IS NOT OLD.generation BEGIN
	INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (CAST(ROUND((julianday('now') - 2440587.5) * 86400000.0) AS INTEGER) * 1000000, NEW.owner, 'vm.state', NEW.id, NEW.id, json_object('name', NEW.name, 'state', NEW.state, 'previousState', OLD.state, 'generation', NEW.generation, 'stateRevision', json_extract(NEW.doc, '$.stateRevision'), 'error', json_extract(NEW.doc, '$.lastError.code')));
END;
CREATE TRIGGER events_operation_insert AFTER INSERT ON operations BEGIN
	INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (CAST(ROUND((julianday('now') - 2440587.5) * 86400000.0) AS INTEGER) * 1000000, NEW.owner, 'operation.created', NEW.vm_id, NEW.id, json_object('kind', NEW.kind, 'state', NEW.state));
END;
CREATE TRIGGER events_operation_state AFTER UPDATE OF state ON operations
WHEN NEW.state IS NOT OLD.state BEGIN
	INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (CAST(ROUND((julianday('now') - 2440587.5) * 86400000.0) AS INTEGER) * 1000000, NEW.owner, 'operation.state', NEW.vm_id, NEW.id, json_object('kind', NEW.kind, 'state', NEW.state, 'previousState', OLD.state, 'phase', json_extract(NEW.doc, '$.phase'), 'error', json_extract(NEW.doc, '$.error.code')));
END;
CREATE TRIGGER events_operation_progress AFTER INSERT ON operation_events BEGIN
	INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (CAST(ROUND((julianday('now') - 2440587.5) * 86400000.0) AS INTEGER) * 1000000, (SELECT owner FROM operations WHERE id = NEW.operation_id), 'operation.progress', (SELECT vm_id FROM operations WHERE id = NEW.operation_id), NEW.operation_id, json_object('seq', NEW.seq, 'state', json_extract(NEW.doc, '$.state'), 'phase', json_extract(NEW.doc, '$.phase'), 'progress', json_extract(NEW.doc, '$.progress')));
END;
CREATE TRIGGER events_run_insert AFTER INSERT ON runs BEGIN
	INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (CAST(ROUND((julianday('now') - 2440587.5) * 86400000.0) AS INTEGER) * 1000000, NEW.owner, 'run.created', COALESCE(json_extract(NEW.doc, '$.vmId'), ''), NEW.id, json_object('state', NEW.state));
END;
CREATE TRIGGER events_run_state AFTER UPDATE OF state ON runs
WHEN NEW.state IS NOT OLD.state BEGIN
	INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (CAST(ROUND((julianday('now') - 2440587.5) * 86400000.0) AS INTEGER) * 1000000, NEW.owner, 'run.state', COALESCE(json_extract(NEW.doc, '$.vmId'), ''), NEW.id, json_object('state', NEW.state, 'previousState', OLD.state, 'exitCode', json_extract(NEW.doc, '$.exitCode'), 'error', json_extract(NEW.doc, '$.error.code')));
END;
CREATE TRIGGER events_prompt_insert AFTER INSERT ON prompts BEGIN
	INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (CAST(ROUND((julianday('now') - 2440587.5) * 86400000.0) AS INTEGER) * 1000000, NEW.owner, 'prompt.created', NEW.vm_id, NEW.id, json_object('conversationId', NEW.conversation_id, 'harness', json_extract(NEW.doc, '$.harness'), 'state', NEW.state));
END;
CREATE TRIGGER events_prompt_state AFTER UPDATE OF state ON prompts
WHEN NEW.state IS NOT OLD.state BEGIN
	INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (CAST(ROUND((julianday('now') - 2440587.5) * 86400000.0) AS INTEGER) * 1000000, NEW.owner, 'prompt.state', NEW.vm_id, NEW.id, json_object('conversationId', NEW.conversation_id, 'harness', json_extract(NEW.doc, '$.harness'), 'state', NEW.state, 'previousState', OLD.state, 'error', json_extract(NEW.doc, '$.error.code')));
END;

-- G16 webhooks: subscriptions and their durable delivery outbox. Signing
-- secrets live in the host secret store, never here.
CREATE TABLE webhooks (
	id            TEXT PRIMARY KEY,
	owner         TEXT NOT NULL,
	created_ns    INTEGER NOT NULL,
	last_event    INTEGER NOT NULL, -- events.seq fanned out so far
	doc           TEXT NOT NULL
) STRICT;
CREATE INDEX webhooks_page ON webhooks(owner, created_ns, id);

CREATE TABLE webhook_deliveries (
	id          TEXT PRIMARY KEY,
	webhook_id  TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
	event_seq   INTEGER NOT NULL,
	state       TEXT NOT NULL CHECK (state IN ('pending', 'delivered', 'dead')),
	next_ns     INTEGER NOT NULL,
	created_ns  INTEGER NOT NULL,
	doc         TEXT NOT NULL,
	UNIQUE (webhook_id, event_seq)
) STRICT;
CREATE INDEX webhook_deliveries_due ON webhook_deliveries(webhook_id, next_ns) WHERE state = 'pending';
CREATE INDEX webhook_deliveries_page ON webhook_deliveries(webhook_id, created_ns, id);
`,
	// 8: host capacity: operation.created carries why the host stopped a VM
	// on its own (Operation.reason: idle, ttl, reclaim, gc).
	8: `
DROP TRIGGER events_operation_insert;
CREATE TRIGGER events_operation_insert AFTER INSERT ON operations BEGIN
	INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (CAST(ROUND((julianday('now') - 2440587.5) * 86400000.0) AS INTEGER) * 1000000, NEW.owner, 'operation.created', NEW.vm_id, NEW.id, json_object('kind', NEW.kind, 'state', NEW.state, 'reason', json_extract(NEW.doc, '$.reason')));
END;
`,
	// 9: the slipway -> davit rename. Project-box labels move to the davit.
	// prefix, so boxes made before the rename stay boxes (docs/migrating-from-slipway.md).
	// A document that is not JSON is left as it is.
	9: `
UPDATE vms SET doc = json_set(json_remove(doc, '$.labels."slipway.box"'), '$.labels."davit.box"', json_extract(doc, '$.labels."slipway.box"'))
	WHERE CASE WHEN json_valid(doc) THEN json_extract(doc, '$.labels."slipway.box"') IS NOT NULL ELSE 0 END;
UPDATE vms SET doc = json_set(json_remove(doc, '$.labels."slipway.box.instance"'), '$.labels."davit.box.instance"', json_extract(doc, '$.labels."slipway.box.instance"'))
	WHERE CASE WHEN json_valid(doc) THEN json_extract(doc, '$.labels."slipway.box.instance"') IS NOT NULL ELSE 0 END;
UPDATE vms SET doc = json_set(json_remove(doc, '$.labels."slipway.box.role"'), '$.labels."davit.box.role"', json_extract(doc, '$.labels."slipway.box.role"'))
	WHERE CASE WHEN json_valid(doc) THEN json_extract(doc, '$.labels."slipway.box.role"') IS NOT NULL ELSE 0 END;
UPDATE vms SET doc = json_set(json_remove(doc, '$.labels."slipway.box.generation"'), '$.labels."davit.box.generation"', json_extract(doc, '$.labels."slipway.box.generation"'))
	WHERE CASE WHEN json_valid(doc) THEN json_extract(doc, '$.labels."slipway.box.generation"') IS NOT NULL ELSE 0 END;
`,
}

func (d *DB) migrate(ctx context.Context) error {
	return d.Tx(ctx, func(t *Tx) error {
		if _, err := t.exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_ns INTEGER NOT NULL) STRICT`); err != nil {
			return err
		}
		var have int
		if err := t.row(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&have); err != nil {
			return err
		}
		if have >= len(migrations) {
			return fmt.Errorf("state: database schema v%d is newer than this binary (v%d)", have, len(migrations)-1)
		}
		for v := have + 1; v < len(migrations); v++ {
			if _, err := t.exec(migrations[v]); err != nil {
				return fmt.Errorf("state: migration %d: %w", v, err)
			}
			if _, err := t.exec(`INSERT INTO schema_migrations VALUES (?, ?)`, v, time.Now().UnixNano()); err != nil {
				return err
			}
		}
		return nil
	})
}

// SchemaVersion returns the applied schema version.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := d.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// IntegrityCheck runs SQLite's integrity and foreign-key checks.
func (d *DB) IntegrityCheck(ctx context.Context) error {
	var res string
	if err := d.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("state: integrity_check: %s", res)
	}
	rows, err := d.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("state: foreign_key_check found violations")
	}
	return rows.Err()
}

func notFound(kind string, id any) error {
	return domain.Errorf(domain.CodeNotFound, "%s %v not found", kind, id)
}
