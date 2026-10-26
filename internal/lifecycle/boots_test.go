package lifecycle

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
	"davit/internal/state"
)

// countingProber holds every boot's readiness probe briefly and records how
// many run at once.
type countingProber struct {
	inner          ReadinessProber
	inFlight, peak atomic.Int32
}

func (p *countingProber) Probe(ctx context.Context, rt runtime.Runtime, h runtime.Handle) (domain.Readiness, error) {
	n := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		peak := p.peak.Load()
		if n <= peak || p.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	return p.inner.Probe(ctx, rt, h)
}

// TestConcurrentBootsBounded: a batch of 6 with limits.concurrentBoots=2
// never has more than 2 boots in flight, the rest wait queued with an event
// saying why, and every VM still boots.
func TestConcurrentBootsBounded(t *testing.T) {
	prober := &countingProber{inner: RuntimeProber{Poll: time.Millisecond}}
	e := newEnv(t, fake.Options{}, func(c *Config) {
		c.Prober = prober
		c.Limits.ConcurrentBoots = 2
	})
	ctx := t.Context()
	res, err := e.svc.CreateVMs(ctx, e.req("batch-k"), api.CreateVMsRequest{Count: 6, Template: api.CreateVMRequest{Name: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	waited := 0
	for _, it := range res.Items {
		if it.Error != nil || it.Accepted == nil {
			t.Fatalf("item %d: %+v", it.Index, it.Error)
		}
		op := e.wait(it.Accepted.Operation.ID)
		if op.State != domain.OpSucceeded {
			t.Fatalf("item %d: %s %+v", it.Index, op.State, op.Error)
		}
		var evs []domain.OperationEvent
		e.svc.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) { evs, err = tx.Events(op.ID, 0, 100); return err })
		for _, ev := range evs {
			if strings.Contains(ev.Message, "waiting for a boot slot") {
				waited++
			}
		}
	}
	if p := prober.peak.Load(); p != 2 {
		t.Errorf("peak boots in flight = %d, want 2", p)
	}
	if waited == 0 {
		t.Error("no operation reported waiting for a boot slot")
	}
	if got := (domain.Limits{}).BootSlots(); got != domain.DefaultConcurrentBoots {
		t.Errorf("default boot slots %d", got)
	}
	if got := (domain.Limits{ConcurrentBoots: -1}).BootSlots(); got != 0 {
		t.Errorf("negative boot slots %d, want unlimited", got)
	}
}
