package credproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

const realKey = "sk-ant-REAL-0123456789"

func newHost(t *testing.T) (*Host, FileSecrets, *state.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC()
	if err := db.Tx(context.Background(), func(tx *state.Tx) error {
		for _, id := range []domain.VMID{"vm_a", "vm_child"} {
			if err := tx.PutVM(&state.VMRecord{VM: domain.VM{ID: id, Owner: "owner", Name: string(id),
				State: domain.StateStopped, StateRevision: 1, InstanceGeneration: 1, CreatedAt: now, UpdatedAt: now}, DiskID: string(id) + "-g1"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	secrets := FileSecrets{Dir: filepath.Join(dir, "credentials")}
	// Test upstreams are httptest servers on loopback.
	return &Host{DB: db, Secrets: secrets, AllowPrivateUpstreams: true}, secrets, db
}

// The real proxy passes the same contract as the fake.
func TestHostGrantContract(t *testing.T) {
	h, secrets, _ := newHost(t)
	grantContract(t, h, secrets)
}

func TestFileSecrets(t *testing.T) {
	ctx := context.Background()
	s := FileSecrets{Dir: filepath.Join(t.TempDir(), "credentials")}
	if _, err := s.Secret(ctx, "cp_1", "x"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("missing secret: %v", err)
	}
	for _, v := range []string{"one", "two"} {
		if err := s.PutSecret(ctx, "cp_1", "../../etc/passwd", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := s.Secret(ctx, "cp_1", "../../etc/passwd"); err != nil || string(b) != "two" {
		t.Fatalf("secret = %q, %v", b, err)
	}
	var files int
	_ = filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		want := fs.FileMode(0o600)
		if d.IsDir() {
			want = fs.ModeDir | 0o700
		} else {
			files++
		}
		if fi.Mode() != want {
			t.Errorf("%s mode %v, want %v", p, fi.Mode(), want)
		}
		if !strings.HasPrefix(p, s.Dir) {
			t.Errorf("%s escaped the secrets dir", p)
		}
		return nil
	})
	if files != 1 {
		t.Errorf("%d secret files, want 1 (no temp files left)", files)
	}
	if err := s.DeleteSecret(ctx, "cp_1", "../../etc/passwd"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Secret(ctx, "cp_1", "../../etc/passwd"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("deleted secret: %v", err)
	}
	if err := s.PutSecret(ctx, "cp_1", "x", nil); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("empty secret: %v", err)
	}
}

// upstream is a fake model API: it records every request and streams SSE
// on /v1/stream, sending the second event only once release is closed.
type upstream struct {
	*httptest.Server
	mu       sync.Mutex
	reqs     []*http.Request
	release  chan struct{}
	canceled chan struct{}
}

func newUpstream(t *testing.T) *upstream {
	u := &upstream{release: make(chan struct{}), canceled: make(chan struct{}, 1)}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.reqs = append(u.reqs, r.Clone(context.Background()))
		u.mu.Unlock()
		// An upstream that echoes the key back must not leak it.
		w.Header().Set("X-Echo-Key", r.Header.Get("X-Api-Key"))
		if r.URL.Path != "/base/v1/stream" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"path":%q}`, r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: one\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-u.release:
			fmt.Fprint(w, "event: two\ndata: {}\n\n")
		case <-r.Context().Done():
			u.canceled <- struct{}{}
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) requests() []*http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*http.Request(nil), u.reqs...)
}

// guestFor serves vm's proxy handler as the guest would see it.
func guestFor(t *testing.T, h *Host, vm domain.VMID) string {
	srv := httptest.NewServer(h.Handler(vm))
	t.Cleanup(srv.Close)
	return srv.URL
}

func grantFor(t *testing.T, h *Host, up string) domain.CredentialGrant {
	t.Helper()
	ctx := context.Background()
	if err := h.Secrets.PutSecret(ctx, "cp_1", "anthropic", []byte(realKey)); err != nil {
		t.Fatal(err)
	}
	g, err := h.Grant(ctx, GrantSpec{VM: "vm_a", ProfileID: "cp_1", Credential: "anthropic", EnvVar: "ANTHROPIC_API_KEY", Upstream: up + "/base/"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestProxyForwards: the placeholder becomes the real key upstream, only
// at the grant's upstream, never the other way around.
func TestProxyForwards(t *testing.T) {
	h, _, _ := newHost(t)
	up := newUpstream(t)
	g := grantFor(t, h, up.URL)
	guest := guestFor(t, h, "vm_a")

	req, _ := http.NewRequest("POST", guest+"/v1/messages?beta=true", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("X-Api-Key", g.Placeholder)
	req.Header.Set("Authorization", "Bearer "+g.Placeholder)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Host = "evil.example" // the guest cannot pick the upstream
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != `{"path":"/base/v1/messages"}` {
		t.Fatalf("proxied: %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Echo-Key"); got != "[redacted]" {
		t.Errorf("echoed key reached the guest: %q", got)
	}
	rs := up.requests()
	if len(rs) != 1 {
		t.Fatalf("%d upstream requests", len(rs))
	}
	r := rs[0]
	if r.Header.Get("X-Api-Key") != realKey || r.Header.Get("Authorization") != "Bearer "+realKey || r.URL.RawQuery != "beta=true" {
		t.Errorf("upstream request: %v %v", r.Header, r.URL)
	}
	if r.Host != strings.TrimPrefix(up.URL, "http://") {
		t.Errorf("upstream Host = %q", r.Host)
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			if strings.Contains(v, placeholderPrefix) {
				t.Errorf("placeholder leaked upstream in %s: %q", k, v)
			}
			if k == "X-Forwarded-For" {
				t.Errorf("guest-supplied %s forwarded: %q", k, v)
			}
		}
	}
	if strings.Contains(string(body), realKey) {
		t.Error("real key in the guest's response body")
	}
}

// TestProxyRefuses: no placeholder, an unknown one, two different ones, a
// fork child presenting its source's placeholder and CONNECT never reach
// the upstream.
func TestProxyRefuses(t *testing.T) {
	h, _, _ := newHost(t)
	up := newUpstream(t)
	g := grantFor(t, h, up.URL)
	other, err := h.Grant(context.Background(), GrantSpec{VM: "vm_a", ProfileID: "cp_1", Credential: "anthropic", EnvVar: "X", Upstream: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		vm   domain.VMID
		hdr  map[string]string
		code int
	}{
		"none":         {"vm_a", map[string]string{"X-Api-Key": "sk-guess"}, 401},
		"unknown":      {"vm_a", map[string]string{"X-Api-Key": placeholderPrefix + strings.Repeat("0", 48)}, 403},
		"two":          {"vm_a", map[string]string{"X-Api-Key": g.Placeholder, "Authorization": "Bearer " + other.Placeholder}, 401},
		"fork child":   {"vm_child", map[string]string{"X-Api-Key": g.Placeholder}, 403},
		"source again": {"vm_a", map[string]string{"X-Api-Key": g.Placeholder}, 200},
	} {
		req, _ := http.NewRequest("GET", guestFor(t, h, c.vm)+"/v1/models", nil)
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.code {
			t.Errorf("%s: HTTP %d %s, want %d", name, resp.StatusCode, b, c.code)
		}
		if bytes.Contains(b, []byte(realKey)) {
			t.Errorf("%s: real key in the answer", name)
		}
	}
	if n := len(up.requests()); n != 1 {
		t.Errorf("upstream saw %d requests, want 1 (only the source's)", n)
	}
	// CONNECT: no tunnel to anywhere.
	c, err := net.Dial("tcp", strings.TrimPrefix(guestFor(t, h, "vm_a"), "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\nX-Api-Key: %s\r\n\r\n", g.Placeholder)
	line, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.Contains(line, "405") {
		t.Errorf("CONNECT answered %q", line)
	}
	// An upstream the grant could aim elsewhere is refused at grant time.
	for _, bad := range []string{"ftp://x.example", "https://user:pw@x.example", "https://x.example/?q=1", "x.example"} {
		if _, err := h.Grant(context.Background(), GrantSpec{VM: "vm_a", ProfileID: "cp_1", Credential: "anthropic", EnvVar: "X", Upstream: bad}); !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Errorf("upstream %q: %v, want invalid_argument", bad, err)
		}
	}
}

// TestProxyStreamsAndRevokeCuts: SSE events reach the guest as the
// upstream sends them, and revoking the grant cuts a live stream (and the
// upstream request) at once.
func TestProxyStreamsAndRevokeCuts(t *testing.T) {
	h, _, _ := newHost(t)
	up := newUpstream(t)
	g := grantFor(t, h, up.URL)
	guest := guestFor(t, h, "vm_a")
	open := func() (*http.Response, *bufio.Reader) {
		req, _ := http.NewRequest("POST", guest+"/v1/stream", nil)
		req.Header.Set("X-Api-Key", g.Placeholder)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r := bufio.NewReader(resp.Body)
		// The first event arrives while the upstream still holds the second.
		if line, err := r.ReadString('\n'); err != nil || line != "event: one\n" {
			t.Fatalf("first event: %q %v", line, err)
		}
		return resp, r
	}

	resp, r := open()
	close(up.release)
	rest, _ := io.ReadAll(r)
	resp.Body.Close()
	if !strings.Contains(string(rest), "event: two") {
		t.Fatalf("second event: %q", rest)
	}

	up.release = make(chan struct{}) // hold the next stream open
	resp, r = open()
	defer resp.Body.Close()
	if _, err := h.Revoke(context.Background(), g.ID, domain.RevokeExplicit); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	select {
	case b := <-done:
		if strings.Contains(b, "event: two") {
			t.Errorf("stream continued after revoke: %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("revoke did not cut the live stream")
	}
	select {
	case <-up.canceled:
	case <-time.After(5 * time.Second):
		t.Error("upstream request not canceled by revoke")
	}
	req, _ := http.NewRequest("GET", guest+"/v1/models", nil)
	req.Header.Set("X-Api-Key", g.Placeholder)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Errorf("after revoke: %v %v", resp.StatusCode, err)
	}
}

// TestControl drives the control plane: profiles, write-only secrets,
// grants and the revokes profile and credential removal imply. The secret
// is never in any API answer, nor in the state database file.
func TestControl(t *testing.T) {
	h, secrets, db := newHost(t)
	ctx := context.Background()
	c := &Control{DB: db, Secrets: secrets, Proxy: h}
	p, err := c.CreateProfile(ctx, "owner", api.CreateCredentialProfileRequest{Name: "work", Default: true}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := c.CreateProfile(ctx, "owner", api.CreateCredentialProfileRequest{Name: "work", Default: true}, "k1"); err != nil || again.ID != p.ID {
		t.Fatalf("create replay: %+v %v", again, err)
	}
	if _, err := c.CreateProfile(ctx, "owner", api.CreateCredentialProfileRequest{Name: "other"}, "k1"); !domain.IsCode(err, domain.CodeIdempotencyConflict) {
		t.Fatalf("key reuse with another body: %v", err)
	}
	p2, err := c.CreateProfile(ctx, "owner", api.CreateCredentialProfileRequest{Name: "home", Default: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetProfile(ctx, "owner", "work"); got.Default {
		t.Error("a second default profile left the first default")
	}
	if _, err := c.GetProfile(ctx, "intruder", p.ID); !domain.IsCode(err, domain.CodeNotFound) {
		t.Errorf("another owner's profile: %v", err)
	}
	if _, err := c.PutCredential(ctx, "owner", "work", "anthropic", api.PutCredentialRequest{Kind: domain.CredentialAPIKey, Provider: "anthropic", EnvVar: "ANTHROPIC_API_KEY"}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("new credential without secret: %v", err)
	}
	for _, bad := range []api.PutCredentialRequest{
		{Kind: "password", EnvVar: "X", Secret: "s"},
		{Kind: domain.CredentialAPIKey, Provider: "unknown", EnvVar: "X", Secret: "s"},
		{Kind: domain.CredentialCustomAPI, EnvVar: "X", Secret: "s"},
		{Kind: domain.CredentialAPIKey, Provider: "openai", EnvVar: "1BAD", Secret: "s"},
	} {
		if _, err := c.PutCredential(ctx, "owner", "work", "bad", bad); !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Errorf("credential %+v: %v", bad, err)
		}
	}
	got, err := c.PutCredential(ctx, "owner", "work", "anthropic", api.PutCredentialRequest{Kind: domain.CredentialAPIKey, Provider: "anthropic", EnvVar: "ANTHROPIC_API_KEY", Secret: realKey})
	if err != nil || len(got.Credentials) != 1 || !got.Credentials[0].HasSecret {
		t.Fatalf("put credential: %+v %v", got, err)
	}
	if _, err := c.PutCredential(ctx, "owner", "work", "llm", api.PutCredentialRequest{Kind: domain.CredentialCustomAPI, BaseURL: "https://llm.example/v1/", EnvVar: "LLM_KEY", Secret: "sk-other"}); err != nil {
		t.Fatal(err)
	}
	// Metadata-only update keeps the secret.
	if _, err := c.PutCredential(ctx, "owner", "work", "llm", api.PutCredentialRequest{Kind: domain.CredentialCustomAPI, BaseURL: "https://llm.example/v2", EnvVar: "LLM_KEY"}); err != nil {
		t.Fatal(err)
	}

	g, err := c.CreateGrant(ctx, "owner", "vm_a", api.CreateCredentialGrantRequest{Profile: "work", Credential: "anthropic"}, "g1")
	if err != nil || g.Upstream != "https://api.anthropic.com" || g.EnvVar != "ANTHROPIC_API_KEY" {
		t.Fatalf("grant: %+v %v", g, err)
	}
	if again, err := c.CreateGrant(ctx, "owner", "vm_a", api.CreateCredentialGrantRequest{Profile: "work", Credential: "anthropic"}, "g1"); err != nil || again.ID != g.ID {
		t.Fatalf("grant replay: %+v %v", again, err)
	}
	g2, err := c.CreateGrant(ctx, "owner", "vm_a", api.CreateCredentialGrantRequest{Profile: p.ID, Credential: "llm"}, "")
	if err != nil || g2.Upstream != "https://llm.example/v2" {
		t.Fatalf("custom grant: %+v %v", g2, err)
	}
	if _, err := c.CreateGrant(ctx, "intruder", "vm_a", api.CreateCredentialGrantRequest{Profile: p.ID, Credential: "llm"}, ""); !domain.IsCode(err, domain.CodeNotFound) {
		t.Errorf("grant from another owner's profile: %v", err)
	}
	if _, err := c.RevokeGrant(ctx, "owner", "vm_child", g.ID); !domain.IsCode(err, domain.CodeNotFound) {
		t.Errorf("revoke through another VM: %v", err)
	}

	if err := c.DeleteCredential(ctx, "owner", "work", "llm"); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Secret(ctx, p.ID, "llm"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Errorf("removed credential's secret kept: %v", err)
	}
	if err := c.DeleteProfile(ctx, "owner", "work"); err != nil {
		t.Fatal(err)
	}
	l, _ := c.ListGrants(ctx, "vm_a")
	reasons := map[string]domain.RevokeReason{}
	for _, x := range l.Items {
		reasons[x.ID] = x.RevokeReason
	}
	if reasons[g.ID] != domain.RevokeProfileDeleted || reasons[g2.ID] != domain.RevokeCredentialRemoved {
		t.Errorf("revoke reasons: %v", reasons)
	}
	if pl, _ := c.ListProfiles(ctx, "owner", 0, ""); len(pl.Items) != 1 || pl.Items[0].ID != p2.ID {
		t.Errorf("profiles after delete: %+v", pl.Items)
	}

	// The secret never appears in answers or in the database.
	doc, _ := json.Marshal([]any{got, g, l, p})
	if bytes.Contains(doc, []byte(realKey)) {
		t.Errorf("an API answer carries the secret: %s", doc)
	}
	db.Close()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(secrets.Dir), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(realKey)) || bytes.Contains(raw, []byte("sk-other")) {
		t.Error("a secret is in the state database file")
	}
}

// TestLegacyPlaceholder: a placeholder from a grant made before the
// slipway -> davit rename is still recognized in guest requests.
func TestLegacyPlaceholder(t *testing.T) {
	for _, p := range []string{"davit-placeholder-", "slipway-placeholder-"} {
		ph := p + strings.Repeat("ab", 24)
		hdr := http.Header{"Authorization": {"Bearer " + ph}}
		if got, ok := placeholderOf(hdr); !ok || got != ph {
			t.Fatalf("%s: %q %v", p, got, ok)
		}
	}
}
