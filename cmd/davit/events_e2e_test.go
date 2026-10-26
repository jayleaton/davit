package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/webhooks"
)

// TestEventsAndWebhooksCLI drives G16 end to end through the daemon and
// the CLI: a webhook receives signed vm.created deliveries, `events` lists
// the stream and `events --follow` sees a VM created while it runs.
func TestEventsAndWebhooksCLI(t *testing.T) {
	var mu sync.Mutex
	var got []struct {
		sig  string
		body []byte
	}
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, struct {
			sig  string
			body []byte
		}{r.Header.Get(api.HeaderWebhookSignature), b})
		mu.Unlock()
	}))
	defer rcv.Close()

	h := newHost(t)
	h.extraConfig = `"network":{"allowPrivateUpstreams":true}` // the receiver is on loopback
	h.start(0, 0)
	r := h.cli("webhook", "create", rcv.URL, "--event", "vm.created")
	var created api.WebhookSecret
	if r.code != 0 || json.Unmarshal(r.raw[len(r.raw)-1], &created) != nil || created.Secret == "" {
		t.Fatalf("webhook create: exit %d %s", r.code, r.raw)
	}
	h.waited(h.cli("new", "box"), domain.OpCreate)

	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no delivery")
		}
	}
	mu.Lock()
	d := got[0]
	mu.Unlock()
	var p api.WebhookPayload
	if err := webhooks.Verify(d.sig, d.body, time.Now(), time.Minute, []byte(created.Secret)); err != nil || json.Unmarshal(d.body, &p) != nil || p.Event.Type != api.EventVMCreated {
		t.Fatalf("delivery %s: %v", d.body, err)
	}

	r = h.cli("events", "--types", "vm.")
	types := map[string]int{}
	for _, raw := range r.raw[:len(r.raw)-1] {
		var e api.Event
		json.Unmarshal(raw, &e)
		types[e.Type]++
	}
	if r.code != 0 || types[api.EventVMCreated] != 1 || types[api.EventVMState] == 0 || len(types) != 2 {
		t.Fatalf("events --types vm.: exit %d %v", r.code, types)
	}
	if r := h.cli("webhook", "deliveries", created.Webhook.ID); r.code != 0 || len(r.raw) < 2 {
		t.Fatalf("webhook deliveries: exit %d %s", r.code, r.raw)
	}

	// --follow starts at the end of the stream and prints new events.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	follow := exec.CommandContext(ctx, bin, "--json", "events", "--follow", "--types", "vm.created")
	follow.Env = append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"))
	out, err := follow.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := follow.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { follow.Process.Kill(); follow.Wait() }()
	time.Sleep(300 * time.Millisecond)
	h.waited(h.cli("new", "late"), domain.OpCreate)
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"vm.created"`) {
			if strings.Contains(sc.Text(), `"box"`) {
				t.Fatal("--follow replayed history")
			}
			return
		}
	}
	t.Fatalf("--follow ended without the new VM: %v", sc.Err())
}

// TestOTLPExport: with telemetry.otlpEndpoint set, the daemon exports the
// event stream as OTLP/HTTP JSON logs and VM gauges as metrics.
func TestOTLPExport(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string][]string{}
	col := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies[r.URL.Path] = append(bodies[r.URL.Path], string(b))
		mu.Unlock()
		w.Write([]byte("{}"))
	}))
	defer col.Close()
	h := newHost(t)
	h.extraConfig = `"telemetry":{"otlpEndpoint":"` + col.URL + `","intervalSeconds":1}`
	h.start(0, 0)
	h.waited(h.cli("new", "box"), domain.OpCreate)
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		mu.Lock()
		logs, metrics := strings.Join(bodies["/v1/logs"], ""), strings.Join(bodies["/v1/metrics"], "")
		mu.Unlock()
		if strings.Contains(logs, `"vm.created"`) && strings.Contains(metrics, `"davit.vms"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("collector got logs %q / metrics %q", logs, metrics)
		}
	}
}
