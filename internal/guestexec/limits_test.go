//go:build linux

package guestexec_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestagent"
	"davit/internal/guestclient"
	"davit/internal/guestexec"
	"davit/internal/runtime"
)

func setupLimits(t *testing.T, perVM, all int, privacy domain.PrivacyPolicy) *guestexec.Manager {
	t.Helper()
	a, err := guestagent.New(guestagent.Config{Root: t.TempDir(), RunDir: t.TempDir(), System: nopSystem{}, SSHUnit: "-"})
	if err != nil {
		t.Fatal(err)
	}
	g := &guest{agent: a, privacy: privacy}
	secrets := &guestclient.MemorySecrets{}
	g.boot(t, secrets, runtime.Key{VMID: "vm_t", Generation: 1})
	m := guestexec.New(guestexec.Config{Targets: g, Secrets: secrets, AttachGrace: time.Minute,
		MaxLivePerVM: perVM, MaxLive: all, Store: guestexec.DBStore{DB: openDB(t, filepath.Join(t.TempDir(), "s.db"))}})
	t.Cleanup(m.Close)
	return m
}

// TestLiveProcessLimit: a VM runs at most MaxLivePerVM processes at once;
// the next exec is refused resource_exhausted (retryable), a replay of an
// admitted one still answers, and an exit frees the slot.
func TestLiveProcessLimit(t *testing.T) {
	m := setupLimits(t, 2, 100, domain.PrivacyPolicy{})
	ctx := ctxT(t)
	sleep := api.ExecRequest{Argv: []string{"sleep", "30"}, Detached: true}
	var ids []string
	for i := range 2 {
		p, err := m.Exec(ctx, "owner", "vm_t", fmt.Sprint("k", i), sleep)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	_, err := m.Exec(ctx, "owner", "vm_t", "k-over", sleep)
	if !domain.IsCode(err, domain.CodeResourceExhausted) || !domain.AsError(err).Retryable {
		t.Fatalf("third exec on a full VM: %v", err)
	}
	if p, err := m.Exec(ctx, "owner", "vm_t", "k0", sleep); err != nil || p.ID != ids[0] {
		t.Fatalf("replay at the limit: %+v %v", p, err)
	}
	if _, err := m.Signal(ctx, "owner", "vm_t", ids[0], "KILL"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if p, _ := m.Get(ctx, "owner", "vm_t", ids[0]); p.ExitedAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("killed process never exited")
		}
	}
	if _, err := m.Exec(ctx, "owner", "vm_t", "k-over", sleep); err != nil {
		t.Fatalf("exec after a slot freed: %v", err)
	}
	for _, id := range ids[1:] {
		m.Signal(ctx, "owner", "vm_t", id, "KILL")
	}
}

// TestLiveProcessLimitSeesUnobservedExits: under noOutputRetention no
// collector watches a detached process, so its exit is only learnt by
// asking; a full VM asks the agent before refusing.
func TestLiveProcessLimitSeesUnobservedExits(t *testing.T) {
	m := setupLimits(t, 2, 2, domain.PrivacyPolicy{NoOutputRetention: true})
	ctx := ctxT(t)
	for i := range 2 {
		if _, err := m.Exec(ctx, "owner", "vm_t", fmt.Sprint("t", i), api.ExecRequest{Argv: []string{"true"}, Detached: true}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // both have exited in the guest
	if _, err := m.Exec(ctx, "owner", "vm_t", "t-next", api.ExecRequest{Argv: []string{"true"}, Detached: true}); err != nil {
		t.Fatalf("exec after unobserved exits: %v", err)
	}
}
