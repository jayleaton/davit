package httpx

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestHardenBoundsConnections: oversized headers are refused and a client
// that sends headers too slowly (slowloris) or idles is disconnected
// (SEC-11).
func TestHardenBoundsConnections(t *testing.T) {
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })}
	Harden(srv)
	if srv.ReadHeaderTimeout == 0 || srv.IdleTimeout == 0 || srv.MaxHeaderBytes == 0 || srv.MaxHeaderBytes > 1<<17 {
		t.Fatalf("limits not set: %+v", srv)
	}
	srv.ReadHeaderTimeout, srv.IdleTimeout = 200*time.Millisecond, 200*time.Millisecond // fast test
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	c, _ := net.Dial("tcp", ln.Addr().String())
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nX-Big: " + strings.Repeat("a", 1<<18) + "\r\n\r\n"))
	line, _ := bufio.NewReader(c).ReadString('\n')
	c.Close()
	if !strings.Contains(line, "431") {
		t.Errorf("oversized header: %q, want 431", line)
	}

	for name, req := range map[string]string{"slow headers": "GET / HTTP/1.1\r\nHost: x\r\n", "idle keep-alive": "GET / HTTP/1.1\r\nHost: x\r\n\r\n"} {
		c, _ := net.Dial("tcp", ln.Addr().String())
		c.Write([]byte(req))
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(c)
		var err error
		for err == nil {
			_, err = r.ReadString('\n')
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Errorf("%s: the server kept the connection open", name)
		}
		c.Close()
	}
}
