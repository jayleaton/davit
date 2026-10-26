package credproxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// echoUpstream answers every request with the request's headers and path in
// the body and an echoed key in a trailer, as a careless API's error page
// might.
func echoUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Trailer", "X-Echo")
		w.WriteHeader(400)
		io.WriteString(w, "path "+r.URL.Path+"; ")
		for k, vs := range r.Header {
			io.WriteString(w, k+": "+strings.Join(vs, ",")+"; ")
		}
		w.(http.Flusher).Flush()
		io.WriteString(w, "key again: "+r.Header.Get("X-Api-Key"))
		w.Header().Set("X-Echo", r.Header.Get("X-Api-Key"))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func send(t *testing.T, guest, path string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", guest+path, strings.NewReader("{}"))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Trailer
}

// TestProxyPlaceholderOnlyInAuthHeaders: the secret replaces the
// placeholder only in the headers APIs read keys from; a placeholder in any
// other header (which an upstream could echo back) is refused before
// anything is sent, and an echoed secret is scrubbed from the response
// body and trailers too (SEC-5).
func TestProxyPlaceholderOnlyInAuthHeaders(t *testing.T) {
	h, _, _ := newHost(t)
	up, n := echoUpstream(t)
	g := grantFor(t, h, up.URL)
	guest := guestFor(t, h, "vm_a")

	for _, hdr := range []string{"Content-Type", "User-Agent", "Referer", "X-Debug"} {
		code, body, _ := send(t, guest, "/v1/messages", map[string]string{hdr: g.Placeholder, "X-Api-Key": g.Placeholder})
		if code != http.StatusBadRequest || strings.Contains(body, realKey) {
			t.Errorf("placeholder in %s: %d %q, want 400 without the key", hdr, code, body)
		}
	}
	if got := n.Load(); got != 0 {
		t.Fatalf("upstream saw %d requests with a misplaced placeholder", got)
	}

	code, body, trailer := send(t, guest, "/v1/messages", map[string]string{"X-Api-Key": g.Placeholder})
	if code != 400 || n.Load() != 1 {
		t.Fatalf("auth-header request: %d %q", code, body)
	}
	if strings.Contains(body, realKey) || !strings.Contains(body, "[redacted]") {
		t.Errorf("echoed key reached the guest in the body: %q", body)
	}
	if strings.Contains(trailer.Get("X-Echo"), realKey) {
		t.Errorf("echoed key reached the guest in a trailer: %q", trailer.Get("X-Echo"))
	}
}

type chunks struct{ parts []string }

func (c *chunks) Read(p []byte) (int, error) {
	if len(c.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.parts[0])
	c.parts = c.parts[1:]
	return n, nil
}

func (c *chunks) Close() error { return nil }

// TestRedactingBodySplitSecret: a secret split across reads is still
// scrubbed, and text that cannot start the secret streams at once.
func TestRedactingBodySplitSecret(t *testing.T) {
	b := &redactingBody{r: &chunks{parts: []string{"event: one\n\n", "key sk-ant-RE", "AL-0123456789 end"}}, secret: []byte(realKey)}
	first := make([]byte, 64)
	n, _ := b.Read(first)
	if string(first[:n]) != "event: one\n\n" {
		t.Fatalf("first read %q: an SSE event was held back", first[:n])
	}
	rest, _ := io.ReadAll(b)
	if string(rest) != "key [redacted] end" {
		t.Fatalf("rest %q", rest)
	}
}

// TestProxyRefusesInternalUpstreams: by default the proxy does not carry a
// guest's requests to loopback, private, tailnet or metadata addresses,
// whatever the credential's baseUrl says or its name resolves to; an
// operator opts in with network.allowPrivateUpstreams (SEC-6).
func TestProxyRefusesInternalUpstreams(t *testing.T) {
	h, _, _ := newHost(t)
	h.AllowPrivateUpstreams = false
	up, n := echoUpstream(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(up.URL, "http://"))
	for _, base := range []string{up.URL, "http://localhost:" + port} {
		g := grantFor(t, h, base)
		code, body, _ := send(t, guestFor(t, h, "vm_a"), "/v1/messages", map[string]string{"X-Api-Key": g.Placeholder})
		if code != http.StatusBadGateway {
			t.Errorf("upstream %s: %d %q, want 502", base, code, body)
		}
		h.Revoke(context.Background(), g.ID, "revoked")
	}
	if got := n.Load(); got != 0 {
		t.Fatalf("an internal upstream was reached %d times", got)
	}
	h.AllowPrivateUpstreams = true
	g := grantFor(t, h, up.URL)
	if code, _, _ := send(t, guestFor(t, h, "vm_a"), "/v1/messages", map[string]string{"X-Api-Key": g.Placeholder}); n.Load() != 1 {
		t.Fatalf("allowed private upstream: %d, upstream saw %d", code, n.Load())
	}
}

// TestProxyRejectsDotSegments: the guest's path is appended to the grant's
// base path; dot segments (raw or percent-encoded) could climb out of it on
// an upstream that normalizes paths, so they are refused (SEC-7).
func TestProxyRejectsDotSegments(t *testing.T) {
	h, _, _ := newHost(t)
	up, n := echoUpstream(t)
	g := grantFor(t, h, up.URL)
	guest := guestFor(t, h, "vm_a")
	for _, path := range []string{"/../admin", "/v1/../../admin", "/v1/%2e%2e/admin", "/v1/%2E%2e%2Fadmin", "/v1/./x"} {
		c, err := net.Dial("tcp", strings.TrimPrefix(guest, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		// Raw request line: http.NewRequest would clean the path.
		io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: x\r\nX-Api-Key: "+g.Placeholder+"\r\nConnection: close\r\n\r\n")
		b, _ := io.ReadAll(c)
		c.Close()
		if !strings.HasPrefix(string(b), "HTTP/1.1 400") {
			t.Errorf("path %s: %.40q, want 400", path, b)
		}
	}
	if got := n.Load(); got != 0 {
		t.Fatalf("upstream saw %d dot-segment requests", got)
	}
}
