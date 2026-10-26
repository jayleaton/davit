package tokens

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

type vms map[string]domain.VMID

func (v vms) GetVM(_ context.Context, _ domain.PrincipalID, ref string) (*domain.VM, error) {
	if id, ok := v[ref]; ok {
		return &domain.VM{ID: id}, nil
	}
	return nil, domain.Errorf(domain.CodeNotFound, "vm %q not found", ref)
}

func isCode(err error, c domain.Code) bool { return errors.Is(err, &domain.Error{Code: c}) }

func TestTokensLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := state.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := &Service{DB: db, VMs: vms{"devbox": "vm_a"}, Now: func() time.Time { return now }}
	owner := domain.PrincipalID("local:1000")

	req := api.CreateTokenRequest{Name: "ci", TTLSeconds: 3600,
		Scopes: []api.TokenScope{{VMID: "devbox", Actions: []string{"exec", "exec"}}}}
	tok, err := s.Create(ctx, owner, req, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.Secret, SecretPrefix) || len(tok.Secret) < 40 || tok.Scopes[0].VMID != "vm_a" ||
		len(tok.Scopes[0].Actions) != 1 || !tok.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("created %+v", tok)
	}

	// The secret itself is never stored: only its hash is.
	var stored *state.TokenRecord
	db.Tx(ctx, func(tx *state.Tx) (err error) { stored, err = tx.GetToken(owner, tok.ID); return err })
	if strings.Contains(string(stored.Hash), tok.Secret) || len(stored.Hash) != 32 {
		t.Fatal("token record holds the secret")
	}

	// Replays: same key and body within the window return the same token;
	// a different body is a conflict; after the window, never a new token.
	again, err := s.Create(ctx, owner, req, "k1")
	if err != nil || again.ID != tok.ID || again.Secret != tok.Secret {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err := s.Create(ctx, owner, api.CreateTokenRequest{Name: "other", TTLSeconds: 60, Scopes: req.Scopes}, "k1"); !isCode(err, domain.CodeIdempotencyConflict) {
		t.Fatalf("reused key, other body: %v", err)
	}
	now = now.Add(replayWindow + time.Second)
	if _, err := s.Create(ctx, owner, req, "k1"); !isCode(err, domain.CodeInvalidState) {
		t.Fatalf("replay after the window: %v", err)
	}
	if l, _ := s.List(ctx, owner, 0, ""); len(l.Items) != 1 {
		t.Fatalf("replays created tokens: %+v", l)
	}

	// Verify: scopes nest (exec implies view, not admin) and stay on their VM.
	p, g, err := s.Verify(ctx, tok.Secret)
	if err != nil || p != owner {
		t.Fatalf("verify: %q %v", p, err)
	}
	if !g.Allows("view", "vm_a") || !g.Allows("exec", "vm_a") || g.Allows("admin", "vm_a") || g.Allows("view", "vm_b") ||
		g.Allows("view", "") || !g.AllowsAny("view") {
		t.Fatalf("grant %+v", g)
	}
	admin := &Grant{Scopes: []api.TokenScope{{Actions: []string{"admin"}}}}
	if !admin.Allows("exec", "vm_b") || !admin.Allows("view", "") {
		t.Fatal("an all-VM admin scope must allow everything")
	}
	if _, _, err := s.Verify(ctx, tok.Secret+"x"); !isCode(err, domain.CodeUnauthenticated) {
		t.Fatalf("wrong secret: %v", err)
	}
	db.Tx(ctx, func(tx *state.Tx) (err error) { stored, err = tx.GetToken(owner, tok.ID); return err })
	if stored.Token.LastUsedAt == nil || !stored.Token.LastUsedAt.Equal(now) {
		t.Fatalf("lastUsedAt %v", stored.Token.LastUsedAt)
	}

	// Validation.
	for name, r := range map[string]api.CreateTokenRequest{
		"no scopes":   {Name: "x", TTLSeconds: 60},
		"bad action":  {Name: "x", TTLSeconds: 60, Scopes: []api.TokenScope{{Actions: []string{"root"}}}},
		"no actions":  {Name: "x", TTLSeconds: 60, Scopes: []api.TokenScope{{}}},
		"ttl too big": {Name: "x", TTLSeconds: int64(MaxTTL/time.Second) + 1, Scopes: req.Scopes},
		"no name":     {TTLSeconds: 60, Scopes: req.Scopes},
	} {
		if _, err := s.Create(ctx, owner, r, ""); !isCode(err, domain.CodeInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.Create(ctx, owner, api.CreateTokenRequest{Name: "x", TTLSeconds: 60,
		Scopes: []api.TokenScope{{VMID: "nope", Actions: []string{"view"}}}}, ""); !isCode(err, domain.CodeNotFound) {
		t.Errorf("unknown VM scope: %v", err)
	}

	// Expiry and revocation take effect on the next use.
	now = tok.ExpiresAt
	if _, _, err := s.Verify(ctx, tok.Secret); !isCode(err, domain.CodeUnauthenticated) {
		t.Fatalf("expired token: %v", err)
	}
	now = tok.CreatedAt.Add(time.Minute)
	if err := s.Revoke(ctx, owner, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Verify(ctx, tok.Secret); !isCode(err, domain.CodeUnauthenticated) {
		t.Fatalf("revoked token: %v", err)
	}
	if err := s.Revoke(ctx, owner, tok.ID); !isCode(err, domain.CodeNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	if err := s.Revoke(ctx, "local:2000", tok.ID); !isCode(err, domain.CodeNotFound) {
		t.Fatalf("another principal revoked: %v", err)
	}
	if l, _ := s.List(ctx, owner, 0, ""); len(l.Items) != 0 {
		t.Fatalf("revoked token listed: %+v", l)
	}
}
