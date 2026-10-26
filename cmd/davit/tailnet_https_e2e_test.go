//go:build linux

package main_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"davit/api"
	"davit/internal/config"
	"davit/internal/daemon"
	"davit/internal/gateway"
	"davit/internal/gateway/gatewaytest"
	"davit/internal/preflight"
)

// writeTestPKI writes a throwaway CA and a server certificate for names (IP
// literals become IP SANs) into dir.
func writeTestPKI(t *testing.T, dir string, names ...string) (caFile, certFile, keyFile string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "davit test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	write := func(name, typ string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return write("ca.pem", "CERTIFICATE", caDER), write("cert.pem", "CERTIFICATE", der), write("key.pem", "EC PRIVATE KEY", keyDER)
}

// laptopRelay stands in for a laptop on the tailnet: a loopback listener
// whose connections p (a tsnet peer) carries to the gateway's port 443, so
// the CLI and SDKs run unmodified, end to end over TLS, as that tailnet
// identity. It returns the https URL.
func laptopRelay(t *testing.T, ctx context.Context, p gatewaytest.Peer, gatewayIP string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer c.Close()
				dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				up, err := p.Srv.Dial(dctx, "tcp", net.JoinHostPort(gatewayIP, "443"))
				cancel()
				if err != nil {
					return
				}
				defer up.Close()
				go func() { io.Copy(up, c); up.Close() }()
				io.Copy(c, up)
			})
		}
	})
	return "https://" + ln.Addr().String()
}

// statusChecks parses `davit --json status` items by check ID.
func statusChecks(t *testing.T, r run) map[string]preflight.Check {
	t.Helper()
	out := map[string]preflight.Check{}
	for i, l := range r.lines {
		if l.Event != "item" {
			continue
		}
		var c preflight.Check
		if err := json.Unmarshal(r.raw[i], &c); err != nil {
			t.Fatal(err)
		}
		out[c.ID] = c
	}
	return out
}

// TestRemoteHTTPSOverTailnet is the hermetic laptop → host run: the real
// daemon (fake runtime) serves its API on an in-process test tailnet over
// TLS with an operator certificate, and the CLI, `davit ssh` and the SDK
// examples reach it with only DAVIT_API_URL, DAVIT_TOKEN and
// DAVIT_CA_FILE. A non-owner peer, a revoked token and a revoked (expired)
// device are denied, and `status` names the fix for each.
func TestRemoteHTTPSOverTailnet(t *testing.T) {
	if testing.Short() {
		t.Skip("starts in-process tailnet nodes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	control := gatewaytest.StartControlServer(t)
	controlURL := control.HTTPTestServer.URL
	alice := gatewaytest.StartPeer(t, ctx, controlURL, "alice-laptop")
	mallory := gatewaytest.StartPeer(t, ctx, controlURL, "ci-runner")

	h := newHost(t)
	const gwName = "davit-g2"
	fqdn := gwName + ".tail-scale.ts.net"
	caFile, certFile, keyFile := writeTestPKI(t, h.dir, fqdn, "127.0.0.1")
	cfgJSON := fmt.Sprintf(`{"dataDir":%q,"socket":%q,"fake":{"guestAgent":true},
		"tailnet":{"enabled":true,"hostname":%q,"controlURL":%q,"ownerLogins":[%q],"tlsCertFile":%q,"tlsKeyFile":%q}}`,
		filepath.Join(h.dir, "data"), h.sock, gwName, controlURL, alice.LoginName, certFile, keyFile)
	cfgPath := filepath.Join(h.dir, "daemon.json")
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- daemon.Run(runCtx, cfg, slog.New(slog.DiscardHandler)) }()
	t.Cleanup(func() {
		stop()
		if err := <-errc; err != nil {
			t.Errorf("daemon: %v", err)
		}
	})

	gwIP := gatewaytest.PeerAddr(t, ctx, alice, gwName).String()
	gatewaytest.PeerAddr(t, ctx, mallory, gwName)
	aliceURL := laptopRelay(t, ctx, alice, gwIP)
	malloryURL := laptopRelay(t, ctx, mallory, gwIP)

	home := filepath.Join(h.dir, "home")
	os.MkdirAll(home, 0o700)
	remote := func(url, token string, args ...string) run {
		t.Helper()
		h.env = []string{"HOME=" + home, "DAVIT_API_URL=" + url, "DAVIT_TOKEN=" + token, "DAVIT_CA_FILE=" + caFile}
		defer func() { h.env = nil }()
		return h.cli(args...)
	}

	// The gateway reports itself up with the https URL; on the host,
	// `status` (local socket) shows how remote clients reach it.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if s, err := gateway.ReadStatus(filepath.Join(cfg.TailnetStateDir(), gateway.StatusFile)); err == nil && s.State == gateway.EnrollUp {
			if s.APIURL != "https://"+fqdn || s.TLS != gateway.TLSFiles {
				t.Fatalf("gateway status %+v", s)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway never reported up")
		}
	}
	r := h.cli("status", "--config", cfgPath)
	if c := statusChecks(t, r); r.code != 0 || c["tailnet_gateway"].Status != preflight.Pass ||
		!strings.Contains(c["tailnet_gateway"].Detail, "DAVIT_API_URL=https://"+fqdn) || c["tailnet_tls"].Status != preflight.Pass {
		t.Fatalf("host status: exit %d %s", r.code, r.raw)
	}

	// The owner's laptop: tailnet identity, no token. The daemon's node may
	// still be finishing DERP setup, so the first call is retried.
	for i := 0; ; i++ {
		r = remote(aliceURL, "", "status")
		if r.code == 0 || i == 40 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if c := statusChecks(t, r); r.code != 0 || c["api"].Status != preflight.Pass || c["ca_file"].Status != preflight.Pass {
		t.Fatalf("owner status: exit %d %s", r.code, r.raw)
	}
	h.waited(remote(aliceURL, "", "new", "box"), "create")
	if r = remote(aliceURL, "", "exec", "box", "--", "echo", "over-https"); r.code != 0 || output(t, r, "stdout") != "over-https\n" {
		t.Fatalf("owner exec over https: exit %d %s", r.code, r.raw)
	}

	// Without the CA, the certificate is refused and status says to set it.
	h.env = []string{"HOME=" + home, "DAVIT_API_URL=" + aliceURL, "DAVIT_TOKEN=", "DAVIT_CA_FILE="}
	r = h.cli("status")
	h.env = nil
	if c := statusChecks(t, r); r.code == 0 || !strings.Contains(c["api"].Hint, "DAVIT_CA_FILE") {
		t.Fatalf("status without the CA: exit %d %s", r.code, r.raw)
	}

	// A CI runner on the tailnet that is not the owner is refused...
	r = remote(malloryURL, "", "status")
	if c := statusChecks(t, r); r.code == 0 || !strings.Contains(c["api"].Hint, "ownerLogins") {
		t.Fatalf("non-owner status: exit %d %s", r.code, r.raw)
	}
	if r = remote(malloryURL, "", "list"); r.code == 0 || r.last().Error.Code != "permission_denied" {
		t.Fatalf("non-owner list: exit %d %s", r.code, r.raw)
	}
	// ...until the owner gives it a service token.
	r = h.cli("token", "create", "ci", "--action", "exec", "--ttl", "1h")
	var tok api.CreatedToken
	json.Unmarshal(r.raw[len(r.raw)-1], &tok)
	if r.code != 0 || tok.Secret == "" {
		t.Fatalf("token create: exit %d %s", r.code, r.raw)
	}
	if r = remote(malloryURL, tok.Secret, "status"); r.code != 0 {
		t.Fatalf("token status: exit %d %s", r.code, r.raw)
	}
	if r = remote(malloryURL, tok.Secret, "exec", "box", "--", "echo", "ci"); r.code != 0 || output(t, r, "stdout") != "ci\n" {
		t.Fatalf("token exec: exit %d %s", r.code, r.raw)
	}
	if _, err := exec.LookPath("ssh"); err == nil {
		// `davit ssh` remote: API tunnel plus a certificate from
		// issueSSHCredential, so a token is enough (no front door).
		cmd := exec.Command(bin, "ssh", "box", "--", "echo", "ssh-over-https")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config-ci"), "DAVIT_CURRENT_ID=",
			"DAVIT_API_URL="+malloryURL, "DAVIT_TOKEN="+tok.Secret, "DAVIT_CA_FILE="+caFile)
		out, err := cmd.Output()
		if err != nil || string(out) != "ssh-over-https\n" {
			t.Fatalf("davit ssh over https with a token: %q %v", out, err)
		}
	}
	// The SDK examples create and delete a VM, which needs admin.
	r = h.cli("token", "create", "sdk", "--action", "admin", "--ttl", "1h")
	var admin api.CreatedToken
	json.Unmarshal(r.raw[len(r.raw)-1], &admin)
	if r.code != 0 || admin.Secret == "" {
		t.Fatalf("admin token create: exit %d %s", r.code, r.raw)
	}
	runSDKExamples(t, malloryURL, admin.Secret, caFile)

	// Reverse forwards reach out of the VM: owner or an admin token only.
	if r = remote(malloryURL, "", "forward", "--reverse", "--list", "box"); r.code == 0 || r.last().Error.Code != "permission_denied" {
		t.Fatalf("non-owner reverse list: exit %d %s", r.code, r.raw)
	}
	if r = remote(malloryURL, tok.Secret, "forward", "--reverse", "--list", "box"); r.code == 0 || r.last().Error.Code != "permission_denied" {
		t.Fatalf("exec token reverse list: exit %d %s", r.code, r.raw)
	}
	// With an admin token, a session from the CI runner over tailnet HTTPS.
	laptop := echoServer(t)
	gport := freePort(t)
	h.env = []string{"HOME=" + home, "DAVIT_API_URL=" + malloryURL, "DAVIT_TOKEN=" + admin.Secret, "DAVIT_CA_FILE=" + caFile}
	revSession, revWait := h.background(`"reverse.listening"`, "--json", "forward", "--reverse", "box", fmt.Sprintf("%d:127.0.0.1:%d", gport, laptop))
	h.env = nil
	if got, err := guestRoundTrip(t, gport, "over-tailnet-https"); err != nil || got != "over-tailnet-https" {
		t.Fatalf("reverse round trip over tailnet HTTPS: %q %v", got, err)
	}
	revSession.Process.Signal(syscall.SIGINT)
	if code, out, _ := revWait(); code != 0 {
		t.Fatalf("reverse session over HTTPS after Ctrl-C: exit %d\n%s", code, out)
	}

	// A revoked token is denied, and status says to create a new one.
	if r = h.cli("token", "revoke", tok.ID, "--yes"); r.code != 0 {
		t.Fatalf("revoke: exit %d", r.code)
	}
	r = remote(malloryURL, tok.Secret, "status")
	if c := statusChecks(t, r); r.code == 0 || c["api"].Status != preflight.Fail || !strings.Contains(c["api"].Hint, "revoked") {
		t.Fatalf("revoked token status: exit %d %s", r.code, r.raw)
	}
	if r = remote(malloryURL, tok.Secret, "list"); r.code == 0 || r.last().Error.Code != "unauthenticated" {
		t.Fatalf("revoked token list: exit %d %s", r.code, r.raw)
	}

	// A revoked device: the admin expires the owner's laptop on the control
	// server. Its identity no longer reaches the API, owner or not.
	gatewaytest.ExpireNode(t, ctx, control, alice)
	denied := func() bool {
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(rctx, "GET", "http://"+gwIP+":443/", nil)
		res, err := alice.Srv.HTTPClient().Do(req)
		if err != nil {
			return true
		}
		res.Body.Close()
		return false // TLS port answered plain HTTP: still reachable
	}
	for deadline := time.Now().Add(30 * time.Second); !denied(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("expired device still reaches the gateway")
		}
	}
	if r = remote(aliceURL, "", "list"); r.code == 0 {
		t.Fatalf("expired device list: exit %d %s", r.code, r.raw)
	}
}

// runSDKExamples runs the Python and TypeScript HTTPS examples against url
// when their runtimes are available (`make sdk-check` provisions the Python
// venv; Node runs the TypeScript source directly).
func runSDKExamples(t *testing.T, url, token, caFile string) {
	t.Helper()
	root, _ := filepath.Abs("../..")
	try := func(name string, cmd *exec.Cmd) {
		t.Helper()
		cmd.Env = append(os.Environ(), "DAVIT_API_URL="+url, "DAVIT_TOKEN="+token, "DAVIT_CA_FILE="+caFile,
			"PYTHONPATH="+filepath.Join(root, "sdk", "python", "src"))
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "deleted:") {
			t.Errorf("%s SDK example: %v\n%s", name, err, out)
		}
	}
	if py := filepath.Join(root, ".build", "py-venv", "bin", "python"); fileExists(py) {
		try("python", exec.Command(py, filepath.Join(root, "sdk", "python", "examples", "remote_https.py"), "sdk-py"))
	} else {
		t.Log("Python SDK example skipped: no .build/py-venv (make sdk-check creates it)")
	}
	if out, err := exec.Command("node", "-p", "process.features.typescript").Output(); err == nil && strings.TrimSpace(string(out)) != "undefined" {
		try("typescript", exec.Command("node", filepath.Join(root, "sdk", "typescript", "examples", "remote-https.ts"), "sdk-ts"))
	} else {
		t.Log("TypeScript SDK example skipped: no Node with TypeScript support (>= 22.18)")
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
