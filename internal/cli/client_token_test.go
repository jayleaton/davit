package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerOnlyForRemoteAPIs(t *testing.T) {
	t.Setenv("DAVIT_TOKEN", "slw_secret")
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c, err := newClient(srv.URL, "req_x")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.get(context.Background(), "/capabilities", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got != "Bearer slw_secret" {
		t.Fatalf("remote API got Authorization %q", got)
	}
	local, err := newClient("unix:///nonexistent.sock", "req_x")
	if err != nil {
		t.Fatal(err)
	}
	if local.token != "" {
		t.Fatal("the local socket client must never send a token")
	}
}
