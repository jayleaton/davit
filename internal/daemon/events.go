package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"davit/internal/config"
	"davit/internal/credproxy"
	"davit/internal/events"
	"davit/internal/httpapi"
	"davit/internal/lifecycle"
	"davit/internal/otlp"
	"davit/internal/state"
	"davit/internal/webhooks"
)

// g16 is the event stream, webhooks and optional OTLP export (G16). All of
// it runs beside lifecycle: lifecycle only writes events (by trigger, in
// its own transactions) and never waits for a consumer.
type g16 struct {
	events   *events.Service
	webhooks *webhooks.Service
}

// startEvents starts retention, webhook delivery and, when configured,
// OTLP export, until ctx ends.
func startEvents(ctx context.Context, cfg config.Daemon, db *state.DB, svc *lifecycle.Service, log *slog.Logger) (*g16, error) {
	ev := &events.Service{DB: db, Changed: svc.Changed}
	wh := &webhooks.Service{DB: db, Events: ev, Log: log.With("component", "webhooks"),
		Secrets:      credproxy.FileSecrets{Dir: filepath.Join(cfg.DataDir, "webhook-secrets")},
		AllowPrivate: cfg.Network.AllowPrivateUpstreams}
	go ev.RunRetention(ctx)
	go wh.Run(ctx)
	if t := cfg.Telemetry; t.OTLPEndpoint != "" {
		host, _ := os.Hostname()
		exp, err := otlp.New(otlp.Options{
			Endpoint: t.OTLPEndpoint, Headers: t.OTLPHeaders, Interval: time.Duration(t.IntervalSeconds) * time.Second,
			Resource: map[string]string{"host.name": host}, Log: log.With("component", "otlp"),
			Gauges: func() []otlp.Gauge { return gauges(db) },
		})
		if err != nil {
			return nil, err
		}
		go exp.Run(ctx)
		// Event never blocks: a slow or dead collector drops, it does not
		// back-pressure the stream.
		go ev.Tail(ctx, exp.Event)
		log.Info("exporting events and metrics over OTLP", "endpoint", t.OTLPEndpoint)
	}
	return &g16{events: ev, webhooks: wh}, nil
}

// apply enables the G16 routes on an API server.
func (g *g16) apply(api *httpapi.Server) {
	api.SetEvents(g.events)
	api.SetWebhooks(g.webhooks)
}

// gauges samples VM and webhook delivery counts by state.
func gauges(db *state.DB) []otlp.Gauge {
	var vms, deliveries map[string]int64
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := db.Tx(ctx, func(tx *state.Tx) (err error) { vms, deliveries, err = tx.CountByState(); return err }); err != nil {
		return nil
	}
	var out []otlp.Gauge
	for s, n := range vms {
		out = append(out, otlp.Gauge{Name: "davit.vms", Unit: "{vm}", Description: "VMs by state", Value: float64(n), Attributes: map[string]string{"state": s}})
	}
	for s, n := range deliveries {
		out = append(out, otlp.Gauge{Name: "davit.webhook.deliveries", Unit: "{delivery}", Description: "webhook deliveries by state", Value: float64(n), Attributes: map[string]string{"state": s}})
	}
	return out
}
