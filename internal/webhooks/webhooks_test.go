package webhooks_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/credproxy"
	"davit/internal/events/eventstest"
	"davit/internal/webhooks"
)

const owner = eventstest.Owner

// receiver records deliveries; respond decides the status for each.
type receiver struct {
	*httptest.Server
	mu      sync.Mutex
	got     []delivery
	respond func(n int) int
	delay   time.Duration
}

type delivery struct {
	sig     string
	body    []byte
	payload api.WebhookPayload
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{respond: func(int) int { return 200 }}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if r.delay > 0 {
			select {
			case <-time.After(r.delay):
			case <-req.Context().Done():
				return
			}
		}
		var p api.WebhookPayload
		json.Unmarshal(body, &p)
		r.mu.Lock()
		r.got = append(r.got, delivery{sig: req.Header.Get(api.HeaderWebhookSignature), body: body, payload: p})
		n := len(r.got)
		r.mu.Unlock()
		w.WriteHeader(r.respond(n))
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) deliveries() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery(nil), r.got...)
}

func service(h *eventstest.Host, secretsDir string) *webhooks.Service {
	// Test receivers are httptest servers on loopback.
	return &webhooks.Service{DB: h.DB, Secrets: credproxy.FileSecrets{Dir: secretsDir}, Events: h.Events, AllowPrivate: true,
		BaseBackoff: 10 * time.Millisecond, MaxBackoff: 40 * time.Millisecond, Timeout: 500 * time.Millisecond, MaxAttempts: 3}
}

func run(t *testing.T, s *webhooks.Service) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return func() { cancel(); <-done }
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestSignedDeliveries(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	s := service(h, t.TempDir())
	run(t, s)
	rcv := newReceiver(t)
	ctx := context.Background()
	created, err := s.Create(ctx, owner, "", api.CreateWebhookRequest{URL: rcv.URL, Events: []string{"vm."}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Secret, "whsec_") || !created.Webhook.Enabled {
		t.Fatalf("created %+v", created)
	}
	h.CreateVM("web", nil)
	eventually(t, "vm deliveries", func() bool { return len(rcv.deliveries()) >= 2 })
	for _, d := range rcv.deliveries() {
		if err := webhooks.Verify(d.sig, d.body, time.Now(), 5*time.Minute, []byte(created.Secret)); err != nil {
			t.Fatalf("signature of %s: %v (%s)", d.payload.DeliveryID, err, d.sig)
		}
		if !strings.HasPrefix(d.payload.Event.Type, "vm.") || d.payload.WebhookID != created.Webhook.ID {
			t.Fatalf("payload %+v", d.payload)
		}
		// Tampering or another secret fails.
		if webhooks.Verify(d.sig, append(d.body, ' '), time.Now(), 5*time.Minute, []byte(created.Secret)) == nil ||
			webhooks.Verify(d.sig, d.body, time.Now(), 5*time.Minute, []byte("whsec_other")) == nil ||
			webhooks.Verify(d.sig, d.body, time.Now().Add(time.Hour), 5*time.Minute, []byte(created.Secret)) == nil {
			t.Fatal("a tampered, foreign-key or stale delivery verified")
		}
	}
	// History before creation is not replayed; events arrive in seq order.
	ds := rcv.deliveries()
	for i := 1; i < len(ds); i++ {
		if ds[i].payload.Event.Seq <= ds[i-1].payload.Event.Seq {
			t.Fatalf("out of order: %d after %d", ds[i].payload.Event.Seq, ds[i-1].payload.Event.Seq)
		}
	}
}

func TestRetriesThenDeadLetterThenManualRetry(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	s := service(h, t.TempDir())
	run(t, s)
	rcv := newReceiver(t)
	failing := true
	var mu sync.Mutex
	rcv.respond = func(int) int {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return 503
		}
		return 204
	}
	ctx := context.Background()
	wh, err := s.Create(ctx, owner, "", api.CreateWebhookRequest{URL: rcv.URL, Events: []string{"vm.created"}})
	if err != nil {
		t.Fatal(err)
	}
	h.CreateVM("web", nil)
	eventually(t, "dead letter", func() bool {
		l, _ := s.Deliveries(ctx, owner, wh.Webhook.ID, api.DeliveryDead, 0, "")
		return l != nil && len(l.Items) == 1
	})
	l, _ := s.Deliveries(ctx, owner, wh.Webhook.ID, api.DeliveryDead, 0, "")
	d := l.Items[0]
	if d.Attempts != 3 || d.LastStatus != 503 || d.LastError == "" || len(rcv.deliveries()) != 3 {
		t.Fatalf("dead letter %+v after %d attempts at the receiver", d, len(rcv.deliveries()))
	}
	if g, _ := s.Get(ctx, owner, wh.Webhook.ID); g.Dead != 1 || g.Pending != 0 {
		t.Fatalf("counts %+v", g)
	}
	// The endpoint recovers; a manual retry delivers it.
	mu.Lock()
	failing = false
	mu.Unlock()
	r, err := s.Retry(ctx, owner, wh.Webhook.ID, d.ID)
	if err != nil || r.State != api.DeliveryPending || r.Attempts != 0 {
		t.Fatalf("retry: %+v %v", r, err)
	}
	eventually(t, "redelivery", func() bool {
		l, _ := s.Deliveries(ctx, owner, wh.Webhook.ID, api.DeliveryDelivered, 0, "")
		return l != nil && len(l.Items) == 1
	})
	ds := rcv.deliveries()
	if ds[len(ds)-1].payload.DeliveryID != d.ID {
		t.Fatal("redelivery has another delivery ID; receivers deduplicate by it")
	}
}

func TestRotationOverlapsSecrets(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	s := service(h, t.TempDir())
	run(t, s)
	rcv := newReceiver(t)
	ctx := context.Background()
	wh, err := s.Create(ctx, owner, "", api.CreateWebhookRequest{URL: rcv.URL, Events: []string{"vm.created"}})
	if err != nil {
		t.Fatal(err)
	}
	rot, err := s.Rotate(ctx, owner, wh.Webhook.ID, "k1", api.RotateWebhookSecretRequest{})
	if err != nil || rot.Secret == wh.Secret || rot.Webhook.PreviousSecretExpiresAt == nil {
		t.Fatalf("rotate: %+v %v", rot, err)
	}
	if again, _ := s.Rotate(ctx, owner, wh.Webhook.ID, "k1", api.RotateWebhookSecretRequest{}); again.Secret != rot.Secret {
		t.Fatal("rotate replay with the same key made another secret")
	}
	h.CreateVM("a", nil)
	eventually(t, "delivery", func() bool { return len(rcv.deliveries()) == 1 })
	d := rcv.deliveries()[0]
	if strings.Count(d.sig, "v1=") != 2 {
		t.Fatalf("overlap signature %q, want two v1", d.sig)
	}
	for _, sec := range []string{wh.Secret, rot.Secret} {
		if err := webhooks.Verify(d.sig, d.body, time.Now(), time.Minute, []byte(sec)); err != nil {
			t.Fatalf("during the overlap a receiver with either secret verifies: %v", err)
		}
	}
	zero := int64(0)
	rot2, err := s.Rotate(ctx, owner, wh.Webhook.ID, "", api.RotateWebhookSecretRequest{OverlapSeconds: &zero})
	if err != nil || rot2.Webhook.PreviousSecretExpiresAt != nil {
		t.Fatalf("rotate without overlap: %+v %v", rot2, err)
	}
	h.CreateVM("b", nil)
	eventually(t, "second delivery", func() bool { return len(rcv.deliveries()) == 2 })
	d = rcv.deliveries()[1]
	if strings.Count(d.sig, "v1=") != 1 || webhooks.Verify(d.sig, d.body, time.Now(), time.Minute, []byte(rot.Secret)) == nil ||
		webhooks.Verify(d.sig, d.body, time.Now(), time.Minute, []byte(rot2.Secret)) != nil {
		t.Fatalf("after a zero-overlap rotation only the new secret signs: %q", d.sig)
	}
}

// TestRestartResumesFromOutbox: deliveries pending when the daemon stops
// (endpoint down) and events not yet fanned out are delivered after a
// restart over the same database.
func TestRestartResumesFromOutbox(t *testing.T) {
	dir, secrets := t.TempDir(), t.TempDir()
	h := eventstest.New(t, dir)
	s := service(h, secrets)
	s.MaxAttempts = 100
	stop := run(t, s)
	rcv := newReceiver(t)
	rcv.respond = func(int) int { return 500 }
	ctx := context.Background()
	wh, err := s.Create(ctx, owner, "", api.CreateWebhookRequest{URL: rcv.URL, Events: []string{"vm.created"}})
	if err != nil {
		t.Fatal(err)
	}
	h.CreateVM("before", nil)
	eventually(t, "a failed attempt", func() bool { return len(rcv.deliveries()) >= 1 })
	stop()
	h.CreateVM("while-down", nil) // fanned out only after the restart
	h.Close()

	rcv.respond = func(int) int { return 200 }
	h2 := eventstest.New(t, dir)
	s2 := service(h2, secrets)
	run(t, s2)
	eventually(t, "both delivered after restart", func() bool {
		l, _ := s2.Deliveries(ctx, owner, wh.Webhook.ID, api.DeliveryDelivered, 0, "")
		return l != nil && len(l.Items) == 2
	})
	names := map[string]bool{}
	for _, d := range rcv.deliveries() {
		if n, _ := d.payload.Event.Data["name"].(string); n != "" {
			names[n] = true
		}
	}
	if !names["before"] || !names["while-down"] {
		t.Fatalf("delivered %v", names)
	}
}

// TestSlowReceiverBlocksNothing: an endpoint that hangs neither slows VM
// creation nor delays another webhook's deliveries.
func TestSlowReceiverBlocksNothing(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	// Receivers first: cleanups run in reverse, so the service stops (and
	// abandons the hanging request) before the receivers close.
	slow, fast := newReceiver(t), newReceiver(t)
	slow.delay = 5 * time.Second
	s := service(h, t.TempDir())
	s.Timeout = 5 * time.Second
	run(t, s)
	ctx := context.Background()
	for _, u := range []string{slow.URL, fast.URL} {
		if _, err := s.Create(ctx, owner, "", api.CreateWebhookRequest{URL: u}); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Now()
	for _, n := range []string{"a", "b", "c"} {
		h.CreateVM(n, nil)
	}
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("creating three VMs took %s with a hanging webhook", d)
	}
	eventually(t, "the fast webhook gets every vm.created", func() bool {
		n := 0
		for _, d := range fast.deliveries() {
			if d.payload.Event.Type == api.EventVMCreated {
				n++
			}
		}
		return n == 3
	})
	if len(slow.deliveries()) != 0 {
		t.Fatal("the slow receiver answered already?")
	}
}

func TestPayloadsAreRedacted(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	s := service(h, t.TempDir())
	run(t, s)
	rcv := newReceiver(t)
	if _, err := s.Create(context.Background(), owner, "", api.CreateWebhookRequest{URL: rcv.URL}); err != nil {
		t.Fatal(err)
	}
	h.CreateVM("sec", map[string]string{"DB_PASSWORD": "correct-horse-battery-staple"})
	h.StopVM("sec")
	eventually(t, "deliveries", func() bool { return len(rcv.deliveries()) > 5 })
	for _, d := range rcv.deliveries() {
		if strings.Contains(string(d.body), "correct-horse") || strings.Contains(string(d.body), "DB_PASSWORD") {
			t.Fatalf("delivery carries an env value: %s", d.body)
		}
	}
}

func TestValidation(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	s := service(h, t.TempDir())
	ctx := context.Background()
	for _, req := range []api.CreateWebhookRequest{
		{URL: "ftp://x/"}, {URL: "/relative"}, {URL: "https://user:pass@x/"},
		{URL: "https://x/", Events: []string{"VM.Bad"}},
	} {
		if _, err := s.Create(ctx, owner, "", req); err == nil {
			t.Errorf("accepted %+v", req)
		}
	}
	a, err := s.Create(ctx, owner, "key1", api.CreateWebhookRequest{URL: "https://example.invalid/hook"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, owner, "key1", api.CreateWebhookRequest{URL: "https://example.invalid/hook"})
	if err != nil || b.Webhook.ID != a.Webhook.ID || b.Secret != a.Secret {
		t.Fatalf("idempotent create: %+v %v", b, err)
	}
	off := false
	u, err := s.Update(ctx, owner, a.Webhook.ID, api.UpdateWebhookRequest{Enabled: &off})
	if err != nil || u.Enabled {
		t.Fatalf("disable: %+v %v", u, err)
	}
	if err := s.Delete(ctx, owner, a.Webhook.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, owner, a.Webhook.ID); err == nil {
		t.Fatal("second delete succeeded")
	}
	if _, err := s.Get(ctx, "mallory", b.Webhook.ID); err == nil {
		t.Fatal("another principal sees the webhook")
	}
}
