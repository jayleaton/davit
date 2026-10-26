package tokens

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

func newTestService(t *testing.T, now *time.Time) (*Service, *state.DB) {
	t.Helper()
	db, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Service{DB: db, VMs: vms{"devbox": "vm_a", "vm_a": "vm_a"}, Now: func() time.Time { return *now }}, db
}

func stored(t *testing.T, db *state.DB, owner domain.PrincipalID, id string) *state.TokenRecord {
	t.Helper()
	var r *state.TokenRecord
	if err := db.Tx(context.Background(), func(tx *state.Tx) (err error) { r, err = tx.GetToken(owner, id); return err }); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestUsageBatched: every use is counted, but the database is written at
// most once per touchEvery per token (and by Flush); listings include
// what is still pending.
func TestUsageBatched(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s, db := newTestService(t, &now)
	owner := domain.PrincipalID("local:1000")
	tok, err := s.Create(ctx, owner, api.CreateTokenRequest{Name: "ci", TTLSeconds: 3600, Scopes: []api.TokenScope{{Actions: []string{"view"}}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, _, err := s.Verify(ctx, tok.Secret); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if r := stored(t, db, owner, tok.ID); r.Token.UseCount != 1 {
		t.Errorf("written count %d after 5 uses within a minute, want 1 (only the first touch)", r.Token.UseCount)
	}
	l, _ := s.List(ctx, owner, 0, "")
	if l.Items[0].UseCount != 5 || !l.Items[0].LastUsedAt.Equal(now.Add(-time.Second)) {
		t.Errorf("listed usage %d at %v", l.Items[0].UseCount, l.Items[0].LastUsedAt)
	}
	now = now.Add(time.Minute)
	s.Verify(ctx, tok.Secret) // a minute later: the batch is written
	if r := stored(t, db, owner, tok.ID); r.Token.UseCount != 6 || !r.Token.LastUsedAt.Equal(now) {
		t.Errorf("after the touch interval: count %d at %v", r.Token.UseCount, r.Token.LastUsedAt)
	}
	s.Verify(ctx, tok.Secret)
	s.Flush(ctx)
	if r := stored(t, db, owner, tok.ID); r.Token.UseCount != 7 {
		t.Errorf("after Flush: %d, want 7", r.Token.UseCount)
	}
	if got, err := s.Get(ctx, owner, tok.ID); err != nil || got.UseCount != 7 {
		t.Errorf("Get: %+v %v", got, err)
	}
}

// TestRotate: the new token has the old scopes and lifetime; the old one
// is revoked at once or after the overlap (never extended); a keyed replay
// returns the same new token; a revoked token cannot be rotated.
func TestRotate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s, _ := newTestService(t, &now)
	owner := domain.PrincipalID("local:1000")
	old, err := s.Create(ctx, owner, api.CreateTokenRequest{Name: "ci", TTLSeconds: 7200,
		Scopes: []api.TokenScope{{VMID: "devbox", Actions: []string{"exec"}}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	nt, err := s.Rotate(ctx, owner, old.ID, api.RotateTokenRequest{OverlapSeconds: 300}, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if nt.ID == old.ID || nt.Secret == old.Secret || nt.Name != "ci" || len(nt.Scopes) != 1 || nt.Scopes[0].VMID != "vm_a" ||
		!nt.ExpiresAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("rotated %+v", nt)
	}
	again, err := s.Rotate(ctx, owner, old.ID, api.RotateTokenRequest{OverlapSeconds: 300}, "r1")
	if err != nil || again.ID != nt.ID || again.Secret != nt.Secret {
		t.Fatalf("keyed replay: %+v %v", again, err)
	}
	if _, _, err := s.Verify(ctx, old.Secret); err != nil {
		t.Fatalf("old secret within the overlap: %v", err)
	}
	now = now.Add(5 * time.Minute)
	if _, _, err := s.Verify(ctx, old.Secret); !isCode(err, domain.CodeUnauthenticated) {
		t.Fatalf("old secret after the overlap: %v", err)
	}
	if _, _, err := s.Verify(ctx, nt.Secret); err != nil {
		t.Fatalf("new secret: %v", err)
	}
	if l, _ := s.List(ctx, owner, 0, ""); len(l.Items) != 1 || l.Items[0].ID != nt.ID {
		t.Errorf("live tokens after the overlap: %+v", l.Items)
	}

	// Overlap 0 revokes at once; a revoked token is not rotatable.
	now2, err := s.Rotate(ctx, owner, nt.ID, api.RotateTokenRequest{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Verify(ctx, nt.Secret); !isCode(err, domain.CodeUnauthenticated) {
		t.Errorf("rotated without overlap, old still works: %v", err)
	}
	if _, err := s.Rotate(ctx, owner, nt.ID, api.RotateTokenRequest{}, ""); !isCode(err, domain.CodeNotFound) {
		t.Errorf("rotate a revoked token: %v", err)
	}
	// A long overlap never extends the old token's life.
	ttl := int64(60)
	short, _ := s.Rotate(ctx, owner, now2.ID, api.RotateTokenRequest{OverlapSeconds: 30 * 24 * 3600, TTLSeconds: &ttl}, "")
	if !short.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Errorf("ttlSeconds ignored: %v", short.ExpiresAt)
	}
	if got, err := s.Get(ctx, owner, now2.ID); err != nil || !got.ExpiresAt.Equal(now2.ExpiresAt) {
		t.Errorf("overlap extended the old token: %+v %v", got, err)
	}
	if _, err := s.Rotate(ctx, owner, short.ID, api.RotateTokenRequest{OverlapSeconds: -1}, ""); !isCode(err, domain.CodeInvalidArgument) {
		t.Errorf("negative overlap: %v", err)
	}
}
