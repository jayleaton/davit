package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/lifecycle"
	"davit/internal/runtime/fake"
	"davit/internal/state"
	"davit/internal/storage"
)

type fixedAuth struct {
	p   domain.PrincipalID
	err error
}

func (a fixedAuth) Authenticate(*http.Request) (domain.PrincipalID, error) { return a.p, a.err }

func newServer(t *testing.T, auth Authenticator) http.Handler {
	t.Helper()
	s, _ := newServerDB(t, auth)
	return s.Handler()
}

// newServerDB is newServer before Handler, with its state database, for
// tests that wire more services.
func newServerDB(t *testing.T, auth Authenticator) (*Server, *state.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := state.Open(context.Background(), filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := fake.New(fake.Options{Dir: filepath.Join(dir, "rt")})
	st, _ := storage.Open(filepath.Join(dir, "st"), storage.ModeCopy)
	img, _ := lifecycle.DevImage(filepath.Join(dir, "img"))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := lifecycle.New(lifecycle.Config{DB: db, Runtime: rt, Storage: st, Images: lifecycle.NewStaticCatalog(img), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close(); db.Close() })
	return New(svc, auth, log), db
}

func call(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) (*httptest.ResponseRecorder, api.ErrorResponse) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var er api.ErrorResponse
	if rec.Code >= 400 {
		if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil || er.Error.Code == "" || er.Error.RequestID == "" {
			t.Fatalf("%s %s: error body %q is not an ErrorResponse", method, path, rec.Body.String())
		}
	}
	return rec, er
}

func TestAuthFailsClosed(t *testing.T) {
	h := newServer(t, fixedAuth{err: domain.Errorf(domain.CodePermissionDenied, "nope")})
	rec, er := call(t, h, "GET", "/api/v1/vms", "", nil)
	if rec.Code != http.StatusForbidden || er.Error.Code != domain.CodePermissionDenied {
		t.Fatalf("got %d %+v", rec.Code, er)
	}

	a := PeerAuthenticator{OwnerUID: 1000}
	r := httptest.NewRequest("GET", "/", nil)
	if _, err := a.Authenticate(r); !domain.IsCode(err, domain.CodeUnauthenticated) {
		t.Fatalf("no peer credentials: %v", err)
	}
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, 1001))
	if _, err := a.Authenticate(r); !domain.IsCode(err, domain.CodePermissionDenied) {
		t.Fatalf("other uid: %v", err)
	}
	r = r.WithContext(context.WithValue(r.Context(), peerKey{}, 1000))
	if p, err := a.Authenticate(r); err != nil || p != LocalPrincipal(1000) {
		t.Fatalf("owner: %v %v", p, err)
	}
}

func TestErrorMappingAndLimits(t *testing.T) {
	h := newServer(t, fixedAuth{p: "local:1"})
	key := map[string]string{api.HeaderIdempotencyKey: "k1", api.HeaderRequestID: "client-req-1"}

	rec, er := call(t, h, "POST", "/api/v1/vms", `{"name":"a","bogus":1}`, key)
	if rec.Code != 400 || er.Error.Code != domain.CodeInvalidArgument || er.Error.RequestID != "client-req-1" {
		t.Fatalf("unknown field: %d %+v", rec.Code, er)
	}
	rec, er = call(t, h, "POST", "/api/v1/vms", `{"name":"`+strings.Repeat("a", MaxBodyBytes)+`"}`, key)
	if rec.Code != 413 || er.Error.Code != domain.CodePayloadTooLarge {
		t.Fatalf("oversize: %d %+v", rec.Code, er)
	}
	rec, _ = call(t, h, "POST", "/api/v1/vms", `{"name":"a"}`, nil)
	if rec.Code != 400 {
		t.Fatalf("create without Idempotency-Key: %d", rec.Code)
	}
	rec, _ = call(t, h, "POST", "/api/v1/vms", `{"name":"a"}`, key)
	if rec.Code != 202 || !strings.HasPrefix(rec.Header().Get("Location"), "/api/v1/operations/op_") {
		t.Fatalf("create: %d %v", rec.Code, rec.Header())
	}
	rec, er = call(t, h, "POST", "/api/v1/vms", `{"name":"b"}`, key)
	if rec.Code != 409 || er.Error.Code != domain.CodeIdempotencyConflict {
		t.Fatalf("key reuse with new body: %d %+v", rec.Code, er)
	}
	rec, er = call(t, h, "POST", "/api/v1/vms/a/stop", "", map[string]string{api.HeaderIfMatch: "x"})
	if rec.Code != 400 {
		t.Fatalf("bad If-Match: %d %+v", rec.Code, er)
	}
	rec, er = call(t, h, "GET", "/api/v1/vms?limit=500", "", nil)
	if rec.Code != 400 || er.Error.Details["field"] != "limit" {
		t.Fatalf("limit: %d %+v", rec.Code, er)
	}
	rec, er = call(t, h, "POST", "/api/v1/vms/a/processes", `{}`, nil)
	if rec.Code != 422 || er.Error.Code != domain.CodeUnsupportedCapability {
		t.Fatalf("exec without guest transport: %d %+v", rec.Code, er)
	}
	rec, er = call(t, h, "POST", "/api/v1/vms/a/tunnels", `{"port":22}`, nil)
	if rec.Code != 422 || er.Error.Code != domain.CodeUnsupportedCapability {
		t.Fatalf("tunnel without guest transport: %d %+v", rec.Code, er)
	}
	rec, er = call(t, h, "POST", "/api/v1/vms/a/ssh-credentials", `{"publicKey":"x"}`, nil)
	if rec.Code != 422 || er.Error.Code != domain.CodeUnsupportedCapability {
		t.Fatalf("ssh-credentials without guest transport: %d %+v", rec.Code, er)
	}
	rec, er = call(t, h, "GET", "/api/v1/tokens", "", nil)
	if rec.Code != 422 || er.Error.Code != domain.CodeUnsupportedCapability {
		t.Fatalf("tokens without a token service: %d %+v", rec.Code, er)
	}
	rec, _ = call(t, h, "GET", "/api/v1/nope", "", nil)
	if rec.Code != 404 {
		t.Fatalf("unknown route: %d", rec.Code)
	}
	for c, want := range map[domain.Code]int{
		domain.CodeOperationInProgress: 409, domain.CodeRevisionMismatch: 412, domain.CodeUnsupportedCapability: 422,
		domain.CodeResourceExhausted: 429, domain.CodeUnavailable: 503, domain.CodeInternal: 500,
	} {
		if got := StatusOf(c); got != want {
			t.Errorf("StatusOf(%s) = %d, want %d", c, got, want)
		}
	}
}
