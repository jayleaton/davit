package events_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/events"
	"davit/internal/events/eventstest"
	"davit/internal/state"
)

func list(t *testing.T, h *eventstest.Host, q events.Query) *api.EventList {
	t.Helper()
	l, err := h.Events.List(context.Background(), eventstest.Owner, q)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func all(t *testing.T, h *eventstest.Host, q events.Query) []api.Event {
	t.Helper()
	var out []api.Event
	for {
		l := list(t, h, q)
		if len(l.Items) == 0 {
			return out
		}
		out = append(out, l.Items...)
		q.Cursor = l.NextCursor
	}
}

func TestStreamFollowsLifecycle(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	vm := h.CreateVM("web", nil)
	h.StopVM("web")
	evs := all(t, h, events.Query{Limit: 7})
	var types []string
	var last int64
	for _, e := range evs {
		if e.Seq <= last {
			t.Fatalf("seq not increasing: %d after %d", e.Seq, last)
		}
		last = e.Seq
		types = append(types, e.Type)
	}
	joined := strings.Join(types, " ")
	for _, want := range []string{"vm.created", "operation.created", "operation.progress", "operation.state", "vm.state"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("no %s in %s", want, joined)
		}
	}
	// The VM went provisioning -> ... -> ready -> ... -> stopped, in order.
	var states []string
	for _, e := range evs {
		if e.Type == api.EventVMState && e.VMID == vm {
			states = append(states, e.Data["state"].(string))
		}
	}
	if len(states) < 2 || states[len(states)-1] != string(domain.StateStopped) {
		t.Fatalf("vm states %v", states)
	}
	// Filters: VM events only, and a cursor bound to its filter.
	vmOnly := all(t, h, events.Query{Types: []string{"vm."}})
	for _, e := range vmOnly {
		if !strings.HasPrefix(e.Type, "vm.") {
			t.Fatalf("filtered stream has %s", e.Type)
		}
	}
	if len(vmOnly) == 0 || len(vmOnly) >= len(evs) {
		t.Fatalf("vm filter: %d of %d", len(vmOnly), len(evs))
	}
	page := list(t, h, events.Query{Types: []string{"vm."}, Limit: 1})
	if _, err := h.Events.List(context.Background(), eventstest.Owner, events.Query{Cursor: page.NextCursor}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("cursor under another filter: %v", err)
	}
	if other, _ := h.Events.List(context.Background(), "mallory", events.Query{}); len(other.Items) != 0 {
		t.Fatalf("another principal sees %d events", len(other.Items))
	}
}

func TestLongPollWakesOnNewEvent(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	start := list(t, h, events.Query{Latest: true})
	if len(start.Items) != 0 {
		t.Fatalf("start=latest returned %d old events", len(start.Items))
	}
	done := make(chan *api.EventList)
	go func() {
		l, _ := h.Events.List(context.Background(), eventstest.Owner, events.Query{Cursor: start.NextCursor, Wait: 10 * time.Second})
		done <- l
	}()
	time.Sleep(50 * time.Millisecond)
	t0 := time.Now()
	h.CreateVM("late", nil)
	select {
	case l := <-done:
		if len(l.Items) == 0 || time.Since(t0) > 5*time.Second {
			t.Fatalf("long-poll returned %d events after %s", len(l.Items), time.Since(t0))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("long-poll never returned")
	}
	// An empty wait returns at its deadline with the same cursor position.
	t0 = time.Now()
	l := list(t, h, events.Query{Latest: true, Wait: 100 * time.Millisecond})
	if len(l.Items) != 0 || time.Since(t0) < 90*time.Millisecond {
		t.Fatalf("idle wait: %d items after %s", len(l.Items), time.Since(t0))
	}
}

// TestEventsNeverCarrySecrets: env values (VM docs hold them) and prompt
// text never reach the stream.
func TestEventsNeverCarrySecrets(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	vm := h.CreateVM("sec", map[string]string{"API_KEY": "hunter2-secret-value"})
	err := h.DB.Tx(context.Background(), func(tx *state.Tx) error {
		if err := tx.PutConversation(&state.ConversationRecord{ID: "conv1", VMID: vm, Owner: eventstest.Owner, Harness: "claude", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
			return err
		}
		return tx.PutPrompt(&state.PromptRecord{ID: "p1", ConversationID: "conv1", VMID: vm, Owner: eventstest.Owner, Harness: "claude",
			Text: "please print the launch codes", EnvNames: []string{"API_KEY"}, State: "running", CreatedAt: time.Now()})
	})
	if err != nil {
		t.Fatal(err)
	}
	// A run whose stored document carries its spec (argv, env): the
	// trigger copies state, VM and exit code only.
	err = h.DB.Tx(context.Background(), func(tx *state.Tx) error {
		run := api.Run{ID: "run1", State: api.RunRunning, VMID: vm, CreatedAt: time.Now()}
		doc := struct {
			api.Run
			Spec api.RunRequest `json:"spec"`
		}{run, api.RunRequest{Argv: []string{"deploy", "--token=argv-secret"}, Env: map[string]string{"RUN_KEY": "run-env-secret"}}}
		return tx.PutRunDoc(eventstest.Owner, "", "", &run, doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	evs := all(t, h, events.Query{})
	raw, _ := json.Marshal(evs)
	for _, secret := range []string{"hunter2-secret-value", "launch codes", "API_KEY", "argv-secret", "run-env-secret", "RUN_KEY"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("event stream carries %q", secret)
		}
	}
	prompt, run := false, false
	for _, e := range evs {
		prompt = prompt || (e.Type == api.EventPromptCreated && e.Subject == "p1" && e.Data["harness"] == "claude")
		run = run || (e.Type == api.EventRunCreated && e.Subject == "run1" && e.VMID == vm && e.Data["state"] == string(api.RunRunning))
	}
	if !prompt || !run {
		t.Fatalf("prompt.created %v, run.created %v", prompt, run)
	}
}

// TestLongPollLimit: at most MaxWaitersPerCaller long-polls of one
// caller wait at once; the next is refused resource_exhausted (retryable),
// a list without a wait still answers, and a finished poll frees its slot.
func TestLongPollLimit(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	h.Events.MaxWaitersPerCaller = 2
	// A caller with no events: its polls wait their whole time.
	const idle domain.PrincipalID = "local:idle"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := h.Events.List(ctx, idle, events.Query{Latest: true, Wait: 20 * time.Second})
			done <- err
		}()
	}
	for deadline := time.Now().Add(5 * time.Second); h.Events.Waiting() < 2; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d polls waiting, want 2", h.Events.Waiting())
		}
	}
	_, err := h.Events.List(context.Background(), idle, events.Query{Latest: true, Wait: 50 * time.Millisecond})
	if !domain.IsCode(err, domain.CodeResourceExhausted) || !domain.AsError(err).Retryable {
		t.Fatalf("third waiting poll: %v", err)
	}
	if _, err := h.Events.List(context.Background(), idle, events.Query{Latest: true}); err != nil {
		t.Fatalf("a list without a wait: %v", err) // needs no slot
	}
	if _, err := h.Events.List(context.Background(), eventstest.Owner, events.Query{Latest: true, Wait: 10 * time.Millisecond}); err != nil {
		t.Fatalf("another caller's poll: %v", err) // the limit is per caller
	}
	cancel()
	<-done
	<-done
	if _, err := h.Events.List(context.Background(), idle, events.Query{Latest: true, Wait: 10 * time.Millisecond}); err != nil {
		t.Fatalf("poll after the slots freed: %v", err)
	}
}

// TestLongPollCapPerGrant: the per-caller cap counts each grant (token,
// member) and the owner's own session separately, although they share the
// owner's principal. A token holding all its slots does not refuse the
// owner's polls or another token's; the owner's grants together stop at
// MaxWaitersPerOwner, which the owner's own session is not counted in.
func TestLongPollCapPerGrant(t *testing.T) {
	h := eventstest.New(t, t.TempDir())
	h.Events.MaxWaitersPerCaller, h.Events.MaxWaitersPerOwner = 2, 3
	const idle domain.PrincipalID = "local:idle"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := 0
	hold := func(caller string) {
		waiting++
		go h.Events.List(ctx, idle, events.Query{Latest: true, Wait: 20 * time.Second, Caller: caller})
		for deadline := time.Now().Add(5 * time.Second); h.Events.Waiting() < waiting; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d polls waiting, want %d", h.Events.Waiting(), waiting)
			}
		}
	}
	poll := func(caller string) error {
		_, err := h.Events.List(context.Background(), idle, events.Query{Latest: true, Wait: 10 * time.Millisecond, Caller: caller})
		return err
	}
	hold("token:tok_a")
	hold("token:tok_a")
	if err := poll("token:tok_a"); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("a third poll of the full token: %v", err)
	}
	if err := poll(""); err != nil {
		t.Fatalf("the owner's poll while a token holds its slots: %v", err)
	}
	if err := poll("token:tok_b"); err != nil {
		t.Fatalf("another token's poll: %v", err)
	}
	hold("member:mem_c") // the owner's grants now hold 3 = MaxWaitersPerOwner
	if err := poll("token:tok_b"); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("a grant's poll beyond the per-owner ceiling: %v", err)
	}
	hold("") // the owner's own session is outside the ceiling
	hold("")
	if err := poll(""); !domain.IsCode(err, domain.CodeResourceExhausted) {
		t.Fatalf("the owner beyond its own per-caller cap: %v", err)
	}
}
