package httpapi

import (
	"net/http"
	"strconv"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/tokens"
)

// GrantAuthenticator is an Authenticator that may also return a service
// token's grant, limiting what the request may do. A nil grant means the
// principal acts with its full rights (owner identity).
type GrantAuthenticator interface {
	Authenticator
	AuthenticateGrant(r *http.Request) (domain.PrincipalID, *tokens.Grant, error)
}

// access is what a route needs from a token grant: an action and the VM the
// request is about.
type access struct {
	action string
	// vm finds the VM the request is about; nil means none (a list or a
	// create across VMs, which only an all-VM scope allows).
	vm func(s *Server, r *http.Request, p domain.PrincipalID) (domain.VMID, error)
	// any: VM-independent reads (capabilities, images) any scope with the
	// action allows.
	any bool
	// owner: token management needs the owner identity itself; no token,
	// whatever its scopes, may create, list or revoke tokens.
	owner bool
	// Member handling (members.go): list routes filter for members,
	// noMembers refuses them, snapshot and env resolve shares.
	list, noMembers, snapshot, env bool
}

var ownerOnly = access{owner: true}

func anyVM(action string) access  { return access{action: action, any: true} }
func allVMs(action string) access { return access{action: action} }

func onVM(action string) access {
	return access{action: action, vm: func(s *Server, r *http.Request, p domain.PrincipalID) (domain.VMID, error) {
		vm, err := s.svc.GetVM(r.Context(), p, r.PathValue("vmId"))
		if err != nil {
			return "", err
		}
		return vm.ID, nil
	}}
}

func onOperation(action string) access {
	return access{action: action, vm: func(s *Server, r *http.Request, p domain.PrincipalID) (domain.VMID, error) {
		op, err := s.svc.GetOperation(r.Context(), p, domain.OperationID(r.PathValue("operationId")))
		if err != nil {
			return "", err
		}
		return op.VMID, nil
	}}
}

func onSnapshot(action string) access {
	return access{action: action, snapshot: true, vm: func(s *Server, r *http.Request, p domain.PrincipalID) (domain.VMID, error) {
		snap, err := s.svc.GetSnapshot(r.Context(), p, r.PathValue("snapshotId"))
		if err != nil {
			return "", err
		}
		return snap.SourceVMID, nil
	}}
}

func onTunnel(action string) access {
	return access{action: action, vm: func(s *Server, r *http.Request, p domain.PrincipalID) (domain.VMID, error) {
		m, err := s.processes()
		if err != nil {
			return "", err
		}
		return m.TunnelVM(p, r.PathValue("tunnelId"))
	}}
}

// checkGrant enforces a token or member grant before the handler runs and
// returns the VM the request is about ("" if none).
func (s *Server) checkGrant(r *http.Request, p domain.PrincipalID, g *tokens.Grant, need access) (domain.VMID, error) {
	if g.Member != nil {
		return s.checkMember(r, p, g, need)
	}
	vm, err := s.checkToken(r, p, g, need)
	return vm, err
}

func (s *Server) checkToken(r *http.Request, p domain.PrincipalID, g *tokens.Grant, need access) (domain.VMID, error) {
	denied := func() error {
		return domain.Errorf(domain.CodePermissionDenied, "token %s does not grant %s here", g.TokenID, need.action).
			WithDetail("tokenId", g.TokenID)
	}
	switch {
	case need.owner:
		return "", domain.Errorf(domain.CodePermissionDenied, "tokens cannot manage tokens; use the owner identity").
			WithDetail("tokenId", g.TokenID)
	case need.any:
		if !g.AllowsAny(need.action) {
			return "", denied()
		}
		return "", nil
	case need.vm == nil:
		if !g.Allows(need.action, "") {
			return "", denied()
		}
		return "", nil
	}
	vm, err := need.vm(s, r, p)
	if err != nil {
		return "", err
	}
	if !g.Allows(need.action, vm) {
		return "", denied()
	}
	return vm, nil
}

// SetTokens enables the /tokens endpoints. Without it they answer 422
// unsupported_capability.
func (s *Server) SetTokens(t *tokens.Service) { s.tokens = t }

func (s *Server) tokenService() (*tokens.Service, error) {
	if s.tokens == nil {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "service tokens are not enabled on this daemon")
	}
	return s.tokens, nil
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) error {
	ts, err := s.tokenService()
	if err != nil {
		return err
	}
	var body api.CreateTokenRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	t, err := ts.Create(r.Context(), principal(r), body, r.Header.Get(api.HeaderIdempotencyKey))
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/tokens/"+t.ID)
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusCreated, t)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) error {
	ts, err := s.tokenService()
	if err != nil {
		return err
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 200 {
			return domain.Errorf(domain.CodeInvalidArgument, "limit must be 1..200").WithDetail("field", "limit")
		}
	}
	l, err := ts.List(r.Context(), principal(r), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) error {
	ts, err := s.tokenService()
	if err != nil {
		return err
	}
	if err := ts.Revoke(r.Context(), principal(r), r.PathValue("tokenId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
