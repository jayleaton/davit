package main_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/cli"
	"davit/internal/domain"
)

// TestEndToEndCredentials drives `davit credentials` against a daemon:
// the secret goes in on stdin and never comes back out, nor into the state
// database; the grant hands out only a placeholder; deleting the VM
// revokes it.
func TestEndToEndCredentials(t *testing.T) {
	h := newHost(t)
	h.start(10*time.Millisecond, 0)
	const secret = "sk-ant-e2e-SECRET"
	h.waited(h.cli("new", "web"), domain.OpCreate)

	ok := func(r run) json.RawMessage {
		t.Helper()
		if r.code != 0 {
			t.Fatalf("exit %d: %+v %s", r.code, r.last().Error, r.stderr)
		}
		for _, raw := range r.raw {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("output carries the secret: %s", raw)
			}
		}
		return r.raw[len(r.raw)-1]
	}
	ok(h.cli("credentials", "profile", "create", "work", "--default"))
	h.stdin = secret + "\n"
	ok(h.cli("credentials", "set", "work", "anthropic", "--provider", "anthropic", "--env", "ANTHROPIC_API_KEY"))
	var p api.CredentialProfile
	json.Unmarshal(ok(h.cli("credentials", "profile", "show", "work")), &p)
	if !p.Default || len(p.Credentials) != 1 || !p.Credentials[0].HasSecret || p.Credentials[0].EnvVar != "ANTHROPIC_API_KEY" {
		t.Fatalf("profile: %+v", p)
	}
	var g api.CredentialGrant
	json.Unmarshal(ok(h.cli("credentials", "grant", "web", "work", "anthropic")), &g)
	if g.State != domain.GrantActive || !strings.HasPrefix(g.Placeholder, "davit-placeholder-") || g.Upstream != "https://api.anthropic.com" {
		t.Fatalf("grant: %+v", g)
	}
	h.expectError(h.cli("credentials", "set", "work", "other", "--env", "X"), cli.ExitUsage, domain.CodeInvalidArgument) // no secret on stdin

	h.waited(h.cli("delete", "web", "--yes"), domain.OpDelete)
	// The grant list of a deleted VM is no longer reachable by name; read
	// the record by ID.
	r := h.cli("credentials", "grants", string(g.VMID))
	ok(r)
	var items []api.CredentialGrant
	for i, l := range r.lines {
		if l.Event == "item" {
			var x api.CredentialGrant
			json.Unmarshal(r.raw[i], &x)
			items = append(items, x)
		}
	}
	if len(items) != 1 || items[0].State != domain.GrantRevoked || items[0].RevokeReason != domain.RevokeVMDeleted {
		t.Fatalf("grants after delete: %+v", items)
	}
	ok(h.cli("credentials", "rm", "work", "anthropic", "--yes"))
	ok(h.cli("credentials", "profile", "delete", "work", "--yes"))

	data := filepath.Join(h.dir, "data")
	_ = filepath.WalkDir(data, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, _ := os.ReadFile(path); strings.Contains(string(b), secret) {
			t.Errorf("secret found in %s after its deletion", path)
		}
		return nil
	})
}
