// Package otlp exports davit's host event stream (as log records) and a
// few gauges (as metrics) to an OpenTelemetry collector. It is optional and
// off by default, and it has no dependencies beyond the standard library: it
// speaks OTLP/HTTP with the JSON encoding of the OTLP protobuf messages.
//
// What it follows:
//   - OTLP specification, "OTLP/HTTP" (opentelemetry.io/docs/specs/otlp):
//     POST to <endpoint>/v1/logs and <endpoint>/v1/metrics with
//     Content-Type application/json; "JSON Protobuf Encoding": proto3 JSON
//     mapping with lowerCamelCase field names, enum values as integers, and
//     (from the proto3 JSON mapping) 64-bit integers as decimal strings;
//     "Failures": only 429, 502, 503 and 504 (and transport errors) are
//     retryable, Retry-After is honoured; "Partial Success" responses.
//   - opentelemetry-proto messages: collector/logs/v1 ExportLogsServiceRequest,
//     collector/metrics/v1 ExportMetricsServiceRequest, logs/v1 (ResourceLogs,
//     ScopeLogs, LogRecord, SeverityNumber), metrics/v1 (ResourceMetrics,
//     ScopeMetrics, Metric, Gauge, Sum, NumberDataPoint,
//     AggregationTemporality), common/v1 (AnyValue, KeyValue,
//     InstrumentationScope), resource/v1 (Resource).
//
// Isolation is the design goal: Event never blocks (it enqueues into a
// bounded queue or drops and counts), Run exports from a single goroutine
// with a per-request timeout, retries a failed request at most once, then
// drops the batch and counts it, logs failures at most once per interval,
// and simply tries again on the next interval, so it resumes when the
// collector recovers. Memory is bounded by QueueSize plus one batch.
package otlp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"davit/api"
)

const (
	defaultServiceName = "davit"
	defaultInterval    = 10 * time.Second
	defaultQueueSize   = 2048
	defaultTimeout     = 5 * time.Second

	scopeName     = "davit/internal/otlp"
	maxBatch      = 512
	maxAttempts   = 2 // one retry
	maxRetryDelay = time.Second
	maxRetryAfter = time.Minute
	maxRespBody   = 64 << 10
)

// Options configures an Exporter. Only Endpoint is required.
type Options struct {
	// Endpoint is the collector's OTLP/HTTP base URL, e.g.
	// http://collector:4318; /v1/logs and /v1/metrics are appended.
	Endpoint string
	// Headers are added to every request (e.g. auth for the collector).
	Headers map[string]string
	// ServiceName is the service.name resource attribute (default "davit").
	ServiceName string
	// Resource holds extra resource attributes (host.name etc.).
	Resource map[string]string
	// Interval is the flush and metrics sampling interval (default 10s).
	Interval time.Duration
	// QueueSize bounds buffered events (default 2048); overflow is dropped
	// and counted.
	QueueSize int
	// Timeout bounds each export request (default 5s).
	Timeout time.Duration
	// Gauges is sampled every Interval; nil disables metrics export.
	Gauges func() []Gauge
	// Client is the HTTP client (default: a new http.Client).
	Client *http.Client
	// Log receives export failures, at most one line per Interval.
	Log *slog.Logger
}

// Gauge is one sampled value, exported as an OTLP Gauge data point. Gauges
// sharing a Name become data points of one metric.
type Gauge struct {
	Name, Unit, Description string
	Value                   float64
	Attributes              map[string]string
}

// Stats counts what the exporter did with events and metric exports.
type Stats struct {
	EventsExported int64 // accepted by the collector
	EventsDropped  int64 // queue full: never sent
	EventsFailed   int64 // in a batch given up on after retry, or rejected
	MetricExports  int64 // successful metrics requests
	MetricFailures int64 // metrics requests given up on
}

// Exporter batches events into OTLP log records and samples gauges.
type Exporter struct {
	o          Options
	logsURL    string
	metricsURL string
	client     *http.Client
	log        *slog.Logger
	resource   resource
	start      time.Time
	queue      chan logRecord
	batchMax   int
	retryDelay time.Duration
	running    atomic.Bool

	exported, dropped, failed atomic.Int64
	mExported, mFailed        atomic.Int64

	// Owned by the Run goroutine.
	lastLog    time.Time
	suppressed int
	failing    bool
	pause      map[string]time.Time // signal -> Retry-After deadline
}

// New validates o and returns an Exporter; call Run to start exporting.
func New(o Options) (*Exporter, error) {
	ep := strings.TrimRight(strings.TrimSpace(o.Endpoint), "/")
	if ep == "" {
		return nil, errors.New("otlp: endpoint is required")
	}
	u, err := url.Parse(ep)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("otlp: endpoint %q must be an http(s) URL", o.Endpoint)
	}
	if o.ServiceName == "" {
		o.ServiceName = defaultServiceName
	}
	if o.Interval <= 0 {
		o.Interval = defaultInterval
	}
	if o.QueueSize <= 0 {
		o.QueueSize = defaultQueueSize
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	e := &Exporter{
		o:          o,
		logsURL:    ep + "/v1/logs",
		metricsURL: ep + "/v1/metrics",
		client:     o.Client,
		log:        o.Log,
		start:      time.Now(),
		queue:      make(chan logRecord, o.QueueSize),
		batchMax:   min(maxBatch, o.QueueSize),
		retryDelay: min(o.Interval, maxRetryDelay),
		pause:      map[string]time.Time{},
	}
	if e.client == nil {
		e.client = &http.Client{}
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	attrs := []keyValue{strAttr("service.name", o.ServiceName)}
	for _, k := range sortedKeys(o.Resource) {
		if k != "service.name" {
			attrs = append(attrs, strAttr(k, o.Resource[k]))
		}
	}
	e.resource = resource{Attributes: attrs}
	return e, nil
}

// Event enqueues ev for export. It never blocks: when the queue is full the
// event is dropped and counted in Stats.EventsDropped.
func (e *Exporter) Event(ev api.Event) {
	r := eventRecord(ev, time.Now())
	select {
	case e.queue <- r:
	default:
		e.dropped.Add(1)
	}
}

// Stats returns the current counters.
func (e *Exporter) Stats() Stats {
	return Stats{
		EventsExported: e.exported.Load(),
		EventsDropped:  e.dropped.Load(),
		EventsFailed:   e.failed.Load(),
		MetricExports:  e.mExported.Load(),
		MetricFailures: e.mFailed.Load(),
	}
}

type outcome int

const (
	sent outcome = iota
	gaveUp
	canceled
)

// Run exports until ctx is done, then makes one bounded final flush (at most
// Timeout, single attempt) of what is queued and returns. Only one Run is
// active at a time; a concurrent second call returns immediately.
func (e *Exporter) Run(ctx context.Context) {
	if !e.running.CompareAndSwap(false, true) {
		return
	}
	defer e.running.Store(false)

	tick := time.NewTicker(e.o.Interval)
	defer tick.Stop()
	batch := make([]logRecord, 0, e.batchMax)
	// After a failed export, a full batch waits for the next tick instead
	// of being sent at once, so a dead collector costs one attempt per
	// interval, not a tight loop; meanwhile the queue fills and drops.
	var holdUntil time.Time
	flush := func() {
		if len(batch) == 0 {
			return
		}
		switch e.exportLogs(ctx, batch) {
		case canceled:
			return // keep the batch for the final flush
		case gaveUp:
			holdUntil = time.Now().Add(e.o.Interval)
		}
		batch = batch[:0]
	}
	for {
		in := e.queue
		if len(batch) >= e.batchMax {
			in = nil
		}
		select {
		case <-ctx.Done():
			e.final(batch)
			return
		case r := <-in:
			batch = append(batch, r)
			if len(batch) >= e.batchMax && !time.Now().Before(holdUntil) {
				flush()
			}
		case <-tick.C:
			flush()
			e.exportMetrics(ctx)
		}
	}
}

// final drains what is queued now and sends it with a single attempt per
// batch, all within one Timeout.
func (e *Exporter) final(batch []logRecord) {
drain:
	for n := cap(e.queue); n > 0; n-- { // bounded: Event may still be adding
		select {
		case r := <-e.queue:
			batch = append(batch, r)
		default:
			break drain
		}
	}
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.o.Timeout)
	defer cancel()
	for len(batch) > 0 {
		n := min(len(batch), e.batchMax)
		chunk := batch[:n]
		batch = batch[n:]
		if ctx.Err() != nil || e.paused("logs") {
			e.failed.Add(int64(len(chunk)))
			continue
		}
		body, err := json.Marshal(e.logsRequest(chunk))
		if err != nil {
			e.failed.Add(int64(len(chunk)))
			continue
		}
		res := e.post(ctx, e.logsURL, body)
		if res.err != nil {
			e.failed.Add(int64(len(chunk)))
			e.logFailure("logs", res.err)
			continue
		}
		e.countLogs(int64(len(chunk)), res.rejected)
	}
}

func (e *Exporter) logsRequest(batch []logRecord) logsRequest {
	return logsRequest{ResourceLogs: []resourceLogs{{
		Resource:  e.resource,
		ScopeLogs: []scopeLogs{{Scope: scope{Name: scopeName}, LogRecords: batch}},
	}}}
}

func (e *Exporter) exportLogs(ctx context.Context, batch []logRecord) outcome {
	n := int64(len(batch))
	if e.paused("logs") {
		e.failed.Add(n)
		return gaveUp
	}
	body, err := json.Marshal(e.logsRequest(batch))
	if err != nil {
		e.failed.Add(n)
		e.logFailure("logs", err)
		return gaveUp
	}
	out, rejected := e.send(ctx, "logs", e.logsURL, body)
	switch out {
	case sent:
		e.countLogs(n, rejected)
	case gaveUp:
		e.failed.Add(n)
	}
	return out
}

func (e *Exporter) countLogs(n, rejected int64) {
	rejected = max(0, min(rejected, n))
	e.exported.Add(n - rejected)
	e.failed.Add(rejected)
}

func (e *Exporter) exportMetrics(ctx context.Context) {
	if e.o.Gauges == nil || e.paused("metrics") {
		return
	}
	now := time.Now()
	ms := gaugeMetrics(e.sampleGauges(), now)
	start := nanos(e.start)
	for _, c := range []struct {
		name, desc string
		v          int64
	}{
		{"davit.otlp.events.exported", "Events accepted by the OTLP collector.", e.exported.Load()},
		{"davit.otlp.events.dropped", "Events dropped because the export queue was full.", e.dropped.Load()},
		{"davit.otlp.events.failed", "Events dropped after a failed or rejected export.", e.failed.Load()},
	} {
		v := strconv.FormatInt(c.v, 10)
		ms = append(ms, metric{Name: c.name, Description: c.desc, Unit: "{event}", Sum: &sum{
			DataPoints:             []numberDataPoint{{StartTimeUnixNano: start, TimeUnixNano: nanos(now), AsInt: &v}},
			AggregationTemporality: temporalityCumulative,
			IsMonotonic:            true,
		}})
	}
	body, err := json.Marshal(metricsRequest{ResourceMetrics: []resourceMetrics{{
		Resource:     e.resource,
		ScopeMetrics: []scopeMetrics{{Scope: scope{Name: scopeName}, Metrics: ms}},
	}}})
	if err != nil {
		e.mFailed.Add(1)
		e.logFailure("metrics", err)
		return
	}
	switch out, _ := e.send(ctx, "metrics", e.metricsURL, body); out {
	case sent:
		e.mExported.Add(1)
	case gaveUp:
		e.mFailed.Add(1)
	}
}

// sampleGauges calls the Gauges callback, treating a panic as no samples.
func (e *Exporter) sampleGauges() (gs []Gauge) {
	defer func() {
		if p := recover(); p != nil {
			e.logFailure("metrics", fmt.Errorf("gauges callback panicked: %v", p))
			gs = nil
		}
	}()
	return e.o.Gauges()
}

// send posts body with at most one retry, for retryable failures only.
func (e *Exporter) send(ctx context.Context, signal, url string, body []byte) (outcome, int64) {
	for attempt := 1; ; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, e.o.Timeout)
		res := e.post(rctx, url, body)
		cancel()
		if res.err == nil {
			e.recovered()
			return sent, res.rejected
		}
		if ctx.Err() != nil {
			return canceled, 0
		}
		if res.retryAfter > e.retryDelay {
			// The collector asked for a longer pause than we are willing to
			// block for: stop sending this signal until then.
			e.pause[signal] = time.Now().Add(min(res.retryAfter, maxRetryAfter))
		}
		if !res.retryable || attempt >= maxAttempts || res.retryAfter > e.retryDelay {
			e.logFailure(signal, res.err)
			return gaveUp, 0
		}
		t := time.NewTimer(e.retryDelay)
		select {
		case <-ctx.Done():
			t.Stop()
			return canceled, 0
		case <-t.C:
		}
	}
}

func (e *Exporter) paused(signal string) bool {
	until, ok := e.pause[signal]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	delete(e.pause, signal)
	return false
}

type postResult struct {
	err        error
	retryable  bool
	retryAfter time.Duration
	rejected   int64
}

func (e *Exporter) post(ctx context.Context, url string, body []byte) postResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return postResult{err: err}
	}
	for k, v := range e.o.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return postResult{err: err, retryable: true}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBody))
	if resp.StatusCode/100 == 2 {
		var er exportResponse
		_ = json.Unmarshal(data, &er) // empty or unknown bodies are fine
		ps := er.PartialSuccess
		n, _ := ps.RejectedLogRecords.Int64()
		if m, _ := ps.RejectedDataPoints.Int64(); m > n {
			n = m
		}
		return postResult{rejected: n}
	}
	res := postResult{err: fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, snippet(data))}
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		res.retryable = true
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			res.retryAfter = time.Duration(s) * time.Second
		}
	}
	return res
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// logFailure logs at most once per Interval, reporting suppressed lines.
func (e *Exporter) logFailure(signal string, err error) {
	e.failing = true
	now := time.Now()
	if !e.lastLog.IsZero() && now.Sub(e.lastLog) < e.o.Interval {
		e.suppressed++
		return
	}
	s := e.Stats()
	e.log.Warn("otlp export failed", "signal", signal, "err", err,
		"events_dropped", s.EventsDropped, "events_failed", s.EventsFailed, "suppressed", e.suppressed)
	e.lastLog = now
	e.suppressed = 0
}

func (e *Exporter) recovered() {
	if e.failing {
		e.failing = false
		e.log.Info("otlp export recovered")
	}
}

// eventRecord converts an event into a log record. Done at enqueue time so
// the queue never aliases the caller's Data map.
func eventRecord(ev api.Event, now time.Time) logRecord {
	attrs := []keyValue{
		intAttr("davit.event.seq", ev.Seq),
		strAttr("davit.event.type", ev.Type),
	}
	if ev.Subject != "" {
		attrs = append(attrs, strAttr("davit.event.subject", ev.Subject))
	}
	if ev.VMID != "" {
		attrs = append(attrs, strAttr("davit.event.vm_id", string(ev.VMID)))
	}
	for _, k := range sortedKeys(ev.Data) {
		if v, ok := toAnyValue(ev.Data[k]); ok {
			attrs = append(attrs, keyValue{Key: "davit.event.data." + k, Value: v})
		}
	}
	r := logRecord{
		ObservedTimeUnixNano: nanos(now),
		SeverityNumber:       severityInfo,
		SeverityText:         "INFO",
		Body:                 strValue(ev.Type),
		Attributes:           attrs,
	}
	if !ev.Time.IsZero() {
		r.TimeUnixNano = nanos(ev.Time)
	}
	return r
}

// toAnyValue maps string, bool and number kinds (including named types)
// to the matching AnyValue; anything else is JSON-encoded into a string.
// nil is skipped.
func toAnyValue(v any) (anyValue, bool) {
	if v == nil {
		return anyValue{}, false
	}
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			s := strconv.FormatInt(i, 10)
			return anyValue{IntValue: &s}, true
		}
		return strValue(n.String()), true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.String:
		return strValue(rv.String()), true
	case reflect.Bool:
		b := rv.Bool()
		return anyValue{BoolValue: &b}, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s := strconv.FormatInt(rv.Int(), 10)
		return anyValue{IntValue: &s}, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := rv.Uint()
		if u > math.MaxInt64 {
			return strValue(strconv.FormatUint(u, 10)), true
		}
		s := strconv.FormatUint(u, 10)
		return anyValue{IntValue: &s}, true
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return strValue(strconv.FormatFloat(f, 'g', -1, 64)), true
		}
		return anyValue{DoubleValue: &f}, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strValue(fmt.Sprint(v)), true
	}
	if len(b) > 1 && b[0] == '"' {
		var s string
		if json.Unmarshal(b, &s) == nil {
			return strValue(s), true // e.g. time.Time or another TextMarshaler
		}
	}
	return strValue(string(b)), true
}

// gaugeMetrics groups gauges by name (first-seen order) into OTLP Gauge
// metrics. Unnamed and non-finite samples are skipped (JSON cannot carry
// NaN or Inf).
func gaugeMetrics(gs []Gauge, now time.Time) []metric {
	var ms []metric
	idx := map[string]int{}
	ts := nanos(now)
	for _, g := range gs {
		if g.Name == "" || math.IsNaN(g.Value) || math.IsInf(g.Value, 0) {
			continue
		}
		v := g.Value
		dp := numberDataPoint{TimeUnixNano: ts, AsDouble: &v}
		for _, k := range sortedKeys(g.Attributes) {
			dp.Attributes = append(dp.Attributes, strAttr(k, g.Attributes[k]))
		}
		i, ok := idx[g.Name]
		if !ok {
			i = len(ms)
			idx[g.Name] = i
			ms = append(ms, metric{Name: g.Name, Unit: g.Unit, Description: g.Description, Gauge: &gauge{}})
		}
		ms[i].Gauge.DataPoints = append(ms[i].Gauge.DataPoints, dp)
	}
	return ms
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func strValue(s string) anyValue { return anyValue{StringValue: &s} }

func strAttr(k, v string) keyValue { return keyValue{Key: k, Value: strValue(v)} }

func intAttr(k string, v int64) keyValue {
	s := strconv.FormatInt(v, 10)
	return keyValue{Key: k, Value: anyValue{IntValue: &s}}
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
