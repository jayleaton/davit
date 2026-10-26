package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/tokens"
)

// OwnerPolicy names the tailnet identities that act as the host's single
// owner (SPEC §4: v0 has one configured owner). Tailnet membership alone
// grants nothing.
type OwnerPolicy struct {
	// Logins are tailnet login names (e.g. "alice@example.com"), matched
	// case-insensitively against untagged nodes only.
	Logins []string
	// Tags are ACL tags (e.g. "tag:ci"); a tagged node is the owner if it
	// carries any of them. Tagged nodes never match by login.
	Tags []string
}

// Validate rejects a policy that would admit nobody or malformed entries.
func (p OwnerPolicy) Validate() error {
	if len(p.Logins) == 0 && len(p.Tags) == 0 {
		return errors.New("gateway: owner policy needs at least one login or tag")
	}
	for _, l := range p.Logins {
		if l == "" || strings.ContainsAny(l, " \t\r\n") {
			return fmt.Errorf("gateway: invalid owner login %q", l)
		}
	}
	for _, t := range p.Tags {
		if !strings.HasPrefix(t, "tag:") || len(t) == len("tag:") {
			return fmt.Errorf("gateway: owner tag %q must look like tag:<name>", t)
		}
	}
	return nil
}

// IsOwner reports whether id is the host owner.
func (p OwnerPolicy) IsOwner(id Identity) bool {
	if id.Tagged() {
		for _, t := range id.Tags {
			if slices.Contains(p.Tags, t) {
				return true
			}
		}
		return false
	}
	if id.LoginName == "" {
		return false
	}
	for _, l := range p.Logins {
		if strings.EqualFold(l, id.LoginName) {
			return true
		}
	}
	return false
}

// Authenticator maps requests that passed RequireIdentity to the owner
// principal, satisfying httpapi.Authenticator. Every other identity is
// refused, so the API handler only ever runs as the owner.
type Authenticator struct {
	// Members grants other tailnet users what is shared with them; nil
	// refuses everyone but the owner.
	Members MemberDirectory
	// Owners decides who the owner is (BoundOwners in the daemon).
	Owners OwnerMatcher
	// Principal is the owner principal; use the same one as the local socket
	// so both transports see the same VMs.
	Principal domain.PrincipalID
	// Tokens verifies service-token bearer secrets; nil refuses bearers.
	Tokens TokenVerifier
}

// TokenVerifier authenticates a bearer secret (tokens.Service).
type TokenVerifier interface {
	Verify(ctx context.Context, secret string) (domain.PrincipalID, *tokens.Grant, error)
}

// Authenticate implements httpapi.Authenticator for callers that do not
// enforce grants: it refuses bearer requests rather than granting a token
// its issuer's full rights.
func (a Authenticator) Authenticate(r *http.Request) (domain.PrincipalID, error) {
	p, g, err := a.AuthenticateGrant(r)
	if err == nil && g != nil {
		return "", domain.Errorf(domain.CodeUnauthenticated, "service tokens are not accepted here")
	}
	return p, err
}

// Identify implements httpapi.Identifier: the tailnet identity the gateway
// resolved (a token request reports auth "token" from the grant).
func (a Authenticator) Identify(r *http.Request) (method, identity string) {
	if id, ok := IdentityFromContext(r.Context()); ok {
		return api.AuthTailnet, id.String()
	}
	return api.AuthTailnet, ""
}

// AuthenticateGrant implements httpapi.GrantAuthenticator. Every request
// needs a tailnet identity (RequireIdentity). With "Authorization: Bearer
// <secret>" it runs as the token's issuer, limited to the token's grant; the
// tailnet ACL decides who may reach the listener, the token what they may
// do. Without one, only the owner identity is accepted.
func (a Authenticator) AuthenticateGrant(r *http.Request) (domain.PrincipalID, *tokens.Grant, error) {
	id, ok := IdentityFromContext(r.Context())
	if !ok {
		return "", nil, domain.Errorf(domain.CodeUnauthenticated, "tailnet identity required")
	}
	if h := r.Header.Get("Authorization"); h != "" {
		secret, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || a.Tokens == nil || secret == "" {
			return "", nil, domain.Errorf(domain.CodeUnauthenticated, "unsupported Authorization header (want Bearer <token>)")
		}
		p, g, err := a.Tokens.Verify(r.Context(), strings.TrimSpace(secret))
		if err != nil {
			return "", nil, err
		}
		if p != a.Principal { // a token cannot exceed its issuer, who must be this host's owner
			return "", nil, domain.Errorf(domain.CodeUnauthenticated, "token was not issued by this host's owner")
		}
		return p, g, nil
	}
	if a.Principal != "" && a.Owners != nil && a.Owners.IsOwner(id) {
		return a.Principal, nil, nil
	}
	// A member acts within the owner's resources, limited to its shares.
	if a.Principal != "" && a.Members != nil {
		if g, ok := a.Members.MemberGrant(id); ok {
			return a.Principal, g, nil
		}
	}
	return "", nil, domain.Errorf(domain.CodePermissionDenied, "tailnet identity is not authorized on this host")
}

// VMLookup resolves a VM name or ID within a principal's scope;
// *lifecycle.Service satisfies it.
type VMLookup interface {
	GetVM(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.VM, error)
}

// OwnerAuthorizer is the v0 Authorizer: the owner may use every VM it owns
// that is ready or running; nobody else may use any.
type OwnerAuthorizer struct {
	Owners    OwnerMatcher
	Principal domain.PrincipalID
	VMs       VMLookup
	// Members may use VMs shared with them with exec (ssh, forwards).
	Members MemberDirectory
}

// Authorize implements Authorizer. The VM is looked up afresh on every call,
// so a stop, delete or new generation takes effect on the next connection.
func (a OwnerAuthorizer) Authorize(ctx context.Context, who Identity, req Request) (Target, error) {
	if a.Principal == "" || a.VMs == nil || a.Owners == nil {
		return Target{}, ErrDenied
	}
	var member *tokens.Grant
	if !a.Owners.IsOwner(who) {
		g, ok := (*tokens.Grant)(nil), false
		if a.Members != nil {
			g, ok = a.Members.MemberGrant(who)
		}
		if !ok {
			return Target{}, ErrDenied
		}
		member = g
	}
	vm, err := a.VMs.GetVM(ctx, a.Principal, req.VMRef)
	if err == nil && member != nil {
		switch {
		case !member.Allows(api.TokenView, vm.ID):
			return Target{}, ErrNotFound // invisible to the member
		case !member.Allows(api.TokenExec, vm.ID):
			return Target{}, ErrDenied
		}
	}
	if errors.Is(err, &domain.Error{Code: domain.CodeNotFound}) {
		return Target{}, ErrNotFound
	}
	if err != nil {
		return Target{}, err
	}
	switch vm.State {
	case domain.StateReady, domain.StateRunning:
		return Target{VMID: vm.ID, Generation: vm.InstanceGeneration}, nil
	case domain.StateDeleting, domain.StateDeleted:
		return Target{}, ErrNotFound
	default:
		return Target{}, fmt.Errorf("%w: vm is %s", ErrNotRunning, vm.State)
	}
}
