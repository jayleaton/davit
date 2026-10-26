package lifecycle

import (
	"context"
	"encoding/json"
	"reflect"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

// Environments (G3): named series of immutable manifest revisions, owned by
// a principal. VMs pin a revision (workspace.go).

// ListEnvironments returns one page of owner's environments.
func (s *Service) ListEnvironments(ctx context.Context, owner domain.PrincipalID, p Page) (out []domain.Environment, next string, err error) {
	out = []domain.Environment{}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		es, n, err := tx.ListEnvironments(owner, p.state(string(owner), "environments"))
		for _, e := range es {
			out = append(out, *e)
		}
		next = n
		return err
	})
	return out, next, err
}

// CreateEnvironment creates an environment with revision 1. A retry with the
// same name and manifest returns the existing environment (created false);
// the same name with another manifest is refused.
func (s *Service) CreateEnvironment(ctx context.Context, owner domain.PrincipalID, body api.CreateEnvironmentRequest) (env *domain.Environment, created bool, err error) {
	if !domain.ValidName(body.Name) {
		return nil, false, domain.Errorf(domain.CodeInvalidArgument, "invalid environment name %q: use 1-63 lowercase letters, digits and '-'", body.Name).WithDetail("field", "name")
	}
	if err := checkManifest(body.Manifest); err != nil {
		return nil, false, err
	}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		if prev, err := tx.GetEnvironment(owner, body.Name); err == nil {
			first, ferr := tx.GetEnvironmentRevision(prev.ID, 1)
			if ferr == nil && prev.LatestRevision == 1 && sameManifest(first.Manifest, body.Manifest) {
				env = prev
				return nil
			}
			return domain.Errorf(domain.CodeInvalidArgument, "environment name %q is taken", body.Name).WithDetail("field", "name")
		} else if !domain.IsCode(err, domain.CodeNotFound) {
			return err
		}
		now := s.now()
		env = &domain.Environment{ID: domain.EnvironmentID(domain.NewID("env")), Owner: owner, Name: body.Name, CreatedAt: now, UpdatedAt: now}
		if err := tx.PutEnvironment(env); err != nil {
			return err
		}
		created = true
		_, err := tx.AddEnvironmentRevision(env, body.Manifest, now, "", "")
		return err
	})
	return env, created, err
}

// GetEnvironment returns owner's environment by ID or name.
func (s *Service) GetEnvironment(ctx context.Context, owner domain.PrincipalID, ref string) (env *domain.Environment, err error) {
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		env, err = tx.GetEnvironment(owner, ref)
		return err
	})
	return env, err
}

// DeleteEnvironment deletes an environment and its revisions. Pinned VMs
// keep their copy of the manifest.
func (s *Service) DeleteEnvironment(ctx context.Context, owner domain.PrincipalID, ref string) error {
	return s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		env, err := tx.GetEnvironment(owner, ref)
		if err != nil {
			return err
		}
		return tx.DeleteEnvironment(env.ID)
	})
}

// ListEnvironmentRevisions returns one page of revisions, oldest first.
func (s *Service) ListEnvironmentRevisions(ctx context.Context, owner domain.PrincipalID, ref string, p Page) (out []domain.EnvironmentRevision, next string, err error) {
	out = []domain.EnvironmentRevision{}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		env, err := tx.GetEnvironment(owner, ref)
		if err != nil {
			return err
		}
		rs, n, err := tx.ListEnvironmentRevisions(env.ID, p.state(string(env.ID), "revisions"))
		for _, r := range rs {
			out = append(out, *r)
		}
		next = n
		return err
	})
	return out, next, err
}

// CreateEnvironmentRevision mints the next revision. ifMatch, when set,
// must equal the latest revision number. A retry with the same
// Idempotency-Key replays the revision it minted.
func (s *Service) CreateEnvironmentRevision(ctx context.Context, owner domain.PrincipalID, ref, key string, ifMatch *int64, m domain.EnvManifest) (rev *domain.EnvironmentRevision, created bool, err error) {
	if err := checkManifest(m); err != nil {
		return nil, false, err
	}
	if len(key) > maxIdempotencyKey {
		return nil, false, domain.Errorf(domain.CodeInvalidArgument, "Idempotency-Key is longer than %d bytes", maxIdempotencyKey).WithDetail("field", api.HeaderIdempotencyKey)
	}
	body, _ := json.Marshal(m)
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		env, err := tx.GetEnvironment(owner, ref)
		if err != nil {
			return err
		}
		// Scoped by the canonical ID, so ID and name address one request.
		hash := hashOf("createEnvironmentRevision", string(env.ID), string(body))
		if key != "" {
			prev, phash, err := tx.EnvironmentRevisionByKey(env.ID, key)
			if err != nil {
				return err
			}
			if prev != nil {
				if phash != hash {
					return domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key was used with a different request")
				}
				rev = prev // a retry replays, even after later revisions
				return nil
			}
		}
		if ifMatch != nil && *ifMatch != env.LatestRevision {
			return domain.Errorf(domain.CodeRevisionMismatch, "latest revision is %d, not %d", env.LatestRevision, *ifMatch).
				WithDetail("latestRevision", env.LatestRevision)
		}
		created = true
		rev, err = tx.AddEnvironmentRevision(env, m, s.now(), key, hash)
		return err
	})
	return rev, created, err
}

// GetEnvironmentRevision returns one revision of owner's environment.
func (s *Service) GetEnvironmentRevision(ctx context.Context, owner domain.PrincipalID, ref string, revision int64) (rev *domain.EnvironmentRevision, err error) {
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		env, err := tx.GetEnvironment(owner, ref)
		if err != nil {
			return err
		}
		rev, err = tx.GetEnvironmentRevision(env.ID, revision)
		return err
	})
	return rev, err
}

// checkManifest validates a manifest for this build: credential profiles
// (G4) are not served yet, so a manifest naming one is refused rather than
// silently prepared without credentials.
func checkManifest(m domain.EnvManifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.CredentialProfile != "" {
		return domain.NotImplemented(domain.FeatureCredentialProxy, "environment.manifest.credentialProfile")
	}
	return nil
}

// sameManifest compares manifests as JSON values (nil and empty
// collections are the same manifest).
func sameManifest(a, b domain.EnvManifest) bool {
	norm := func(m domain.EnvManifest) any {
		raw, _ := json.Marshal(m)
		var v any
		_ = json.Unmarshal(raw, &v)
		if mm, ok := v.(map[string]any); ok {
			for k, x := range mm {
				switch y := x.(type) {
				case nil:
					delete(mm, k)
				case []any:
					if len(y) == 0 {
						delete(mm, k)
					}
				case map[string]any:
					if len(y) == 0 {
						delete(mm, k)
					}
				}
			}
		}
		return v
	}
	return reflect.DeepEqual(norm(a), norm(b))
}
