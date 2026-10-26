//go:build linux

package harness_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/harness"
	"davit/internal/state"
)

func (e *env) prompt(ctx context.Context, req api.PromptRequest) api.Prompt {
	e.t.Helper()
	p, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), req)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// running waits until turn id's harness has its first message.
func (e *env) running(ctx context.Context, id string) {
	e.t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if l, _ := e.svc.Events(ctx, "owner", "box", id, 0, 0); l.Prompt.State == api.PromptRunning && len(l.Items) > 0 {
			return
		}
	}
	e.t.Fatalf("prompt %s never ran", id)
}

func wantCode(t *testing.T, what string, err error, code domain.Code) {
	t.Helper()
	if !domain.IsCode(err, code) {
		t.Errorf("%s: err = %v, want %s", what, err, code)
	}
}

func feedKeys(items []api.PromptFeedItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, fmt.Sprintf("%s/%d", it.Event.PromptID, it.Event.Seq))
	}
	return out
}

// TestPromptFeed: the VM-wide feed replays every turn's events in recorded
// order, pages and resumes by cursor (also across a daemon restart),
// filters by conversation and long-polls for new events.
func TestPromptFeed(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	p1 := e.prompt(ctx, api.PromptRequest{Text: "first"})
	_, ev1 := e.wait(ctx, p1.ID)
	p2 := e.prompt(ctx, api.PromptRequest{Text: "second", Harness: "codex"})
	_, ev2 := e.wait(ctx, p2.ID)

	all, err := e.svc.Feed(ctx, "owner", "box", "", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, ev := range append(ev1, ev2...) {
		want = append(want, fmt.Sprintf("%s/%d", ev.PromptID, ev.Seq))
	}
	if got := feedKeys(all.Items); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("feed\n got %v\nwant %v", got, want)
	}
	if all.NextCursor != all.Items[len(all.Items)-1].Cursor {
		t.Fatalf("nextCursor %q is not the last item's", all.NextCursor)
	}

	// Pages of 3 by cursor give the same sequence.
	var paged []api.PromptFeedItem
	cursor := ""
	for {
		pg, err := e.svc.Feed(ctx, "owner", "box", cursor, "", 3, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(pg.Items) == 0 {
			if pg.NextCursor != cursor {
				t.Fatalf("empty page moved the cursor: %q -> %q", cursor, pg.NextCursor)
			}
			break
		}
		paged = append(paged, pg.Items...)
		cursor = pg.NextCursor
	}
	if strings.Join(feedKeys(paged), " ") != strings.Join(want, " ") {
		t.Fatalf("paged feed %v", feedKeys(paged))
	}

	// One conversation only.
	conv, err := e.svc.Feed(ctx, "owner", "box", "", p2.ConversationID, 0, 0)
	if err != nil || strings.Join(feedKeys(conv.Items), " ") != strings.Join(want[len(ev1):], " ") {
		t.Fatalf("conversation feed %v %v", feedKeys(conv.Items), err)
	}

	// Reconnect: a cursor from before a daemon restart still resumes.
	mid := all.Items[2].Cursor
	e.svc.Close()
	e.start()
	rest, err := e.svc.Feed(ctx, "owner", "box", mid, "", 0, 0)
	if err != nil || strings.Join(feedKeys(rest.Items), " ") != strings.Join(want[3:], " ") {
		t.Fatalf("after restart from %s: %v %v", mid, feedKeys(rest.Items), err)
	}

	// Long-poll: nothing new times out empty; a new turn wakes it.
	start := time.Now()
	idle, err := e.svc.Feed(ctx, "owner", "box", all.NextCursor, "", 0, 100*time.Millisecond)
	if err != nil || len(idle.Items) != 0 || idle.NextCursor != all.NextCursor || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("idle long-poll %+v %v after %s", idle, err, time.Since(start))
	}
	got := make(chan api.PromptFeed, 1)
	go func() {
		f, err := e.svc.Feed(ctx, "owner", "box", all.NextCursor, "", 0, 10*time.Second)
		if err != nil {
			t.Error(err)
		}
		got <- f
	}()
	time.Sleep(50 * time.Millisecond)
	p3 := e.prompt(ctx, api.PromptRequest{Text: "third", New: true})
	select {
	case f := <-got:
		if len(f.Items) == 0 || f.Items[0].Event.PromptID != p3.ID {
			t.Fatalf("long-poll woke with %v", feedKeys(f.Items))
		}
	case <-ctx.Done():
		t.Fatal("long-poll never woke")
	}
	e.wait(ctx, p3.ID)

	other := "pf1." + base64.RawURLEncoding.EncodeToString([]byte("vm_other:1"))
	for _, c := range []string{"garbage", "pf1.!!", other} {
		_, err := e.svc.Feed(ctx, "owner", "box", c, "", 0, 0)
		wantCode(t, "cursor "+c, err, domain.CodeInvalidArgument)
	}
	_, err = e.svc.Feed(ctx, "owner", "box", "", "conv_nope", 0, 0)
	wantCode(t, "unknown conversation", err, domain.CodeNotFound)
	_, err = e.svc.Feed(ctx, "intruder", "box", "", "", 0, 0)
	wantCode(t, "other principal", err, domain.CodeNotFound)
}

// TestInterrupt: one conversation's active turn, then all of them; a
// repeat cancels nothing.
func TestInterrupt(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	a := e.prompt(ctx, api.PromptRequest{Text: "slow a", New: true})
	b := e.prompt(ctx, api.PromptRequest{Text: "slow b", Harness: "codex", New: true})
	c := e.prompt(ctx, api.PromptRequest{Text: "slow c", New: true})

	// Interrupted right away, possibly before the harness ran.
	res, err := e.svc.Interrupt(ctx, "owner", "box", api.InterruptRequest{ConversationID: a.ConversationID})
	if err != nil || len(res.Cancelled) != 1 || res.Cancelled[0].ID != a.ID || res.Cancelled[0].State != api.PromptCancelled ||
		res.Cancelled[0].Error.Code != harness.ErrCancelled {
		t.Fatalf("interrupt a: %+v %v", res, err)
	}
	if p, _ := e.svc.Get(ctx, "owner", "box", b.ID); p.State == api.PromptCancelled {
		t.Fatal("interrupting a cancelled b")
	}
	res, err = e.svc.Interrupt(ctx, "owner", "box", api.InterruptRequest{All: true})
	if err != nil || len(res.Cancelled) != 2 {
		t.Fatalf("interrupt all: %+v %v", res, err)
	}
	for _, p := range res.Cancelled {
		if (p.ID != b.ID && p.ID != c.ID) || p.State != api.PromptCancelled {
			t.Fatalf("interrupt all returned %+v", p)
		}
	}
	if res, err := e.svc.Interrupt(ctx, "owner", "box", api.InterruptRequest{All: true}); err != nil || len(res.Cancelled) != 0 {
		t.Fatalf("repeat: %+v %v", res, err)
	}
	// The conversations are free again.
	if done, _ := e.wait(ctx, e.prompt(ctx, api.PromptRequest{Text: "after", ConversationID: b.ConversationID}).ID); done.State != api.PromptSucceeded {
		t.Fatalf("after interrupt: %+v", done)
	}

	_, err = e.svc.Interrupt(ctx, "owner", "box", api.InterruptRequest{})
	wantCode(t, "neither", err, domain.CodeInvalidArgument)
	_, err = e.svc.Interrupt(ctx, "owner", "box", api.InterruptRequest{All: true, ConversationID: a.ConversationID})
	wantCode(t, "both", err, domain.CodeInvalidArgument)
	_, err = e.svc.Interrupt(ctx, "owner", "box", api.InterruptRequest{ConversationID: "conv_nope"})
	wantCode(t, "unknown conversation", err, domain.CodeNotFound)
}

// TestTurnTimeoutAndLease: a turn holds the VM's auto-stop lease until its
// deadline; at the deadline the guest kills the harness and the turn fails
// with turn_timeout, releasing the lease.
func TestTurnTimeoutAndLease(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	if until, err := e.svc.TurnLease(ctx, "vm_h"); err != nil || !until.IsZero() {
		t.Fatalf("idle lease %v %v", until, err)
	}
	p := e.prompt(ctx, api.PromptRequest{Text: "hang", TurnTimeoutSeconds: 1})
	if p.TurnDeadlineAt == nil || p.TurnDeadlineAt.Sub(p.CreatedAt) != time.Second {
		t.Fatalf("deadline %v (created %v)", p.TurnDeadlineAt, p.CreatedAt)
	}
	if until, err := e.svc.TurnLease(ctx, "vm_h"); err != nil || !until.Equal(*p.TurnDeadlineAt) {
		t.Fatalf("lease while running %v %v", until, err)
	}
	if until, _ := e.svc.TurnLease(ctx, "vm_other"); !until.IsZero() {
		t.Fatalf("another VM is leased until %v", until)
	}
	done, evs := e.wait(ctx, p.ID)
	if done.State != api.PromptFailed || done.Error.Code != harness.ErrTimeout || evs[len(evs)-1].Error.Code != harness.ErrTimeout {
		t.Fatalf("timed-out turn %+v %+v", done, done.Error)
	}
	if until, _ := e.svc.TurnLease(ctx, "vm_h"); !until.IsZero() {
		t.Fatalf("lease after the turn ended: %v", until)
	}
	// Without turnTimeoutSeconds the default bounds the turn.
	q := e.prompt(ctx, api.PromptRequest{Text: "quick"})
	if q.TurnDeadlineAt == nil || q.TurnDeadlineAt.Sub(q.CreatedAt) != time.Hour {
		t.Fatalf("default deadline %v", q.TurnDeadlineAt)
	}
	e.wait(ctx, q.ID)
	for _, s := range []int64{-1, 86401} {
		_, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "x", TurnTimeoutSeconds: s})
		wantCode(t, fmt.Sprint("turnTimeoutSeconds ", s), err, domain.CodeInvalidArgument)
	}
}

// TestAttachments: inline data lands in a per-turn guest directory; guest
// paths are used in place; images reach each harness natively (Claude
// inline, Codex --image) and every file is named in the prompt.
func TestAttachments(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	dir := t.TempDir()
	img, notes := filepath.Join(dir, "shot.png"), filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(img, []byte("PNG-ON-DISK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notes, []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	atts := []api.PromptAttachment{
		{Name: "inline.png", MediaType: "image/png", Data: b64("PNG-INLINE")},
		{Name: "shot.png", MediaType: "image/png", Path: img},
		{Name: "spec.md", MediaType: "text/markdown; charset=utf-8", Data: b64("# spec")},
		{Name: "notes.txt", MediaType: "text/plain", Path: notes},
	}
	for _, h := range harness.Names() {
		p := e.prompt(ctx, api.PromptRequest{Text: "see attached", Harness: h, New: true, Attachments: atts})
		done, evs := e.wait(ctx, p.ID)
		if done.State != api.PromptSucceeded {
			t.Fatalf("%s: %+v %+v", h, done, done.Error)
		}
		placed := filepath.Join(e.cfg.AttachmentDir, "davit-"+p.ID)
		for name, content := range map[string]string{"inline.png": "PNG-INLINE", "spec.md": "# spec"} {
			if b, err := os.ReadFile(filepath.Join(placed, name)); err != nil || string(b) != content {
				t.Fatalf("%s: placed %s = %q %v", h, name, b, err)
			}
		}
		out := text(evs)
		for _, line := range []string{placed + "/inline.png (image/png)", img + " (image/png)", placed + "/spec.md (text/markdown)", notes + " (text/plain)"} {
			if !strings.Contains(out, line) {
				t.Fatalf("%s: prompt does not name %q: %q", h, line, out)
			}
		}
		want := "images=2" // claude: two inline image blocks
		if h != "claude" { // image paths on the command line
			want = "images=" + placed + "/inline.png " + img
		}
		if !strings.Contains(out, want) {
			t.Fatalf("%s: want %q in %q", h, want, out)
		}
	}

	// A guest path that does not exist fails the turn, typed.
	p := e.prompt(ctx, api.PromptRequest{Text: "x", New: true, Attachments: []api.PromptAttachment{{Name: "gone.txt", MediaType: "text/plain", Path: filepath.Join(dir, "gone.txt")}}})
	if p.State != api.PromptFailed || p.Error.Code != harness.ErrAttachment {
		t.Fatalf("missing path: %+v %+v", p, p.Error)
	}
	for what, a := range map[string]api.PromptAttachment{
		"bad base64":   {Name: "a.png", MediaType: "image/png", Data: "%%%"},
		"both":         {Name: "a.png", MediaType: "image/png", Data: b64("x"), Path: img},
		"neither":      {Name: "a.png", MediaType: "image/png"},
		"slash name":   {Name: "../a.png", MediaType: "image/png", Data: b64("x")},
		"relative":     {Name: "a.png", MediaType: "image/png", Path: "a.png"},
		"no mediaType": {Name: "a.png", Data: b64("x")},
	} {
		_, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "x", Attachments: []api.PromptAttachment{a}})
		wantCode(t, what, err, domain.CodeInvalidArgument)
	}
	dup := []api.PromptAttachment{{Name: "a.txt", MediaType: "text/plain", Data: b64("1")}, {Name: "a.txt", MediaType: "text/plain", Data: b64("2")}}
	_, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "x", Attachments: dup})
	wantCode(t, "duplicate name", err, domain.CodeInvalidArgument)
	_, err = e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "x", Attachments: []api.PromptAttachment{{Name: "a.zip", MediaType: "application/zip", Data: b64("PK")}}})
	wantCode(t, "unlisted type", err, domain.CodeUnsupportedCapability)
}

// TestOptions: reasoning effort and fast mode reach each harness as its
// own CLI flags; values a harness does not list are refused.
func TestOptions(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	for h, effort := range map[string]string{"claude": "max", "codex": "minimal"} {
		p := e.prompt(ctx, api.PromptRequest{Text: "think", Harness: h, New: true, Options: &api.PromptOptions{ReasoningEffort: effort, Fast: true}})
		done, evs := e.wait(ctx, p.ID)
		if want := "effort=" + effort + " fast=yes"; done.State != api.PromptSucceeded || !strings.Contains(text(evs), want) {
			t.Fatalf("%s: want %q in %q (%+v)", h, want, text(evs), done.Error)
		}
	}
	for h, effort := range map[string]string{"claude": "minimal", "codex": "max", "pi": "none", "opencode": "off"} {
		_, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "x", Harness: h, Options: &api.PromptOptions{ReasoningEffort: effort}})
		wantCode(t, h+" effort "+effort, err, domain.CodeInvalidArgument)
	}
	// OpenCode (--variant) and pi (--thinking) take an effort but have no
	// fast mode.
	for h, effort := range map[string]string{"opencode": "max", "pi": "off"} {
		p := e.prompt(ctx, api.PromptRequest{Text: "think", Harness: h, New: true, Options: &api.PromptOptions{ReasoningEffort: effort}})
		done, evs := e.wait(ctx, p.ID)
		if want := "effort=" + effort + " "; done.State != api.PromptSucceeded || !strings.Contains(text(evs), want) {
			t.Fatalf("%s: want %q in %q (%+v)", h, want, text(evs), done.Error)
		}
		_, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "x", Harness: h, New: true, Options: &api.PromptOptions{Fast: true}})
		wantCode(t, h+" fast", err, domain.CodeUnsupportedCapability)
	}
}

// TestSteer: Claude takes a message in the running turn (native); Codex
// cannot, so its turn is cancelled and the conversation resumes with the
// message (restart). Each says which happened.
func TestSteer(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)

	p := e.prompt(ctx, api.PromptRequest{Text: "slow start", New: true})
	e.running(ctx, p.ID)
	res, err := e.svc.Steer(ctx, "owner", "box", p.ID, "", "also this")
	if err != nil || res.Delivery != domain.SteerNative || res.Prompt.ID != p.ID {
		t.Fatalf("claude steer: %+v %v", res, err)
	}
	done, evs := e.wait(ctx, p.ID)
	out := text(evs)
	if done.State != api.PromptSucceeded || !strings.Contains(out, "echo: slow start") || !strings.Contains(out, "echo: also this") ||
		!strings.Contains(out, "msg=2") || strings.Count(kinds(evs), "turn.completed") != 1 || done.Usage.InputTokens != 6 {
		t.Fatalf("steered turn %+v %q %s", done, out, kinds(evs))
	}
	_, err = e.svc.Steer(ctx, "owner", "box", p.ID, "", "too late")
	wantCode(t, "finished turn", err, domain.CodeInvalidState)
	_, err = e.svc.Steer(ctx, "owner", "box", p.ID, "", " ")
	wantCode(t, "empty text", err, domain.CodeInvalidArgument)

	q := e.prompt(ctx, api.PromptRequest{Text: "slow job", Harness: "codex", New: true, Options: &api.PromptOptions{ReasoningEffort: "high"}})
	e.running(ctx, q.ID)
	res, err = e.svc.Steer(ctx, "owner", "box", q.ID, "", "change course")
	if err != nil || res.Delivery != domain.SteerRestart || res.Prompt.ID == q.ID || res.Prompt.ConversationID != q.ConversationID {
		t.Fatalf("codex steer: %+v %v", res, err)
	}
	old, _ := e.svc.Get(ctx, "owner", "box", q.ID)
	if old.State != api.PromptCancelled || !strings.Contains(old.Error.Message, "restart") {
		t.Fatalf("restarted turn %+v %+v", old, old.Error)
	}
	conv, _ := e.svc.Conversation(ctx, "owner", "box", q.ConversationID)
	done, evs = e.wait(ctx, res.Prompt.ID)
	if done.State != api.PromptSucceeded || !strings.Contains(text(evs), "echo: change course resumed="+conv.SessionID+" effort=high") {
		t.Fatalf("restart turn %+v %q (session %s)", done, text(evs), conv.SessionID)
	}

	// A recovered Claude turn has lost its stdin: the steer restarts, and
	// says so.
	r := e.prompt(ctx, api.PromptRequest{Text: "slow again", New: true})
	e.running(ctx, r.ID)
	e.svc.Close()
	e.start()
	res, err = e.svc.Steer(ctx, "owner", "box", r.ID, "", "after restart")
	if err != nil || res.Delivery != domain.SteerRestart {
		t.Fatalf("steer after daemon restart: %+v %v", res, err)
	}
	if done, evs := e.wait(ctx, res.Prompt.ID); done.State != api.PromptSucceeded || !strings.Contains(text(evs), "echo: after restart resumed=sess-") {
		t.Fatalf("restart after recovery %+v %q", done, text(evs))
	}
}

// TestCredentialGrantsInjected: a VM's active grants reach the harness as
// placeholders with the provider base URL pointing at the credential
// proxy; they take precedence over caller env, and the turn reports the
// override as its first event. Without a proxy, grants are not injected.
func TestCredentialGrantsInjected(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	const proxy = "http://172.16.0.1:15080"
	put := func(g domain.CredentialGrant) {
		t.Helper()
		g.VMID, g.ProfileID, g.CreatedAt = "vm_h", "prof_1", time.Now()
		time.Sleep(time.Millisecond) // distinct creation order
		if err := e.db.Tx(ctx, func(tx *state.Tx) error { return tx.PutGrant("owner", &g) }); err != nil {
			t.Fatal(err)
		}
	}
	put(domain.CredentialGrant{ID: "grant_old", Credential: "claude", State: domain.GrantRevoked, Placeholder: "ph-revoked",
		EnvVar: "ANTHROPIC_API_KEY", Upstream: "https://api.anthropic.com", RevokeReason: domain.RevokeExplicit})
	put(domain.CredentialGrant{ID: "grant_a", Credential: "claude", State: domain.GrantActive, Placeholder: "ph-anthropic",
		EnvVar: "ANTHROPIC_API_KEY", Upstream: "https://api.anthropic.com"})
	put(domain.CredentialGrant{ID: "grant_o", Credential: "codex", State: domain.GrantActive, Placeholder: "ph-openai",
		EnvVar: "OPENAI_API_KEY", Upstream: "https://api.openai.com"})

	// No proxy configured: grants are not injected; caller env is used.
	p := e.prompt(ctx, api.PromptRequest{Text: "hi", New: true, Env: map[string]string{"ANTHROPIC_API_KEY": "sk-caller"}})
	if _, evs := e.wait(ctx, p.ID); !strings.Contains(text(evs), "key=sk-caller base=") {
		t.Fatalf("without a proxy: %q", text(evs))
	}

	e.svc.Close()
	e.cfg.CredentialProxyURL = proxy
	e.start()
	// The grant replaces the caller's key, and the turn says so first.
	p = e.prompt(ctx, api.PromptRequest{Text: "hi", New: true, Env: map[string]string{"ANTHROPIC_API_KEY": "sk-caller"}})
	done, evs := e.wait(ctx, p.ID)
	out := text(evs)
	if done.State != api.PromptSucceeded || !strings.Contains(out, "key=ph-anthropic base="+proxy) || strings.Contains(out, "sk-caller") {
		t.Fatalf("claude with grants: %+v %q", done, out)
	}
	raw, _ := json.Marshal(evs[0].Raw)
	if evs[0].Seq != 1 || evs[0].Type != api.PromptEventRaw || !strings.Contains(string(raw), `"code":"credential_env_conflict"`) ||
		!strings.Contains(string(raw), `"overridden":["ANTHROPIC_API_KEY"]`) || strings.Contains(string(raw), "sk-caller") ||
		evs[1].Seq != 2 || evs[1].Type != api.PromptEventConversationStarted {
		t.Fatalf("notice %s then %+v", raw, evs[1])
	}
	// Codex: OpenAI base URL with /v1; no conflict, no notice.
	q := e.prompt(ctx, api.PromptRequest{Text: "hi", Harness: "codex", New: true})
	done, evs = e.wait(ctx, q.ID)
	if done.State != api.PromptSucceeded || !strings.Contains(text(evs), "key=ph-openai base="+proxy+"/v1") || evs[0].Type != api.PromptEventConversationStarted {
		t.Fatalf("codex with grants: %+v %q %s", done, text(evs), kinds(evs))
	}

	// OpenCode reads endpoints only from config: the proxy URLs travel in
	// OPENCODE_CONFIG_CONTENT.
	q = e.prompt(ctx, api.PromptRequest{Text: "hi", Harness: "opencode", New: true})
	done, evs = e.wait(ctx, q.ID)
	if out := text(evs); done.State != api.PromptSucceeded || !strings.Contains(out, "key=ph-anthropic ") ||
		!strings.Contains(out, `"anthropic":{"options":{"baseURL":"`+proxy+`/v1"}}`) || !strings.Contains(out, `"openai":{"options":{"baseURL":"`+proxy+`/v1"}}`) {
		t.Fatalf("opencode with grants: %+v %q", done, out)
	}
	// pi cannot use the proxy: no grant variables, the caller's key is
	// kept, and the turn says why first.
	q = e.prompt(ctx, api.PromptRequest{Text: "hi", Harness: "pi", New: true, Env: map[string]string{"ANTHROPIC_API_KEY": "sk-caller"}})
	done, evs = e.wait(ctx, q.ID)
	raw, _ = json.Marshal(evs[0].Raw)
	if done.State != api.PromptSucceeded || !strings.Contains(text(evs), "key=sk-caller") ||
		!strings.Contains(string(raw), `"code":"credential_grants_unsupported"`) || !strings.Contains(string(raw), "grant_a") {
		t.Fatalf("pi with grants: %+v %q notice %s", done, text(evs), raw)
	}

	// Two active grants for one variable: the newest wins, reported.
	put(domain.CredentialGrant{ID: "grant_b", Credential: "claude2", State: domain.GrantActive, Placeholder: "ph-anthropic-2",
		EnvVar: "ANTHROPIC_API_KEY", Upstream: "https://api.anthropic.com"})
	r := e.prompt(ctx, api.PromptRequest{Text: "hi", New: true})
	_, evs = e.wait(ctx, r.ID)
	raw, _ = json.Marshal(evs[0].Raw)
	if !strings.Contains(text(evs), "key=ph-anthropic-2 ") || !strings.Contains(string(raw), "grant grant_b replaces grant grant_a") {
		t.Fatalf("newest grant: %q notice %s", text(evs), raw)
	}
}

// TestSteerIdempotencyKey: a keyed steer delivers once; its repeat returns
// the first result (also after the turn ended or was restarted), and the
// key with other text is a conflict.
func TestSteerIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	p := e.prompt(ctx, api.PromptRequest{Text: "slow start", New: true})
	e.running(ctx, p.ID)
	r1, err := e.svc.Steer(ctx, "owner", "box", p.ID, "k1", "also this")
	if err != nil || r1.Delivery != domain.SteerNative {
		t.Fatalf("first: %+v %v", r1, err)
	}
	r2, err := e.svc.Steer(ctx, "owner", "box", p.ID, "k1", "also this")
	if err != nil || r2.Delivery != domain.SteerNative || r2.Prompt.ID != p.ID {
		t.Fatalf("repeat: %+v %v", r2, err)
	}
	_, err = e.svc.Steer(ctx, "owner", "box", p.ID, "k1", "something else")
	wantCode(t, "same key, other text", err, domain.CodeIdempotencyConflict)
	done, evs := e.wait(ctx, p.ID)
	if out := text(evs); done.State != api.PromptSucceeded || !strings.Contains(out, "msg=2") || strings.Contains(out, "msg=3") {
		t.Fatalf("delivered more than once: %q", out)
	}
	if r3, err := e.svc.Steer(ctx, "owner", "box", p.ID, "k1", "also this"); err != nil || r3.Prompt.ID != p.ID || r3.Prompt.State != api.PromptSucceeded {
		t.Fatalf("repeat after the turn: %+v %v", r3, err)
	}

	q := e.prompt(ctx, api.PromptRequest{Text: "slow job", Harness: "codex", New: true})
	e.running(ctx, q.ID)
	a, err := e.svc.Steer(ctx, "owner", "box", q.ID, "k2", "change course")
	if err != nil || a.Delivery != domain.SteerRestart {
		t.Fatalf("restart: %+v %v", a, err)
	}
	b, err := e.svc.Steer(ctx, "owner", "box", q.ID, "k2", "change course")
	if err != nil || b.Delivery != domain.SteerRestart || b.Prompt.ID != a.Prompt.ID {
		t.Fatalf("restart repeat started another turn: %+v %v", b, err)
	}
	e.wait(ctx, a.Prompt.ID)
	if c, _, _ := e.svc.Conversations(ctx, "owner", "box", 0, ""); len(c) != 2 || c[1].Turns != 2 {
		t.Fatalf("conversations %+v", c)
	}
}
