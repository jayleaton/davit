package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"davit/internal/domain"
	"davit/internal/preflight"
)

func TestEndpointAndCredentialChecks(t *testing.T) {
	for _, tc := range []struct {
		url, token string
		want       preflight.Status
	}{
		{"unix:///run/s.sock", "", preflight.Pass},
		{"ssh://u@h?jump=j", "", preflight.Pass},
		{"ssh://u@h?bogus=1", "", preflight.Fail},
		{"https://host.ts.net", "slw_x", preflight.Pass},
		{"http://127.0.0.1:8080", "slw_x", preflight.Pass},
		{"http://host.ts.net", "", preflight.Warn},
		{"http://host.ts.net", "slw_x", preflight.Fail},
		{"ftp://host", "", preflight.Fail},
	} {
		if c := endpointCheck(tc.url, tc.token); c.Status != tc.want {
			t.Errorf("endpoint %s token=%q: %s %q", tc.url, tc.token, c.Status, c.Detail)
		}
	}
	if c := credentialCheck("slw_secretsecret"); c.Status != preflight.Pass || strings.Contains(c.Detail, "secret") && strings.Contains(c.Detail, "slw_") {
		t.Errorf("token credential: %+v", c)
	}
	if c := credentialCheck("ghp_nope"); c.Status != preflight.Fail {
		t.Errorf("foreign token: %+v", c)
	}
	if c := credentialCheck(""); c.Status != preflight.Pass || !strings.Contains(c.Detail, "tailnet identity") {
		t.Errorf("no token: %+v", c)
	}
}

func TestCAFileCheck(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(CAFileEnv, "")
	if _, ok := caFileCheck(); ok {
		t.Fatal("check without a CA file")
	}
	bad := filepath.Join(dir, "bad.pem")
	os.WriteFile(bad, []byte("not pem"), 0o600)
	t.Setenv(CAFileEnv, bad)
	if c, _ := caFileCheck(); c.Status != preflight.Fail {
		t.Fatalf("bad CA: %+v", c)
	}
	t.Setenv(CAFileEnv, filepath.Join(dir, "missing.pem"))
	if c, _ := caFileCheck(); c.Status != preflight.Fail {
		t.Fatalf("missing CA: %+v", c)
	}
}

// TestRemoteFailureNamesTheFix: each kind of remote failure maps to the fix
// that applies.
func TestRemoteFailureNamesTheFix(t *testing.T) {
	api := func(code domain.Code) error { return &cliError{ErrorBody{Code: code, Message: "denied"}} }
	for _, tc := range []struct {
		err           error
		token, remote bool
		hint          string
	}{
		{api(domain.CodeUnauthenticated), true, true, "token create"},
		{api(domain.CodeUnauthenticated), false, true, "over the tailnet"},
		{api(domain.CodePermissionDenied), true, true, "--action"},
		{api(domain.CodePermissionDenied), false, true, "ownerLogins"},
		{errors.New("dial unix /run/s.sock: connect: no such file or directory"), false, false, "start the daemon"},
		{errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), false, true, CAFileEnv},
		{errors.New("x509: certificate is valid for a.ts.net, not b.ts.net"), false, true, "name the certificate covers"},
		{errors.New("dial tcp: lookup host.ts.net: no such host"), false, true, "MagicDNS"},
		{errors.New("dial tcp 100.64.0.1:443: connect: connection refused"), false, true, "doctor"},
		{errors.New("dial tcp 100.64.0.1:443: i/o timeout"), false, true, "tailscale status"},
	} {
		if _, hint := remoteFailure(tc.err, tc.token, tc.remote); !strings.Contains(hint, tc.hint) {
			t.Errorf("%v (token=%v): hint %q, want %q", tc.err, tc.token, hint, tc.hint)
		}
	}
}
