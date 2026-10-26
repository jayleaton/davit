package webhooks_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/events/eventstest"
)

// TestDeliveryRefusesInternalAddresses: by default a webhook cannot make
// the daemon POST to loopback, private, tailnet or metadata addresses,
// also through a name that resolves to one (checked at connect time); the
// delivery fails without reaching the receiver, and its error says why
// rather than how the target answered (SEC-8). A literal internal address
// is refused when the webhook is created or updated; a refusal at connect
// time cannot heal by retrying, so it dead-letters at once instead of
// holding the bounded outbox for hours of backoff.
func TestDeliveryRefusesInternalAddresses(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	s := service(h, t.TempDir())
	s.AllowPrivate = false
	run(t, s)
	rcv := newReceiver(t)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(rcv.URL, "http://"))
	ctx := context.Background()
	for _, u := range []string{rcv.URL, "http://169.254.169.254/latest", "https://[fd00::1]/", "http://10.0.0.1:8080/"} {
		_, err := s.Create(ctx, owner, "", api.CreateWebhookRequest{URL: u, Events: []string{"vm."}})
		if !invalidArgument(err) {
			t.Errorf("Create(%s) = %v, want invalid_argument", u, err)
		}
	}
	created, err := s.Create(ctx, owner, "", api.CreateWebhookRequest{URL: "http://localhost:" + port, Events: []string{"vm."}})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Webhook.ID
	bad := rcv.URL
	if _, err := s.Update(ctx, owner, id, api.UpdateWebhookRequest{URL: &bad}); !invalidArgument(err) {
		t.Errorf("Update to %s = %v, want invalid_argument", bad, err)
	}
	h.CreateVM("web", nil)
	eventually(t, "a dead-lettered delivery", func() bool {
		l, err := s.Deliveries(ctx, owner, id, "", 0, "")
		if err != nil || len(l.Items) == 0 {
			return false
		}
		d := l.Items[0]
		return d.State == api.DeliveryDead && d.Attempts == 1 && strings.Contains(d.LastError, "refused by policy")
	})
	time.Sleep(50 * time.Millisecond)
	if n := len(rcv.deliveries()); n != 0 {
		t.Fatalf("the loopback receiver got %d deliveries", n)
	}
}

func invalidArgument(err error) bool {
	e := domain.AsError(err)
	return e != nil && e.Code == domain.CodeInvalidArgument
}
