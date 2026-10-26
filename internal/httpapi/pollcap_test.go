package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"davit/api"
	"davit/internal/events"
	"davit/internal/tokens"
)

// TestLongPollSlotsPerGrant: tokens share the owner's principal, but a
// token holding all its long-poll slots does not get the owner's own
// long-polls refused; the token's next one is (429).
func TestLongPollSlotsPerGrant(t *testing.T) {
	srv, db := newServerDB(t, grantAuth{"narrow": &tokens.Grant{TokenID: "tok_narrow", Scopes: scope("", api.TokenView)}})
	ev := &events.Service{DB: db, MaxWaitersPerCaller: 1}
	srv.SetEvents(ev)
	h := srv.Handler()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest("GET", "/api/v1/events?start=latest&waitMs=20000", nil).WithContext(ctx)
		req.Header.Set("X-Test-Grant", "narrow")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(5 * time.Second); ev.Waiting() < 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the token's long-poll is not waiting")
		}
	}
	if rec, _ := call(t, h, "GET", "/api/v1/events?start=latest&waitMs=10", "", map[string]string{"X-Test-Grant": "narrow"}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the token's second long-poll: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := call(t, h, "GET", "/api/v1/events?start=latest&waitMs=10", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("the owner's long-poll while the token holds its slot: %d %s", rec.Code, rec.Body)
	}
}
