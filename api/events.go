package api

import (
	"time"

	"davit/internal/domain"
)

// G16 automation events and webhooks.

// Event types. A webhook or listEvents filter names types or prefixes
// ending in "." ("vm.", "operation.").
const (
	EventVMCreated         = "vm.created"
	EventVMState           = "vm.state"
	EventOperationCreated  = "operation.created"
	EventOperationState    = "operation.state"
	EventOperationProgress = "operation.progress"
	EventRunCreated        = "run.created"
	EventRunState          = "run.state"
	EventPromptCreated     = "prompt.created"
	EventPromptState       = "prompt.state"
	// EventGoldenEvicted: the host evicted a golden (a booted memory snapshot
	// of an image or project image) to keep free disk space; the next create
	// that needs it captures it again.
	EventGoldenEvicted = "golden.evicted"
	// EventVMAutoDeleted: the host deleted an unused box to free disk space
	// (the low-disk cleanup); data is an AutoDeletion.
	EventVMAutoDeleted = "vm.autodeleted"
)

// Event is one entry of the host event stream. Seq orders events; Data is
// an allowlisted summary (IDs, states, error codes), never environment
// values, prompt text or secrets.
type Event struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Time    time.Time      `json:"time"`
	VMID    domain.VMID    `json:"vmId,omitempty"`
	Subject string         `json:"subject"` // the VM, operation, run or prompt ID
	Data    map[string]any `json:"data"`
}

// EventList is a page of events in Seq order. NextCursor continues after
// the last item (or where the stream was, when Items is empty).
type EventList struct {
	Items      []Event `json:"items"`
	NextCursor string  `json:"nextCursor"`
}

// Webhook delivery states.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryDead      = "dead"
)

// Webhook is a subscription that receives signed event deliveries. Its
// signing secret is never returned again after createWebhook or
// rotateWebhookSecret.
type Webhook struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Description string    `json:"description,omitempty"`
	Events      []string  `json:"events"` // types or prefixes ("vm."); empty = all
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	// PreviousSecretExpiresAt ends the rotation overlap: until then
	// deliveries carry signatures with both secrets.
	PreviousSecretExpiresAt *time.Time `json:"previousSecretExpiresAt,omitempty"`
	Pending                 int64      `json:"pending"`
	Dead                    int64      `json:"dead"`
}

// WebhookList is a page of webhooks.
type WebhookList struct {
	Items      []Webhook `json:"items"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// CreateWebhookRequest is the body of createWebhook.
type CreateWebhookRequest struct {
	URL         string   `json:"url"`
	Events      []string `json:"events,omitempty"`
	Description string   `json:"description,omitempty"`
	// Enabled defaults to true.
	Enabled *bool `json:"enabled,omitempty"`
}

// UpdateWebhookRequest is the body of updateWebhook; absent fields keep
// their value.
type UpdateWebhookRequest struct {
	URL         *string   `json:"url,omitempty"`
	Events      *[]string `json:"events,omitempty"`
	Description *string   `json:"description,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
}

// RotateWebhookSecretRequest is the body of rotateWebhookSecret.
type RotateWebhookSecretRequest struct {
	// OverlapSeconds keeps the old secret signing too, for receivers to
	// switch over (default 86400, at most 7 days; 0 drops it at once).
	OverlapSeconds *int64 `json:"overlapSeconds,omitempty"`
}

// WebhookSecret is a webhook with its signing secret, returned only by
// createWebhook and rotateWebhookSecret.
type WebhookSecret struct {
	Webhook Webhook `json:"webhook"`
	// Secret signs deliveries: X-Davit-Signature: t=<unix>,v1=<hex
	// HMAC-SHA256(secret, "<t>.<body>")>.
	Secret string `json:"secret"`
}

// WebhookDelivery is one event's delivery to one webhook.
type WebhookDelivery struct {
	ID            string     `json:"id"`
	WebhookID     string     `json:"webhookId"`
	EventSeq      int64      `json:"eventSeq"`
	EventType     string     `json:"eventType"`
	State         string     `json:"state"` // pending | delivered | dead
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"nextAttemptAt,omitempty"`
	LastStatus    int        `json:"lastStatus,omitempty"` // HTTP status of the last attempt
	LastError     string     `json:"lastError,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	DeliveredAt   *time.Time `json:"deliveredAt,omitempty"`
}

// WebhookDeliveryList is a page of deliveries, newest first.
type WebhookDeliveryList struct {
	Items      []WebhookDelivery `json:"items"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

// WebhookPayload is the JSON body of a delivery.
type WebhookPayload struct {
	DeliveryID string `json:"deliveryId"`
	WebhookID  string `json:"webhookId"`
	Event      Event  `json:"event"`
}

// Delivery headers.
const (
	HeaderWebhookSignature = "X-Davit-Signature"
	HeaderWebhookDelivery  = "X-Davit-Delivery"
	HeaderWebhookEvent     = "X-Davit-Event"
)
