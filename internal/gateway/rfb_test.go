package gateway

import (
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"davit/internal/gateway/rfbtest"
)

// proxyTo runs ProxyRFB between a pipe (the viewer's end returned) and the
// fake server.
func proxyTo(t *testing.T, srv *rfbtest.Server, viewOnly bool) (net.Conn, chan error, *int) {
	t.Helper()
	port, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	guest, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	viewer, gw := net.Pipe()
	drops := new(int)
	done := make(chan error, 1)
	go func() { done <- ProxyRFB(gw, guest, viewOnly, func() { *drops++ }) }()
	t.Cleanup(func() { viewer.Close() })
	return viewer, done, drops
}

func itoa(n int) string { return strconv.Itoa(n) }

func waitCount(t *testing.T, what string, f func() int64, want int64) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); f() != want; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d, want %d", what, f(), want)
		}
	}
}

func TestProxyRFBRelaysAndDropsViewOnlyInput(t *testing.T) {
	for _, minor := range []int{8, 7, 3} {
		for _, viewOnly := range []bool{false, true} {
			srv := &rfbtest.Server{Name: "guest desktop", Width: 8, Height: 4, Minor: minor}
			viewer, _, drops := proxyTo(t, srv, viewOnly)
			c, err := rfbtest.Handshake(viewer)
			if err != nil {
				t.Fatalf("minor %d: handshake: %v", minor, err)
			}
			if c.Name != "guest desktop" || c.Width != 8 || c.Height != 4 {
				t.Fatalf("ServerInit %+v", c)
			}
			if err := c.RequestUpdate(); err != nil {
				t.Fatal(err)
			}
			px, err := c.ReadFrame()
			if err != nil || !slices.Equal(px[:3], []byte{0xff, 0x40, 0x10}) {
				t.Fatalf("frame %v %v", px, err)
			}
			c.Key(0x61)
			c.Pointer(1, 2, 1)
			c.CutText("secret paste")
			c.RequestUpdate() // after the input, in order: the input arrived first
			if _, err := c.ReadFrame(); err != nil {
				t.Fatal(err)
			}
			want := int64(2)
			if viewOnly {
				want = 0
			}
			if srv.Keys.Load() != want || srv.Pointers.Load() != want/2 || srv.CutTexts.Load() != want/2 {
				t.Fatalf("minor %d viewOnly %v: server got keys %d pointers %d cut %d", minor, viewOnly,
					srv.Keys.Load(), srv.Pointers.Load(), srv.CutTexts.Load())
			}
			if viewOnly && *drops != 4 {
				t.Fatalf("dropped %d messages, want 4", *drops)
			}
			viewer.Close()
		}
	}
}

func TestProxyRFBFailsClosed(t *testing.T) {
	// A guest VNC server demanding a password is refused, with a fix.
	srv := &rfbtest.Server{Width: 4, Height: 4, Auth: true}
	viewer, done, _ := proxyTo(t, srv, false)
	if _, err := rfbtest.Handshake(viewer); err == nil {
		t.Fatal("handshake through a password-protected guest server succeeded")
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "without a password") {
		t.Fatalf("proxy error %v", err)
	}

	// An unknown viewer message ends the session.
	srv = &rfbtest.Server{Width: 4, Height: 4}
	viewer, done, _ = proxyTo(t, srv, true)
	if _, err := rfbtest.Handshake(viewer); err != nil {
		t.Fatal(err)
	}
	viewer.Write([]byte{99})
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unknown viewer message") {
			t.Fatalf("proxy error %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unknown message did not end the session")
	}
	waitCount(t, "guest connections", func() int64 { return int64(srv.Conns()) }, 0)
}
