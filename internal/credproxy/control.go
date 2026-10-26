package credproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

// Control is the credential control plane behind the API: profiles,
// credentials and grants, scoped to their owner. Secrets go to Secrets and
// are never returned; profile and grant metadata live in the state
// database.
type Control struct {
	DB      *state.DB
	Secrets SecretStore
	Proxy   *Host
	Now     func() time.Time
}

func (c *Control) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func bodyHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func idemConflict() error {
	return domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key was used with a different request body")
}

// ListProfiles pages owner's profiles.
func (c *Control) ListProfiles(ctx context.Context, owner domain.PrincipalID, limit int, cursor string) (api.CredentialProfileList, error) {
	if limit <= 0 {
		limit = 100
	}
	out := api.CredentialProfileList{Items: []api.CredentialProfile{}}
	err := c.DB.Tx(ctx, func(tx *state.Tx) error {
		ps, next, err := tx.ListCredentialProfiles(owner, state.Page{Limit: limit, Cursor: cursor})
		for _, p := range ps {
			out.Items = append(out.Items, *p)
		}
		out.NextCursor = next
		return err
	})
	return out, err
}

// CreateProfile creates a profile; with key, a replay of the same body
// returns the same profile. A default profile clears the owner's previous
// default.
func (c *Control) CreateProfile(ctx context.Context, owner domain.PrincipalID, req api.CreateCredentialProfileRequest, key string) (*domain.CredentialProfile, error) {
	if !domain.ValidName(req.Name) {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "invalid profile name %q: use 1-63 lowercase letters, digits and '-'", req.Name).WithDetail("field", "name")
	}
	hash := bodyHash(req)
	var out *domain.CredentialProfile
	err := c.DB.Tx(ctx, func(tx *state.Tx) error {
		if key != "" {
			prev, h, err := tx.CredentialProfileByKey(owner, key)
			switch {
			case err == nil && h != hash:
				return idemConflict()
			case err == nil:
				out = prev
				return nil
			case !domain.IsCode(err, domain.CodeNotFound):
				return err
			}
		}
		now := c.now()
		p := &domain.CredentialProfile{ID: domain.NewID("cp"), Owner: owner, Name: req.Name, Default: req.Default,
			Credentials: []domain.Credential{}, CreatedAt: now, UpdatedAt: now}
		if req.Default {
			if err := c.clearDefault(tx, owner); err != nil {
				return err
			}
		}
		out = p
		return tx.PutCredentialProfileKeyed(p, key, hash)
	})
	return out, err
}

func (c *Control) clearDefault(tx *state.Tx, owner domain.PrincipalID) error {
	cursor := ""
	for {
		ps, next, err := tx.ListCredentialProfiles(owner, state.Page{Limit: 200, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, p := range ps {
			if p.Default {
				p.Default = false
				if err := c.put(tx, p); err != nil {
					return err
				}
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// put rewrites a profile, keeping the idempotency key of its create.
func (c *Control) put(tx *state.Tx, p *domain.CredentialProfile) error {
	key, hash, err := tx.CredentialProfileKeys(p.Owner, p.ID)
	if err != nil {
		return err
	}
	return tx.PutCredentialProfileKeyed(p, key, hash)
}

// GetProfile returns owner's profile by ID or name.
func (c *Control) GetProfile(ctx context.Context, owner domain.PrincipalID, ref string) (*domain.CredentialProfile, error) {
	var out *domain.CredentialProfile
	err := c.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		out, err = tx.GetCredentialProfile(owner, ref)
		return err
	})
	return out, err
}

// DeleteProfile revokes the profile's grants (profile_deleted), then removes
// it and its secrets.
func (c *Control) DeleteProfile(ctx context.Context, owner domain.PrincipalID, ref string) error {
	var p *domain.CredentialProfile
	var ids []string
	err := c.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		if p, err = tx.GetCredentialProfile(owner, ref); err != nil {
			return err
		}
		if ids, err = tx.RevokeGrantsWhere(state.GrantFilter{Profile: p.ID}, domain.RevokeProfileDeleted, c.now()); err != nil {
			return err
		}
		return tx.DeleteCredentialProfile(p.ID)
	})
	if err != nil {
		return err
	}
	c.Proxy.Cut(ids...)
	for _, cr := range p.Credentials {
		if err := c.Secrets.DeleteSecret(ctx, p.ID, cr.Name); err != nil {
			c.Proxy.log().Warn("delete credential secret", "profile", p.ID, "credential", cr.Name, "err", err)
		}
	}
	return nil
}

// PutCredential sets a credential of owner's profile. A non-empty secret
// replaces the stored one (grants use the new value from their next
// request); an empty one keeps it, and a new credential needs one.
func (c *Control) PutCredential(ctx context.Context, owner domain.PrincipalID, ref, name string, req api.PutCredentialRequest) (*domain.CredentialProfile, error) {
	cr := domain.Credential{Name: name, Kind: req.Kind, Provider: req.Provider, BaseURL: req.BaseURL, EnvVar: req.EnvVar}
	if err := validCredential(&cr); err != nil {
		return nil, err
	}
	var out *domain.CredentialProfile
	err := c.DB.Tx(ctx, func(tx *state.Tx) error {
		p, err := tx.GetCredentialProfile(owner, ref)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(p.Credentials, func(x domain.Credential) bool { return x.Name == name })
		if req.Secret == "" && (i < 0 || !p.Credentials[i].HasSecret) {
			return domain.Errorf(domain.CodeInvalidArgument, "secret is required for a new credential").WithDetail("field", "secret")
		}
		if req.Secret != "" {
			// Before the metadata: a grant must never find HasSecret without a
			// secret.
			if err := c.Secrets.PutSecret(ctx, p.ID, name, []byte(req.Secret)); err != nil {
				return err
			}
		}
		now := c.now()
		cr.HasSecret, cr.UpdatedAt = true, now
		if i < 0 {
			p.Credentials = append(p.Credentials, cr)
		} else {
			p.Credentials[i] = cr
		}
		p.UpdatedAt = now
		out = p
		return c.put(tx, p)
	})
	return out, err
}

func validCredential(cr *domain.Credential) error {
	if !domain.ValidName(cr.Name) {
		return domain.Errorf(domain.CodeInvalidArgument, "invalid credential name %q: use 1-63 lowercase letters, digits and '-'", cr.Name).WithDetail("field", "credentialName")
	}
	switch cr.Kind {
	case domain.CredentialAPIKey:
	case domain.CredentialCustomAPI:
		if cr.BaseURL == "" {
			return domain.Errorf(domain.CodeInvalidArgument, "a custom_api credential needs baseUrl").WithDetail("field", "baseUrl")
		}
	default:
		return domain.Errorf(domain.CodeInvalidArgument, "kind must be api_key or custom_api").WithDetail("field", "kind")
	}
	if err := ValidEnvVar(cr.EnvVar); err != nil {
		return err
	}
	if cr.BaseURL != "" {
		u, err := ValidUpstream(cr.BaseURL)
		if err != nil {
			return err
		}
		cr.BaseURL = u
	}
	_, err := UpstreamFor(*cr)
	return err
}

// DeleteCredential revokes the credential's grants (credential_removed) and
// removes it and its secret.
func (c *Control) DeleteCredential(ctx context.Context, owner domain.PrincipalID, ref, name string) error {
	var p *domain.CredentialProfile
	var ids []string
	err := c.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		if p, err = tx.GetCredentialProfile(owner, ref); err != nil {
			return err
		}
		i := slices.IndexFunc(p.Credentials, func(x domain.Credential) bool { return x.Name == name })
		if i < 0 {
			return domain.Errorf(domain.CodeNotFound, "credential %s not found in profile %s", name, p.Name)
		}
		if ids, err = tx.RevokeGrantsWhere(state.GrantFilter{Profile: p.ID, Credential: name}, domain.RevokeCredentialRemoved, c.now()); err != nil {
			return err
		}
		p.Credentials = slices.Delete(p.Credentials, i, i+1)
		p.UpdatedAt = c.now()
		return c.put(tx, p)
	})
	if err != nil {
		return err
	}
	c.Proxy.Cut(ids...)
	return c.Secrets.DeleteSecret(ctx, p.ID, name)
}

// ListGrants returns vm's grants, active and revoked, oldest first. The
// list is not paged (a VM has few grants); limit/cursor are accepted for
// the contract and a cursor is never returned.
func (c *Control) ListGrants(ctx context.Context, vm domain.VMID) (api.CredentialGrantList, error) {
	out := api.CredentialGrantList{Items: []api.CredentialGrant{}}
	err := c.DB.Tx(ctx, func(tx *state.Tx) error {
		gs, err := tx.Grants(vm)
		for _, g := range gs {
			out.Items = append(out.Items, *g)
		}
		return err
	})
	return out, err
}

// CreateGrant grants vm (owner's, already resolved) proxied use of a
// credential: its placeholder goes in the credential's envVar inside the
// guest, its upstream is the credential's. With key, a replay of the same
// body returns the same grant.
func (c *Control) CreateGrant(ctx context.Context, owner domain.PrincipalID, vm domain.VMID, req api.CreateCredentialGrantRequest, key string) (*domain.CredentialGrant, error) {
	hash := bodyHash(req)
	var out *domain.CredentialGrant
	err := c.DB.Tx(ctx, func(tx *state.Tx) error {
		if key != "" {
			prev, h, err := tx.GrantByKey(owner, vm, key)
			switch {
			case err == nil && h != hash:
				return idemConflict()
			case err == nil:
				out = prev
				return nil
			case !domain.IsCode(err, domain.CodeNotFound):
				return err
			}
		}
		p, err := tx.GetCredentialProfile(owner, req.Profile)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(p.Credentials, func(x domain.Credential) bool { return x.Name == req.Credential })
		if i < 0 {
			return domain.Errorf(domain.CodeNotFound, "credential %s not found in profile %s", req.Credential, p.Name).WithDetail("field", "credential")
		}
		cr := p.Credentials[i]
		up, err := UpstreamFor(cr)
		if err != nil {
			return err
		}
		g, gOwner, err := c.Proxy.newGrant(ctx, tx, GrantSpec{VM: vm, ProfileID: p.ID, Credential: cr.Name, EnvVar: cr.EnvVar, Upstream: up})
		if err != nil {
			return err
		}
		if gOwner != owner {
			return domain.Errorf(domain.CodeNotFound, "vm %s not found", vm)
		}
		out = g
		return tx.PutGrantKeyed(owner, g, key, hash)
	})
	return out, err
}

// RevokeGrant revokes one of vm's grants.
func (c *Control) RevokeGrant(ctx context.Context, owner domain.PrincipalID, vm domain.VMID, id string) (*domain.CredentialGrant, error) {
	err := c.DB.Tx(ctx, func(tx *state.Tx) error {
		g, gOwner, err := tx.GetGrant(id)
		if err == nil && (gOwner != owner || g.VMID != vm) {
			err = domain.Errorf(domain.CodeNotFound, "grant %s not found", id)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	g, err := c.Proxy.Revoke(ctx, id, domain.RevokeExplicit)
	return &g, err
}
