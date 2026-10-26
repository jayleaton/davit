package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/tokens"
)

// identAuth is scopeAuth that also identifies its transport.
type identAuth struct{ scopeAuth }

func (identAuth) Identify(*http.Request) (string, string) {
	return api.AuthTailnet, "alice via laptop (n1)"
}

// TestWhoami: the owner and any token see who they are; a token caller
// sees its scopes and never a secret.
func TestWhoami(t *testing.T) {
	h := newServer(t, identAuth{})
	get := func(action string) api.Me {
		t.Helper()
		hdr := map[string]string{}
		if action != "" {
			hdr["X-Test-Action"] = action
		}
		rec, _ := call(t, h, "GET", "/api/v1/me", "", hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("whoami (%q): HTTP %d %s", action, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "slw_") {
			t.Fatalf("whoami returned a secret: %s", rec.Body)
		}
		var me api.Me
		json.Unmarshal(rec.Body.Bytes(), &me)
		return me
	}
	if me := get(""); me.Principal != "owner" || me.Auth != api.AuthTailnet || me.Identity != "alice via laptop (n1)" || me.Token != nil || me.Version == "" {
		t.Errorf("owner: %+v", me)
	}
	for _, a := range []string{api.TokenView, api.TokenExec, api.TokenAdmin} {
		me := get(a)
		if me.Auth != api.AuthToken || me.Token == nil || me.Token.ID != "tok_test" || len(me.Token.Scopes) != 1 || me.Token.Scopes[0].Actions[0] != a {
			t.Errorf("token %s: %+v", a, me)
		}
	}
	// A member sees its shares, not a token.
	mh := newServer(t, memberAuth{})
	rec, _ := call(t, mh, "GET", "/api/v1/me", "", nil)
	var me api.Me
	json.Unmarshal(rec.Body.Bytes(), &me)
	if rec.Code != http.StatusOK || me.Auth != api.AuthMember || me.Token != nil || me.Member == nil || me.Member.Login != "bob@example.com" ||
		len(me.Member.Scopes) != 1 || me.Member.Scopes[0].VMID != "vm_shared" || me.Member.Snapshots["snap_1"] != api.TokenView {
		t.Errorf("member: %d %+v", rec.Code, me.Member)
	}

	// Rotation is owner-only, like the rest of token management.
	if rec, _ := call(t, h, "POST", "/api/v1/tokens/tok_test/rotate", "{}", map[string]string{"X-Test-Action": api.TokenAdmin}); rec.Code != http.StatusForbidden {
		t.Errorf("token rotating a token: HTTP %d", rec.Code)
	}
}

// memberAuth authenticates a tailnet member with one shared VM and snapshot.
type memberAuth struct{}

func (memberAuth) Authenticate(*http.Request) (domain.PrincipalID, error) { return "owner", nil }

func (memberAuth) AuthenticateGrant(*http.Request) (domain.PrincipalID, *tokens.Grant, error) {
	return "owner", &tokens.Grant{
		Scopes:    []api.TokenScope{{VMID: "vm_shared", Actions: []string{api.TokenExec}}},
		Member:    &tokens.Member{ID: "mem_1", Login: "bob@example.com"},
		Snapshots: map[string]string{"snap_1": api.TokenView},
	}, nil
}
