package state

import (
	"database/sql"
	"encoding/json"
	"errors"

	"davit/internal/domain"
)

// Privacy guard (G17). The store is the authority on what is kept: every
// write of process output (AppendOutput) and of process samples
// (PutGuestSample) asks the VM's current policy first, inside the writing
// transaction, so no writer (a collector started before the policy
// changed, or after a daemon restart, or a future one) can store what the
// policy forbids. Callers may skip the work earlier (guestexec does); they
// never need to for correctness.

// vmPrivacy returns the stored privacy policy of vm (sql.ErrNoRows if the
// VM does not exist).
func (t *Tx) vmPrivacy(vm domain.VMID) (domain.PrivacyPolicy, error) {
	var doc sql.NullString
	var p domain.PrivacyPolicy
	if err := t.row(`SELECT json_extract(doc, '$.privacy') FROM vms WHERE id = ?`, vm).Scan(&doc); err != nil {
		return p, err
	}
	if !doc.Valid || doc.String == "" {
		return p, nil // records from before G17: the usual retention
	}
	return p, json.Unmarshal([]byte(doc.String), &p)
}

// outputRetained reports whether output of process may be stored: its
// VM's policy retains output. An unknown process or VM retains nothing.
func (t *Tx) outputRetained(process string) (bool, error) {
	var vm domain.VMID
	err := t.row(`SELECT vm_id FROM processes WHERE id = ?`, process).Scan(&vm)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	p, err := t.vmPrivacy(vm)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && p.RetainsOutput(), err
}

// samplesRetained reports whether process samples of vm may be stored. An
// unknown VM retains nothing.
func (t *Tx) samplesRetained(vm domain.VMID) (bool, error) {
	p, err := t.vmPrivacy(vm)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && p.RetainsSamples(), err
}

// PurgeVMOutput deletes every retained output chunk of vm's processes
// (the process records stay), records that the policy withheld their
// output, and returns how many chunks it deleted. It is for turning
// noOutputRetention on.
func (t *Tx) PurgeVMOutput(vm domain.VMID) (int, error) {
	res, err := t.exec(`DELETE FROM process_output WHERE seq > 0 AND process_id IN (SELECT id FROM processes WHERE vm_id = ?)`, vm)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// A retention drop marker means nothing once the rest is gone.
	if _, err := t.exec(`DELETE FROM process_output WHERE seq = ? AND process_id IN (SELECT id FROM processes WHERE vm_id = ?)`, droppedMarkerSeq, vm); err != nil {
		return 0, err
	}
	_, err = t.exec(`INSERT OR IGNORE INTO process_output (process_id, seq, stream, data)
SELECT id, ?, 'stdout', x'' FROM processes WHERE vm_id = ?`, withheldMarkerSeq, vm)
	return int(n), err
}

// DeleteGuestSample deletes vm's stored process sample, if any. It is for
// turning noProcessSamples on.
func (t *Tx) DeleteGuestSample(vm domain.VMID) error {
	_, err := t.exec(`DELETE FROM guest_process_samples WHERE vm_id = ?`, vm)
	return err
}
