//go:build linux

package harness_test

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestagent"
	"davit/internal/guestclient"
	"davit/internal/guestexec"
	"davit/internal/guestproto"
	"davit/internal/harness"
	"davit/internal/lifecycle"
	"davit/internal/runtime"
	"davit/internal/state"
)

type nopSystem struct{}

func (nopSystem) SetClock(time.Time) error                     { return nil }
func (nopSystem) SetHostname(string) error                     { return nil }
func (nopSystem) RestartUnits(context.Context, []string) error { return nil }
func (nopSystem) SystemState(context.Context) guestproto.ServiceState {
	return guestproto.ServiceUnknown
}
func (nopSystem) UnitState(context.Context, string) guestproto.ServiceState {
	return guestproto.ServiceUnknown
}
func (nopSystem) SSHReady(context.Context) bool  { return false }
func (nopSystem) PowerOff(context.Context) error { return nil }

// guest is one VM "vm_h" whose agent runs in-process.
type guest struct {
	agent *guestagent.Agent
	mu    sync.Mutex
	key   runtime.Key
}

func (g *guest) DialGuest(context.Context, uint32) (net.Conn, error) {
	a, b := net.Pipe()
	go g.agent.ServeConn(context.Background(), b)
	return a, nil
}

func (g *guest) GuestTarget(_ context.Context, p domain.PrincipalID, ref string) (lifecycle.GuestTarget, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p != "owner" || (ref != "vm_h" && ref != "box") {
		return lifecycle.GuestTarget{}, domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
	}
	return lifecycle.GuestTarget{Key: g.key, Dialer: g}, nil
}

func (g *guest) GetVM(ctx context.Context, p domain.PrincipalID, ref string) (*domain.VM, error) {
	t, err := g.GuestTarget(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	return &domain.VM{ID: t.Key.VMID}, nil
}

type env struct {
	t     *testing.T
	db    *state.DB
	g     *guest
	procs *guestexec.Manager
	svc   *harness.Service
	cfg   harness.Config
}

func fixture(name string) string {
	p, _ := filepath.Abs(filepath.Join("testdata", name))
	return p
}

func newEnv(t *testing.T) *env {
	t.Helper()
	a, err := guestagent.New(guestagent.Config{Root: t.TempDir(), RunDir: t.TempDir(), System: nopSystem{}, SSHUnit: "-"})
	if err != nil {
		t.Fatal(err)
	}
	g := &guest{agent: a, key: runtime.Key{VMID: "vm_h", Generation: 1}}
	secrets := &guestclient.MemorySecrets{}
	p := &guestclient.Prober{Secrets: secrets, Poll: time.Millisecond,
		Plan: func(context.Context, runtime.Key) (guestclient.Plan, error) {
			return guestclient.Plan{Hostname: "box"}, nil
		}}
	if _, err := p.Barrier(context.Background(), g.key, func(ctx context.Context) (net.Conn, error) { return g.DialGuest(ctx, 0) }, nil); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now()
	if err := db.Tx(context.Background(), func(tx *state.Tx) error {
		return tx.PutVM(&state.VMRecord{VM: domain.VM{ID: "vm_h", Owner: "owner", Name: "box", State: domain.StateReady,
			InstanceGeneration: 1, BootMode: domain.BootModeColdBoot, CreatedAt: now, UpdatedAt: now}})
	}); err != nil {
		t.Fatal(err)
	}
	procs := guestexec.New(guestexec.Config{Targets: g, Secrets: secrets, Store: guestexec.DBStore{DB: db}})
	t.Cleanup(procs.Close)
	e := &env{t: t, db: db, g: g, procs: procs}
	e.cfg = harness.Config{DB: db, Procs: procs, VMs: g, AttachmentDir: t.TempDir(),
		Commands: map[string]string{"claude": fixture("fake-claude"), "codex": fixture("fake-codex"),
			"opencode": fixture("fake-opencode"), "pi": fixture("fake-pi")},
		Secrets: func(name string) (string, bool, error) {
			if name == "FAKE_TOKEN" {
				return "from-daemon-secret", true, nil
			}
			return "", false, nil
		}}
	e.start()
	return e
}

func (e *env) start() {
	e.svc = harness.New(e.cfg)
	if err := e.svc.Start(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(e.svc.Close)
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// wait follows a turn to the end and returns its events.
func (e *env) wait(ctx context.Context, id string) (api.Prompt, []api.PromptEvent) {
	e.t.Helper()
	var all []api.PromptEvent
	var after int64
	for {
		l, err := e.svc.Events(ctx, "owner", "box", id, after, 5*time.Second)
		if err != nil {
			e.t.Fatal(err)
		}
		all = append(all, l.Items...)
		if n := len(l.Items); n > 0 {
			after = l.Items[n-1].Seq
		}
		if l.Prompt.State != api.PromptStarting && l.Prompt.State != api.PromptRunning && len(l.Items) == 0 {
			return l.Prompt, all
		}
	}
}

func kinds(evs []api.PromptEvent) string {
	var out []string
	for _, e := range evs {
		if e.Type != api.PromptEventRaw {
			out = append(out, e.Type)
		}
	}
	return strings.Join(out, ",")
}

func text(evs []api.PromptEvent) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Type == api.PromptEventMessageDelta {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

func TestConversationResumesNativeSession(t *testing.T) {
	for _, h := range harness.Names() {
		t.Run(h, func(t *testing.T) {
			e := newEnv(t)
			ctx := ctxT(t)
			p1, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "first", Harness: h, Env: map[string]string{"FAKE_TOKEN": "from-caller"}})
			if err != nil {
				t.Fatal(err)
			}
			done, evs := e.wait(ctx, p1.ID)
			if done.State != api.PromptSucceeded || done.Usage == nil || done.Usage.OutputTokens == 0 {
				t.Fatalf("turn 1: %+v %+v", done, done.Error)
			}
			want := "conversation.started,message.delta,tool.call,tool.result,turn.completed"
			if h != "claude" {
				want = "conversation.started,tool.call,tool.result,message.delta,turn.completed"
			}
			if got := kinds(evs); got != want {
				t.Fatalf("events %s, want %s", got, want)
			}
			for i, ev := range evs {
				if ev.Seq != int64(i+1) || ev.PromptID != p1.ID || ev.ConversationID != p1.ConversationID {
					t.Fatalf("event %d: %+v", i, ev)
				}
				if ev.Type != api.PromptEventTurnCompleted && ev.Raw == nil {
					t.Fatalf("event %s lost its raw line", ev.Type)
				}
			}
			if last := evs[len(evs)-1]; last.Status != "succeeded" || last.Usage == nil {
				t.Fatalf("turn.completed %+v", last)
			}
			if !strings.Contains(text(evs), "echo: first resumed=none") {
				t.Fatalf("text %q", text(evs))
			}
			if h == "claude" && !strings.Contains(text(evs), "token=from-caller") {
				t.Fatalf("caller env not passed: %q", text(evs))
			}
			conv, err := e.svc.Conversation(ctx, "owner", "box", p1.ConversationID)
			if err != nil || conv.SessionID == "" || conv.ActivePromptID != "" || conv.Turns != 1 {
				t.Fatalf("conversation %+v %v", conv, err)
			}

			// Turn 2 (no flags) continues the same conversation and resumes
			// the native session; a daemon secret is passed by name.
			p2, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "second", Harness: h, Secrets: []string{"FAKE_TOKEN"}})
			if err != nil {
				t.Fatal(err)
			}
			done, evs = e.wait(ctx, p2.ID)
			if p2.ConversationID != p1.ConversationID || done.State != api.PromptSucceeded {
				t.Fatalf("turn 2: %+v", done)
			}
			if !strings.Contains(text(evs), "echo: second resumed="+conv.SessionID) {
				t.Fatalf("turn 2 did not resume %s: %q", conv.SessionID, text(evs))
			}
			if h == "claude" && !strings.Contains(text(evs), "token=from-daemon-secret") {
				t.Fatalf("daemon secret not passed: %q", text(evs))
			}
			// --new starts a fresh conversation.
			p3, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "third", Harness: h, New: true})
			if err != nil {
				t.Fatal(err)
			}
			_, evs = e.wait(ctx, p3.ID)
			if p3.ConversationID == p1.ConversationID || !strings.Contains(text(evs), "resumed=none") {
				t.Fatalf("new conversation: %+v %q", p3, text(evs))
			}
			list, next, err := e.svc.Conversations(ctx, "owner", "box", 0, "")
			if err != nil || len(list) != 2 || next != "" || list[0].ID != p1.ConversationID || list[0].Turns != 2 || list[1].ID != p3.ConversationID {
				t.Fatalf("conversations (createdAt order) %+v %v", list, err)
			}
			first, cursor, err := e.svc.Conversations(ctx, "owner", "box", 1, "")
			if err != nil || len(first) != 1 || cursor == "" {
				t.Fatalf("page 1: %+v %q %v", first, cursor, err)
			}
			second, end, err := e.svc.Conversations(ctx, "owner", "box", 1, cursor)
			if err != nil || len(second) != 1 || end != "" || second[0].ID != p3.ConversationID {
				t.Fatalf("page 2: %+v %q %v", second, end, err)
			}
		})
	}
}

func TestOneTurnAtATimeAndCancel(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	slow, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "slow please"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "again", ConversationID: slow.ConversationID})
	if !domain.IsCode(err, domain.CodeOperationInProgress) || domain.AsError(err).Details["promptId"] != slow.ID || !domain.AsError(err).Retryable {
		t.Fatalf("second turn while running: %v", err)
	}
	// Another conversation on the same VM runs alongside.
	other, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "parallel", New: true})
	if err != nil {
		t.Fatal(err)
	}
	if done, _ := e.wait(ctx, other.ID); done.State != api.PromptSucceeded {
		t.Fatalf("parallel conversation: %+v", done)
	}
	// Cancel once the harness runs.
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if p, _ := e.svc.Get(ctx, "owner", "box", slow.ID); p.State == api.PromptRunning {
			break
		}
	}
	if _, err := e.svc.Cancel(ctx, "owner", "box", slow.ID); err != nil {
		t.Fatal(err)
	}
	done, evs := e.wait(ctx, slow.ID)
	if done.State != api.PromptCancelled || done.Error == nil || done.Error.Code != harness.ErrCancelled || evs[len(evs)-1].Status != "cancelled" {
		t.Fatalf("cancelled turn: %+v %+v", done, done.Error)
	}
	// The conversation is free again and keeps its session.
	next, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "after cancel", ConversationID: slow.ConversationID})
	if err != nil {
		t.Fatal(err)
	}
	if _, evs := e.wait(ctx, next.ID); !strings.Contains(text(evs), "resumed=sess-") {
		t.Fatalf("after cancel: %q", text(evs))
	}
}

func TestErrorMapping(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	run := func(req api.PromptRequest) api.Prompt {
		t.Helper()
		p, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), req)
		if err != nil {
			t.Fatal(err)
		}
		done, evs := e.wait(ctx, p.ID)
		if last := evs[len(evs)-1]; last.Type != api.PromptEventTurnCompleted || last.Error == nil || last.Error.Code != done.Error.Code {
			t.Fatalf("last event %+v", last)
		}
		return done
	}
	if p := run(api.PromptRequest{Text: "please fail", New: true}); p.State != api.PromptFailed || p.Error.Code != harness.ErrHarnessFailed || p.Error.Message != "Invalid API key" {
		t.Fatalf("claude reported error: %+v", p.Error)
	}
	if p := run(api.PromptRequest{Text: "please fail", Harness: "codex"}); p.Error.Code != harness.ErrHarnessFailed || p.Error.Message != "401 Unauthorized" {
		t.Fatalf("codex reported error: %+v", p.Error)
	}
	// OpenCode exits 1 after its error line; pi exits 0 and its stop
	// reason fails the turn.
	for _, h := range []string{"opencode", "pi"} {
		if p := run(api.PromptRequest{Text: "please fail", Harness: h, New: true}); p.State != api.PromptFailed ||
			p.Error.Code != harness.ErrHarnessFailed || p.Error.Message != "invalid x-api-key" {
			t.Fatalf("%s reported error: %+v", h, p.Error)
		}
	}
	if p := run(api.PromptRequest{Text: "crash now", New: true}); p.Error.Code != harness.ErrHarnessExit || !strings.Contains(p.Error.Message, "status 2") ||
		!strings.Contains(p.Error.Message, "harness crashed") {
		t.Fatalf("crash: %+v", p.Error)
	}

	// Missing binaries: an absolute path that does not exist, and a bare
	// name a login shell cannot find (exit 127).
	for _, cmd := range []string{"/nonexistent/claude", "davit-no-such-harness"} {
		e.cfg.Commands = map[string]string{"claude": cmd}
		e.start()
		p := run(api.PromptRequest{Text: "hi", New: true})
		if p.State != api.PromptFailed || p.Error.Code != harness.ErrHarnessMissing || !strings.Contains(p.Error.Message, "harness.claude") {
			t.Fatalf("%s: %+v", cmd, p.Error)
		}
	}

	for _, bad := range []api.PromptRequest{
		{Text: " "},
		{Text: "x", Harness: "aider"},
		{Text: "x", New: true, ConversationID: "conv_x"},
		{Text: "x", Env: map[string]string{"BAD-NAME": "v"}},
		{Text: "x", Secrets: []string{"UNKNOWN"}},
	} {
		if _, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), bad); !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Errorf("%+v: err = %v, want invalid_argument", bad, err)
		}
	}
	if _, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "x", ConversationID: "conv_nope"}); !domain.IsCode(err, domain.CodeNotFound) {
		t.Errorf("unknown conversation: %v", err)
	}
	if _, err := e.svc.Get(ctx, "intruder", "box", "pr_x"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Errorf("other principal: %v", err)
	}
}

// TestRecoveryAfterRestart closes the service mid-turn (daemon stop) and
// starts a new one over the same database: the turn is followed again and
// its events are not duplicated.
func TestRecoveryAfterRestart(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	p, err := e.svc.Prompt(ctx, "owner", "box", domain.NewID("key"), api.PromptRequest{Text: "slow then answer"})
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if l, _ := e.svc.Events(ctx, "owner", "box", p.ID, 0, 0); len(l.Items) > 0 && l.Prompt.State == api.PromptRunning {
			break
		}
	}
	e.svc.Close()
	e.start()
	done, evs := e.wait(ctx, p.ID)
	if done.State != api.PromptSucceeded {
		t.Fatalf("recovered turn: %+v %+v", done, done.Error)
	}
	if got := kinds(evs); got != "conversation.started,message.delta,tool.call,tool.result,turn.completed" {
		t.Fatalf("events after recovery: %s", got)
	}
}
