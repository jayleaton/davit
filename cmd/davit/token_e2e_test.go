package main_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/cli"
)

// TestTokenCLI: create shows the secret once, list never does, revoke
// removes the token.
func TestTokenCLI(t *testing.T) {
	h := newHost(t)
	h.start(20*time.Millisecond, 0)
	h.waited(h.cli("new", "box"), "create")

	r := h.cli("token", "create", "ci", "--action", "exec", "--vm", "box", "--ttl", "2d")
	var tok api.CreatedToken
	json.Unmarshal(r.raw[len(r.raw)-1], &tok)
	if r.code != 0 || !strings.HasPrefix(tok.Secret, "slw_") || len(tok.Scopes) != 1 || !strings.HasPrefix(string(tok.Scopes[0].VMID), "vm_") ||
		tok.ExpiresAt.Sub(tok.CreatedAt) != 48*time.Hour {
		t.Fatalf("token create: exit %d %+v", r.code, tok)
	}
	r = h.cli("token", "list")
	if r.code != 0 || len(r.lines) != 2 || r.lines[0].Event != "item" || strings.Contains(string(r.raw[0]), tok.Secret) ||
		!strings.Contains(string(r.raw[0]), tok.ID) {
		t.Fatalf("token list: exit %d %s", r.code, r.raw)
	}
	if r = h.cli("token", "create", "x"); r.code != cli.ExitUsage {
		t.Fatalf("create without --action: exit %d", r.code)
	}
	if r = h.cli("token", "revoke", tok.ID, "--yes"); r.code != 0 {
		t.Fatalf("revoke: exit %d %+v", r.code, r.last())
	}
	if r = h.cli("token", "list"); r.code != 0 || len(r.lines) != 1 {
		t.Fatalf("list after revoke: %s", r.raw)
	}
	if r = h.cli("token", "revoke", tok.ID, "--yes"); r.code != cli.ExitCode("not_found") {
		t.Fatalf("second revoke: exit %d", r.code)
	}
}
