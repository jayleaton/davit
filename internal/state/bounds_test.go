package state

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
)

// TestBoundDeliveries: an endpoint that stays down cannot grow its outbox
// past maxPending (the oldest pending are dead-lettered with a reason,
// newest kept), and finished records are trimmed to keepFinished.
func TestBoundDeliveries(t *testing.T) {
	db := openT(t, filepath.Join(t.TempDir(), "s.db"))
	now := time.Unix(5000, 0).UTC()
	err := db.Tx(context.Background(), func(tx *Tx) error {
		if err := tx.PutWebhook(&WebhookRecord{ID: "wh", Owner: "owner", URL: "https://x", Enabled: true, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		for seq := int64(1); seq <= 10; seq++ {
			at := now.Add(time.Duration(seq) * time.Second)
			if err := tx.EnqueueDelivery(&DeliveryRecord{ID: fmt.Sprint("d", seq), WebhookID: "wh", EventSeq: seq,
				State: api.DeliveryPending, NextAt: at, CreatedAt: at, Event: []byte(`{}`)}); err != nil {
				return err
			}
		}
		dead, deleted, err := tx.BoundDeliveries("wh", 3, 2)
		if err != nil || dead != 7 || deleted != 5 {
			t.Fatalf("bound: dead %d deleted %d %v, want 7 and 5", dead, deleted, err)
		}
		pending, deadN, err := tx.DeliveryCounts("wh")
		if err != nil || pending != 3 || deadN != 2 {
			t.Fatalf("counts after the bound: pending %d dead %d %v", pending, deadN, err)
		}
		for _, id := range []string{"d8", "d9", "d10"} {
			if d, err := tx.GetDelivery("wh", id); err != nil || d.State != api.DeliveryPending {
				t.Fatalf("newest %s: %+v %v", id, d, err)
			}
		}
		for _, id := range []string{"d6", "d7"} { // the newest dead-lettered stay visible
			d, err := tx.GetDelivery("wh", id)
			if err != nil || d.State != api.DeliveryDead || !strings.Contains(d.LastError, "outbox full") {
				t.Fatalf("dead-lettered %s: %+v %v", id, d, err)
			}
		}
		if dead, deleted, err := tx.BoundDeliveries("wh", 3, 2); err != nil || dead+deleted != 0 {
			t.Fatalf("second bound: %d %d %v", dead, deleted, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
