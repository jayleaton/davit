package otlp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"davit/api"
)

// collector is a hermetic OTLP/HTTP receiver. fail, when set, may handle a
// request itself (returning true) to simulate a broken collector.
type collector struct {
	srv     *httptest.Server
	reqs    atomic.Int64
	mu      sync.Mutex
	logs    []map[string]any
	metrics []map[string]any
	headers []http.Header
	fail    func(n int64, w http.ResponseWriter, r *http.Request) bool
}

func newCollector(t *testing.T, fail func(n int64, w http.ResponseWriter, r *http.Request) bool) *collector {
	t.Helper()
	c := &collector{fail: fail}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := c.reqs.Add(1)
		if c.fail != nil && c.fail(n, w, r) {
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		switch r.URL.Path {
		case "/v1/logs":
			c.logs = append(c.logs, m)
		case "/v1/metrics":
			c.metrics = append(c.metrics, m)
		}
		c.headers = append(c.headers, r.Header.Clone())
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// records returns every log record received so far.
func (c *collector) records() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, req := range c.logs {
		for _, rl := range req["resourceLogs"].([]any) {
			for _, sl := range rl.(map[string]any)["scopeLogs"].([]any) {
				for _, lr := range sl.(map[string]any)["logRecords"].([]any) {
					out = append(out, lr.(map[string]any))
				}
			}
		}
	}
	return out
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// start runs e until the test ends (or the returned stop is called) and
// returns stop, which reports how long Run took to return.
func start(t *testing.T, e *Exporter) (stop func() time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	var once sync.Once
	var took time.Duration
	stop = func() time.Duration {
		once.Do(func() {
			t0 := time.Now()
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return after cancel")
			}
			took = time.Since(t0)
		})
		return took
	}
	t.Cleanup(func() { stop() })
	return stop
}

// attrs turns an OTLP JSON attribute list into key -> anyValue object.
func attrs(t *testing.T, v any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, _ := v.([]any)
	for _, kv := range list {
		m := kv.(map[string]any)
		out[m["key"].(string)] = m["value"].(map[string]any)
	}
	return out
}

type vmState string

func TestJSONShape(t *testing.T) {
	c := newCollector(t, nil)
	e, err := New(Options{
		Endpoint: c.srv.URL + "/",
		Headers:  map[string]string{"Authorization": "Bearer t0k"},
		Resource: map[string]string{"host.name": "box1"},
		Interval: 20 * time.Millisecond,
		Timeout:  time.Second,
		Gauges: func() []Gauge {
			return []Gauge{
				{Name: "davit.vms.running", Unit: "{vm}", Description: "Running VMs", Value: 3, Attributes: map[string]string{"pool": "default"}},
				{Name: "davit.skip", Value: math.NaN()},
			}
		},
		Log: quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 10, 22, 12, 0, 0, 123, time.UTC)
	e.Event(api.Event{
		Seq: 7, Type: api.EventVMState, Time: ts, VMID: "vm-1", Subject: "vm-1",
		Data: map[string]any{
			"state":  vmState("running"),
			"ok":     true,
			"count":  42,
			"ratio":  0.5,
			"nested": map[string]any{"a": 1},
			"nil":    nil,
		},
	})
	start(t, e)
	waitFor(t, 2*time.Second, "log record", func() bool { return len(c.records()) == 1 })
	waitFor(t, 2*time.Second, "metrics", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.metrics) > 0 })

	c.mu.Lock()
	logsReq := c.logs[0]
	metricsReq := c.metrics[0]
	hdr := c.headers[0]
	c.mu.Unlock()
	if got := hdr.Get("Authorization"); got != "Bearer t0k" {
		t.Errorf("Authorization header = %q", got)
	}

	rl := logsReq["resourceLogs"].([]any)[0].(map[string]any)
	ra := attrs(t, rl["resource"].(map[string]any)["attributes"])
	if ra["service.name"]["stringValue"] != "davit" || ra["host.name"]["stringValue"] != "box1" {
		t.Errorf("resource attributes = %v", ra)
	}
	sl := rl["scopeLogs"].([]any)[0].(map[string]any)
	if sl["scope"].(map[string]any)["name"] != scopeName {
		t.Errorf("scope = %v", sl["scope"])
	}
	lr := sl["logRecords"].([]any)[0].(map[string]any)
	if lr["body"].(map[string]any)["stringValue"] != "vm.state" {
		t.Errorf("body = %v", lr["body"])
	}
	if lr["timeUnixNano"] != "1792670400000000123" {
		t.Errorf("timeUnixNano = %#v (want the decimal string)", lr["timeUnixNano"])
	}
	if _, ok := lr["observedTimeUnixNano"].(string); !ok {
		t.Errorf("observedTimeUnixNano = %#v", lr["observedTimeUnixNano"])
	}
	if lr["severityNumber"] != float64(9) {
		t.Errorf("severityNumber = %#v", lr["severityNumber"])
	}
	la := attrs(t, lr["attributes"])
	want := map[string]map[string]any{
		"davit.event.seq":         {"intValue": "7"},
		"davit.event.type":        {"stringValue": "vm.state"},
		"davit.event.subject":     {"stringValue": "vm-1"},
		"davit.event.vm_id":       {"stringValue": "vm-1"},
		"davit.event.data.state":  {"stringValue": "running"},
		"davit.event.data.ok":     {"boolValue": true},
		"davit.event.data.count":  {"intValue": "42"},
		"davit.event.data.ratio":  {"doubleValue": 0.5},
		"davit.event.data.nested": {"stringValue": `{"a":1}`},
	}
	for k, w := range want {
		got := la[k]
		for wk, wv := range w {
			if got[wk] != wv || len(got) != 1 {
				t.Errorf("attribute %s = %v, want %v", k, got, w)
			}
		}
	}
	if _, ok := la["davit.event.data.nil"]; ok {
		t.Error("nil data value should be skipped")
	}
	if len(la) != len(want) {
		t.Errorf("got %d attributes, want %d: %v", len(la), len(want), la)
	}

	rm := metricsReq["resourceMetrics"].([]any)[0].(map[string]any)
	if attrs(t, rm["resource"].(map[string]any)["attributes"])["service.name"]["stringValue"] != "davit" {
		t.Errorf("metrics resource = %v", rm["resource"])
	}
	ms := rm["scopeMetrics"].([]any)[0].(map[string]any)["metrics"].([]any)
	byName := map[string]map[string]any{}
	for _, m := range ms {
		mm := m.(map[string]any)
		byName[mm["name"].(string)] = mm
	}
	if _, ok := byName["davit.skip"]; ok {
		t.Error("NaN gauge should be skipped")
	}
	g := byName["davit.vms.running"]
	if g == nil || g["unit"] != "{vm}" || g["description"] != "Running VMs" {
		t.Fatalf("gauge metric = %v", g)
	}
	dp := g["gauge"].(map[string]any)["dataPoints"].([]any)[0].(map[string]any)
	if dp["asDouble"] != float64(3) {
		t.Errorf("asDouble = %#v", dp["asDouble"])
	}
	if _, ok := dp["timeUnixNano"].(string); !ok {
		t.Errorf("gauge timeUnixNano = %#v", dp["timeUnixNano"])
	}
	if attrs(t, dp["attributes"])["pool"]["stringValue"] != "default" {
		t.Errorf("gauge attributes = %v", dp["attributes"])
	}
	s := byName["davit.otlp.events.dropped"]
	if s == nil {
		t.Fatal("no davit.otlp.events.dropped metric")
	}
	sm := s["sum"].(map[string]any)
	if sm["isMonotonic"] != true || sm["aggregationTemporality"] != float64(2) {
		t.Errorf("sum = %v", sm)
	}
	sdp := sm["dataPoints"].([]any)[0].(map[string]any)
	if sdp["asInt"] != "0" {
		t.Errorf("dropped asInt = %#v", sdp["asInt"])
	}
	if _, ok := sdp["startTimeUnixNano"].(string); !ok {
		t.Errorf("sum startTimeUnixNano = %#v", sdp["startTimeUnixNano"])
	}
	if st := e.Stats(); st.EventsExported != 1 || st.EventsDropped != 0 || st.MetricExports == 0 {
		t.Errorf("stats = %+v", st)
	}
}

func TestNewValidates(t *testing.T) {
	for _, ep := range []string{"", "collector:4318", "ftp://x", "http://"} {
		if _, err := New(Options{Endpoint: ep}); err == nil {
			t.Errorf("New(%q) succeeded", ep)
		}
	}
}

// isolation drives 10k events at a broken collector and checks Event never
// blocks, memory stays bounded, the exporter does not spin and Run exits
// promptly.
func isolation(t *testing.T, fail func(n int64, w http.ResponseWriter, r *http.Request) bool) (*collector, Stats) {
	c := newCollector(t, fail)
	const queue = 64
	e, err := New(Options{
		Endpoint:  c.srv.URL,
		Interval:  20 * time.Millisecond,
		Timeout:   100 * time.Millisecond,
		QueueSize: queue,
		Gauges:    func() []Gauge { return []Gauge{{Name: "g", Value: 1}} },
		Log:       quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, e)
	t0 := time.Now()
	for i := range 10000 {
		e.Event(api.Event{Seq: int64(i), Type: api.EventVMCreated, Time: time.Now()})
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("10k Event calls took %v", d)
	}
	if len(e.queue) > queue {
		t.Fatalf("queue len %d > %d", len(e.queue), queue)
	}
	time.Sleep(800 * time.Millisecond)
	// Over ~800ms with a 20ms interval there are ~40 ticks; each costs at
	// most two attempts per signal. A spinning exporter would make
	// thousands of requests.
	if n := c.reqs.Load(); n > 200 {
		t.Errorf("collector saw %d requests: exporter is spinning", n)
	}
	if took := stop(); took > time.Second {
		t.Errorf("Run took %v to return after cancel", took)
	}
	st := e.Stats()
	// Run consumes the queue concurrently, so how many get in depends on
	// timing; most of a 10k burst against a failing collector must drop.
	if st.EventsDropped < 5000 {
		t.Errorf("dropped %d of 10000, want most", st.EventsDropped)
	}
	if st.EventsExported != 0 {
		t.Errorf("exported %d to a broken collector", st.EventsExported)
	}
	if got := st.EventsDropped + st.EventsFailed + int64(len(e.queue)); got != 10000 {
		t.Errorf("dropped+failed+queued = %d, want 10000 (%+v)", got, st)
	}
	return c, st
}

func TestIsolationHangingCollector(t *testing.T) {
	release := make(chan struct{})
	_, st := isolation(t, func(_ int64, _ http.ResponseWriter, r *http.Request) bool {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return true
	})
	// Unblock handlers before the server's Close (cleanups run LIFO, so
	// this runs first).
	t.Cleanup(func() { close(release) })
	if st.MetricFailures == 0 {
		t.Errorf("metric failures not counted: %+v", st)
	}
}

func TestIsolationServerError(t *testing.T) {
	c, st := isolation(t, func(_ int64, w http.ResponseWriter, _ *http.Request) bool {
		http.Error(w, "boom", http.StatusInternalServerError)
		return true
	})
	if st.EventsFailed == 0 || st.MetricFailures == 0 {
		t.Errorf("failures not counted: %+v", st)
	}
	if c.reqs.Load() == 0 {
		t.Error("collector saw no requests")
	}
}

func TestRetryableThenRecovery(t *testing.T) {
	var healthy atomic.Bool
	c := newCollector(t, func(_ int64, w http.ResponseWriter, _ *http.Request) bool {
		if healthy.Load() {
			return false
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return true
	})
	e, err := New(Options{
		Endpoint: c.srv.URL,
		Interval: 20 * time.Millisecond,
		Timeout:  200 * time.Millisecond,
		Log:      quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	start(t, e)
	e.Event(api.Event{Seq: 1, Type: api.EventRunCreated, Time: time.Now()})
	waitFor(t, 2*time.Second, "failed batch", func() bool { return e.Stats().EventsFailed > 0 })
	// 503 is retryable: the failed batch was sent twice, not more.
	if n := c.reqs.Load(); n != 2 {
		t.Errorf("collector saw %d requests for one failed batch, want 2 (one retry)", n)
	}
	healthy.Store(true)
	e.Event(api.Event{Seq: 2, Type: api.EventRunState, Time: time.Now()})
	waitFor(t, 2*time.Second, "event after recovery", func() bool {
		for _, r := range c.records() {
			for _, a := range r["attributes"].([]any) {
				kv := a.(map[string]any)
				if kv["key"] == "davit.event.seq" && kv["value"].(map[string]any)["intValue"] == "2" {
					return true
				}
			}
		}
		return false
	})
	if st := e.Stats(); st.EventsExported != 1 || st.EventsFailed != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestRetryAfterPauses(t *testing.T) {
	c := newCollector(t, func(_ int64, w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return true
	})
	e, err := New(Options{Endpoint: c.srv.URL, Interval: 10 * time.Millisecond, Timeout: time.Second, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	start(t, e)
	e.Event(api.Event{Seq: 1, Type: api.EventVMCreated})
	waitFor(t, 2*time.Second, "failed batch", func() bool { return e.Stats().EventsFailed == 1 })
	e.Event(api.Event{Seq: 2, Type: api.EventVMCreated})
	time.Sleep(100 * time.Millisecond)
	if n := c.reqs.Load(); n != 1 {
		t.Errorf("collector saw %d requests during a 30s Retry-After, want 1", n)
	}
}

func TestFinalFlushOnCancel(t *testing.T) {
	c := newCollector(t, nil)
	e, err := New(Options{Endpoint: c.srv.URL, Interval: time.Hour, Timeout: time.Second, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, e)
	for i := range 3 {
		e.Event(api.Event{Seq: int64(i), Type: api.EventPromptCreated})
	}
	time.Sleep(20 * time.Millisecond)
	stop()
	if n := len(c.records()); n != 3 {
		t.Errorf("final flush delivered %d records, want 3", n)
	}
}

func TestPartialSuccess(t *testing.T) {
	c := newCollector(t, func(_ int64, w http.ResponseWriter, r *http.Request) bool {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"partialSuccess":{"rejectedLogRecords":"1","errorMessage":"bad"}}`)
		return true
	})
	e, err := New(Options{Endpoint: c.srv.URL, Interval: time.Hour, Timeout: time.Second, Log: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, e)
	e.Event(api.Event{Seq: 1, Type: "a"})
	e.Event(api.Event{Seq: 2, Type: "b"})
	time.Sleep(20 * time.Millisecond)
	stop()
	if st := e.Stats(); st.EventsExported != 1 || st.EventsFailed != 1 {
		t.Errorf("stats = %+v", st)
	}
}
