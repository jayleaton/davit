package tokens

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

type closer struct{ n atomic.Int32 }

func (c *closer) Close() error { c.n.Add(1); return nil }

// TestRevokeCutsLiveSessions: a token's live streams (tunnels, exec
// attach, long polls) end when it is revoked, rotated without overlap, at
// the end of a rotation's overlap and at its expiry, not only its next
// request (SEC-4).
func TestRevokeCutsLiveSessions(t *testing.T) {
	ctx := context.Background()
	db, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Service{DB: db, VMs: vms{}}
	owner := domain.PrincipalID("local:1000")
	mk := func(name string, ttl int64) (api.CreatedToken, *Grant) {
		t.Helper()
		tok, err := s.Create(ctx, owner, api.CreateTokenRequest{Name: name, TTLSeconds: ttl, Scopes: []api.TokenScope{{Actions: []string{"exec"}}}}, "")
		if err != nil {
			t.Fatal(err)
		}
		_, g, err := s.Verify(ctx, tok.Secret)
		if err != nil {
			t.Fatal(err)
		}
		return tok, g
	}
	waitClosed := func(what string, c *closer) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); c.n.Load() == 0; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s left the live session open", what)
			}
		}
	}

	tok, g := mk("revoke", 3600)
	if g.ExpiresAt.IsZero() {
		t.Fatal("grant carries no expiry")
	}
	c := &closer{}
	s.Track(g, c)
	if err := s.Revoke(ctx, owner, tok.ID); err != nil {
		t.Fatal(err)
	}
	waitClosed("revoke", c)

	tok, g = mk("rotate", 3600)
	c = &closer{}
	s.Track(g, c)
	if _, err := s.Rotate(ctx, owner, tok.ID, api.RotateTokenRequest{}, ""); err != nil {
		t.Fatal(err)
	}
	waitClosed("rotate without overlap", c)

	tok, g = mk("overlap", 3600)
	c = &closer{}
	s.Track(g, c)
	if _, err := s.Rotate(ctx, owner, tok.ID, api.RotateTokenRequest{OverlapSeconds: 1}, ""); err != nil {
		t.Fatal(err)
	}
	if c.n.Load() != 0 {
		t.Fatal("rotation with overlap cut at once")
	}
	waitClosed("the end of the rotation overlap", c)

	// 3 s, not the minimum 1 s: creating and verifying the token are two
	// fsynced writes, which on a loaded host can take a second and expire
	// it before it is ever used.
	_, g = mk("expiry", 3)
	c = &closer{}
	s.Track(g, c)
	waitClosed("expiry", c)

	// Untracked sessions are not cut later.
	tok, g = mk("untrack", 3600)
	c = &closer{}
	s.Track(g, c)()
	s.Revoke(ctx, owner, tok.ID)
	time.Sleep(20 * time.Millisecond)
	if c.n.Load() != 0 {
		t.Fatal("an ended session was closed again")
	}
}
