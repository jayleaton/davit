package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/tokens"
)

// grantAuth authenticates the owner with the grant named by the
// X-Test-Grant header (none: the owner identity itself).
type grantAuth map[string]*tokens.Grant

func (grantAuth) Authenticate(*http.Request) (domain.PrincipalID, error) { return "owner", nil }

func (a grantAuth) AuthenticateGrant(r *http.Request) (domain.PrincipalID, *tokens.Grant, error) {
	name := r.Header.Get("X-Test-Grant")
	if name == "" {
		return "owner", nil, nil
	}
	return "owner", a[name], nil
}

func scope(vm domain.VMID, action string) []api.TokenScope {
	return []api.TokenScope{{VMID: vm, Actions: []string{action}}}
}

// securityServer runs the API with one VM ("box") and one environment
// ("web"), and the grants a test names by header.
func securityServer(t *testing.T) (http.Handler, domain.VMID, domain.EnvironmentID, grantAuth) {
	t.Helper()
	auth := grantAuth{}
	h := newServer(t, auth)
	rec, _ := call(t, h, "POST", "/api/v1/vms", `{"name":"box"}`, map[string]string{api.HeaderIdempotencyKey: "k-box"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create vm: %d %s", rec.Code, rec.Body)
	}
	var acc struct {
		VM api.VM `json:"vm"`
	}
	json.Unmarshal(rec.Body.Bytes(), &acc)
	rec, _ = call(t, h, "POST", "/api/v1/environments", `{"name":"web","manifest":{}}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create environment: %d %s", rec.Code, rec.Body)
	}
	var env api.Environment
	json.Unmarshal(rec.Body.Bytes(), &env)
	vm := acc.VM.ID
	if vm == "" {
		rec, _ = call(t, h, "GET", "/api/v1/vms/box", "", nil)
		var v api.VM
		json.Unmarshal(rec.Body.Bytes(), &v)
		vm = v.ID
	}
	return h, vm, env.ID, auth
}

// TestDaemonSecretsNeedOwnerOrAdmin: a prompt's daemon secrets are the
// owner's raw keys put into the guest; a member, whatever its role, and a
// token without admin on the VM must not request them (finding SEC-1).
func TestDaemonSecretsNeedOwnerOrAdmin(t *testing.T) {
	h, vm, _, auth := securityServer(t)
	member := &tokens.Member{ID: "mem_1", Login: "bob@example.com", UserID: 2}
	auth["member-admin"] = &tokens.Grant{TokenID: "member:mem_1", Scopes: scope(vm, api.TokenAdmin), Member: member}
	auth["token-exec"] = &tokens.Grant{TokenID: "tok_exec", Scopes: scope(vm, api.TokenExec)}
	auth["token-admin"] = &tokens.Grant{TokenID: "tok_admin", Scopes: scope(vm, api.TokenAdmin)}
	body := `{"text":"print the key","secrets":["ANTHROPIC_API_KEY"]}`
	for _, who := range []string{"member-admin", "token-exec"} {
		rec, er := call(t, h, "POST", "/api/v1/vms/box/prompts", body, map[string]string{"X-Test-Grant": who, api.HeaderIdempotencyKey: "k-" + who})
		if rec.Code != http.StatusForbidden || er.Error.Code != domain.CodePermissionDenied {
			t.Errorf("%s requesting daemon secrets: %d %+v, want 403", who, rec.Code, er.Error)
		}
	}
	// The owner and an admin token pass the check (this server has no
	// prompt service, so they then get unsupported_capability).
	for _, who := range []string{"", "token-admin"} {
		if rec, er := call(t, h, "POST", "/api/v1/vms/box/prompts", body, map[string]string{"X-Test-Grant": who, api.HeaderIdempotencyKey: "k2-" + who}); rec.Code == http.StatusForbidden {
			t.Errorf("%q refused daemon secrets: %+v", who, er.Error)
		}
	}
	// Without secrets an exec member may prompt (refused only for lack of
	// a prompt service here).
	auth["member-exec"] = &tokens.Grant{TokenID: "member:mem_1", Scopes: scope(vm, api.TokenExec), Member: member}
	if rec, er := call(t, h, "POST", "/api/v1/vms/box/prompts", `{"text":"hi"}`, map[string]string{"X-Test-Grant": "member-exec", api.HeaderIdempotencyKey: "k3"}); rec.Code == http.StatusForbidden {
		t.Errorf("exec member prompt without secrets: %+v", er.Error)
	}
}

// TestEnvironmentUseNeedsEnvironmentRights: resuming or forking with an
// environment writes its secret files into the guest; a member needs an
// exec share on that environment and a token an all-VM admin scope, not
// just admin on the VM (finding SEC-2).
func TestEnvironmentUseNeedsEnvironmentRights(t *testing.T) {
	h, vm, env, auth := securityServer(t)
	member := &tokens.Member{ID: "mem_1", Login: "bob@example.com", UserID: 2}
	auth["member-vm-admin"] = &tokens.Grant{TokenID: "member:mem_1", Scopes: scope(vm, api.TokenAdmin), Member: member}
	auth["member-env-exec"] = &tokens.Grant{TokenID: "member:mem_1", Scopes: scope(vm, api.TokenAdmin), Member: member,
		Environments: map[string]string{string(env): api.TokenExec}}
	auth["token-vm-admin"] = &tokens.Grant{TokenID: "tok_vm", Scopes: scope(vm, api.TokenAdmin)}
	auth["token-all-admin"] = &tokens.Grant{TokenID: "tok_all", Scopes: scope("", api.TokenAdmin)}
	body := `{"environment":{"environment":"web"}}`
	for _, c := range []struct {
		who     string
		allowed bool
	}{{"member-vm-admin", false}, {"token-vm-admin", false}, {"member-env-exec", true}, {"token-all-admin", true}, {"", true}} {
		rec, er := call(t, h, "POST", "/api/v1/vms/box/resume", body, map[string]string{"X-Test-Grant": c.who, api.HeaderIdempotencyKey: "k-r-" + c.who})
		if denied := rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound; denied == c.allowed {
			t.Errorf("resume with environment as %q: %d %+v (allowed %v)", c.who, rec.Code, er.Error, c.allowed)
		}
	}
	// Fork: members never fork; a VM-scoped admin token may fork, but not
	// with an environment.
	if rec, er := call(t, h, "POST", "/api/v1/vms/box/fork", body, map[string]string{"X-Test-Grant": "token-vm-admin", api.HeaderIdempotencyKey: "k-f"}); rec.Code != http.StatusForbidden {
		t.Errorf("fork with environment as a VM-scoped token: %d %+v", rec.Code, er.Error)
	}
}

// TestViewCannotReadContent: view (viewer members, readonly tokens) sees
// state and metadata, not what processes printed or what the coding agent
// said and read; that is exec, like attachProcess and readFile (SEC-3).
func TestViewCannotReadContent(t *testing.T) {
	h, vm, _, auth := securityServer(t)
	auth["token-view"] = &tokens.Grant{TokenID: "tok_view", Scopes: scope(vm, api.TokenView)}
	auth["member-viewer"] = &tokens.Grant{TokenID: "member:mem_1", Scopes: scope(vm, api.TokenView),
		Member: &tokens.Member{ID: "mem_1", Login: "bob@example.com", UserID: 2}}
	for _, who := range []string{"token-view", "member-viewer"} {
		for _, path := range []string{
			"/api/v1/vms/box/processes/proc_1/output",
			"/api/v1/vms/box/prompts/prm_1/events",
			"/api/v1/vms/box/prompt-events",
		} {
			if rec, er := call(t, h, "GET", path, "", map[string]string{"X-Test-Grant": who}); rec.Code != http.StatusForbidden {
				t.Errorf("%s GET %s: %d %+v, want 403", who, path, rec.Code, er.Error)
			}
		}
		// Metadata stays readable.
		if rec, er := call(t, h, "GET", "/api/v1/vms/box", "", map[string]string{"X-Test-Grant": who}); rec.Code != http.StatusOK {
			t.Errorf("%s GET vm: %d %+v", who, rec.Code, er.Error)
		}
	}
}
