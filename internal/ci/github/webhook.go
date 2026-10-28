package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Webhook headers.
const (
	HeaderEvent     = "X-GitHub-Event"
	HeaderDelivery  = "X-GitHub-Delivery"
	HeaderSignature = "X-Hub-Signature-256"
)

// ErrSignature is a missing or wrong X-Hub-Signature-256.
var ErrSignature = errors.New("github: webhook signature does not match")

// VerifySignature checks X-Hub-Signature-256 ("sha256=" and the hex
// HMAC-SHA256 of the raw body under the webhook secret) in constant time.
func VerifySignature(secret, body []byte, header string) error {
	if len(secret) == 0 {
		return errors.New("github: no webhook secret configured")
	}
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return ErrSignature
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return ErrSignature
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	if !hmac.Equal(got, m.Sum(nil)) {
		return ErrSignature
	}
	return nil
}

// Sign returns the X-Hub-Signature-256 value of body (tests and tools).
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// WorkflowJobEvent is a workflow_job webhook delivery. Action is queued,
// in_progress, completed or waiting.
type WorkflowJobEvent struct {
	Action       string     `json:"action"`
	WorkflowJob  Job        `json:"workflow_job"`
	Repository   Repository `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}
