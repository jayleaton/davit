// Package tokens implements service tokens (SPEC §4 "Authentication and
// authorization"): random bearer secrets for automation, scoped by action
// and optionally by VM, expiring and revocable. Only the SHA-256 of a secret
// is stored; the secret is returned once, at creation.
//
// A token acts as its issuer (the owner principal), limited to its scopes.
// Actions nest: exec implies view, admin implies exec and view. The tailnet
// listener still requires a WhoIs identity for every request; the token only
// decides what that request may do.
package tokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

// Limits.
const (
	MaxTTL       = 365 * 24 * time.Hour
	maxScopes    = 16
	maxNameBytes = 64
	// replayWindow is how long a createToken replay can still return the
	// secret (kept in memory only, never stored).
	replayWindow = 10 * time.Minute
	// touchEvery bounds lastUsedAt writes to one per token per interval.
	touchEvery = time.Minute
	// SecretPrefix marks davit token secrets (and helps secret scanners).
	SecretPrefix = "slw_"
)

// VMs resolves a VM name or ID within the issuer's scope (lifecycle).
type VMs interface {
	GetVM(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.VM, error)
}

// Service is safe for concurrent use.
type Service struct {
	DB  *state.DB
	VMs VMs
	Now func() time.Time

	mu     sync.Mutex
	replay map[replayKey]replayed
	// uses are requests per token not yet written (useCount), with the
	// time of the latest; they are written with lastUsedAt at most once per
	// touchEvery per token, and by Flush.
	uses map[string]pendingUse
	// live are tracked sessions (sessions.go).
	live     map[uint64]*tracked
	nextLive uint64
}

type pendingUse struct {
	owner domain.PrincipalID
	n     int64
	last  time.Time
}

type replayKey struct {
	owner domain.PrincipalID
	key   string
}

type replayed struct {
	t  api.CreatedToken
	at time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func hashSecret(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

func invalid(field, format string, args ...any) error {
	return domain.Errorf(domain.CodeInvalidArgument, format, args...).WithDetail("field", field)
}

// normalize validates req and resolves VM scopes to IDs.
func (s *Service) normalize(ctx context.Context, owner domain.PrincipalID, req api.CreateTokenRequest) ([]api.TokenScope, error) {
	name := req.Name
	if name == "" || len(name) > maxNameBytes || strings.ContainsFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return nil, invalid("name", "name must be 1..%d printable bytes", maxNameBytes)
	}
	if req.TTLSeconds < 1 || time.Duration(req.TTLSeconds)*time.Second > MaxTTL {
		return nil, invalid("ttlSeconds", "ttlSeconds must be 1..%d", int64(MaxTTL/time.Second))
	}
	if len(req.Scopes) == 0 || len(req.Scopes) > maxScopes {
		return nil, invalid("scopes", "scopes must have 1..%d entries", maxScopes)
	}
	out := make([]api.TokenScope, 0, len(req.Scopes))
	for _, sc := range req.Scopes {
		if len(sc.Actions) == 0 {
			return nil, invalid("scopes", "every scope needs at least one action")
		}
		acts := slices.Clone(sc.Actions)
		for _, a := range acts {
			if a != api.TokenView && a != api.TokenExec && a != api.TokenAdmin {
				return nil, invalid("scopes", "unknown action %q (want view, exec or admin)", a)
			}
		}
		slices.Sort(acts)
		acts = slices.Compact(acts)
		n := api.TokenScope{Actions: acts}
		if sc.VMID != "" {
			vm, err := s.VMs.GetVM(ctx, owner, string(sc.VMID))
			if err != nil {
				return nil, err
			}
			n.VMID = vm.ID
		}
		out = append(out, n)
	}
	return out, nil
}

// Create issues a token for owner. key is the request's Idempotency-Key.
func (s *Service) Create(ctx context.Context, owner domain.PrincipalID, req api.CreateTokenRequest, key string) (api.CreatedToken, error) {
	body, _ := json.Marshal(req)
	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])
	scopes, err := s.normalize(ctx, owner, req)
	if err != nil {
		return api.CreatedToken{}, err
	}
	// Serialize keyed creation so a concurrent replay sees the first result.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replay == nil {
		s.replay = map[replayKey]replayed{}
	}
	now := s.now()
	for k, r := range s.replay {
		if now.Sub(r.at) > replayWindow {
			delete(s.replay, k)
		}
	}
	if key != "" {
		var prev *state.TokenRecord
		err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) { prev, err = tx.TokenByKey(owner, key); return err })
		switch {
		case err == nil && prev.BodyHash != bodyHash:
			return api.CreatedToken{}, domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key %q was used for a different token request", key)
		case err == nil:
			if r, ok := s.replay[replayKey{owner, key}]; ok {
				return r.t, nil
			}
			return api.CreatedToken{}, domain.Errorf(domain.CodeInvalidState,
				"token %s was already created with this Idempotency-Key; its secret was shown once and is not kept", prev.Token.ID).
				WithDetail("tokenId", prev.Token.ID)
		case !errors.Is(err, &domain.Error{Code: domain.CodeNotFound}):
			return api.CreatedToken{}, err
		}
	}

	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return api.CreatedToken{}, err
	}
	secret := SecretPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	t := api.Token{ID: domain.NewID("tok"), Name: req.Name, Scopes: scopes, CreatedAt: now,
		ExpiresAt: now.Add(time.Duration(req.TTLSeconds) * time.Second)}
	rec := &state.TokenRecord{Token: t, Owner: owner, Hash: hashSecret(secret), IdempotencyKey: key, BodyHash: bodyHash}
	if err := s.DB.Tx(ctx, func(tx *state.Tx) error { return tx.PutToken(rec) }); err != nil {
		return api.CreatedToken{}, err
	}
	out := api.CreatedToken{ID: t.ID, Name: t.Name, Scopes: t.Scopes, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, Secret: secret}
	if key != "" {
		s.replay[replayKey{owner, key}] = replayed{t: out, at: now}
	}
	return out, nil
}

// List pages owner's active tokens.
func (s *Service) List(ctx context.Context, owner domain.PrincipalID, limit int, cursor string) (api.TokenList, error) {
	if limit <= 0 {
		limit = 50
	}
	var recs []*state.TokenRecord
	var next string
	err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		recs, next, err = tx.ListTokens(owner, s.now(), state.Page{Limit: min(limit, 200), Cursor: cursor})
		return err
	})
	if err != nil {
		return api.TokenList{}, err
	}
	out := api.TokenList{Items: []api.Token{}, NextCursor: next}
	for _, r := range recs {
		out.Items = append(out.Items, s.withPending(r.Token))
	}
	return out, nil
}

// Revoke revokes owner's token. A revoked or unknown token is not found.
func (s *Service) Revoke(ctx context.Context, owner domain.PrincipalID, id string) error {
	err := s.DB.Tx(ctx, func(tx *state.Tx) error {
		r, err := tx.GetToken(owner, id)
		if err != nil {
			return err
		}
		if r.RevokedAt != nil {
			return domain.Errorf(domain.CodeNotFound, "token not found")
		}
		now := s.now()
		r.RevokedAt = &now
		return tx.PutToken(r)
	})
	if err == nil {
		s.cut(id)
	}
	return err
}

// Grant is what an authenticated token, or a tailnet member, may do.
type Grant struct {
	TokenID string
	Scopes  []api.TokenScope
	// ExpiresAt is when the token expires (zero for members).
	ExpiresAt time.Time
	// Member is set when the grant is a tailnet member's shares rather
	// than a token: members get 404 for what is not shared with them, may
	// list only what is, and are refused member-excluded routes (creating
	// VMs, credential grants) whatever their role.
	Member *Member
	// Snapshots and Environments are per-resource shares (ID -> action),
	// members only.
	Snapshots    map[string]string
	Environments map[string]string
}

// Member identifies the tailnet member a grant belongs to.
type Member struct {
	ID     string
	Login  string
	UserID int64
}

// AllowsSnapshot reports whether the grant permits action on a snapshot
// through a snapshot share.
func (g *Grant) AllowsSnapshot(action, id string) bool {
	a, ok := g.Snapshots[id]
	return ok && implies(a, action)
}

// AllowsEnvironment reports whether the grant permits action on an
// environment through an environment share.
func (g *Grant) AllowsEnvironment(action, id string) bool {
	a, ok := g.Environments[id]
	return ok && implies(a, action)
}

// VMs lists the VMs a scoped grant reaches with action (nil and all=true
// for an all-VM scope).
func (g *Grant) VMs(action string) (vms map[domain.VMID]bool, all bool) {
	vms = map[domain.VMID]bool{}
	for _, sc := range g.Scopes {
		for _, a := range sc.Actions {
			if implies(a, action) {
				if sc.VMID == "" {
					return nil, true
				}
				vms[sc.VMID] = true
			}
		}
	}
	return vms, false
}

// implies reports whether a granted action covers the wanted one.
func implies(granted, want string) bool {
	switch granted {
	case api.TokenAdmin:
		return true
	case api.TokenExec:
		return want == api.TokenExec || want == api.TokenView
	}
	return granted == want
}

// Allows reports whether the grant permits action on vm. vm "" is a request
// that is not about one VM (lists, images): only all-VM scopes allow it.
func (g *Grant) Allows(action string, vm domain.VMID) bool {
	for _, sc := range g.Scopes {
		if sc.VMID != "" && sc.VMID != vm {
			continue
		}
		for _, a := range sc.Actions {
			if implies(a, action) {
				return true
			}
		}
	}
	return false
}

// AllowsAny reports whether any scope grants action (VM-independent reads
// such as capabilities and images).
func (g *Grant) AllowsAny(action string) bool {
	for _, sc := range g.Scopes {
		for _, a := range sc.Actions {
			if implies(a, action) {
				return true
			}
		}
	}
	return false
}

// Verify authenticates a bearer secret: it returns the issuing owner and the
// token's grant, or unauthenticated for unknown, expired or revoked tokens.
func (s *Service) Verify(ctx context.Context, secret string) (domain.PrincipalID, *Grant, error) {
	unauth := domain.Errorf(domain.CodeUnauthenticated, "invalid, expired or revoked token")
	if !strings.HasPrefix(secret, SecretPrefix) {
		return "", nil, unauth
	}
	var rec *state.TokenRecord
	err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) { rec, err = tx.TokenByHash(hashSecret(secret)); return err })
	if errors.Is(err, &domain.Error{Code: domain.CodeNotFound}) {
		return "", nil, unauth
	}
	if err != nil {
		return "", nil, err
	}
	now := s.now()
	if rec.RevokedAt != nil || !now.Before(rec.Token.ExpiresAt) {
		return "", nil, unauth
	}
	s.mu.Lock()
	if s.uses == nil {
		s.uses = map[string]pendingUse{}
	}
	u := s.uses[rec.Token.ID]
	u.owner, u.n, u.last = rec.Owner, u.n+1, now
	s.uses[rec.Token.ID] = u
	s.mu.Unlock()
	if lu := rec.Token.LastUsedAt; lu == nil || now.Sub(*lu) >= touchEvery {
		s.flush(ctx, rec.Token.ID)
	}
	return rec.Owner, &Grant{TokenID: rec.Token.ID, Scopes: rec.Token.Scopes, ExpiresAt: rec.Token.ExpiresAt}, nil
}

// flush writes id's pending uses (useCount and lastUsedAt). On failure the
// uses stay pending for the next attempt.
func (s *Service) flush(ctx context.Context, id string) {
	s.mu.Lock()
	u, ok := s.uses[id]
	delete(s.uses, id)
	s.mu.Unlock()
	if !ok || u.n == 0 {
		return
	}
	err := s.DB.Tx(ctx, func(tx *state.Tx) error {
		r, err := tx.GetToken(u.owner, id)
		if err != nil {
			return err
		}
		r.Token.UseCount += u.n
		if r.Token.LastUsedAt == nil || u.last.After(*r.Token.LastUsedAt) {
			last := u.last
			r.Token.LastUsedAt = &last
		}
		return tx.PutToken(r)
	})
	if err != nil && !domain.IsCode(err, domain.CodeNotFound) {
		s.mu.Lock()
		p := s.uses[id]
		p.owner, p.n = u.owner, p.n+u.n
		if u.last.After(p.last) {
			p.last = u.last
		}
		s.uses[id] = p
		s.mu.Unlock()
	}
}

// Flush writes every token's pending uses. Run calls it periodically and
// once more when its context ends; a daemon crash loses at most the uses
// since the last flush.
func (s *Service) Flush(ctx context.Context) {
	s.mu.Lock()
	ids := make([]string, 0, len(s.uses))
	for id := range s.uses {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.flush(ctx, id)
	}
}

// Run flushes pending uses every touchEvery until ctx ends, then once more.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(touchEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Flush(ctx)
		case <-ctx.Done():
			s.Flush(context.WithoutCancel(ctx))
			return
		}
	}
}

// withPending adds uses not yet written to t, so listings are exact.
func (s *Service) withPending(t api.Token) api.Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.uses[t.ID]; ok {
		t.UseCount += u.n
		if t.LastUsedAt == nil || u.last.After(*t.LastUsedAt) {
			last := u.last
			t.LastUsedAt = &last
		}
	}
	return t
}

// Get returns owner's live token (expired and revoked tokens are not found).
func (s *Service) Get(ctx context.Context, owner domain.PrincipalID, id string) (api.Token, error) {
	var r *state.TokenRecord
	if err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) { r, err = tx.GetToken(owner, id); return err }); err != nil {
		return api.Token{}, err
	}
	if r.RevokedAt != nil || !s.now().Before(r.Token.ExpiresAt) {
		return api.Token{}, domain.Errorf(domain.CodeNotFound, "token not found")
	}
	return s.withPending(r.Token), nil
}

// Rotate issues a new token with id's name and scopes and retires id: at
// once, or after req.OverlapSeconds (its expiry is brought forward; it never
// lives longer than before). The new token lives req.TTLSeconds, else as
// long as id was issued for. With key, a replay within the createToken
// replay window returns the same new token (its secret is held in memory
// only); later, a replay is refused like createToken's.
func (s *Service) Rotate(ctx context.Context, owner domain.PrincipalID, id string, req api.RotateTokenRequest, key string) (api.CreatedToken, error) {
	if req.OverlapSeconds < 0 || time.Duration(req.OverlapSeconds)*time.Second > MaxTTL {
		return api.CreatedToken{}, invalid("overlapSeconds", "overlapSeconds must be 0..%d", int64(MaxTTL/time.Second))
	}
	rkey := ""
	if key != "" {
		rkey = "rotate/" + id + "/" + key
		if out, ok, err := s.replayed(ctx, owner, rkey); ok || err != nil {
			return out, err
		}
	}
	var old *state.TokenRecord
	if err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) { old, err = tx.GetToken(owner, id); return err }); err != nil {
		return api.CreatedToken{}, err
	}
	now := s.now()
	if old.RevokedAt != nil || !now.Before(old.Token.ExpiresAt) {
		return api.CreatedToken{}, domain.Errorf(domain.CodeNotFound, "token not found")
	}
	ttl := int64(old.Token.ExpiresAt.Sub(old.Token.CreatedAt) / time.Second)
	if req.TTLSeconds != nil {
		ttl = *req.TTLSeconds
	}
	ttl = max(ttl, 1)
	created, err := s.Create(ctx, owner, api.CreateTokenRequest{Name: old.Token.Name, Scopes: old.Token.Scopes, TTLSeconds: ttl}, rkey)
	if err != nil {
		return api.CreatedToken{}, err
	}
	var cutAt time.Time
	err = s.DB.Tx(ctx, func(tx *state.Tx) error {
		r, err := tx.GetToken(owner, id)
		if err != nil || r.RevokedAt != nil {
			return err
		}
		if req.OverlapSeconds == 0 {
			r.RevokedAt = &now
		} else if end := now.Add(time.Duration(req.OverlapSeconds) * time.Second); end.Before(r.Token.ExpiresAt) {
			r.Token.ExpiresAt = end
		}
		cutAt = r.Token.ExpiresAt
		if r.RevokedAt != nil {
			cutAt = now
		}
		return tx.PutToken(r)
	})
	if err == nil && !cutAt.IsZero() {
		if !cutAt.After(now) {
			s.cut(id)
		} else {
			s.cutAt(id, cutAt)
		}
	}
	return created, err
}

// replayed returns the token created with key, if any: its secret while the
// replay window holds it, else invalid_state (the secret is not kept).
func (s *Service) replayed(ctx context.Context, owner domain.PrincipalID, key string) (api.CreatedToken, bool, error) {
	var prev *state.TokenRecord
	err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) { prev, err = tx.TokenByKey(owner, key); return err })
	if errors.Is(err, &domain.Error{Code: domain.CodeNotFound}) {
		return api.CreatedToken{}, false, nil
	}
	if err != nil {
		return api.CreatedToken{}, false, err
	}
	s.mu.Lock()
	r, ok := s.replay[replayKey{owner, key}]
	s.mu.Unlock()
	if ok && s.now().Sub(r.at) <= replayWindow {
		return r.t, true, nil
	}
	return api.CreatedToken{}, true, domain.Errorf(domain.CodeInvalidState,
		"token %s was already issued with this Idempotency-Key; its secret was shown once and is not kept", prev.Token.ID).
		WithDetail("tokenId", prev.Token.ID)
}
