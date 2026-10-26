package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"davit/internal/domain"
	"davit/internal/httpapi"
	"davit/internal/lifecycle"
	"davit/internal/runtime/fake"
	"davit/internal/state"
	"davit/internal/storage"
)

// serve runs the real API over a Unix socket against the fake runtime and
// points the CLI at it with an isolated config dir.
func serve(t *testing.T) { serveWith(t, nil) }

// serveWith is serve with a hook to adjust the lifecycle config.
func serveWith(t *testing.T, adjust func(*lifecycle.Config)) { serveAuth(t, adjust, nil) }

// serveAuth is serveWith with another Authenticator (nil: the owner's peer
// credentials, as the daemon does).
func serveAuth(t *testing.T, adjust func(*lifecycle.Config), auth httpapi.Authenticator) {
	dir := t.TempDir()
	db, err := state.Open(context.Background(), filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := fake.New(fake.Options{Dir: filepath.Join(dir, "rt")})
	st, _ := storage.Open(filepath.Join(dir, "st"), storage.ModeCopy)
	img, _ := lifecycle.DevImage(filepath.Join(dir, "img"))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := lifecycle.Config{DB: db, Runtime: rt, Storage: st, Images: lifecycle.NewStaticCatalog(img), Log: log}
	if adjust != nil {
		adjust(&cfg)
	}
	svc, err := lifecycle.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(shortSockDir(t), "api.sock")
	ln, err := httpapi.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	if auth == nil {
		auth = httpapi.PeerAuthenticator{OwnerUID: os.Getuid()}
	}
	srv := &http.Server{Handler: httpapi.New(svc, auth, log).Handler(), ConnContext: httpapi.PeerConnContext}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); svc.Close(); db.Close() })
	t.Setenv("DAVIT_API_URL", "unix://"+sock)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("DAVIT_CURRENT_ID", "")
}

type result struct {
	code int
	last Envelope
	data map[string]any
}

func runCLI(t *testing.T, interactive bool, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(append([]string{"--json"}, args...), Stdio{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Interactive: interactive})
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var r result
	r.code = code
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &r.last); err != nil {
		t.Fatalf("%v: last line %q", args, lines[len(lines)-1])
	}
	var raw struct {
		Data map[string]any `json:"data"`
	}
	json.Unmarshal([]byte(lines[len(lines)-1]), &raw)
	r.data = raw.Data
	return r
}

func vmName(t *testing.T, r result) string {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d: %+v", r.code, r.last.Error)
	}
	if vm, ok := r.data["vm"].(map[string]any); ok {
		return vm["name"].(string)
	}
	return r.data["name"].(string) // info returns the VM itself
}

func TestSavedContextSemantics(t *testing.T) {
	serve(t)
	const tty, script = true, false

	vmName(t, runCLI(t, tty, "new", "web"))
	if got := vmName(t, runCLI(t, tty, "info")); got != "web" {
		t.Fatalf("interactive new did not select context: %s", got)
	}
	if r := runCLI(t, script, "info"); r.code != ExitUsage {
		t.Fatalf("scripts must not use saved context: exit %d", r.code)
	}
	vmName(t, runCLI(t, script, "new", "batch"))
	if got := vmName(t, runCLI(t, tty, "info")); got != "web" {
		t.Fatalf("non-interactive new changed context to %s", got)
	}

	vmName(t, runCLI(t, tty, "new", "other"))
	vmName(t, runCLI(t, tty, "stop", "web"))
	vmName(t, runCLI(t, tty, "resume", "web"))
	if got := vmName(t, runCLI(t, tty, "info")); got != "other" {
		t.Fatalf("resume must not change context: %s", got)
	}

	t.Setenv("DAVIT_CURRENT_ID", "web")
	if got := vmName(t, runCLI(t, script, "info")); got != "web" {
		t.Fatalf("env must override saved context: %s", got)
	}
	if r := runCLI(t, tty, "context", "show"); r.data["source"] != "env" {
		t.Fatalf("context show with env: %+v", r.data)
	}
	t.Setenv("DAVIT_CURRENT_ID", "")

	// snapshot create NAME uses the context VM.
	if r := runCLI(t, tty, "snapshot", "create", "snap-a"); r.code != ExitConflict || r.last.Error.Code != domain.CodeStopRequired {
		t.Fatalf("snapshot of running context VM: %d %+v", r.code, r.last.Error)
	}
	vmName(t, runCLI(t, tty, "stop"))
	if r := runCLI(t, tty, "snapshot", "create", "snap-a"); r.code != 0 || r.data["snapshot"].(map[string]any)["name"] != "snap-a" {
		t.Fatalf("snapshot create via context: %d %+v", r.code, r.data)
	}

	if r := runCLI(t, tty, "context", "use", "web"); r.code != 0 {
		t.Fatalf("context use: %+v", r.last.Error)
	}
	vmName(t, runCLI(t, tty, "delete", "web", "--yes"))
	if r := runCLI(t, tty, "context", "show"); r.data["source"] != "none" {
		t.Fatalf("deleting the context VM must clear it: %+v", r.data)
	}
	if r := runCLI(t, tty, "context", "use", "nope"); r.code != ExitNotFound {
		t.Fatalf("context use of a missing VM: exit %d", r.code)
	}
	runCLI(t, tty, "context", "use", "other")
	if r := runCLI(t, tty, "context", "clear"); r.code != 0 {
		t.Fatal(r.last.Error)
	}
	if r := runCLI(t, tty, "info"); r.code != ExitUsage {
		t.Fatalf("after clear: exit %d", r.code)
	}
}

// shortSockDir returns a short-lived directory for Unix sockets. t.TempDir
// can exceed the 108-byte sun_path limit (macOS /var/folders, or GOTMPDIR,
// which t.TempDir follows since Go 1.26), so sockets live under TMPDIR.
func shortSockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "slw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// TestHostSelectsRemoteGateway: --host (or DAVIT_HOST) points the CLI at a
// remote host's tailnet gateway, which serves the same API over HTTP(S).
func TestHostSelectsRemoteGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/capabilities" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"version":"remote","runtime":{"engine":"firecracker"},"storage":{"mode":"reflink"}}`))
	}))
	defer srv.Close()
	t.Setenv("DAVIT_API_URL", "")
	addr := strings.TrimPrefix(srv.URL, "http://")
	for _, args := range [][]string{{"--host", addr, "capabilities"}, {"--host", srv.URL, "capabilities"}} {
		if r := runCLI(t, false, args...); r.code != 0 || r.data["version"] != "remote" {
			t.Fatalf("%v: exit %d %+v", args, r.code, r.last.Error)
		}
	}
	t.Setenv("DAVIT_HOST", addr)
	if r := runCLI(t, false, "capabilities"); r.code != 0 || r.data["version"] != "remote" {
		t.Fatalf("DAVIT_HOST: exit %d %+v", r.code, r.last.Error)
	}
	if r := runCLI(t, false, "--host", "ftp://x", "capabilities"); r.code != ExitUsage {
		t.Fatalf("bad scheme: exit %d", r.code)
	}
}

// TestJSONFlagAfterDashDashBelongsToGuest: "exec ID -- tool --json" passes
// --json to the guest command; davit's own output stays human.
func TestJSONFlagAfterDashDashBelongsToGuest(t *testing.T) {
	t.Setenv("DAVIT_API_URL", "unix://"+filepath.Join(t.TempDir(), "none.sock"))
	var stdout, stderr bytes.Buffer
	code := Main([]string{"exec", "box", "--", "tool", "--json"}, Stdio{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
	if code != ExitTransport || stdout.Len() != 0 || !strings.Contains(stderr.String(), "error:") {
		t.Fatalf("exit %d, stdout %q, stderr %q: want a human error, no JSON", code, stdout.String(), stderr.String())
	}
}
