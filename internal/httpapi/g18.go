package httpapi

import (
	"net/http"
	"os"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/tokens"
)

// Identifier is implemented by authenticators that can say how a request
// authenticated (whoami): the method (api.AuthLocal, api.AuthTailnet) and
// a description of the transport identity.
type Identifier interface {
	Identify(r *http.Request) (method, identity string)
}

// g18Routes registers the G18 operator conveniences.
func (s *Server) g18Routes(route func(string, access, handler)) {
	b := api.BasePath
	// Any authenticated caller, any token (every action implies view).
	route("GET "+b+"/me", anyVM(api.TokenView), s.whoami)
	route("POST "+b+"/tokens/{tokenId}/rotate", ownerOnly, s.rotateToken)
}

// whoami describes the caller. It never returns a secret: a token caller
// sees its token's metadata and scopes, never the bearer value.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) error {
	me := api.Me{Principal: string(principal(r)), Version: buildinfo.Version}
	me.Host, _ = os.Hostname()
	if id, ok := s.auth.(Identifier); ok {
		me.Auth, me.Identity = id.Identify(r)
	}
	if g := memberOf(r); g != nil {
		me.Auth = api.AuthMember
		me.Member = &api.MeMember{ID: g.Member.ID, Login: g.Member.Login, Scopes: g.Scopes,
			Snapshots: g.Snapshots, Environments: g.Environments}
		if me.Member.Scopes == nil {
			me.Member.Scopes = []api.TokenScope{}
		}
	} else if g, ok := r.Context().Value(keyGrant).(*tokens.Grant); ok && g != nil {
		me.Auth = api.AuthToken
		if ts, err := s.tokenService(); err == nil {
			if t, err := ts.Get(r.Context(), principal(r), g.TokenID); err == nil {
				me.Token = &t
			}
		}
		if me.Token == nil { // the grant still says what the caller may do
			me.Token = &api.Token{ID: g.TokenID, Scopes: g.Scopes}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, me)
}

func (s *Server) rotateToken(w http.ResponseWriter, r *http.Request) error {
	ts, err := s.tokenService()
	if err != nil {
		return err
	}
	var body api.RotateTokenRequest
	if err := decode(r, &body, true); err != nil {
		return err
	}
	t, err := ts.Rotate(r.Context(), principal(r), r.PathValue("tokenId"), body, r.Header.Get(api.HeaderIdempotencyKey))
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/tokens/"+t.ID)
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusCreated, t)
}
