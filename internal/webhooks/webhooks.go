// Package webhooks delivers the host event stream (internal/events) to
// owner-configured HTTP endpoints (G16): subscriptions, a durable outbox in
// SQLite, signed deliveries, retries with backoff, dead letters and secret
// rotation with an overlap window.
//
// Isolation: lifecycle only ever writes events (by trigger, in its own
// transaction). Fan-out and delivery run in this package's goroutines with
// short transactions of their own; each webhook has one worker, every
// attempt has a timeout, and a slow or failing endpoint only delays its
// own queue. Delivery is at-least-once: a receiver deduplicates by the
// deliveryId (or event seq).
//
// Signing secrets live in the host secret store (credproxy.FileSecrets
// under its own directory), never in SQLite, and are returned only by
// createWebhook and rotateWebhookSecret.
package webhooks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/credproxy"
	"davit/internal/domain"
	"davit/internal/events"
	"davit/internal/network/netguard"
	"davit/internal/state"
)

// Defaults of the delivery policy.
const (
	DefaultMaxAttempts  = 12
	DefaultBaseBackoff  = 2 * time.Second
	DefaultMaxBackoff   = time.Hour
	DefaultTimeout      = 10 * time.Second
	DefaultOverlap      = 24 * time.Hour
	MaxOverlap          = 7 * 24 * time.Hour
	DefaultMaxPending   = 10000
	DefaultKeepFinished = 1000
	idempotencyWindow   = 10 * time.Minute
	fanoutBatch         = 256
)

// Service manages webhooks and delivers to them.
type Service struct {
	DB      *state.DB
	Secrets credproxy.SecretStore
	Events  *events.Service
	Client  *http.Client
	Log     *slog.Logger
	// AllowPrivate lets webhook URLs reach loopback, private and tailnet
	// addresses (network.allowPrivateUpstreams); link-local and metadata
	// addresses are refused either way. It applies to the default Client.
	AllowPrivate bool

	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Timeout     time.Duration
	// MaxPending bounds each webhook's undelivered outbox (an endpoint
	// that is down keeps getting events); beyond it the oldest are
	// dead-lettered. KeepFinished bounds the delivered and dead records
	// kept for listWebhookDeliveries. Defaults DefaultMaxPending and
	// DefaultKeepFinished.
	MaxPending   int
	KeepFinished int
	// Now is the clock (tests); nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	workers map[string]*worker
	wake    chan struct{} // webhook set changed
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

func (s *Service) poke() {
	s.mu.Lock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	w := s.wake
	s.mu.Unlock()
	select {
	case w <- struct{}{}:
	default:
	}
}

// secret keys in the secret store.
func secretKey(id string) string { return "webhook:" + id }

const (
	keyCurrent  = "current"
	keyPrevious = "previous"
)

func newSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// checkURL validates a webhook URL. A literal address the delivery policy
// refuses is refused here already; names are checked at connect time.
func (s *Service) checkURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return domain.Errorf(domain.CodeInvalidArgument, "url must be an absolute http or https URL").WithDetail("field", "url")
	case u.User != nil:
		return domain.Errorf(domain.CodeInvalidArgument, "url must not carry credentials (they would be shown back); authenticate deliveries by their signature").WithDetail("field", "url")
	case len(raw) > 2048:
		return domain.Errorf(domain.CodeInvalidArgument, "url longer than 2048 bytes").WithDetail("field", "url")
	}
	if a, err := netip.ParseAddr(u.Hostname()); err == nil {
		if err := (netguard.Policy{AllowPrivate: s.AllowPrivate}).Check(a); err != nil {
			return domain.Errorf(domain.CodeInvalidArgument, "url: %v", err).WithDetail("field", "url")
		}
	}
	return nil
}

func checkEvents(evs []string) ([]string, error) {
	types, err := events.ParseTypes(strings.Join(evs, ","))
	if err != nil {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "%s", domain.AsError(err).Message).WithDetail("field", "events")
	}
	if types == nil {
		types = []string{}
	}
	return types, nil
}

func (s *Service) view(tx *state.Tx, w *state.WebhookRecord) (api.Webhook, error) {
	pending, dead, err := tx.DeliveryCounts(w.ID)
	out := api.Webhook{ID: w.ID, URL: w.URL, Description: w.Description, Events: w.Events, Enabled: w.Enabled,
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, Pending: pending, Dead: dead}
	if w.PreviousSecretExpiresAt != nil && w.PreviousSecretExpiresAt.After(s.now()) {
		out.PreviousSecretExpiresAt = w.PreviousSecretExpiresAt
	}
	return out, err
}

// Create makes a webhook that receives events from now on. A replay of
// key within 10 minutes returns the same webhook and secret.
func (s *Service) Create(ctx context.Context, owner domain.PrincipalID, key string, req api.CreateWebhookRequest) (*api.WebhookSecret, error) {
	if err := s.checkURL(req.URL); err != nil {
		return nil, err
	}
	types, err := checkEvents(req.Events)
	if err != nil {
		return nil, err
	}
	if len(req.Description) > 512 {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "description longer than 512 bytes").WithDetail("field", "description")
	}
	if key != "" {
		var w *state.WebhookRecord
		if err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) { w, err = tx.WebhookByKey(owner, key); return err }); err != nil {
			return nil, err
		}
		if w != nil {
			if s.now().Sub(w.CreatedAt) > idempotencyWindow {
				return nil, domain.Errorf(domain.CodeInvalidState, "idempotency key already created webhook %s", w.ID).WithDetail("webhookId", w.ID)
			}
			return s.withSecret(ctx, w)
		}
	}
	now := s.now().UTC()
	w := &state.WebhookRecord{ID: domain.NewID("wh"), Owner: owner, URL: req.URL, Description: req.Description, Events: types,
		Enabled: req.Enabled == nil || *req.Enabled, CreatedAt: now, UpdatedAt: now, IdempotencyKey: key}
	secret := newSecret()
	if err := s.Secrets.PutSecret(ctx, secretKey(w.ID), keyCurrent, []byte(secret)); err != nil {
		return nil, err
	}
	var out api.Webhook
	err = s.DB.Tx(ctx, func(tx *state.Tx) error {
		last, err := tx.LastEventSeq() // from now on; no history replay
		if err != nil {
			return err
		}
		w.LastEvent = last
		if err := tx.PutWebhook(w); err != nil {
			return err
		}
		out, err = s.view(tx, w)
		return err
	})
	if err != nil {
		s.Secrets.DeleteSecret(context.Background(), secretKey(w.ID), keyCurrent)
		return nil, err
	}
	s.poke()
	return &api.WebhookSecret{Webhook: out, Secret: secret}, nil
}

func (s *Service) withSecret(ctx context.Context, w *state.WebhookRecord) (*api.WebhookSecret, error) {
	secret, err := s.Secrets.Secret(ctx, secretKey(w.ID), keyCurrent)
	if err != nil {
		return nil, err
	}
	var out api.Webhook
	err = s.DB.Tx(ctx, func(tx *state.Tx) (err error) { out, err = s.view(tx, w); return err })
	return &api.WebhookSecret{Webhook: out, Secret: string(secret)}, err
}

// Get returns one of owner's webhooks.
func (s *Service) Get(ctx context.Context, owner domain.PrincipalID, id string) (*api.Webhook, error) {
	var out api.Webhook
	err := s.DB.Tx(ctx, func(tx *state.Tx) error {
		w, err := tx.GetWebhook(owner, id)
		if err != nil {
			return err
		}
		out, err = s.view(tx, w)
		return err
	})
	return &out, err
}

// List returns a page of owner's webhooks.
func (s *Service) List(ctx context.Context, owner domain.PrincipalID, limit int, cursor string) (*api.WebhookList, error) {
	if limit <= 0 {
		limit = 50
	}
	out := &api.WebhookList{Items: []api.Webhook{}}
	err := s.DB.Tx(ctx, func(tx *state.Tx) error {
		ws, next, err := tx.ListWebhooks(owner, state.Page{Limit: min(limit, 200), Cursor: cursor, Filter: string(owner)})
		if err != nil {
			return err
		}
		for _, w := range ws {
			v, err := s.view(tx, w)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, v)
		}
		out.NextCursor = next
		return nil
	})
	return out, err
}

// Update changes the fields req sets. Re-enabling starts from the current
// end of the stream: events while disabled are not delivered.
func (s *Service) Update(ctx context.Context, owner domain.PrincipalID, id string, req api.UpdateWebhookRequest) (*api.Webhook, error) {
	if req.URL != nil {
		if err := s.checkURL(*req.URL); err != nil {
			return nil, err
		}
	}
	var types []string
	if req.Events != nil {
		var err error
		if types, err = checkEvents(*req.Events); err != nil {
			return nil, err
		}
	}
	var out api.Webhook
	err := s.DB.Tx(ctx, func(tx *state.Tx) error {
		w, err := tx.GetWebhook(owner, id)
		if err != nil {
			return err
		}
		changed := false
		set := func(cond bool, f func()) {
			if cond {
				f()
				changed = true
			}
		}
		set(req.URL != nil && *req.URL != w.URL, func() { w.URL = *req.URL })
		set(req.Events != nil && strings.Join(types, ",") != strings.Join(w.Events, ","), func() { w.Events = types })
		set(req.Description != nil && *req.Description != w.Description, func() { w.Description = *req.Description })
		if req.Enabled != nil && *req.Enabled != w.Enabled {
			if *req.Enabled {
				if w.LastEvent, err = tx.LastEventSeq(); err != nil {
					return err
				}
			}
			w.Enabled, changed = *req.Enabled, true
		}
		if changed {
			w.UpdatedAt = s.now().UTC()
			if err := tx.PutWebhook(w); err != nil {
				return err
			}
		}
		out, err = s.view(tx, w)
		return err
	})
	if err == nil {
		s.poke()
	}
	return &out, err
}

// Delete removes a webhook, its deliveries and its secrets.
func (s *Service) Delete(ctx context.Context, owner domain.PrincipalID, id string) error {
	if err := s.DB.Tx(ctx, func(tx *state.Tx) error { return tx.DeleteWebhook(owner, id) }); err != nil {
		return err
	}
	s.Secrets.DeleteSecret(ctx, secretKey(id), keyCurrent)
	s.Secrets.DeleteSecret(ctx, secretKey(id), keyPrevious)
	s.poke()
	return nil
}

type rotation struct {
	Key string    `json:"key"`
	At  time.Time `json:"at"`
}

// Rotate replaces the signing secret; the old one keeps signing until
// overlap ends. A replay of key within 10 minutes returns the same secret.
func (s *Service) Rotate(ctx context.Context, owner domain.PrincipalID, id, key string, req api.RotateWebhookSecretRequest) (*api.WebhookSecret, error) {
	overlap := DefaultOverlap
	if req.OverlapSeconds != nil {
		overlap = time.Duration(*req.OverlapSeconds) * time.Second
		if overlap < 0 || overlap > MaxOverlap {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "overlapSeconds must be 0..%d", int64(MaxOverlap/time.Second)).WithDetail("field", "overlapSeconds")
		}
	}
	var w *state.WebhookRecord
	if err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) { w, err = tx.GetWebhook(owner, id); return err }); err != nil {
		return nil, err
	}
	sk := secretKey(id)
	if key != "" {
		if b, err := s.Secrets.Secret(ctx, sk, "rotation"); err == nil {
			var r rotation
			if json.Unmarshal(b, &r) == nil && r.Key == key && s.now().Sub(r.At) < idempotencyWindow {
				return s.withSecret(ctx, w)
			}
		}
	}
	cur, err := s.Secrets.Secret(ctx, sk, keyCurrent)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	if overlap > 0 {
		if err := s.Secrets.PutSecret(ctx, sk, keyPrevious, cur); err != nil {
			return nil, err
		}
		exp := now.Add(overlap)
		w.PreviousSecretExpiresAt = &exp
	} else {
		s.Secrets.DeleteSecret(ctx, sk, keyPrevious)
		w.PreviousSecretExpiresAt = nil
	}
	secret := newSecret()
	if err := s.Secrets.PutSecret(ctx, sk, keyCurrent, []byte(secret)); err != nil {
		return nil, err
	}
	if key != "" {
		b, _ := json.Marshal(rotation{Key: key, At: now})
		s.Secrets.PutSecret(ctx, sk, "rotation", b)
	}
	w.UpdatedAt = now
	var out api.Webhook
	err = s.DB.Tx(ctx, func(tx *state.Tx) error {
		cur, err := tx.GetWebhook(owner, id) // keep the fan-out position
		if err != nil {
			return err
		}
		cur.PreviousSecretExpiresAt, cur.UpdatedAt = w.PreviousSecretExpiresAt, w.UpdatedAt
		if err := tx.PutWebhook(cur); err != nil {
			return err
		}
		out, err = s.view(tx, cur)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &api.WebhookSecret{Webhook: out, Secret: secret}, nil
}

func deliveryView(d *state.DeliveryRecord) api.WebhookDelivery {
	out := api.WebhookDelivery{ID: d.ID, WebhookID: d.WebhookID, EventSeq: d.EventSeq, EventType: d.EventType, State: d.State,
		Attempts: d.Attempts, LastStatus: d.LastStatus, LastError: d.LastError, CreatedAt: d.CreatedAt, DeliveredAt: d.DeliveredAt}
	if d.State == api.DeliveryPending {
		next := d.NextAt
		out.NextAttemptAt = &next
	}
	return out
}

// Deliveries lists a webhook's deliveries, newest first.
func (s *Service) Deliveries(ctx context.Context, owner domain.PrincipalID, id, st string, limit int, cursor string) (*api.WebhookDeliveryList, error) {
	switch st {
	case "", api.DeliveryPending, api.DeliveryDelivered, api.DeliveryDead:
	default:
		return nil, domain.Errorf(domain.CodeInvalidArgument, "state must be pending, delivered or dead").WithDetail("field", "state")
	}
	out := &api.WebhookDeliveryList{Items: []api.WebhookDelivery{}}
	err := s.DB.Tx(ctx, func(tx *state.Tx) error {
		if _, err := tx.GetWebhook(owner, id); err != nil {
			return err
		}
		ds, next, err := tx.ListDeliveries(id, st, state.Page{Limit: min(max(limit, 0), 200), Cursor: cursor, Filter: id + "|" + st})
		if err != nil {
			return err
		}
		for _, d := range ds {
			out.Items = append(out.Items, deliveryView(d))
		}
		out.NextCursor = next
		return nil
	})
	return out, err
}

// Retry makes a delivery pending and due now (a dead letter gets a fresh
// set of attempts). Retrying a pending delivery only moves it up.
func (s *Service) Retry(ctx context.Context, owner domain.PrincipalID, id, delivery string) (*api.WebhookDelivery, error) {
	var out api.WebhookDelivery
	err := s.DB.Tx(ctx, func(tx *state.Tx) error {
		if _, err := tx.GetWebhook(owner, id); err != nil {
			return err
		}
		d, err := tx.GetDelivery(id, delivery)
		if err != nil {
			return err
		}
		if d.State != api.DeliveryPending {
			d.State, d.Attempts, d.DeliveredAt = api.DeliveryPending, 0, nil
		}
		d.NextAt = s.now().UTC()
		if err := tx.PutDelivery(d); err != nil {
			return err
		}
		out = deliveryView(d)
		return nil
	})
	if err == nil {
		s.wakeWorker(id)
	}
	return &out, err
}

// Run fans the event stream out into each enabled webhook's outbox and
// runs one delivery worker per webhook, until ctx ends. Pending deliveries
// left by a previous daemon are resumed from the outbox.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	s.workers = map[string]*worker{}
	wake := s.wake
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for _, w := range s.workers {
			w.stop()
		}
		s.mu.Unlock()
	}()
	for ctx.Err() == nil {
		more, err := s.fanout(ctx)
		if err != nil && ctx.Err() == nil {
			s.log().Warn("webhooks: fan-out", "err", err)
		}
		if more {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, time.Second)
		go func() {
			select {
			case <-wake:
				cancel()
			case <-wctx.Done():
			}
		}()
		s.Events.Wait(wctx, time.Second)
		cancel()
	}
}

// fanout enqueues new matching events for every enabled webhook and keeps
// one worker per webhook. It reports whether a webhook has more to fan out.
func (s *Service) fanout(ctx context.Context) (bool, error) {
	var hooks []*state.WebhookRecord
	if err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		hooks, _, err = tx.ListWebhooks("", state.Page{})
		return err
	}); err != nil {
		return false, err
	}
	live := map[string]bool{}
	more := false
	for _, h := range hooks {
		live[h.ID] = true
		s.ensureWorker(ctx, h.ID)
		if !h.Enabled {
			continue
		}
		n, err := s.fanoutOne(ctx, h.ID)
		if err != nil {
			return more, err
		}
		if n > 0 {
			s.wakeWorker(h.ID)
		}
		more = more || n == fanoutBatch
	}
	s.mu.Lock()
	for id, w := range s.workers {
		if !live[id] {
			w.stop()
			delete(s.workers, id)
		}
	}
	s.mu.Unlock()
	return more, nil
}

// fanoutOne moves one webhook's position forward by up to a batch,
// enqueueing matching events, in one transaction.
func (s *Service) fanoutOne(ctx context.Context, id string) (int, error) {
	n := 0
	err := s.DB.Tx(ctx, func(tx *state.Tx) error {
		h, err := tx.GetWebhook("", id)
		if err != nil || !h.Enabled {
			return err
		}
		rows, err := tx.ListEvents(h.Owner, h.LastEvent, fanoutBatch, state.EventFilter{Types: h.Events})
		if err != nil || len(rows) == 0 {
			return err
		}
		now := s.now().UTC()
		for _, r := range rows {
			ev, err := json.Marshal(events.Render(r))
			if err != nil {
				return err
			}
			if err := tx.EnqueueDelivery(&state.DeliveryRecord{ID: domain.NewID("whd"), WebhookID: id, EventSeq: r.Seq, EventType: r.Type,
				State: api.DeliveryPending, NextAt: now, CreatedAt: now, Event: ev}); err != nil {
				return err
			}
			h.LastEvent = r.Seq
		}
		n = len(rows)
		if err := tx.PutWebhook(h); err != nil {
			return err
		}
		maxPending, keep := s.MaxPending, s.KeepFinished
		if maxPending <= 0 {
			maxPending = DefaultMaxPending
		}
		if keep <= 0 {
			keep = DefaultKeepFinished
		}
		dead, _, err := tx.BoundDeliveries(id, maxPending, keep)
		if dead > 0 {
			s.log().Warn("webhook outbox full; oldest deliveries dead-lettered", "webhook", id, "count", dead, "limit", maxPending)
		}
		return err
	})
	return n, err
}

func (s *Service) ensureWorker(ctx context.Context, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workers == nil {
		s.workers = map[string]*worker{}
	}
	if _, ok := s.workers[id]; ok {
		return
	}
	wctx, cancel := context.WithCancel(ctx)
	w := &worker{s: s, id: id, wake: make(chan struct{}, 1), cancel: cancel, done: make(chan struct{})}
	s.workers[id] = w
	go w.run(wctx)
}

func (s *Service) wakeWorker(id string) {
	s.mu.Lock()
	w := s.workers[id]
	s.mu.Unlock()
	if w != nil {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// worker delivers one webhook's due deliveries, one at a time.
type worker struct {
	s      *Service
	id     string
	wake   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *worker) stop() { w.cancel() }

func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	for ctx.Err() == nil {
		next := w.drain(ctx)
		d := time.Minute
		if next > 0 {
			d = max(time.Until(time.Unix(0, next)), 0)
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-time.After(d):
		}
	}
}

// drain delivers everything due and returns the next due time (ns; 0 when
// nothing is pending).
func (w *worker) drain(ctx context.Context) int64 {
	s := w.s
	for ctx.Err() == nil {
		var h *state.WebhookRecord
		var due []*state.DeliveryRecord
		var next int64
		err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) {
			if h, err = tx.GetWebhook("", w.id); err != nil {
				return err
			}
			if due, err = tx.DueDeliveries(w.id, s.now().UnixNano(), 16); err != nil {
				return err
			}
			next, err = tx.NextDue(w.id)
			return err
		})
		if err != nil || !h.Enabled {
			return 0
		}
		if len(due) == 0 {
			return next
		}
		for _, d := range due {
			if ctx.Err() != nil {
				return 0
			}
			w.deliver(ctx, h, d)
		}
	}
	return 0
}

func (w *worker) secrets(ctx context.Context, h *state.WebhookRecord) [][]byte {
	s := w.s
	sk := secretKey(h.ID)
	var out [][]byte
	if b, err := s.Secrets.Secret(ctx, sk, keyCurrent); err == nil {
		out = append(out, b)
	}
	if h.PreviousSecretExpiresAt != nil {
		if s.now().Before(*h.PreviousSecretExpiresAt) {
			if b, err := s.Secrets.Secret(ctx, sk, keyPrevious); err == nil {
				out = append(out, b)
			}
		} else {
			s.Secrets.DeleteSecret(ctx, sk, keyPrevious)
		}
	}
	return out
}

// deliver makes one attempt and records the outcome.
func (w *worker) deliver(ctx context.Context, h *state.WebhookRecord, d *state.DeliveryRecord) {
	s := w.s
	var ev api.Event
	json.Unmarshal(d.Event, &ev)
	body, _ := json.Marshal(api.WebhookPayload{DeliveryID: d.ID, WebhookID: h.ID, Event: ev})
	status, err := s.post(ctx, h, d, body, w.secrets(ctx, h))
	if ctx.Err() != nil {
		return // shutting down: the delivery stays pending and resumes
	}
	now := s.now().UTC()
	d.Attempts++
	d.LastStatus = status
	switch {
	case err == nil:
		d.State, d.DeliveredAt, d.LastError = api.DeliveryDelivered, &now, ""
	case d.Attempts >= s.maxAttempts() || errors.Is(err, netguard.ErrRefused): // a refusal does not heal by retrying
		d.State, d.LastError = api.DeliveryDead, err.Error()
		s.log().Warn("webhook delivery dead-lettered", "webhook", h.ID, "delivery", d.ID, "attempts", d.Attempts, "err", err)
	default:
		d.LastError = err.Error()
		d.NextAt = now.Add(s.backoff(d.Attempts))
	}
	if err := s.DB.Tx(context.WithoutCancel(ctx), func(tx *state.Tx) error { return tx.PutDelivery(d) }); err != nil {
		s.log().Warn("webhooks: record delivery", "delivery", d.ID, "err", err)
	}
}

func (s *Service) maxAttempts() int {
	if s.MaxAttempts > 0 {
		return s.MaxAttempts
	}
	return DefaultMaxAttempts
}

// backoff is exponential from BaseBackoff, capped, with ±20% jitter.
func (s *Service) backoff(attempt int) time.Duration {
	base, top := s.BaseBackoff, s.MaxBackoff
	if base <= 0 {
		base = DefaultBaseBackoff
	}
	if top <= 0 {
		top = DefaultMaxBackoff
	}
	d := base
	for i := 1; i < attempt && d < top; i++ {
		d *= 2
	}
	d = min(d, top)
	return d + time.Duration((mrand.Float64()*0.4-0.2)*float64(d))
}

var errStatus = errors.New("non-2xx response")

// post sends one signed attempt. Redirects are not followed; any non-2xx
// status fails.
func (s *Service) post(ctx context.Context, h *state.WebhookRecord, d *state.DeliveryRecord, body []byte, secrets [][]byte) (int, error) {
	if len(secrets) == 0 {
		return 0, errors.New("no signing secret")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "davit-webhooks/1")
	req.Header.Set(api.HeaderWebhookDelivery, d.ID)
	req.Header.Set(api.HeaderWebhookEvent, d.EventType)
	req.Header.Set(api.HeaderWebhookSignature, Signature(s.now(), body, secrets...))
	client := s.Client
	if client == nil {
		// SEC-8: the address actually dialed (after DNS) must not be
		// internal unless the operator allowed it; no environment proxy.
		client = &http.Client{
			Transport:     netguard.Policy{AllowPrivate: s.AllowPrivate}.Transport(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is already known; keep the cause
		}
		return 0, fmt.Errorf("deliver: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%w: HTTP %d", errStatus, resp.StatusCode)
	}
	return resp.StatusCode, nil
}
