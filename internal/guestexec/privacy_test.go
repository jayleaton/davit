//go:build linux

package guestexec_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestagent"
	"davit/internal/guestclient"
	"davit/internal/guestexec"
	"davit/internal/runtime"
	"davit/internal/state"
)

// privacyRig is a guest and a store whose VM vm_t has policy p (in the
// store and in the targets the guest resolves).
func privacyRig(t *testing.T, p domain.PrivacyPolicy) (*guest, guestclient.SecretStore, *state.DB) {
	t.Helper()
	a, err := guestagent.New(guestagent.Config{Root: t.TempDir(), RunDir: t.TempDir(), System: nopSystem{}, SSHUnit: "-"})
	if err != nil {
		t.Fatal(err)
	}
	g := &guest{agent: a, privacy: p}
	secrets := &guestclient.MemorySecrets{}
	g.boot(t, secrets, runtime.Key{VMID: "vm_t", Generation: 1})
	db := openDB(t, filepath.Join(t.TempDir(), "s.db"))
	if err := db.Tx(context.Background(), func(tx *state.Tx) error {
		rec, err := tx.GetVM("vm_t")
		if err != nil {
			return err
		}
		rec.VM.Privacy = p
		return tx.PutVM(rec)
	}); err != nil {
		t.Fatal(err)
	}
	return g, secrets, db
}

// TestNoOutputRetention (G17): a noOutputRetention VM's process that prints
// completes and shows in history, but no output is stored, neither by the
// manager that started it nor by one after a daemon restart, even when that
// one's target does not carry the policy (the store enforces it). The same
// run on a usual VM records all of its output.
func TestNoOutputRetention(t *testing.T) {
	for _, tc := range []struct {
		name    string
		privacy domain.PrivacyPolicy
		chunks  int
	}{
		{"usual", domain.PrivacyPolicy{}, 2},
		{"noOutputRetention", domain.PrivacyPolicy{NoOutputRetention: true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, secrets, db := privacyRig(t, tc.privacy)
			newManager := func() *guestexec.Manager {
				return guestexec.New(guestexec.Config{Targets: g, Secrets: secrets, Store: guestexec.DBStore{DB: db}})
			}
			ctx := ctxT(t)
			m1 := newManager()
			p, err := m1.Exec(ctx, "owner", "vm_t", "k", api.ExecRequest{Argv: []string{"sh", "-c", "echo a; sleep 0.4; echo b >&2"}, Detached: true})
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond) // "a" is out; a collector (if any) has seen it
			m1.Close()

			// The restarted daemon resolves targets without the policy: only
			// the store stands between the guest's output and the disk.
			g.mu.Lock()
			g.privacy = domain.PrivacyPolicy{}
			g.mu.Unlock()
			m2 := newManager()
			for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				got, err := m2.Get(ctx, "owner", "vm_t", p.ID)
				if err != nil {
					t.Fatal(err)
				}
				if got.ExitedAt != nil {
					break
				}
				if time.Now().After(end) {
					t.Fatalf("process did not finish: %+v", got)
				}
			}
			// The collector delivers the guest's whole output by the exit;
			// give it a moment to write what it got.
			for end := time.Now().Add(2 * time.Second); len(readOutput(t, db, p.ID)) < 2 && time.Now().Before(end); {
				time.Sleep(20 * time.Millisecond)
			}
			m2.Close()

			if got := readOutput(t, db, p.ID); len(got) != tc.chunks {
				t.Fatalf("stored %d chunks, want %d: %+v", len(got), tc.chunks, got)
			}
			var hist []*state.ProcessRecord
			if err := db.Tx(ctx, func(tx *state.Tx) (err error) {
				hist, _, err = tx.ListProcessHistory("owner", "vm_t", state.Page{Limit: 10})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(hist) != 1 || hist[0].ID != p.ID || hist[0].Status != string(api.ProcessExited) {
				t.Fatalf("history %+v", hist)
			}
		})
	}
}

// TestNoProcessSamples (G17): ps on a noProcessSamples VM answers live and
// stores no sample, also when the target does not carry the policy; a
// usual VM's sample is stored.
func TestNoProcessSamples(t *testing.T) {
	for _, tc := range []struct {
		name    string
		privacy domain.PrivacyPolicy
		stale   bool // the target forgets the policy
		stored  bool
	}{
		{"usual", domain.PrivacyPolicy{}, false, true},
		{"noProcessSamples", domain.PrivacyPolicy{NoProcessSamples: true}, false, false},
		{"noProcessSamples, stale target", domain.PrivacyPolicy{NoProcessSamples: true}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, secrets, db := privacyRig(t, tc.privacy)
			if tc.stale {
				g.privacy = domain.PrivacyPolicy{}
			}
			m := guestexec.New(guestexec.Config{Targets: g, Secrets: secrets, Store: guestexec.DBStore{DB: db}})
			defer m.Close()
			ctx := ctxT(t)
			l, err := m.GuestProcesses(ctx, "owner", "vm_t")
			if err != nil || len(l.Items) == 0 || l.SampledAt.IsZero() {
				t.Fatalf("live sample: %+v %v", l, err)
			}
			err = db.Tx(ctx, func(tx *state.Tx) error { _, err := tx.GuestSample("vm_t"); return err })
			if stored := err == nil; stored != tc.stored || (err != nil && !domain.IsCode(err, domain.CodeNotFound)) {
				t.Fatalf("sample stored %v (want %v): %v", stored, tc.stored, err)
			}
		})
	}
}
