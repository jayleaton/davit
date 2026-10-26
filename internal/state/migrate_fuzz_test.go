package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"davit/internal/domain"
)

// oldDB writes a database at schema version v (migrations 1..v only, as an
// older binary left it) at path and inserts one VM, operation, operation
// event, process and output chunk whose JSON documents and states are the
// fuzzer's. A row the old schema refuses (a constraint, or since v7 a
// trigger that reads the document) is left out: no older binary could
// have stored it either.
func oldDB(t *testing.T, path string, v int, state, vmDoc, opDoc, procDoc string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+uriEscaper.Replace(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_ns INTEGER NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= v; i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations VALUES (?, 1)`, i); err != nil {
			t.Fatal(err)
		}
	}
	if v == 0 {
		return
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO vms (id, owner, name, state, generation, created_ns, doc) VALUES ('vm-old', 'owner', 'old', ?, 1, 1, ?)`, []any{state, vmDoc}},
		{`INSERT INTO operations (id, vm_id, owner, kind, state, created_ns, doc) VALUES ('op-old', 'vm-old', 'owner', 'create', ?, 1, ?)`, []any{state, opDoc}},
		{`INSERT INTO operation_events (operation_id, seq, doc) VALUES ('op-old', 1, ?)`, []any{opDoc}},
		{`UPDATE vms SET active_operation_id = 'op-old' WHERE id = 'vm-old'`, nil},
		{`INSERT INTO processes (id, vm_id, owner, generation, created_ns, doc) VALUES ('p-old', 'vm-old', 'owner', 1, 1, ?)`, []any{procDoc}},
		{`INSERT INTO process_output (process_id, seq, stream, data) VALUES ('p-old', 1, 'stdout', x'00ff')`, nil},
	} {
		db.Exec(q.sql, q.args...) // see above: refused rows are skipped
	}
}

// FuzzMigrateOldDB: a database left by any older schema version, holding
// arbitrary record documents, migrates to the current schema, passes the
// integrity check, and every reader returns records or errors (never
// panics); the daemon's startup scans (unfinished operations, runs and
// prompts) do not fail on one bad record's account by panicking either.
func FuzzMigrateOldDB(f *testing.F) {
	now := time.Unix(1000, 0).UTC()
	vm := testVM("vm-old", now)
	vmDoc, _ := json.Marshal(vmDoc{VM: vm.VM, Env: vm.Env})
	// One seed per schema version (each opens a database with fsyncs), the
	// document shapes rotating: valid, empty, not JSON, wrong types.
	shapes := [][4]string{
		{"running", string(vmDoc), `{"id":"op-old","kind":"create","state":"running"}`, `{"id":"p-old","status":"running"}`},
		{"ready", `{}`, `{}`, `{}`},
		{"", `not json`, `[`, `null`},
		{"stopped", `{"vm":{"id":"vm-old","privacy":"yes","egress":7}}`, `{"error":{"code":5}}`, `{"argv":"sh","exitCode":"x"}`},
	}
	for v := 0; v < len(migrations); v++ {
		s := shapes[v%len(shapes)]
		f.Add(uint8(v), s[0], s[1], s[2], s[3])
	}
	f.Fuzz(func(t *testing.T, v uint8, state, vmDoc, opDoc, procDoc string) {
		ver := int(v) % len(migrations)
		path := filepath.Join(t.TempDir(), "s.db")
		oldDB(t, path, ver, state, vmDoc, opDoc, procDoc)
		db, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("migrate from v%d: %v", ver, err)
		}
		defer db.Close()
		ctx := context.Background()
		if got, err := db.SchemaVersion(ctx); err != nil || got != len(migrations)-1 {
			t.Fatalf("schema after migration: %d %v", got, err)
		}
		if err := db.IntegrityCheck(ctx); err != nil {
			t.Fatalf("integrity after migration from v%d: %v", ver, err)
		}
		_ = db.Tx(ctx, func(tx *Tx) error {
			tx.GetVM("vm-old")
			tx.ListVMs("owner", nil, Page{Limit: 10})
			tx.ListVMs("owner", []domain.VMState{domain.StateReady}, Page{Limit: 10})
			tx.GetOperation("op-old")
			tx.UnfinishedOperations()
			tx.GetProcess("p-old")
			tx.ListProcesses("owner", "vm-old", Page{Limit: 10})
			tx.ListProcessHistory("owner", "vm-old", Page{Limit: 10})
			tx.OutputStats("p-old")
			tx.ReadOutput("p-old", 0, 10)
			tx.UnfinishedRuns()
			tx.UnfinishedPrompts()
			tx.ListEvents("", 0, 10, EventFilter{})
			return nil
		})
	})
}

// TestMigrateBoxLabels: migration 9 moves project-box labels made before the
// slipway -> davit rename to the davit.box prefix and leaves others alone.
func TestMigrateBoxLabels(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	vm := domain.VM{ID: "vm-old", Owner: "owner", Name: "old", State: domain.StateStopped, InstanceGeneration: 1,
		Labels: map[string]string{"slipway.box": "proj", "slipway.box.instance": "main", "slipway.box.generation": "2", "team": "x"}}
	doc, _ := json.Marshal(vmDoc{VM: vm})
	oldDB(t, path, 8, string(domain.StateStopped), string(doc), `{}`, `{}`)
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got *VMRecord
	if err := db.Tx(context.Background(), func(tx *Tx) (err error) { got, err = tx.GetVM("vm-old"); return err }); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"davit.box": "proj", "davit.box.instance": "main", "davit.box.generation": "2", "team": "x"}
	if len(got.VM.Labels) != len(want) {
		t.Fatalf("labels after migration: %v", got.VM.Labels)
	}
	for k, v := range want {
		if got.VM.Labels[k] != v {
			t.Fatalf("labels after migration: %v", got.VM.Labels)
		}
	}
}
