//go:build linux

package main_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/config"
	"davit/internal/daemon"
	"davit/internal/domain"
	"davit/internal/gateway"
	"davit/internal/gateway/gatewaytest"
	"davit/internal/wsock"
)

// guestApp is the "app in the VM": on the fake runtime a guest's loopback is
// the host's, so it listens on the host's 127.0.0.1. / reports what reached
// it, /stream writes in timed chunks, /ws is a WebSocket echo.
func guestApp(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"host": r.Host, "origin": r.Header.Get("Origin"), "path": r.URL.RequestURI(),
			"xfProto": r.Header.Get("X-Forwarded-Proto"), "xfHost": r.Header.Get("X-Forwarded-Host"),
			"xfFor": r.Header.Get("X-Forwarded-For"), "authorization": r.Header.Get("Authorization"),
			"login": r.Header.Get("Tailscale-User-Login"),
		})
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 3 {
			fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(300 * time.Millisecond)
		}
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ws, err := wsock.Upgrade(w, r)
		if err != nil {
			return
		}
		defer ws.Close(wsock.CloseNormal, "")
		for {
			typ, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if ws.WriteMessage(typ, append([]byte("echo:"), data...)) != nil {
				return
			}
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// appClient reaches app endpoints as one tailnet peer, over TLS to the
// gateway's address with the gateway's name (as a browser on that peer).
type appClient struct {
	t     *testing.T
	p     gatewaytest.Peer
	gwIP  string
	fqdn  string
	roots *x509.CertPool
	http  *http.Client
}

func newAppClient(t *testing.T, p gatewaytest.Peer, gwIP, fqdn, caFile string) *appClient {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	tr := &http.Transport{
		DialContext:     func(ctx context.Context, _, addr string) (net.Conn, error) { return p.Srv.Dial(ctx, "tcp", addr) },
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: fqdn},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &appClient{t: t, p: p, gwIP: gwIP, fqdn: fqdn, roots: roots, http: &http.Client{Transport: tr, Timeout: 20 * time.Second}}
}

// get requests path on the endpoint's gateway port; hdr adds headers.
func (c *appClient) get(port int, path string, hdr map[string]string) (int, string, error) {
	req, _ := http.NewRequest("GET", "https://"+net.JoinHostPort(c.gwIP, strconv.Itoa(port))+path, nil)
	req.Host = c.fqdn + ":" + strconv.Itoa(port)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b), nil
}

// websocket opens the app's /ws through the endpoint.
func (c *appClient) websocket(port int, hdr http.Header) (*wsock.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, err := c.p.Srv.Dial(ctx, "tcp", net.JoinHostPort(c.gwIP, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, &tls.Config{RootCAs: c.roots, ServerName: c.fqdn, NextProtos: []string{"http/1.1"}})
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	ws, resp, err := wsock.Client(ctx, tc, c.fqdn+":"+strconv.Itoa(port), "/ws", hdr)
	if err != nil {
		tc.Close()
		if resp != nil {
			return nil, fmt.Errorf("%w (HTTP %d)", err, resp.StatusCode)
		}
		return nil, err
	}
	return ws, nil
}

type appHost struct {
	h        *host
	fqdn     string
	gwIP     string
	caFile   string
	alice    gatewaytest.Peer
	mallory  gatewaytest.Peer
	cfgPath  string
	stateDir string
	control  string
	certFile string
	keyFile  string
}

// startAppHost runs the real daemon (fake runtime with guest agents) with
// the tailnet gateway on the in-process test tailnet and an operator TLS
// certificate for its name; alice owns it, mallory is another tailnet user.
func startAppHost(t *testing.T, ctx context.Context) *appHost {
	t.Helper()
	controlURL := gatewaytest.StartControl(t)
	alice := gatewaytest.StartPeer(t, ctx, controlURL, "alice-laptop")
	mallory := gatewaytest.StartPeer(t, ctx, controlURL, "ci-runner")
	h := newHost(t)
	const gwName = "davit-g11"
	fqdn := gwName + ".tail-scale.ts.net"
	caFile, certFile, keyFile := writeTestPKI(t, h.dir, fqdn)
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
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if s, err := gateway.ReadStatus(filepath.Join(cfg.TailnetStateDir(), gateway.StatusFile)); err == nil && s.State == gateway.EnrollUp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway never reported up")
		}
	}
	return &appHost{h: h, fqdn: fqdn, gwIP: gwIP, caFile: caFile, alice: alice, mallory: mallory, cfgPath: cfgPath,
		stateDir: cfg.TailnetStateDir(), control: controlURL, certFile: certFile, keyFile: keyFile}
}

func endpointOf(t *testing.T, r run) api.AppEndpoint {
	t.Helper()
	var ep api.AppEndpoint
	if r.code != 0 || json.Unmarshal(r.raw[len(r.raw)-1], &ep) != nil || ep.ID == "" {
		t.Fatalf("endpoint: exit %d %s", r.code, r.raw)
	}
	return ep
}

// eventually retries f until it holds (the gateway's node may still be
// settling, a listener opening or closing).
func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !f(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out", what)
		}
	}
}

// TestAppEndpointsOverTailnet: `davit expose` gives an app in the VM a
// stable tailnet HTTPS URL on the shared gateway. HTTP (Host, Origin and
// X-Forwarded-* as a browser expects), streaming and WebSocket pass through;
// only the owner or a token with exec on the VM gets in (the bearer never
// reaches the app); hide, stop and delete cut live connections, and the
// same URL serves again after show and resume.
func TestAppEndpointsOverTailnet(t *testing.T) {
	if testing.Short() {
		t.Skip("starts in-process tailnet nodes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a := startAppHost(t, ctx)
	h := a.h
	appPort := guestApp(t)
	h.waited(h.cli("new", "box"), domain.OpCreate)

	ep := endpointOf(t, h.cli("expose", "box", strconv.Itoa(appPort), "--name", "web"))
	wantURL := "https://" + a.fqdn + ":" + strconv.Itoa(ep.GatewayPort) + "/"
	if ep.URL != wantURL || ep.State != api.AppEndpointActive || ep.Port != appPort || ep.Name != "web" {
		t.Fatalf("expose: %+v, want URL %s", ep, wantURL)
	}
	if r := h.cli("expose", "box", strconv.Itoa(appPort)); r.code == 0 {
		t.Fatal("a second endpoint on the same guest port was accepted")
	}

	alice := newAppClient(t, a.alice, a.gwIP, a.fqdn, a.caFile)
	mallory := newAppClient(t, a.mallory, a.gwIP, a.fqdn, a.caFile)
	var code int
	var body string
	eventually(t, "first request", func() bool {
		var err error
		code, body, err = alice.get(ep.GatewayPort, "/hello?x=1", map[string]string{"Origin": strings.TrimSuffix(wantURL, "/"),
			"X-Forwarded-For": "6.6.6.6", "Tailscale-User-Login": "forged@example.com"})
		return err == nil
	})
	var seen map[string]string
	json.Unmarshal([]byte(body), &seen)
	if code != 200 || seen["host"] != a.fqdn+":"+strconv.Itoa(ep.GatewayPort) || seen["origin"] != strings.TrimSuffix(wantURL, "/") ||
		seen["path"] != "/hello?x=1" || seen["xfProto"] != "https" || seen["xfHost"] != seen["host"] ||
		strings.Contains(seen["xfFor"], "6.6.6.6") || seen["login"] != "" {
		t.Fatalf("owner request (identity not forwarded, forged header dropped): %d %s", code, body)
	}
	// --forward-identity: the app gets the caller's real tailnet login.
	whoEP := endpointOf(t, h.cli("expose", "box", strconv.Itoa(guestApp(t)), "--name", "who", "--forward-identity"))
	if !whoEP.ForwardIdentity || ep.ForwardIdentity {
		t.Fatalf("forwardIdentity: %+v / %+v", whoEP, ep)
	}
	eventually(t, "identity endpoint", func() bool {
		c, b, err := alice.get(whoEP.GatewayPort, "/", map[string]string{"Tailscale-User-Login": "forged@example.com"})
		if err != nil || c != 200 {
			return false
		}
		json.Unmarshal([]byte(b), &seen)
		return true
	})
	if seen["login"] != a.alice.LoginName {
		t.Fatalf("forwarded identity: %q, want %q", seen["login"], a.alice.LoginName)
	}

	// Streaming: the first event arrives before the app finishes.
	req, _ := http.NewRequest("GET", "https://"+net.JoinHostPort(a.gwIP, strconv.Itoa(ep.GatewayPort))+"/stream", nil)
	t0 := time.Now()
	res, err := alice.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, len("data: 0\n\n"))
	if _, err := io.ReadFull(res.Body, first); err != nil || string(first) != "data: 0\n\n" || time.Since(t0) > 550*time.Millisecond {
		t.Fatalf("stream: first event %q after %v (%v)", first, time.Since(t0), err)
	}
	rest, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(rest), "data: 2") {
		t.Fatalf("stream rest %q", rest)
	}

	// WebSocket echo.
	ws, err := alice.websocket(ep.GatewayPort, http.Header{"Origin": {strings.TrimSuffix(wantURL, "/")}})
	if err != nil {
		t.Fatalf("websocket: %v", err)
	}
	ws.WriteMessage(wsock.TextMessage, []byte("hi"))
	if _, msg, err := ws.ReadMessage(); err != nil || string(msg) != "echo:hi" {
		t.Fatalf("websocket echo %q %v", msg, err)
	}

	// Non-owner: refused, then let in by a token with exec on the VM; the
	// token never reaches the app. view is not enough; revoked is 401.
	if code, body, _ := mallory.get(ep.GatewayPort, "/", nil); code != http.StatusForbidden || !strings.Contains(body, "permission_denied") {
		t.Fatalf("non-owner: %d %s", code, body)
	}
	if _, err := mallory.websocket(ep.GatewayPort, nil); err == nil {
		t.Fatal("non-owner opened the WebSocket")
	}
	token := func(action string) api.CreatedToken {
		r := h.cli("token", "create", "app-"+action, "--action", action, "--vm", "box", "--ttl", "1h")
		var tok api.CreatedToken
		json.Unmarshal(r.raw[len(r.raw)-1], &tok)
		if r.code != 0 || tok.Secret == "" {
			t.Fatalf("token create: %s", r.raw)
		}
		return tok
	}
	execTok, viewTok := token("exec"), token("view")
	bearer := func(tok api.CreatedToken) map[string]string {
		return map[string]string{"Authorization": "Bearer " + tok.Secret}
	}
	code, body, _ = mallory.get(ep.GatewayPort, "/", bearer(execTok))
	json.Unmarshal([]byte(body), &seen)
	if code != 200 || seen["authorization"] != "" || seen["login"] != "" {
		t.Fatalf("exec token: %d %s", code, body)
	}
	if code, _, _ := mallory.get(ep.GatewayPort, "/", bearer(viewTok)); code != http.StatusForbidden {
		t.Fatalf("view token: HTTP %d, want 403", code)
	}
	// Managing endpoints needs admin: the exec token cannot expose ports
	// over the tailnet API.
	apiReq, _ := http.NewRequest("POST", "https://"+net.JoinHostPort(a.gwIP, "443")+"/api/v1/vms/box/app-endpoints",
		strings.NewReader(`{"port":9}`))
	apiReq.Header.Set("Content-Type", "application/json")
	apiReq.Header.Set("Authorization", "Bearer "+execTok.Secret)
	if res, err := mallory.http.Do(apiReq); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("exec token createAppEndpoint: %v %v", res, err)
	} else {
		res.Body.Close()
	}
	if r := h.cli("token", "revoke", execTok.ID, "--yes"); r.code != 0 {
		t.Fatalf("revoke: %s", r.raw)
	}
	if code, _, _ := mallory.get(ep.GatewayPort, "/", bearer(execTok)); code != http.StatusUnauthorized {
		t.Fatalf("revoked token: HTTP %d, want 401", code)
	}

	// Rename keeps the URL; hide closes the port and cuts the live
	// WebSocket; show serves the same URL again.
	if ep2 := endpointOf(t, h.cli("endpoints", "rename", "box", "web", "site")); ep2.URL != ep.URL || ep2.Name != "site" {
		t.Fatalf("rename: %+v", ep2)
	}
	if ep2 := endpointOf(t, h.cli("endpoints", "hide", "box", "site")); ep2.State != api.AppEndpointHidden {
		t.Fatalf("hide: %+v", ep2)
	}
	ws.NetConn().SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("hide left the WebSocket open")
	}
	eventually(t, "hidden endpoint refused", func() bool { _, _, err := alice.get(ep.GatewayPort, "/", nil); return err != nil })
	endpointOf(t, h.cli("endpoints", "show", "box", "site"))
	eventually(t, "shown endpoint serves", func() bool { c, _, err := alice.get(ep.GatewayPort, "/", nil); return err == nil && c == 200 })

	// Stop cuts live connections and answers 503 at the same URL; after
	// resume (a new generation) it serves again.
	ws, err = alice.websocket(ep.GatewayPort, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.waited(h.cli("stop", "box"), domain.OpStop)
	ws.NetConn().SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("stop left the WebSocket open")
	}
	if code, body, _ := alice.get(ep.GatewayPort, "/", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("stopped VM: %d %s", code, body)
	}
	r := h.cli("endpoints", "box")
	if r.code != 0 || !strings.Contains(string(r.raw[0]), `"state":"vm_not_running"`) {
		t.Fatalf("endpoints while stopped: %s", r.raw)
	}
	h.waited(h.cli("resume", "box"), domain.OpResume)
	eventually(t, "serves after resume", func() bool { c, _, err := alice.get(ep.GatewayPort, "/", nil); return err == nil && c == 200 })

	// The URL survives a daemon restart (the store); delete frees it.
	if b, err := os.ReadFile(filepath.Join(a.stateDir, "app-endpoints.json")); err != nil || !strings.Contains(string(b), ep.ID) {
		t.Fatalf("endpoint store: %s %v", b, err)
	}
	if r := h.cli("endpoints", "delete", "box", "site"); r.code != 0 {
		t.Fatalf("delete: %s", r.raw)
	}
	eventually(t, "deleted endpoint refused", func() bool { _, _, err := alice.get(ep.GatewayPort, "/", nil); return err != nil })
	if r := h.cli("endpoints", "box"); r.code != 0 || len(r.lines) != 2 || !strings.Contains(string(r.raw[0]), `"name":"who"`) {
		t.Fatalf("endpoints after delete: %s", r.raw)
	}
}

// TestAppEndpointOverhead measures what the endpoint adds per request on the
// simulated tailnet: the same app reached through a plain TLS-terminating
// TCP relay on another tailnet node (baseline: tailnet + TLS, no gateway
// logic) versus through the app endpoint (WhoIs, authorization, proxy and
// the guest-agent tunnel). Sequential keep-alive GETs. Opt-in:
// DAVIT_BENCH_APPS=N (samples).
func TestAppEndpointOverhead(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("DAVIT_BENCH_APPS"))
	if n <= 0 {
		t.Skip("set DAVIT_BENCH_APPS=N to measure app endpoint overhead")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	a := startAppHost(t, ctx)
	appPort := guestApp(t)
	a.h.waited(a.h.cli("new", "box"), domain.OpCreate)
	ep := endpointOf(t, a.h.cli("expose", "box", strconv.Itoa(appPort)))

	// Baseline: a tailnet node terminating TLS with the same certificate and
	// splicing to the app: the tailnet and TLS without the gateway's logic.
	relay := gatewaytest.StartPeer(t, ctx, a.control, "baseline-relay")
	cert, err := tls.LoadX509KeyPair(a.certFile, a.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	rawLn, err := relay.Srv.Listen("tcp", ":443")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(rawLn, &tls.Config{Certificates: []tls.Certificate{cert}})
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(appPort))
				if err != nil {
					return
				}
				defer up.Close()
				go io.Copy(up, c)
				io.Copy(c, up)
			}()
		}
	}()
	relayIP := gatewaytest.PeerAddr(t, ctx, a.alice, "baseline-relay").String()
	viaGateway := newAppClient(t, a.alice, a.gwIP, a.fqdn, a.caFile)
	viaRelay := newAppClient(t, a.alice, relayIP, a.fqdn, a.caFile)
	measure := func(c *appClient, port int) []time.Duration {
		eventually(t, "warm up", func() bool { code, _, err := c.get(port, "/", nil); return err == nil && code == 200 })
		d := make([]time.Duration, 0, n)
		for range n {
			t0 := time.Now()
			if code, _, err := c.get(port, "/", nil); err != nil || code != 200 {
				t.Fatalf("request: %d %v", code, err)
			}
			d = append(d, time.Since(t0))
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d
	}
	pct := func(d []time.Duration, p int) time.Duration { return d[(len(d)*p+99)/100-1] }
	us := func(d time.Duration) string { return strconv.FormatInt(d.Microseconds(), 10) + "us" }
	// Interleave rounds so load drift hits both paths alike.
	var base, app []time.Duration
	for range 2 {
		base = append(base, measure(viaRelay, 443)...)
		app = append(app, measure(viaGateway, ep.GatewayPort)...)
	}
	sort.Slice(base, func(i, j int) bool { return base[i] < base[j] })
	sort.Slice(app, func(i, j int) bool { return app[i] < app[j] })
	t.Logf("samples per path: %d (sequential keep-alive GETs, 2 interleaved rounds)", len(app))
	t.Logf("baseline tailnet+TLS relay: p50=%s p95=%s", us(pct(base, 50)), us(pct(base, 95)))
	t.Logf("app endpoint:               p50=%s p95=%s", us(pct(app, 50)), us(pct(app, 95)))
	t.Logf("endpoint overhead:          p50=%s p95=%s", us(pct(app, 50)-pct(base, 50)), us(pct(app, 95)-pct(base, 95)))
}
