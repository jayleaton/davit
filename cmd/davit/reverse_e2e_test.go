//go:build linux

package main_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"davit/api"
	"davit/internal/cli"
	"davit/internal/domain"
)

// echoServer serves an echo on 127.0.0.1 (a "laptop" or host service): it
// copies until the client's half-close, then closes.
func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// freePort returns a 127.0.0.1 port nobody listens on right now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// guestRoundTrip is a guest process using the forward: on the fake runtime
// the guest's loopback is the host's. It sends msg, half-closes and returns
// what came back.
func guestRoundTrip(t *testing.T, port int, msg string) (string, error) {
	t.Helper()
	var c net.Conn
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if c, err = net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port)); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.WriteString(c, msg); err != nil {
		return "", err
	}
	c.(*net.TCPConn).CloseWrite()
	b, err := io.ReadAll(c)
	return string(b), err
}

// liveConn opens a guest connection through the forward and checks one echo,
// leaving it open.
func liveConn(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(20 * time.Second))
	io.WriteString(c, "ping")
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Fatalf("live connection echo: %q %v", b, err)
	}
	return c
}

// cut waits until c is closed by the far end.
func cut(t *testing.T, c net.Conn, why string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatalf("%s: the guest connection is still open", why)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("%s: the guest connection was not cut", why)
	}
}

func reverseForwards(t *testing.T, h *host, vm string) []api.ReverseForward {
	t.Helper()
	r := h.cli("forward", "--reverse", "--list", vm)
	if r.code != 0 {
		t.Fatalf("forward --reverse --list: exit %d %s", r.code, r.raw)
	}
	var out []api.ReverseForward
	for i, l := range r.lines {
		if l.Event == "item" {
			var fw api.ReverseForward
			json.Unmarshal(r.raw[i], &fw)
			out = append(out, fw)
		}
	}
	return out
}

// TestReverseForwardEndToEnd: `davit forward --reverse` on the real daemon
// (fake runtime, fake guest listener). Guest connections reach the CLI's
// local service with half-close intact; revoke, VM stop and Ctrl-C cut live
// connections; host services need the operator's allowlist.
func TestReverseForwardEndToEnd(t *testing.T) {
	laptop := echoServer(t)
	hostSvc := echoServer(t)
	h := newHost(t)
	h.guestAgent = true
	h.extraConfig = fmt.Sprintf(`"network":{"reverseHostServices":["127.0.0.1:%d"]}`, hostSvc)
	h.start(20*time.Millisecond, 0)
	h.waited(h.cli("new", "box"), domain.OpCreate)

	var caps api.HostCapabilities
	r := h.cli("capabilities")
	json.Unmarshal(r.raw[len(r.raw)-1], &caps)
	available := false
	for _, f := range caps.Features {
		available = available || f.Name == domain.FeatureReverseForward && f.Status == domain.FeatureAvailable
	}
	if !available {
		t.Fatalf("capabilities do not report reverseForward available: %s", r.raw)
	}

	// A laptop session: guest 127.0.0.1:g1 -> the CLI's 127.0.0.1:laptop.
	g1 := freePort(t)
	_, wait := h.background(`"reverse.listening"`, "--json", "forward", "--reverse", "box", fmt.Sprintf("%d:localhost:%d", g1, laptop))
	for i := range 3 {
		msg := strings.Repeat(fmt.Sprintf("hello-%d ", i), 5000) // > one WebSocket chunk
		if got, err := guestRoundTrip(t, g1, msg); err != nil || got != msg {
			t.Fatalf("round trip %d through the session: %d bytes back, err %v", i, len(got), err)
		}
	}
	fws := reverseForwards(t, h, "box")
	if len(fws) != 1 || fws[0].Target.Kind != api.ReverseTargetSession || fws[0].State != api.ReverseActive || fws[0].GuestPort != g1 {
		t.Fatalf("forwards: %+v", fws)
	}
	// One forward per guest port.
	if r := h.cli("forward", "--reverse", "--host-service", "box", fmt.Sprintf("%d:127.0.0.1:%d", g1, hostSvc)); r.code == 0 {
		t.Fatal("a second forward on the same guest port was accepted")
	}

	// Revoke cuts the live connection and ends the session.
	live := liveConn(t, g1)
	if r := h.cli("forward", "--reverse", "--revoke", fws[0].ID, "box"); r.code != 0 {
		t.Fatalf("revoke: exit %d %s", r.code, r.raw)
	}
	cut(t, live, "revoke")
	if code, out, _ := wait(); code == 0 || !strings.Contains(out, "revoked") {
		t.Fatalf("session after revoke: exit %d\n%s", code, out)
	}
	if _, err := guestRoundTrip(t, g1, "x"); err == nil {
		t.Fatal("the revoked forward still accepts guest connections")
	}
	if len(reverseForwards(t, h, "box")) != 0 {
		t.Fatal("revoked forward still listed")
	}

	// Host services: only the listed one.
	g2 := freePort(t)
	r = h.cli("forward", "--reverse", "--host-service", "box", fmt.Sprintf("%d:127.0.0.1:%d", g2, hostSvc))
	if r.code != 0 {
		t.Fatalf("host service forward: exit %d %s", r.code, r.raw)
	}
	if got, err := guestRoundTrip(t, g2, "to-host"); err != nil || got != "to-host" {
		t.Fatalf("host service round trip: %q %v", got, err)
	}
	h.expectError(h.cli("forward", "--reverse", "--host-service", "box", fmt.Sprintf("%d:127.0.0.1:%d", freePort(t), laptop)),
		cli.ExitCode("permission_denied"), domain.CodePermissionDenied)

	// Ctrl-C ends the session cleanly and revokes its forward.
	g3 := freePort(t)
	session, wait := h.background(`"reverse.listening"`, "--json", "forward", "--reverse", "box", fmt.Sprintf("%d:%d", g3, laptop))
	live = liveConn(t, g3)
	session.Process.Signal(syscall.SIGINT)
	cut(t, live, "Ctrl-C")
	if code, out, _ := wait(); code != 0 || !strings.Contains(out, `"event":"result"`) {
		t.Fatalf("session after Ctrl-C: exit %d\n%s", code, out)
	}
	for _, fw := range reverseForwards(t, h, "box") {
		if fw.GuestPort == g3 {
			t.Fatal("Ctrl-C left the session forward behind")
		}
	}

	// Stopping the VM cuts live connections of every forward and ends the
	// session.
	g4 := freePort(t)
	_, wait = h.background(`"reverse.listening"`, "--json", "forward", "--reverse", "box", fmt.Sprintf("%d:127.0.0.1:%d", g4, laptop))
	live = liveConn(t, g4)
	liveHost := liveConn(t, g2)
	h.waited(h.cli("stop", "box"), domain.OpStop)
	cut(t, live, "stop (session)")
	cut(t, liveHost, "stop (host service)")
	if code, out, _ := wait(); code == 0 || !strings.Contains(out, "vm s") {
		t.Fatalf("session after stop: exit %d\n%s", code, out)
	}
	if fws := reverseForwards(t, h, "box"); len(fws) != 0 {
		t.Fatalf("forwards left after stop: %+v", fws)
	}
}
