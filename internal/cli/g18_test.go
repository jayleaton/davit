package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/api"
)

// fakeTokenAPI serves /me and the token endpoints and records requests.
type fakeTokenAPI struct {
	mu   sync.Mutex
	reqs []recorded
}

type recorded struct {
	method, path, key string
	body              map[string]any
}

func (f *fakeTokenAPI) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.Header.Get(api.HeaderIdempotencyKey), body})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
		tok := api.Token{ID: "tok_1", Name: "ci", Scopes: []api.TokenScope{{Actions: []string{"exec"}}}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastUsedAt: &now, UseCount: 42}
		switch {
		case r.URL.Path == "/api/v1/me":
			json.NewEncoder(w).Encode(api.Me{Principal: "tailnet:owner", Auth: api.AuthToken, Identity: "alice via laptop (n1)", Token: &tok, Host: "box", Version: "test"})
		case r.Method == "GET" && r.URL.Path == "/api/v1/tokens":
			json.NewEncoder(w).Encode(api.TokenList{Items: []api.Token{tok}})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/rotate"):
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(api.CreatedToken{ID: "tok_2", Name: "ci", Scopes: tok.Scopes, CreatedAt: now, ExpiresAt: now.Add(time.Hour), Secret: "slw_NEWSECRET"})
		case r.Method == "POST" && r.URL.Path == "/api/v1/tokens":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(api.CreatedToken{ID: "tok_3", Name: "x", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Secret: "slw_CREATED"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DAVIT_API_URL", "")
	t.Setenv("DAVIT_HOST", strings.TrimPrefix(srv.URL, "http://"))
	return srv
}

// TestWhoamiShowsNoSecret: whoami prints the principal, auth, token scopes
// and endpoint, and only the name of the variable the token came from.
func TestWhoamiShowsNoSecret(t *testing.T) {
	(&fakeTokenAPI{}).serve(t)
	t.Setenv("DAVIT_TOKEN", "slw_SUPERSECRET")
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"whoami"}, Stdio{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := stdout.String() + stderr.String()
	for _, want := range []string{"tailnet:owner", "token (alice via laptop (n1))", "tok_1 (ci), from DAVIT_TOKEN", "exec on all VMs", "endpoint"} {
		if !strings.Contains(out, want) {
			t.Errorf("whoami output lacks %q:\n%s", want, out)
		}
	}
	r := runCLI(t, false, "whoami")
	raw, _ := json.Marshal(r.data)
	if strings.Contains(out, "SUPERSECRET") || strings.Contains(string(raw), "SUPERSECRET") {
		t.Fatalf("whoami printed the secret:\n%s\n%s", out, raw)
	}
	if r.data["tokenSource"] != "DAVIT_TOKEN" {
		t.Errorf("json: %v", r.data)
	}
}

// TestTokenOps: presets map to actions, rotate sends the overlap and a
// key and prints the new secret once, usage lists counts.
func TestTokenOps(t *testing.T) {
	f := &fakeTokenAPI{}
	f.serve(t)
	t.Setenv("DAVIT_TOKEN", "slw_x")
	if r := runCLI(t, false, "token", "create", "x", "--preset", "readonly"); r.code != 0 {
		t.Fatalf("create --preset: %+v", r.last.Error)
	}
	if r := runCLI(t, false, "token", "create", "x", "--preset", "readonly", "--action", "admin"); r.code != ExitUsage {
		t.Errorf("--preset with --action: exit %d", r.code)
	}
	if r := runCLI(t, false, "token", "create", "x", "--preset", "root"); r.code != ExitUsage {
		t.Errorf("unknown preset: exit %d", r.code)
	}
	r := runCLI(t, false, "token", "rotate", "tok_1", "--overlap", "10m")
	if r.code != 0 || r.data["secret"] != "slw_NEWSECRET" {
		t.Fatalf("rotate: %d %+v", r.code, r.data)
	}
	if r := runCLI(t, false, "token", "usage"); r.code != 0 || r.data["count"] != float64(1) {
		t.Fatalf("usage: %d %+v", r.code, r.data)
	}
	if r := runCLI(t, false, "token", "usage", "nope"); r.code == 0 {
		t.Error("usage of an unknown token succeeded")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var create, rotate *recorded
	for i := range f.reqs {
		switch q := &f.reqs[i]; {
		case q.method == "POST" && q.path == "/api/v1/tokens" && create == nil:
			create = q
		case strings.HasSuffix(q.path, "/rotate"):
			rotate = q
		}
	}
	if create == nil || create.body["ttlSeconds"] != float64(30*24*3600) {
		t.Fatalf("create request: %+v", create)
	}
	scopes, _ := create.body["scopes"].([]any)
	if len(scopes) != 1 || !strings.Contains(toJSON(scopes[0]), `"actions":["view"]`) {
		t.Errorf("readonly preset sent %v", scopes)
	}
	if rotate == nil || rotate.path != "/api/v1/tokens/tok_1/rotate" || rotate.body["overlapSeconds"] != float64(600) || rotate.key == "" {
		t.Errorf("rotate request: %+v", rotate)
	}
	if _, has := rotate.body["ttlSeconds"]; has {
		t.Errorf("rotate without --ttl sent a ttl: %v", rotate.body)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// TestVersionCheck: a release directory's release.json is read; GitHub's
// latest-release redirect gives the tag, or the newest prerelease while there
// are only prereleases; anything unreachable is "unknown";
// nothing ever fails the command.
func TestVersionCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "release.json"), []byte(`{"version": "v99.0.0"}`), 0o644)
	t.Setenv("DAVIT_RELEASE_URL", "file://"+dir)
	r := runCLI(t, false, "version", "--check")
	if r.code != 0 || r.data["latest"] != "v99.0.0" || r.data["status"] == "unknown" {
		t.Fatalf("release dir: %d %+v", r.code, r.data)
	}
	t.Setenv("DAVIT_RELEASE_URL", "https://127.0.0.1:1")
	if r := runCLI(t, false, "version", "--check"); r.code != 0 || r.data["status"] != "unknown" {
		t.Fatalf("offline: %d %+v", r.code, r.data)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/jayleaton/davit/releases/tag/v98.1.0", http.StatusFound)
	}))
	defer gh.Close()
	old := latestReleaseURL
	latestReleaseURL = gh.URL + "/releases/latest"
	defer func() { latestReleaseURL = old }()
	t.Setenv("DAVIT_RELEASE_URL", "")
	if r := runCLI(t, false, "version", "--check"); r.code != 0 || r.data["latest"] != "v98.1.0" {
		t.Fatalf("github: %d %+v", r.code, r.data)
	}
	// Only prereleases: the latest-release page goes to the release list, and
	// the newest prerelease is the answer.
	pre := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			http.Redirect(w, r, "/jayleaton/davit/releases", http.StatusFound)
			return
		}
		w.Write([]byte(`[{"tag_name": "v98.2.0-rc3", "draft": false, "prerelease": true}]`))
	}))
	defer pre.Close()
	oldAPI := releasesAPIURL
	latestReleaseURL, releasesAPIURL = pre.URL+"/releases/latest", pre.URL+"/repos/jayleaton/davit/releases"
	defer func() { releasesAPIURL = oldAPI }()
	if r := runCLI(t, false, "version", "--check"); r.code != 0 || r.data["latest"] != "v98.2.0-rc3" || r.data["source"] != releasesAPIURL {
		t.Fatalf("prereleases only: %d %+v", r.code, r.data)
	}
	for _, c := range []struct{ cur, latest, want string }{
		{"1.2.3", "v1.2.3", "up-to-date"},
		{"1.2.3", "1.3.0", "update-available"},
		{"1.3.0", "1.2.9", "ahead"},
		{"1.3.0-rc.1", "1.3.0", "update-available"},
		{"1.3.0", "1.3.0-rc.1", "ahead"},
		{"m1-195-gabc", "1.0.0", "different"},
	} {
		if got := compareVersions(c.cur, c.latest); got != c.want {
			t.Errorf("compare(%s, %s) = %s, want %s", c.cur, c.latest, got, c.want)
		}
	}
}
