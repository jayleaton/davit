package httpapi

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"testing"

	"davit/api"
	"davit/internal/domain"
)

// TestRunAndPushRoutes: without a guest transport the run and push
// endpoints refuse with unsupported_capability; pushTree checks its query
// first and refuses mode=mirror as not implemented.
func TestRunAndPushRoutes(t *testing.T) {
	h := newServer(t, fixedAuth{p: "local:1"})
	key := map[string]string{api.HeaderIdempotencyKey: "k1"}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/v1/runs", `{"argv":["true"]}`},
		{"GET", "/api/v1/runs/run_x", ""},
		{"POST", "/api/v1/runs/run_x/start", ""},
		{"DELETE", "/api/v1/runs/run_x", ""},
		{"PUT", "/api/v1/vms/a/tree?path=/work", ""},
	} {
		rec, er := call(t, h, c.method, c.path, c.body, key)
		if rec.Code != http.StatusUnprocessableEntity || er.Error.Code != domain.CodeUnsupportedCapability {
			t.Errorf("%s %s: got %d %+v", c.method, c.path, rec.Code, er.Error)
		}
	}

	rec, er := call(t, h, "PUT", "/api/v1/vms/a/tree?path=/work&mode=mirror", "", nil)
	if rec.Code != http.StatusNotImplemented || er.Error.Details["feature"] != domain.FeaturePush ||
		er.Error.Details["operation"] != "pushTree.mode=mirror" {
		t.Errorf("mirror: got %d %+v", rec.Code, er.Error)
	}
	for _, q := range []string{"", "?path=work", "?path=/a/../b", "?path=/a/", "?path=/a&mode=sync"} {
		rec, er := call(t, h, "PUT", "/api/v1/vms/a/tree"+q, "", nil)
		if rec.Code != http.StatusBadRequest || er.Error.Code != domain.CodeInvalidArgument {
			t.Errorf("pushTree%s: got %d %+v", q, rec.Code, er.Error)
		}
	}
}

func TestCountEntries(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "d/a", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
		{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "d/a"},
	} {
		tw.WriteHeader(h)
		if h.Size > 0 {
			tw.Write([]byte("abc"))
		}
	}
	tw.Close()
	want := buf.Len()
	r, files := countEntries(bytes.NewReader(buf.Bytes()))
	n, err := io.Copy(io.Discard, r)
	if err != nil || int(n) != want || files() != 2 {
		t.Fatalf("copied %d of %d (%v), counted %d entries", n, want, err, files())
	}
	// A reader that gives up early still gets a count, without blocking.
	r, files = countEntries(bytes.NewReader(buf.Bytes()))
	io.CopyN(io.Discard, r, 10)
	if got := files(); got != 0 {
		t.Fatalf("partial read counted %d", got)
	}
}
