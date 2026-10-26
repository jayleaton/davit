//go:build linux

package main_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/gateway/rfbtest"
	"davit/internal/wsock"
)

// desktopStream opens the desktop's RFB WebSocket as a browser would
// (subprotocol "binary") and returns it as a byte stream.
func (c *appClient) desktopStream(port int, bearer string) (*wsock.Stream, error) {
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
	hdr := http.Header{"Sec-WebSocket-Protocol": {"binary"}}
	if bearer != "" {
		hdr.Set("Authorization", "Bearer "+bearer)
	}
	ws, resp, err := wsock.Client(ctx, tc, c.fqdn+":"+strconv.Itoa(port), "/websockify", hdr)
	if err != nil {
		tc.Close()
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			return nil, &httpError{resp.StatusCode, string(b)}
		}
		return nil, err
	}
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "binary" {
		ws.Close(wsock.CloseNormal, "")
		return nil, &httpError{0, "subprotocol " + got + " not echoed"}
	}
	return wsock.NewStream(ws, 10*time.Second), nil
}

type httpError struct {
	code int
	body string
}

func (e *httpError) Error() string { return strconv.Itoa(e.code) + " " + e.body }

// firstFrame handshakes and times the first full-screen update.
func firstFrame(t *testing.T, st *wsock.Stream, t0 time.Time) (*rfbtest.Client, time.Duration) {
	t.Helper()
	c, err := rfbtest.Handshake(st)
	if err != nil {
		t.Fatalf("RFB handshake through the gateway: %v", err)
	}
	if err := c.RequestUpdate(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadFrame(); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	return c, time.Since(t0)
}

// TestDesktopOverTailnet: `davit desktop open` serves the VM's VNC desktop
// (a fake RFB server behind the fake guest) on the shared gateway. The
// viewer page and the RFB stream need the tailnet identity; the owner and an
// exec token send input, a view token is view-only (its input dropped at the
// gateway), others are refused; close and stop cut live streams.
func TestDesktopOverTailnet(t *testing.T) {
	if testing.Short() {
		t.Skip("starts in-process tailnet nodes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a := startAppHost(t, ctx)
	h := a.h
	vnc := &rfbtest.Server{Name: "box desktop", Width: 64, Height: 48}
	vncPort, err := vnc.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vnc.Close)
	h.waited(h.cli("new", "box"), domain.OpCreate)

	r := h.cli("desktop", "open", "box", "--port", strconv.Itoa(vncPort))
	var ep api.AppEndpoint
	json.Unmarshal(r.raw[len(r.raw)-1], &ep)
	if r.code != 0 || ep.Kind != api.AppEndpointDesktop || ep.Port != vncPort || ep.URL == "" || ep.State != api.AppEndpointActive {
		t.Fatalf("desktop open: exit %d %s", r.code, r.raw)
	}
	if r := h.cli("desktop", "url", "box"); r.code != 0 || !strings.Contains(string(r.raw[0]), ep.URL) {
		t.Fatalf("desktop url: %s", r.raw)
	}
	if r := h.cli("desktop", "open", "box", "--port", strconv.Itoa(vncPort)); r.code != 0 {
		t.Fatalf("second open is not idempotent: %s", r.raw)
	}

	alice := newAppClient(t, a.alice, a.gwIP, a.fqdn, a.caFile)
	mallory := newAppClient(t, a.mallory, a.gwIP, a.fqdn, a.caFile)
	var code int
	var page string
	eventually(t, "viewer page", func() bool {
		var err error
		code, page, err = alice.get(ep.GatewayPort, "/", nil)
		return err == nil
	})
	if code != 200 || !strings.Contains(page, `data-view-only="false"`) || !strings.Contains(page, "websockify") {
		t.Fatalf("owner viewer page: %d %.200s", code, page)
	}

	// The owner: frames and input.
	t0 := time.Now()
	st, err := alice.desktopStream(ep.GatewayPort, "")
	if err != nil {
		t.Fatalf("owner stream: %v", err)
	}
	c, latency := firstFrame(t, st, t0)
	t.Logf("first frame through the gateway (simulated tailnet, %dx%d raw): %v", c.Width, c.Height, latency)
	if c.Name != "box desktop" {
		t.Fatalf("desktop name %q", c.Name)
	}
	c.Key(0x61)
	c.Pointer(3, 4, 1)
	c.RequestUpdate()
	c.ReadFrame()
	if vnc.Keys.Load() != 2 || vnc.Pointers.Load() != 1 {
		t.Fatalf("owner input: keys %d pointers %d", vnc.Keys.Load(), vnc.Pointers.Load())
	}

	// Another tailnet user: refused; with a view token, view-only (its
	// input never reaches the guest); with an exec token, input.
	if code, _, _ := mallory.get(ep.GatewayPort, "/", nil); code != http.StatusForbidden {
		t.Fatalf("non-owner viewer page: HTTP %d", code)
	}
	if _, err := mallory.desktopStream(ep.GatewayPort, ""); err == nil {
		t.Fatal("non-owner opened the desktop stream")
	}
	token := func(action string) string {
		r := h.cli("token", "create", "desk-"+action, "--action", action, "--vm", "box", "--ttl", "1h")
		var tok api.CreatedToken
		json.Unmarshal(r.raw[len(r.raw)-1], &tok)
		if r.code != 0 {
			t.Fatalf("token: %s", r.raw)
		}
		return tok.Secret
	}
	viewTok, execTok := token("view"), token("exec")
	if code, page, _ := mallory.get(ep.GatewayPort, "/", map[string]string{"Authorization": "Bearer " + viewTok}); code != 200 ||
		!strings.Contains(page, `data-view-only="true"`) {
		t.Fatalf("view token page: %d", code)
	}
	vst, err := mallory.desktopStream(ep.GatewayPort, viewTok)
	if err != nil {
		t.Fatalf("view-only stream: %v", err)
	}
	vc, _ := firstFrame(t, vst, time.Now())
	vc.Key(0x62)
	vc.Pointer(5, 6, 1)
	vc.CutText("pasted into the guest?")
	vc.RequestUpdate()
	if _, err := vc.ReadFrame(); err != nil {
		t.Fatalf("view-only frames stopped: %v", err)
	}
	if vnc.Keys.Load() != 2 || vnc.Pointers.Load() != 1 || vnc.CutTexts.Load() != 0 {
		t.Fatalf("view-only input reached the guest: keys %d pointers %d cut %d", vnc.Keys.Load(), vnc.Pointers.Load(), vnc.CutTexts.Load())
	}
	est, err := mallory.desktopStream(ep.GatewayPort, execTok)
	if err != nil {
		t.Fatalf("exec token stream: %v", err)
	}
	ec, _ := firstFrame(t, est, time.Now())
	ec.Key(0x63)
	ec.RequestUpdate()
	ec.ReadFrame()
	if vnc.Keys.Load() != 4 {
		t.Fatalf("exec token input: keys %d, want 4", vnc.Keys.Load())
	}

	// A G13 member follows its role: a viewer share is view-only, and
	// unsharing cuts its viewer.
	if r := h.cli("members", "add", a.mallory.LoginName, "--role", "viewer"); r.code != 0 {
		t.Fatalf("members add: %s", r.raw)
	}
	if code, _, _ := mallory.get(ep.GatewayPort, "/", nil); code != http.StatusForbidden {
		t.Fatalf("member without a share: HTTP %d", code)
	}
	if r := h.cli("share", "box", a.mallory.LoginName, "--role", "viewer"); r.code != 0 {
		t.Fatalf("share: %s", r.raw)
	}
	if code, page, _ := mallory.get(ep.GatewayPort, "/", nil); code != 200 || !strings.Contains(page, `data-view-only="true"`) {
		t.Fatalf("viewer member page: %d", code)
	}
	mst, err := mallory.desktopStream(ep.GatewayPort, "")
	if err != nil {
		t.Fatalf("viewer member stream: %v", err)
	}
	mc, _ := firstFrame(t, mst, time.Now())
	keys := vnc.Keys.Load()
	mc.Key(0x64)
	mc.RequestUpdate()
	if _, err := mc.ReadFrame(); err != nil || vnc.Keys.Load() != keys {
		t.Fatalf("viewer member input reached the guest (keys %d -> %d, %v)", keys, vnc.Keys.Load(), err)
	}
	if r := h.cli("unshare", "box", a.mallory.LoginName); r.code != 0 {
		t.Fatalf("unshare: %s", r.raw)
	}
	mst.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(mst); err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("unshare did not cut the member's desktop viewer")
		}
	}

	// Close cuts every viewer and frees the port.
	if r := h.cli("desktop", "close", "box"); r.code != 0 {
		t.Fatalf("desktop close: %s", r.raw)
	}
	for name, s := range map[string]*wsock.Stream{"owner": st, "view-only": vst, "exec": est} {
		s.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadAll(s); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatalf("close did not cut the %s stream", name)
			}
		}
	}
	eventually(t, "closed desktop refused", func() bool { _, err := alice.desktopStream(ep.GatewayPort, ""); return err != nil })
	if r := h.cli("desktop", "url", "box"); r.code == 0 {
		t.Fatal("closed desktop still has a URL")
	}

	// Stop cuts the stream; the URL answers 503 meanwhile.
	r = h.cli("desktop", "open", "box", "--port", strconv.Itoa(vncPort))
	json.Unmarshal(r.raw[len(r.raw)-1], &ep)
	var st2 *wsock.Stream
	eventually(t, "reopened desktop", func() bool { var err error; st2, err = alice.desktopStream(ep.GatewayPort, ""); return err == nil })
	firstFrame(t, st2, time.Now())
	h.waited(h.cli("stop", "box"), domain.OpStop)
	st2.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadAll(st2); err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("stop did not cut the desktop stream")
		}
	}
	if code, _, _ := alice.get(ep.GatewayPort, "/", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("stopped VM viewer page: HTTP %d", code)
	}
	eventually(t, "guest VNC connections closed", func() bool { return vnc.Conns() == 0 })
}

// TestDesktopFirstFrameLatency measures, on the simulated tailnet, the time
// from opening the viewer WebSocket to the first full frame through the
// gateway (TLS, WhoIs, authorization, both RFB handshakes, the guest-agent
// tunnel) against the same handshake straight to the fake VNC server on
// loopback. Simulated: an in-process tailnet and a fake 64x48 raw
// framebuffer, not a real VM or display. Opt-in: DAVIT_BENCH_DESKTOP=N.
func TestDesktopFirstFrameLatency(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("DAVIT_BENCH_DESKTOP"))
	if n <= 0 {
		t.Skip("set DAVIT_BENCH_DESKTOP=N to measure first-frame latency")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	a := startAppHost(t, ctx)
	vnc := &rfbtest.Server{Name: "bench", Width: 64, Height: 48}
	vncPort, err := vnc.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vnc.Close)
	a.h.waited(a.h.cli("new", "box"), domain.OpCreate)
	r := a.h.cli("desktop", "open", "box", "--port", strconv.Itoa(vncPort))
	var ep api.AppEndpoint
	json.Unmarshal(r.raw[len(r.raw)-1], &ep)
	alice := newAppClient(t, a.alice, a.gwIP, a.fqdn, a.caFile)
	eventually(t, "warm up", func() bool {
		st, err := alice.desktopStream(ep.GatewayPort, "")
		if err == nil {
			st.Close()
		}
		return err == nil
	})
	pct := func(d []time.Duration, p int) time.Duration { return d[(len(d)*p+99)/100-1] }
	var via, direct []time.Duration
	for range n {
		t0 := time.Now()
		st, err := alice.desktopStream(ep.GatewayPort, "")
		if err != nil {
			t.Fatal(err)
		}
		_, d := firstFrame(t, st, t0)
		via = append(via, d)
		st.Close()

		t0 = time.Now()
		conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(vncPort))
		if err != nil {
			t.Fatal(err)
		}
		// The fake server is RFB 3.8 None, as the gateway presents itself.
		c, err := rfbtest.Handshake(conn)
		if err != nil {
			t.Fatal(err)
		}
		c.RequestUpdate()
		if _, err := c.ReadFrame(); err != nil {
			t.Fatal(err)
		}
		direct = append(direct, time.Since(t0))
		conn.Close()
	}
	sort.Slice(via, func(i, j int) bool { return via[i] < via[j] })
	sort.Slice(direct, func(i, j int) bool { return direct[i] < direct[j] })
	t.Logf("SIMULATED first-frame latency, %d samples each (new WebSocket per sample, 64x48 raw frame):", n)
	t.Logf("  direct to the fake VNC server on loopback: p50=%v p95=%v", pct(direct, 50), pct(direct, 95))
	t.Logf("  through the gateway on the simulated tailnet: p50=%v p95=%v", pct(via, 50), pct(via, 95))
}
