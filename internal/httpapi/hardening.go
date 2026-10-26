package httpapi

import (
	"net/http"

	"davit/api"
	"davit/internal/domain"
)

// Body-dependent access checks (docs/security-review.md). The route table
// decides access from the route alone; these requests can additionally
// carry owner secrets into the guest, so the body raises what they need.

// mayUseDaemonSecrets: a prompt naming daemon secrets puts the owner's raw
// keys into the guest's environment (credential grants, in contrast, give
// placeholders). Only the owner, or a token with admin on the VM, may ask;
// a member never, whatever its role (SEC-1).
func (s *Server) mayUseDaemonSecrets(r *http.Request, vmRef string, names []string) error {
	g := grantOf(r)
	if g == nil || len(names) == 0 {
		return nil
	}
	if g.Member != nil {
		return domain.Errorf(domain.CodePermissionDenied, "members cannot pass the owner's daemon secrets to a prompt").
			WithDetail("field", "secrets")
	}
	vm, err := s.svc.GetVM(r.Context(), principal(r), vmRef)
	if err != nil {
		return err
	}
	if !g.Allows(api.TokenAdmin, vm.ID) {
		return domain.Errorf(domain.CodePermissionDenied, "token %s needs admin on vm %s to pass daemon secrets to a prompt", g.TokenID, vm.ID).
			WithDetail("tokenId", g.TokenID).WithDetail("field", "secrets")
	}
	return nil
}

// mayUseEnvironment: preparing a workspace from an environment writes its
// secret files into the guest. The owner may use any environment; a member
// needs an exec share on that environment; a token, which cannot even read
// environments, needs an all-VM admin scope (the owner's delegate), not
// just admin on the VM it resumes or forks (SEC-2).
func (s *Server) mayUseEnvironment(r *http.Request, ref *api.EnvironmentRef) error {
	g := grantOf(r)
	if g == nil || ref == nil {
		return nil
	}
	if g.Member != nil {
		env, err := s.svc.GetEnvironment(r.Context(), principal(r), ref.Environment)
		if err != nil || !g.AllowsEnvironment(api.TokenView, string(env.ID)) {
			return domain.Errorf(domain.CodeNotFound, "environment %q not found", ref.Environment)
		}
		if !g.AllowsEnvironment(api.TokenExec, string(env.ID)) {
			return domain.Errorf(domain.CodePermissionDenied, "member %s needs an exec share on environment %s to prepare a VM from it",
				g.Member.Login, env.ID).WithDetail("field", "environment")
		}
		return nil
	}
	if !g.Allows(api.TokenAdmin, "") {
		return domain.Errorf(domain.CodePermissionDenied, "token %s needs an all-VM admin scope to prepare a VM from an environment", g.TokenID).
			WithDetail("tokenId", g.TokenID).WithDetail("field", "environment")
	}
	return nil
}
