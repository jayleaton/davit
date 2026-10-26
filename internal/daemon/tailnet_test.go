package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"davit/api"
	"davit/internal/config"
	"davit/internal/gateway"
	"davit/internal/gateway/gatewaytest"
)

// TestTailnetListenerOwnerOnly runs the real daemon with the tailnet gateway
// against an in-process test tailnet: the configured owner drives the same
// API (and sees the same VMs) as the local socket; another tailnet user is
// refused.
func TestTailnetListenerOwnerOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("starts in-process tailnet nodes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	controlURL := gatewaytest.StartControl(t)
	alice := gatewaytest.StartPeer(t, ctx, controlURL, "alice-laptop")
	mallory := gatewaytest.StartPeer(t, ctx, controlURL, "mallory-laptop")
	if alice.LoginName == "" || alice.LoginName == mallory.LoginName {
		t.Fatalf("need distinct logins, got %q and %q", alice.LoginName, mallory.LoginName)
	}

	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Socket = filepath.Join(shortSockDir(t), "s.sock")
	cfg.Tailnet = config.Tailnet{
		Enabled:     true,
		Hostname:    "davit-test",
		ControlURL:  controlURL,
		OwnerLogins: []string{strings.ToUpper(alice.LoginName)}, // case-insensitive
	}
	// Fake guests (Linux) give the gateway a real guest transport and sshd.
	guests := goruntime.GOOS == "linux"
	cfg.Fake.GuestAgent = guests
	runCtx, stop := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- Run(runCtx, cfg, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() {
		stop()
		if err := <-errc; err != nil {
			t.Errorf("daemon: %v", err)
		}
	})

	base := "http://" + gatewaytest.PeerAddr(t, ctx, alice, "davit-test").String() + "/api/v1"
	do := func(p gatewaytest.Peer, method, path, body string) (int, string) {
		t.Helper()
		var lastErr error
		for range 40 { // the daemon's node may still be finishing DERP setup
			req, _ := http.NewRequestWithContext(ctx, method, base+path, strings.NewReader(body))
			if body != "" {
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set(api.HeaderIdempotencyKey, "k-"+method+path)
			}
			res, err := p.Srv.HTTPClient().Do(req)
			if err != nil {
				lastErr = err
				time.Sleep(250 * time.Millisecond)
				continue
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			return res.StatusCode, string(b)
		}
		t.Fatalf("%s %s: %v", method, path, lastErr)
		return 0, ""
	}

	if code, body := do(mallory, "GET", "/vms", ""); code != http.StatusForbidden || !strings.Contains(body, "permission_denied") {
		t.Fatalf("non-owner list: %d %s", code, body)
	}
	if code, body := do(alice, "POST", "/vms", `{"name":"devbox"}`); code != http.StatusAccepted {
		t.Fatalf("owner create: %d %s", code, body)
	}
	if code, body := do(alice, "GET", "/vms", ""); code != http.StatusOK || !strings.Contains(body, `"devbox"`) {
		t.Fatalf("owner list: %d %s", code, body)
	}
	if code, _ := do(mallory, "GET", "/vms/devbox", ""); code != http.StatusForbidden {
		t.Fatalf("non-owner get: %d", code)
	}

	// The configured login was pinned to alice's stable tailnet user ID on
	// her first request (WhoIs through the real tsnet path).
	owners, err := gateway.OpenOwners(filepath.Join(cfg.TailnetStateDir(), "owners.json"),
		gateway.OwnerPolicy{Logins: cfg.Tailnet.OwnerLogins}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b := owners.Bindings(); len(b) != 1 || b[0].UserID != alice.UserID || b[0].UserID == mallory.UserID {
		t.Fatalf("owner bindings %+v, want alice's user ID %d", b, alice.UserID)
	}

	// Node state lives where `doctor` looks for an enrolled node.
	if fi, err := os.Stat(filepath.Join(cfg.TailnetStateDir(), "tailscaled.state")); err != nil || fi.Size() == 0 {
		t.Fatalf("tailnet state not in %s: %v", cfg.TailnetStateDir(), err)
	}

	// The VM created over the tailnet belongs to the local owner too.
	local := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", cfg.Socket)
	}}}
	res, err := local.Get("http://local/api/v1/vms/devbox")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("local socket get devbox: %d", res.StatusCode)
	}

	// Service tokens: the owner creates one on the local socket (view on
	// devbox only). On the tailnet a WhoIs identity is still required, and
	// the bearer's scopes are all a request gets, owner identity or not.
	localDo := func(method, path, body string, out any) int {
		t.Helper()
		req, _ := http.NewRequest(method, "http://local/api/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := local.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if out != nil {
			json.NewDecoder(res.Body).Decode(out)
		}
		return res.StatusCode
	}
	var tok api.CreatedToken
	if code := localDo("POST", "/tokens", `{"name":"ci","ttlSeconds":600,"scopes":[{"vmId":"devbox","actions":["view"]}]}`, &tok); code != http.StatusCreated || tok.Secret == "" {
		t.Fatalf("create token: %d %+v", code, tok)
	}
	bearer := func(p gatewaytest.Peer, method, path, secret string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, method, base+path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		res, err := p.Srv.HTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	for _, c := range []struct {
		who          gatewaytest.Peer
		method, path string
		secret       string
		want         int
	}{
		{mallory, "GET", "/vms/devbox", tok.Secret, http.StatusOK},              // in scope, non-owner peer
		{mallory, "GET", "/vms", tok.Secret, http.StatusForbidden},              // needs an all-VM scope
		{mallory, "POST", "/vms/devbox/stop", tok.Secret, http.StatusForbidden}, // admin > view
		{mallory, "GET", "/tokens", tok.Secret, http.StatusForbidden},           // tokens never manage tokens
		{alice, "POST", "/vms/devbox/stop", tok.Secret, http.StatusForbidden},   // the token limits the owner too
		{mallory, "GET", "/vms/devbox", tok.Secret + "x", http.StatusUnauthorized},
	} {
		if got := bearer(c.who, c.method, c.path, c.secret); got != c.want {
			t.Errorf("%s %s with token: HTTP %d, want %d", c.method, c.path, got, c.want)
		}
	}
	if code := localDo("DELETE", "/tokens/"+tok.ID, "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if got := bearer(mallory, "GET", "/vms/devbox", tok.Secret); got != http.StatusUnauthorized {
		t.Fatalf("revoked token: HTTP %d, want 401", got)
	}

	if !guests {
		return
	}
	// SSH front door: the gateway authorizes the tailnet identity, then logs
	// in to the guest sshd with a CA certificate for this VM generation and
	// pins the host key the barrier reported.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, body := do(alice, "GET", "/vms/devbox", ""); strings.Contains(body, `"state":"ready"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("devbox never became ready")
		}
	}
	gwKey, err := gateway.LoadOrCreateHostKey(filepath.Join(cfg.TailnetStateDir(), "ssh_host_ed25519_key"))
	if err != nil {
		t.Fatal(err)
	}
	sshAddr := net.JoinHostPort(gatewaytest.PeerAddr(t, ctx, alice, "davit-test").String(), "22")
	run := func(p gatewaytest.Peer, cmd string) (string, error) {
		c, err := p.Srv.Dial(ctx, "tcp", sshAddr)
		if err != nil {
			return "", err
		}
		c.SetDeadline(time.Now().Add(20 * time.Second))
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		signer, _ := ssh.NewSignerFromKey(priv)
		conn, chans, reqs, err := ssh.NewClientConn(c, sshAddr, &ssh.ClientConfig{
			User: "devbox", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(gwKey.PublicKey()),
		})
		if err != nil {
			c.Close()
			return "", err
		}
		client := ssh.NewClient(conn, chans, reqs)
		defer client.Close()
		sess, err := client.NewSession()
		if err != nil {
			return "", err
		}
		defer sess.Close()
		out, err := sess.Output(cmd)
		return string(out), err
	}
	if out, err := run(alice, "echo via-front-door"); err != nil || out != "via-front-door\n" {
		t.Fatalf("owner ssh through the gateway: out %q err %v", out, err)
	}
	if _, err := run(mallory, "true"); err == nil {
		t.Fatal("non-owner reached the guest through the SSH front door")
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
