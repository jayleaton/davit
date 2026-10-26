// Package credproxy is the host credential-proxy seam (G4, parity v1
// contract). A VM never holds a real credential: it holds a grant's random
// placeholder, and the proxy swaps the placeholder for the secret on
// requests from that VM to the credential's upstream. Owner: network/gateway
// (transport, upstream policy); the control plane (profiles, grants API)
// calls this interface.
//
// Memory is a complete in-process fake of the semantics, including the
// revoke contract of domain.GrantAfter; the real proxy must pass the same
// tests.
package credproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"davit/internal/domain"
)

// SecretStore keeps credential secrets on the host, outside the state
// database and outside every guest.
type SecretStore interface {
	PutSecret(ctx context.Context, profileID, credential string, secret []byte) error
	Secret(ctx context.Context, profileID, credential string) ([]byte, error)
	DeleteSecret(ctx context.Context, profileID, credential string) error
}

// GrantSpec asks for one VM's proxied use of one credential.
type GrantSpec struct {
	VM         domain.VMID
	ProfileID  string
	Credential string
	EnvVar     string
	Upstream   string
}

// Resolved is what the proxy may forward for a guest request.
type Resolved struct {
	Grant  domain.CredentialGrant
	Secret []byte
}

// Proxy issues, revokes and resolves grants.
type Proxy interface {
	Grant(ctx context.Context, spec GrantSpec) (domain.CredentialGrant, error)
	Revoke(ctx context.Context, grantID string, reason domain.RevokeReason) (domain.CredentialGrant, error)
	// Lifecycle applies domain.GrantAfter for an event of vm.
	Lifecycle(ctx context.Context, vm domain.VMID, e domain.GrantEvent) error
	// Resolve authenticates a guest request: the placeholder must belong to
	// an active grant of exactly this VM (the proxy learns vm from the
	// transport, never from the guest's claim).
	Resolve(ctx context.Context, vm domain.VMID, placeholder string) (Resolved, error)
}

// Memory is the in-process fake.
type Memory struct {
	Secrets SecretStore
	Now     func() time.Time

	mu     sync.Mutex
	grants map[string]*domain.CredentialGrant // by ID
}

// NewMemory returns a fake proxy over secrets.
func NewMemory(secrets SecretStore) *Memory {
	return &Memory{Secrets: secrets, Now: time.Now, grants: map[string]*domain.CredentialGrant{}}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Grant implements Proxy.
func (m *Memory) Grant(ctx context.Context, s GrantSpec) (domain.CredentialGrant, error) {
	if _, err := m.Secrets.Secret(ctx, s.ProfileID, s.Credential); err != nil {
		return domain.CredentialGrant{}, err
	}
	g := &domain.CredentialGrant{
		ID: "grant_" + randomHex(8), VMID: s.VM, ProfileID: s.ProfileID, Credential: s.Credential,
		State: domain.GrantActive, Placeholder: "davit-placeholder-" + randomHex(24), EnvVar: s.EnvVar,
		Upstream: s.Upstream, CreatedAt: m.Now().UTC(),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants[g.ID] = g
	return *g, nil
}

// Revoke implements Proxy; revoking a revoked grant keeps its first reason.
func (m *Memory) Revoke(_ context.Context, id string, reason domain.RevokeReason) (domain.CredentialGrant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, ok := m.grants[id]
	if !ok {
		return domain.CredentialGrant{}, domain.Errorf(domain.CodeNotFound, "grant %s not found", id)
	}
	m.revoke(g, reason)
	return *g, nil
}

func (m *Memory) revoke(g *domain.CredentialGrant, reason domain.RevokeReason) {
	if g.State == domain.GrantActive {
		now := m.Now().UTC()
		g.State, g.RevokeReason, g.RevokedAt = domain.GrantRevoked, reason, &now
	}
}

// Lifecycle implements Proxy.
func (m *Memory) Lifecycle(_ context.Context, vm domain.VMID, e domain.GrantEvent) error {
	keep, _, reason := domain.GrantAfter(e)
	if keep {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, g := range m.grants {
		if g.VMID == vm {
			m.revoke(g, reason)
		}
	}
	return nil
}

// Resolve implements Proxy.
func (m *Memory) Resolve(ctx context.Context, vm domain.VMID, placeholder string) (Resolved, error) {
	m.mu.Lock()
	var found *domain.CredentialGrant
	for _, g := range m.grants {
		if g.Placeholder == placeholder {
			found = g
			break
		}
	}
	var g domain.CredentialGrant
	if found != nil {
		g = *found
	}
	m.mu.Unlock()
	if found == nil || g.VMID != vm || g.State != domain.GrantActive {
		// One answer for unknown, foreign and revoked placeholders: the
		// guest learns nothing about other grants.
		return Resolved{}, domain.Errorf(domain.CodePermissionDenied, "credential placeholder not valid for this VM")
	}
	secret, err := m.Secrets.Secret(ctx, g.ProfileID, g.Credential)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Grant: g, Secret: secret}, nil
}

// MemorySecrets is an in-memory SecretStore fake.
type MemorySecrets struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *MemorySecrets) key(p, c string) string { return p + "\x00" + c }

// PutSecret implements SecretStore.
func (s *MemorySecrets) PutSecret(_ context.Context, p, c string, secret []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[s.key(p, c)] = append([]byte(nil), secret...)
	return nil
}

// Secret implements SecretStore.
func (s *MemorySecrets) Secret(_ context.Context, p, c string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[s.key(p, c)]
	if !ok {
		return nil, domain.Errorf(domain.CodeNotFound, "credential %s/%s has no secret", p, c)
	}
	return append([]byte(nil), v...), nil
}

// DeleteSecret implements SecretStore.
func (s *MemorySecrets) DeleteSecret(_ context.Context, p, c string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, s.key(p, c))
	return nil
}
