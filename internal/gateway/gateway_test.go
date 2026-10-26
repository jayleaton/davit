package gateway

import (
	"context"
	"davit/api"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"davit/internal/domain"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

type fakeWhoIs map[string]*apitype.WhoIsResponse

func (f fakeWhoIs) WhoIs(_ context.Context, addr string) (*apitype.WhoIsResponse, error) {
	if r, ok := f[addr]; ok {
		return r, nil
	}
	return nil, errors.New("no match for IP:port")
}

func TestIdentifyFailsClosed(t *testing.T) {
	user := &tailcfg.UserProfile{ID: 7, LoginName: "alice@example.com"}
	w := fakeWhoIs{
		"100.64.0.2:1": {Node: &tailcfg.Node{StableID: "n1", Name: "laptop.ts.net."}, UserProfile: user},
		"100.64.0.3:1": {Node: &tailcfg.Node{StableID: "n2"}},      // no user
		"100.64.0.4:1": {Node: &tailcfg.Node{}, UserProfile: user}, // no node ID
		"100.64.0.5:1": {Node: &tailcfg.Node{StableID: "n3", Tags: []string{"tag:ci"}}, UserProfile: &tailcfg.UserProfile{}},
	}
	id, err := Identify(context.Background(), w, "100.64.0.2:1")
	if err != nil || id.UserID != 7 || id.NodeName != "laptop.ts.net" || id.Tagged() {
		t.Fatalf("got %+v, %v", id, err)
	}
	if id, err := Identify(context.Background(), w, "100.64.0.5:1"); err != nil || !id.Tagged() {
		t.Fatalf("tagged node: %+v, %v", id, err)
	}
	for _, addr := range []string{"", "100.64.0.3:1", "100.64.0.4:1", "192.168.1.9:1"} {
		if _, err := Identify(context.Background(), w, addr); !errors.Is(err, ErrUnidentified) {
			t.Errorf("%q: err=%v, want ErrUnidentified", addr, err)
		}
	}

	called := false
	h := RequireIdentity(w, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest("GET", "/api/v1/vms", nil)
	req.RemoteAddr = "192.168.1.9:1"
	req.Header.Set("Tailscale-User-Login", "alice@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if called || rec.Code != http.StatusUnauthorized {
		t.Fatalf("unidentified request reached handler=%v code=%d", called, rec.Code)
	}
	var er api.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil || er.Error.Code != domain.CodeUnauthenticated ||
		er.Error.RequestID == "" || rec.Header().Get(api.HeaderRequestID) != er.Error.RequestID {
		t.Fatalf("401 must be an ErrorResponse with a request ID: %s (%v)", rec.Body.String(), err)
	}
}

func TestParseSSHUser(t *testing.T) {
	for in, want := range map[string][2]string{
		"devbox":         {"devbox", DefaultGuestUser},
		"devbox+root":    {"devbox", "root"},
		"vm_01H.x+dev-1": {"vm_01H.x", "dev-1"},
	} {
		vm, u, err := ParseSSHUser(in)
		if err != nil || vm != want[0] || u != want[1] {
			t.Errorf("%q: got %q %q %v", in, vm, u, err)
		}
	}
	for _, in := range []string{"", "+root", "devbox+", "devbox+Root", "../etc", "a b", "devbox;id", "devbox+root+x"} {
		if _, _, err := ParseSSHUser(in); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("%q: err=%v, want ErrInvalidTarget", in, err)
		}
	}
}

func TestAuthKeyFileMustBePrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("tskey-auth-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Config{AuthKeyFile: p}).authKey(); err == nil {
		t.Fatal("accepted a world-readable auth key file")
	}
	os.Chmod(p, 0o600)
	if k, err := (Config{AuthKeyFile: p}).authKey(); err != nil || k != "tskey-auth-x" {
		t.Fatalf("got %q, %v", k, err)
	}
	t.Setenv("DAVIT_TEST_KEY", " tskey-env ")
	if k, _ := (Config{AuthKeyEnv: "DAVIT_TEST_KEY"}).authKey(); k != "tskey-env" {
		t.Fatalf("env key %q", k)
	}
}

func TestSpliceByteLimit(t *testing.T) {
	a, client := net.Pipe()
	b, guest := net.Pipe()
	errc := make(chan error, 1)
	go func() { errc <- Splice(context.Background(), a, b, Limits{MaxBytes: 4}) }()
	go io.Copy(io.Discard, guest)
	client.Write([]byte("0123456789"))
	if err := <-errc; !errors.Is(err, ErrLimit) {
		t.Fatalf("err=%v, want ErrLimit", err)
	}
	client.Close()
	guest.Close()
}

type fakeVMs map[string]*domain.VM

func (f fakeVMs) GetVM(_ context.Context, p domain.PrincipalID, ref string) (*domain.VM, error) {
	if vm, ok := f[ref]; ok && vm.Owner == p {
		return vm, nil
	}
	return nil, domain.Errorf(domain.CodeNotFound, "vm %q not found", ref)
}

func TestOwnerAuthorizer(t *testing.T) {
	owner := domain.PrincipalID("local:1000")
	policy := OwnerPolicy{Logins: []string{"Alice@Example.com"}, Tags: []string{"tag:ci"}}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []OwnerPolicy{{}, {Tags: []string{"ci"}}, {Logins: []string{"a b"}}} {
		if bad.Validate() == nil {
			t.Errorf("accepted policy %+v", bad)
		}
	}
	a := OwnerAuthorizer{Owners: policy, Principal: owner, VMs: fakeVMs{
		"devbox":  {ID: "vm_a", Owner: owner, State: domain.StateRunning, InstanceGeneration: 4},
		"stopped": {ID: "vm_b", Owner: owner, State: domain.StateStopped},
		"gone":    {ID: "vm_c", Owner: owner, State: domain.StateDeleted},
	}}
	alice := Identity{UserID: 1, LoginName: "alice@example.com", NodeID: "n1"}
	ci := Identity{UserID: 9, LoginName: "tagged-devices", NodeID: "n2", Tags: []string{"tag:ci"}}
	// A tagged node never matches by login, even if the tagger's login is listed.
	taggedAlice := Identity{UserID: 1, LoginName: "alice@example.com", NodeID: "n3", Tags: []string{"tag:web"}}
	bob := Identity{UserID: 2, LoginName: "bob@example.com", NodeID: "n4"}

	for _, tc := range []struct {
		who  Identity
		ref  string
		want error
	}{
		{alice, "devbox", nil},
		{ci, "devbox", nil},
		{alice, "stopped", ErrNotRunning},
		{alice, "gone", ErrNotFound},
		{alice, "absent", ErrNotFound},
		{bob, "devbox", ErrDenied},
		{taggedAlice, "devbox", ErrDenied},
	} {
		got, err := a.Authorize(context.Background(), tc.who, Request{VMRef: tc.ref, Action: ActionSSH, Port: 22})
		if !errors.Is(err, tc.want) || (tc.want == nil && got != (Target{VMID: "vm_a", Generation: 4})) {
			t.Errorf("%s -> %s: got %+v, %v; want %v", tc.who.LoginName, tc.ref, got, err, tc.want)
		}
	}

	req := httptest.NewRequest("GET", "/api/v1/vms", nil)
	auth := Authenticator{Owners: policy, Principal: owner}
	if _, err := auth.Authenticate(req); !errors.Is(err, &domain.Error{Code: domain.CodeUnauthenticated}) {
		t.Errorf("no identity: %v", err)
	}
	if p, err := auth.Authenticate(req.WithContext(WithIdentity(req.Context(), alice))); err != nil || p != owner {
		t.Errorf("owner: %q, %v", p, err)
	}
	if _, err := auth.Authenticate(req.WithContext(WithIdentity(req.Context(), bob))); !errors.Is(err, &domain.Error{Code: domain.CodePermissionDenied}) {
		t.Errorf("non-owner: %v", err)
	}
}
