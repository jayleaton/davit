package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
)

// TestPrivacyGuard (G17): AppendOutput and PutGuestSample store nothing for
// a VM whose policy forbids it, read the policy as it is now (a flag turned
// on later stops the next write), and leave other VMs alone; PurgeVMOutput
// and DeleteGuestSample remove what was kept before, keeping the process
// records.
func TestPrivacyGuard(t *testing.T) {
	db := openT(t, filepath.Join(t.TempDir(), "s.db"))
	now := time.Unix(3000, 0).UTC()
	countOutput := func(tx *Tx, p string) int {
		t.Helper()
		cs, err := tx.ReadOutput(p, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(cs)
	}
	hasSample := func(tx *Tx, vm domain.VMID) bool {
		t.Helper()
		_, err := tx.GuestSample(vm)
		if err != nil && !domain.IsCode(err, domain.CodeNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}
	sample := &api.GuestProcessList{SampledAt: now, Items: []api.GuestProcess{{PID: 1}}}
	err := db.Tx(context.Background(), func(tx *Tx) error {
		private := testVM("vm-private", now)
		private.VM.Privacy = domain.PrivacyPolicy{NoOutputRetention: true, NoProcessSamples: true}
		for _, rec := range []*VMRecord{private, testVM("vm-usual", now), testVM("vm-later", now)} {
			if err := tx.PutVM(rec); err != nil {
				return err
			}
			if err := tx.PutProcess(&ProcessRecord{ID: "p-" + string(rec.VM.ID), VMID: rec.VM.ID, Owner: "owner",
				Generation: 1, CreatedAt: now, Status: "running"}); err != nil {
				return err
			}
		}
		for _, vm := range []domain.VMID{"vm-private", "vm-usual", "vm-later"} {
			for seq := int64(1); seq <= 2; seq++ {
				if err := tx.AppendOutput("p-"+string(vm), OutputChunk{Seq: seq, Stream: api.StreamStdout, Data: []byte("x")}); err != nil {
					t.Fatalf("append %s: %v", vm, err)
				}
			}
			if err := tx.PutGuestSample(vm, sample); err != nil {
				t.Fatalf("sample %s: %v", vm, err)
			}
		}
		if n := countOutput(tx, "p-vm-private"); n != 0 || hasSample(tx, "vm-private") {
			t.Errorf("private VM kept %d chunks, sample %v", n, hasSample(tx, "vm-private"))
		}
		if n := countOutput(tx, "p-vm-usual"); n != 2 || !hasSample(tx, "vm-usual") {
			t.Errorf("usual VM kept %d chunks, sample %v", n, hasSample(tx, "vm-usual"))
		}
		// An unknown process or VM keeps nothing (and is no error).
		if err := tx.AppendOutput("p-none", OutputChunk{Seq: 1, Stream: api.StreamStdout, Data: []byte("x")}); err != nil {
			t.Errorf("append to unknown process: %v", err)
		}
		if err := tx.PutGuestSample("vm-none", sample); err != nil {
			t.Errorf("sample of unknown VM: %v", err)
		}

		// Turning the flags on later: purge what was kept, and the next
		// writes are dropped.
		later, err := tx.GetVM("vm-later")
		if err != nil {
			return err
		}
		later.VM.Privacy = domain.PrivacyPolicy{NoOutputRetention: true, NoProcessSamples: true}
		if err := tx.PutVM(later); err != nil {
			return err
		}
		if n, err := tx.PurgeVMOutput("vm-later"); err != nil || n != 2 {
			t.Fatalf("purge: %d %v", n, err)
		}
		if err := tx.DeleteGuestSample("vm-later"); err != nil {
			return err
		}
		if err := tx.AppendOutput("p-vm-later", OutputChunk{Seq: 3, Stream: api.StreamStdout, Data: []byte("x")}); err != nil {
			return err
		}
		if err := tx.PutGuestSample("vm-later", sample); err != nil {
			return err
		}
		if n := countOutput(tx, "p-vm-later"); n != 0 || hasSample(tx, "vm-later") {
			t.Errorf("after the flags: %d chunks, sample %v", n, hasSample(tx, "vm-later"))
		}
		if _, err := tx.GetProcess("p-vm-later"); err != nil {
			t.Errorf("purge removed the process record: %v", err)
		}
		if n := countOutput(tx, "p-vm-usual"); n != 2 || !hasSample(tx, "vm-usual") {
			t.Errorf("other VM touched: %d chunks, sample %v", n, hasSample(tx, "vm-usual"))
		}
		if n, err := tx.PurgeVMOutput("vm-later"); err != nil || n != 0 {
			t.Errorf("second purge: %d %v", n, err)
		}

		// History's view: the policy withheld the output of the process that
		// ran under it and of the purged one (ended with guest output past
		// what is kept, which is not a retention drop); the usual VM's is
		// kept. The record outlives turning the policy off again.
		if err := tx.PutProcess(&ProcessRecord{ID: "p-vm-later", VMID: "vm-later", Owner: "owner",
			Generation: 1, CreatedAt: now, Status: "exited", OutputCursor: 2}); err != nil {
			return err
		}
		later.VM.Privacy = domain.PrivacyPolicy{}
		if err := tx.PutVM(later); err != nil {
			return err
		}
		for p, want := range map[string]bool{"p-vm-private": true, "p-vm-later": true, "p-vm-usual": false} {
			st, err := tx.OutputStats(p)
			if err != nil || st.Withheld != want || st.Dropped {
				t.Errorf("%s stats %+v %v, want withheld %v and not dropped", p, st, err, want)
			}
		}
		if st, _ := tx.OutputStats("p-vm-usual"); st.Stdout != 2 {
			t.Errorf("usual VM stats %+v", st)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
