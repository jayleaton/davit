package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"davit/internal/domain"
)

// G16: the event stream (written by triggers, migration 7) and webhooks.

// EventRow is one stored event; Data is the trigger's JSON object.
type EventRow struct {
	Seq     int64
	NS      int64
	Owner   domain.PrincipalID
	Type    string
	VMID    domain.VMID
	Subject string
	Data    json.RawMessage
}

// EventFilter narrows ListEvents. Types are exact types or prefixes ending
// in "." ("vm."); empty matches all.
type EventFilter struct {
	Types []string
	VMID  domain.VMID
}

// ListEvents returns up to limit of owner's events after seq (all owners
// when owner is empty), in seq order.
func (t *Tx) ListEvents(owner domain.PrincipalID, after int64, limit int, f EventFilter) ([]EventRow, error) {
	q := `SELECT seq, ns, owner, type, vm_id, subject, data FROM events WHERE seq > ?`
	args := []any{after}
	if owner != "" {
		q += ` AND owner = ?`
		args = append(args, owner)
	}
	if f.VMID != "" {
		q += ` AND vm_id = ?`
		args = append(args, f.VMID)
	}
	if len(f.Types) > 0 {
		var or []string
		for _, ty := range f.Types {
			if strings.HasSuffix(ty, ".") {
				or = append(or, `substr(type, 1, ?) = ?`)
				args = append(args, len(ty), ty)
			} else {
				or = append(or, `type = ?`)
				args = append(args, ty)
			}
		}
		q += ` AND (` + strings.Join(or, ` OR `) + `)`
	}
	q += ` ORDER BY seq LIMIT ?`
	args = append(args, limit)
	rows, err := t.query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var data string
		if err := rows.Scan(&e.Seq, &e.NS, &e.Owner, &e.Type, &e.VMID, &e.Subject, &data); err != nil {
			return nil, err
		}
		e.Data = json.RawMessage(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastEventSeq is the newest event's seq (0 when there is none).
func (t *Tx) LastEventSeq() (int64, error) {
	var seq int64
	err := t.row(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&seq)
	return seq, err
}

// PruneEvents deletes events older than beforeNS, keeping the newest one
// so seq keeps growing across restarts (AUTOINCREMENT also guarantees it).
func (t *Tx) PruneEvents(beforeNS int64) (int64, error) {
	res, err := t.exec(`DELETE FROM events WHERE ns < ? AND seq < (SELECT MAX(seq) FROM events)`, beforeNS)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// WebhookRecord is a stored webhook (its secrets live elsewhere).
type WebhookRecord struct {
	ID          string             `json:"id"`
	Owner       domain.PrincipalID `json:"owner"`
	URL         string             `json:"url"`
	Description string             `json:"description,omitempty"`
	Events      []string           `json:"events"`
	Enabled     bool               `json:"enabled"`
	CreatedAt   time.Time          `json:"createdAt"`
	UpdatedAt   time.Time          `json:"updatedAt"`
	// PreviousSecretExpiresAt ends the rotation overlap.
	PreviousSecretExpiresAt *time.Time `json:"previousSecretExpiresAt,omitempty"`
	// IdempotencyKey dedupes createWebhook (owner, key).
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
	// LastEvent is the events.seq fanned out so far (a column).
	LastEvent int64 `json:"-"`
}

// PutWebhook inserts or updates w (its fan-out position too).
func (t *Tx) PutWebhook(w *WebhookRecord) error {
	doc, err := json.Marshal(w)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO webhooks (id, owner, created_ns, last_event, doc) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET last_event=excluded.last_event, doc=excluded.doc`,
		w.ID, w.Owner, w.CreatedAt.UnixNano(), w.LastEvent, string(doc))
	return err
}

func scanWebhook(s interface{ Scan(...any) error }) (*WebhookRecord, error) {
	var doc string
	var last int64
	if err := s.Scan(&doc, &last); err != nil {
		return nil, err
	}
	var w WebhookRecord
	if err := json.Unmarshal([]byte(doc), &w); err != nil {
		return nil, err
	}
	w.LastEvent = last
	return &w, nil
}

// GetWebhook returns owner's webhook id (any owner when owner is empty).
func (t *Tx) GetWebhook(owner domain.PrincipalID, id string) (*WebhookRecord, error) {
	w, err := scanWebhook(t.row(`SELECT doc, last_event FROM webhooks WHERE id = ? AND (? = '' OR owner = ?)`, id, owner, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("webhook", id)
	}
	return w, err
}

// WebhookByKey finds owner's webhook created with Idempotency-Key key.
func (t *Tx) WebhookByKey(owner domain.PrincipalID, key string) (*WebhookRecord, error) {
	w, err := scanWebhook(t.row(`SELECT doc, last_event FROM webhooks WHERE owner = ? AND json_extract(doc, '$.idempotencyKey') = ?`, owner, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return w, err
}

// ListWebhooks returns one page of owner's webhooks (all owners when empty),
// ordered by (createdAt, id).
func (t *Tx) ListWebhooks(owner domain.PrincipalID, p Page) ([]*WebhookRecord, string, error) {
	c, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 1 << 30
	}
	rows, err := t.query(`SELECT doc, last_event, created_ns, id FROM webhooks WHERE (? = '' OR owner = ?) AND (created_ns, id) > (?, ?)
ORDER BY created_ns, id LIMIT ?`, owner, owner, c.CreatedNS, c.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*WebhookRecord
	var next string
	for rows.Next() {
		var doc, id string
		var last, ns int64
		if err := rows.Scan(&doc, &last, &ns, &id); err != nil {
			return nil, "", err
		}
		if len(out) == limit {
			prev := out[len(out)-1]
			next = encodeCursor(prev.CreatedAt.UnixNano(), prev.ID, p.Filter)
			break
		}
		var w WebhookRecord
		if err := json.Unmarshal([]byte(doc), &w); err != nil {
			return nil, "", err
		}
		w.LastEvent = last
		out = append(out, &w)
	}
	return out, next, rows.Err()
}

// DeleteWebhook removes owner's webhook and its deliveries.
func (t *Tx) DeleteWebhook(owner domain.PrincipalID, id string) error {
	if _, err := t.exec(`DELETE FROM webhook_deliveries WHERE webhook_id = ?`, id); err != nil {
		return err
	}
	res, err := t.exec(`DELETE FROM webhooks WHERE id = ? AND owner = ?`, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("webhook", id)
	}
	return nil
}

// DeliveryRecord is one event's delivery to one webhook. Payload is the
// event as delivered, rendered when it was enqueued.
type DeliveryRecord struct {
	ID          string          `json:"id"`
	WebhookID   string          `json:"webhookId"`
	EventSeq    int64           `json:"eventSeq"`
	EventType   string          `json:"eventType"`
	State       string          `json:"state"`
	Attempts    int             `json:"attempts"`
	NextAt      time.Time       `json:"nextAt"`
	LastStatus  int             `json:"lastStatus,omitempty"`
	LastError   string          `json:"lastError,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	DeliveredAt *time.Time      `json:"deliveredAt,omitempty"`
	Event       json.RawMessage `json:"event"`
}

// EnqueueDelivery inserts d unless the webhook already has that event.
func (t *Tx) EnqueueDelivery(d *DeliveryRecord) error {
	doc, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO webhook_deliveries (id, webhook_id, event_seq, state, next_ns, created_ns, doc) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(webhook_id, event_seq) DO NOTHING`, d.ID, d.WebhookID, d.EventSeq, d.State, d.NextAt.UnixNano(), d.CreatedAt.UnixNano(), string(doc))
	return err
}

// PutDelivery updates an existing delivery.
func (t *Tx) PutDelivery(d *DeliveryRecord) error {
	doc, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = t.exec(`UPDATE webhook_deliveries SET state = ?, next_ns = ?, doc = ? WHERE id = ?`, d.State, d.NextAt.UnixNano(), string(doc), d.ID)
	return err
}

func scanDelivery(s interface{ Scan(...any) error }) (*DeliveryRecord, error) {
	var doc string
	if err := s.Scan(&doc); err != nil {
		return nil, err
	}
	var d DeliveryRecord
	return &d, json.Unmarshal([]byte(doc), &d)
}

// GetDelivery returns delivery id of webhook.
func (t *Tx) GetDelivery(webhook, id string) (*DeliveryRecord, error) {
	d, err := scanDelivery(t.row(`SELECT doc FROM webhook_deliveries WHERE id = ? AND webhook_id = ?`, id, webhook))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("delivery", id)
	}
	return d, err
}

// DueDeliveries returns up to limit pending deliveries of webhook due at
// or before nowNS, oldest due first.
func (t *Tx) DueDeliveries(webhook string, nowNS int64, limit int) ([]*DeliveryRecord, error) {
	rows, err := t.query(`SELECT doc FROM webhook_deliveries WHERE webhook_id = ? AND state = 'pending' AND next_ns <= ?
ORDER BY next_ns, event_seq LIMIT ?`, webhook, nowNS, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DeliveryRecord
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// NextDue is the earliest next_ns of webhook's pending deliveries (0: none).
func (t *Tx) NextDue(webhook string) (int64, error) {
	var ns int64
	err := t.row(`SELECT COALESCE(MIN(next_ns), 0) FROM webhook_deliveries WHERE webhook_id = ? AND state = 'pending'`, webhook).Scan(&ns)
	return ns, err
}

// BoundDeliveries keeps webhook's outbox bounded: beyond maxPending pending
// deliveries the oldest events are dead-lettered (state dead, lastError
// saying why, retryable like any dead delivery), and beyond keepFinished
// delivered or dead ones the oldest records are deleted. It returns how
// many it dead-lettered and deleted.
func (t *Tx) BoundDeliveries(webhook string, maxPending, keepFinished int) (dead, deleted int64, err error) {
	res, err := t.exec(`UPDATE webhook_deliveries SET state = 'dead', doc = json_set(doc, '$.state', 'dead', '$.lastError', ?)
WHERE id IN (SELECT id FROM webhook_deliveries WHERE webhook_id = ? AND state = 'pending' ORDER BY event_seq DESC LIMIT -1 OFFSET ?)`,
		fmt.Sprintf("outbox full: more than %d deliveries pending; retry it once the endpoint recovers", maxPending), webhook, maxPending)
	if err != nil {
		return 0, 0, err
	}
	if dead, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	res, err = t.exec(`DELETE FROM webhook_deliveries
WHERE id IN (SELECT id FROM webhook_deliveries WHERE webhook_id = ? AND state IN ('delivered', 'dead') ORDER BY created_ns DESC, id DESC LIMIT -1 OFFSET ?)`,
		webhook, keepFinished)
	if err != nil {
		return 0, 0, err
	}
	deleted, err = res.RowsAffected()
	return dead, deleted, err
}

// DeliveryCounts returns webhook's pending and dead delivery counts.
func (t *Tx) DeliveryCounts(webhook string) (pending, dead int64, err error) {
	err = t.row(`SELECT COALESCE(SUM(state = 'pending'), 0), COALESCE(SUM(state = 'dead'), 0) FROM webhook_deliveries WHERE webhook_id = ?`, webhook).Scan(&pending, &dead)
	return pending, dead, err
}

// ListDeliveries returns one page of webhook's deliveries, newest first,
// optionally in one state.
func (t *Tx) ListDeliveries(webhook, state string, p Page) ([]*DeliveryRecord, string, error) {
	c, err := p.decode()
	if err != nil {
		return nil, "", err
	}
	q := `SELECT doc, created_ns, id FROM webhook_deliveries WHERE webhook_id = ? AND (? = '' OR state = ?)`
	args := []any{webhook, state, state}
	if p.Cursor != "" {
		q += ` AND (created_ns, id) < (?, ?)`
		args = append(args, c.CreatedNS, c.ID)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	q += ` ORDER BY created_ns DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := t.query(q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*DeliveryRecord
	var next string
	for rows.Next() {
		var doc, id string
		var ns int64
		if err := rows.Scan(&doc, &ns, &id); err != nil {
			return nil, "", err
		}
		if len(out) == limit {
			prev := out[len(out)-1]
			next = encodeCursor(prev.CreatedAt.UnixNano(), prev.ID, p.Filter)
			break
		}
		var d DeliveryRecord
		if err := json.Unmarshal([]byte(doc), &d); err != nil {
			return nil, "", err
		}
		out = append(out, &d)
	}
	return out, next, rows.Err()
}

// CountByState returns live VMs per state and webhook deliveries per state
// (telemetry gauges).
func (t *Tx) CountByState() (vms, deliveries map[string]int64, err error) {
	count := func(q string) (map[string]int64, error) {
		rows, err := t.query(q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]int64{}
		for rows.Next() {
			var s string
			var n int64
			if err := rows.Scan(&s, &n); err != nil {
				return nil, err
			}
			out[s] = n
		}
		return out, rows.Err()
	}
	if vms, err = count(`SELECT state, COUNT(*) FROM vms WHERE state != 'deleted' GROUP BY state`); err != nil {
		return nil, nil, err
	}
	deliveries, err = count(`SELECT state, COUNT(*) FROM webhook_deliveries GROUP BY state`)
	return vms, deliveries, err
}

// AppendHostEvent appends an event that no table trigger produces (host
// housekeeping, e.g. golden.evicted) to owner's stream.
func (t *Tx) AppendHostEvent(owner domain.PrincipalID, typ, vmID, subject string, data map[string]any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO events (ns, owner, type, vm_id, subject, data) VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().UnixNano(), owner, typ, vmID, subject, string(b))
	return err
}
