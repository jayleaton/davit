package credproxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"davit/internal/domain"
)

// TestGrantContract: a placeholder resolves only for the VM it was granted
// to, never carries the secret, survives stop/resume, is refused from a fork
// child that copied it, and dies with revoke or VM delete.
func TestGrantContract(t *testing.T) {
	secrets := &MemorySecrets{}
	grantContract(t, NewMemory(secrets), secrets)
}

// grantContract is the contract every Proxy passes (vm_a and vm_child must
// exist for proxies that check).
func grantContract(t *testing.T, p Proxy, secrets SecretStore) {
	t.Helper()
	ctx := context.Background()
	const key = "sk-real-key"
	if err := secrets.PutSecret(ctx, "cp_1", "anthropic", []byte(key)); err != nil {
		t.Fatal(err)
	}
	g, err := p.Grant(ctx, GrantSpec{VM: "vm_a", ProfileID: "cp_1", Credential: "anthropic", EnvVar: "ANTHROPIC_API_KEY", Upstream: "https://api.anthropic.com"})
	if err != nil {
		t.Fatal(err)
	}
	if doc, _ := json.Marshal(g); strings.Contains(string(doc), key) || strings.Contains(g.Placeholder, key) {
		t.Fatalf("grant carries the secret: %s", doc)
	}
	if r, err := p.Resolve(ctx, "vm_a", g.Placeholder); err != nil || string(r.Secret) != key {
		t.Fatalf("resolve from the granted VM: %v", err)
	}
	for _, e := range []domain.GrantEvent{domain.GrantOnStop, domain.GrantOnResume, domain.GrantOnFork} {
		if err := p.Lifecycle(ctx, "vm_a", e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Resolve(ctx, "vm_a", g.Placeholder); err != nil {
		t.Fatalf("after stop/resume/fork of the source: %v", err)
	}
	// A fork child's disk holds the same placeholder; it is not its grant.
	if _, err := p.Resolve(ctx, "vm_child", g.Placeholder); !domain.IsCode(err, domain.CodePermissionDenied) {
		t.Fatalf("fork child resolved the source's placeholder: %v", err)
	}
	if _, err := p.Resolve(ctx, "vm_a", "davit-placeholder-unknown"); !domain.IsCode(err, domain.CodePermissionDenied) {
		t.Fatalf("unknown placeholder: %v", err)
	}

	r, err := p.Revoke(ctx, g.ID, domain.RevokeExplicit)
	if err != nil || r.State != domain.GrantRevoked || r.RevokeReason != domain.RevokeExplicit {
		t.Fatalf("revoke: %+v %v", r, err)
	}
	if _, err := p.Resolve(ctx, "vm_a", g.Placeholder); !domain.IsCode(err, domain.CodePermissionDenied) {
		t.Fatalf("revoked placeholder resolved: %v", err)
	}

	g2, err := p.Grant(ctx, GrantSpec{VM: "vm_a", ProfileID: "cp_1", Credential: "anthropic", EnvVar: "ANTHROPIC_API_KEY", Upstream: "https://api.anthropic.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(ctx, "vm_a", g2.Placeholder); err != nil {
		t.Fatalf("second grant: %v", err)
	}
	if err := p.Lifecycle(ctx, "vm_a", domain.GrantOnDelete); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(ctx, "vm_a", g2.Placeholder); !domain.IsCode(err, domain.CodePermissionDenied) {
		t.Fatalf("grant survived VM delete: %v", err)
	}
	if r, _ := p.Revoke(ctx, g.ID, domain.RevokeVMDeleted); r.RevokeReason != domain.RevokeExplicit {
		t.Fatalf("a second revoke rewrote the reason: %+v", r)
	}
	if _, err := p.Grant(ctx, GrantSpec{VM: "vm_a", ProfileID: "cp_1", Credential: "missing"}); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("grant of a credential without a secret: %v", err)
	}
}
