package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestexec"
	"davit/internal/lifecycle"
	"davit/internal/state"
)

// Procs is the guest process and file API (guestexec.Manager).
type Procs interface {
	Exec(ctx context.Context, principal domain.PrincipalID, ref, key string, req api.ExecRequest) (api.Process, error)
	Attach(ctx context.Context, principal domain.PrincipalID, ref, id string, after int64,
		send func(api.ProcessStreamMessage) error, run func(guestexec.Stream)) error
	Signal(ctx context.Context, principal domain.PrincipalID, ref, id, signal string) (api.Process, error)
	ReadFile(ctx context.Context, r guestexec.FileRequest, open func(guestexec.FileHead) (io.Writer, error)) error
	WriteFile(ctx context.Context, r guestexec.FileRequest, mode uint32, body io.Reader) (api.FileInfo, error)
}

// VMs resolves VMs for principals (lifecycle.Service).
type VMs interface {
	GetVM(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.VM, error)
	GuestTarget(ctx context.Context, principal domain.PrincipalID, ref string) (lifecycle.GuestTarget, error)
}

// Secrets resolves a daemon-configured secret by name.
type Secrets func(name string) (string, bool, error)

// Config configures a Service.
type Config struct {
	DB    *state.DB
	Procs Procs
	VMs   VMs
	// Secrets resolves PromptRequest.Secrets; nil means none configured.
	Secrets Secrets
	// Commands overrides a harness's executable (by harness name).
	Commands map[string]string
	// DefaultTurnTimeout bounds a turn without turnTimeoutSeconds (default
	// 1h); MaxTurnTimeout caps turnTimeoutSeconds (default 24h).
	DefaultTurnTimeout time.Duration
	MaxTurnTimeout     time.Duration
	// CredentialProxyURL is the guest URL of each VM's credential proxy
	// (credproxy.GuestBaseURL); with it, a VM's active credential grants
	// are injected into every turn. "" means no proxy: grants are not
	// injected.
	CredentialProxyURL string
	// AttachmentDir is the existing guest directory inline attachments are
	// placed under, one directory per turn (default /tmp).
	AttachmentDir string
	Now           func() time.Time
	Log           *slog.Logger
}

// Service runs conversations. Turns are followed in the background on the
// service's context; Start recovers turns a previous daemon run left.
type Service struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	changed chan struct{}        // closed and replaced on every event
	live    map[string]*liveTurn // turns this process follows, by prompt ID
	stops   map[string]*Error    // why the service ended a turn, by prompt ID
	// steering serializes steers per prompt ID.
	steering map[string]*sync.Mutex
}

// liveTurn is the in-memory side of a followed turn.
type liveTurn struct {
	ad        Adapter
	processID string
	input     []byte            // the first message, written once the stream attaches
	env       map[string]string // caller env values (never stored), for a restart steer

	mu    sync.Mutex
	stdin io.WriteCloser // open while a streaming harness takes more messages
	sent  int            // messages written (or, recovered, recorded)
	done  bool           // every message answered: input ended
}

// New returns a service; call Start before use and Close at shutdown.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.DefaultTurnTimeout <= 0 {
		cfg.DefaultTurnTimeout = time.Hour
	}
	if cfg.MaxTurnTimeout <= 0 {
		cfg.MaxTurnTimeout = 24 * time.Hour
	}
	if cfg.AttachmentDir == "" {
		cfg.AttachmentDir = "/tmp"
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{cfg: cfg, ctx: ctx, cancel: cancel, changed: make(chan struct{}),
		live: map[string]*liveTurn{}, stops: map[string]*Error{}, steering: map[string]*sync.Mutex{}}
}

// Close stops following turns; they keep running in the guest and are
// recovered by the next Start.
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

func (s *Service) notify() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) watch() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func newID(prefix string) string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Prompt starts one turn.
//
// key is the request's Idempotency-Key (required, scoped to owner and VM,
// as for execProcess): the same key and body return the existing turn; the
// same key with a different body is idempotency_conflict.
func (s *Service) Prompt(ctx context.Context, principal domain.PrincipalID, ref, key string, req api.PromptRequest) (api.Prompt, error) {
	if key == "" {
		return api.Prompt{}, domain.Errorf(domain.CodeInvalidArgument, "Idempotency-Key is required").WithDetail("field", api.HeaderIdempotencyKey)
	}
	if strings.TrimSpace(req.Text) == "" {
		return api.Prompt{}, domain.Errorf(domain.CodeInvalidArgument, "prompt text is required").WithDetail("field", "text")
	}
	timeout := s.cfg.DefaultTurnTimeout
	switch {
	case req.TurnTimeoutSeconds < 0 || time.Duration(req.TurnTimeoutSeconds)*time.Second > s.cfg.MaxTurnTimeout:
		return api.Prompt{}, domain.Errorf(domain.CodeInvalidArgument, "turnTimeoutSeconds must be 0..%d", int64(s.cfg.MaxTurnTimeout/time.Second)).
			WithDetail("field", "turnTimeoutSeconds")
	case req.TurnTimeoutSeconds > 0:
		timeout = time.Duration(req.TurnTimeoutSeconds) * time.Second
	}
	atts, err := parseAttachments(req.Attachments)
	if err != nil {
		return api.Prompt{}, err
	}
	if req.New && req.ConversationID != "" {
		return api.Prompt{}, domain.Errorf(domain.CodeInvalidArgument, "new and conversationId are exclusive")
	}
	if req.Cwd != "" && !filepath.IsAbs(req.Cwd) {
		return api.Prompt{}, domain.Errorf(domain.CodeInvalidArgument, "cwd must be an absolute guest path").WithDetail("field", "cwd")
	}
	env, err := s.environment(req)
	if err != nil {
		return api.Prompt{}, err
	}
	t, err := s.cfg.VMs.GuestTarget(ctx, principal, ref)
	if err != nil {
		return api.Prompt{}, err
	}

	now := s.cfg.Now().UTC()
	body, _ := json.Marshal(req)
	sum := sha256.Sum256(body)
	var conv *state.ConversationRecord
	var replay *state.PromptRecord
	var granted map[string]string
	deadline := now.Add(timeout)
	p := &state.PromptRecord{ID: newID("pr_"), VMID: t.Key.VMID, Owner: principal, Text: req.Text,
		EnvNames: slices.Sorted(maps.Keys(req.Env)), SecretNames: req.Secrets, State: string(api.PromptStarting),
		Generation: t.Key.Generation, CreatedAt: now, IdempotencyKey: key, BodyHash: hex.EncodeToString(sum[:]),
		TurnDeadline: &deadline}
	if req.Options != nil {
		p.ReasoningEffort, p.Fast = req.Options.ReasoningEffort, req.Options.Fast
	}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		prev, err := tx.PromptByKey(principal, t.Key.VMID, key)
		if err != nil {
			return err
		}
		if prev != nil {
			if prev.BodyHash != p.BodyHash {
				return domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key was used with a different request").
					WithDetail("promptId", prev.ID)
			}
			replay = prev
			return nil
		}
		if conv, err = s.pickConversation(tx, principal, t, req, now); err != nil {
			return err
		}
		ad, _ := Lookup(conv.Harness)
		if err := checkControls(ad.Capabilities(), req.Options, atts); err != nil {
			return err
		}
		if conv.ActivePromptID != "" {
			active, err := tx.GetPrompt(conv.ActivePromptID)
			if err != nil {
				return err
			}
			if !active.Terminal() {
				e := domain.Errorf(domain.CodeOperationInProgress, "conversation %s is running turn %s; one turn at a time", conv.ID, active.ID).
					WithDetail("promptId", active.ID).WithDetail("conversationId", conv.ID)
				e.Retryable = true
				return e
			}
		}
		if req.Model != "" {
			conv.Model = req.Model
		}
		p.ConversationID, p.Harness, p.Model = conv.ID, conv.Harness, conv.Model
		conv.ActivePromptID, conv.UpdatedAt = p.ID, now
		conv.Turns++
		var notice *Event
		if granted, notice, err = grantEnv(tx, ad, t.Key.VMID, s.cfg.CredentialProxyURL, env); err != nil {
			return err
		}
		p.GrantEnv, p.Notice, p.Events = slices.Sorted(maps.Keys(granted)), nil, 0
		if notice != nil {
			p.Notice, p.Events = notice.Raw, 1
		}
		if err := tx.PutConversation(conv); err != nil {
			return err
		}
		if err := tx.PutPrompt(p); err != nil {
			return err
		}
		if notice != nil {
			return tx.AppendPromptEvent(p.ID, 1, s.eventDoc(p, 1, *notice))
		}
		return nil
	})
	if err != nil {
		return api.Prompt{}, err
	}
	if replay != nil {
		return toPrompt(replay), nil
	}
	if p.Notice != nil {
		s.cfg.Log.Warn("credential grant notice", "prompt", p.ID, "notice", string(p.Notice))
	}
	maps.Copy(env, granted) // grants take precedence

	ad, _ := Lookup(conv.Harness)
	lt := &liveTurn{ad: ad, env: req.Env}
	s.mu.Lock()
	s.live[p.ID] = lt
	s.mu.Unlock()
	fail := func(code, msg string) (api.Prompt, error) {
		s.mu.Lock()
		delete(s.live, p.ID)
		s.mu.Unlock()
		s.finish(context.WithoutCancel(ctx), p, nil, &Error{Code: code, Message: msg}, nil)
		return s.Get(ctx, principal, ref, p.ID)
	}
	turn := Turn{Model: conv.Model, Session: conv.SessionID, ReasoningEffort: p.ReasoningEffort, Fast: p.Fast}
	msg := Message{Text: req.Text}
	if len(atts) > 0 {
		if msg, turn.Images, err = s.place(ctx, principal, t, p.ID, ad, req.Text, atts); err != nil {
			return fail(ErrAttachment, err.Error())
		}
	}
	lt.input = ad.Input(msg)
	argv := s.argv(ad, turn)
	proc, err := s.cfg.Procs.Exec(ctx, principal, string(t.Key.VMID), "prompt:"+p.ID, api.ExecRequest{
		Argv: argv, Cwd: conv.Cwd, Env: env, Stdin: true, Detached: true,
		// The guest kills the harness at the turn deadline, even if this
		// daemon is gone by then.
		DeadlineSeconds: int64((timeout + time.Second - 1) / time.Second),
	})
	if err != nil {
		code, msg := ErrHarnessExit, err.Error()
		if domain.IsCode(err, domain.CodeNotFound) && strings.Contains(msg, "start ") {
			code, msg = ErrHarnessMissing, missingMessage(conv.Harness, msg)
		}
		return fail(code, msg)
	}
	p.ProcessID = proc.ID
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error { return tx.PutPrompt(p) }); err != nil {
		return api.Prompt{}, err
	}
	s.mu.Lock()
	lt.processID = proc.ID
	stopped := s.stops[p.ID] != nil // interrupted while starting
	s.mu.Unlock()
	if stopped {
		_, _ = s.cfg.Procs.Signal(context.WithoutCancel(ctx), principal, string(p.VMID), proc.ID, "TERM")
	}
	s.cfg.Log.Info("prompt started", "vm", t.Key.VMID, "conversation", conv.ID, "prompt", p.ID, "harness", conv.Harness,
		"process", proc.ID, "env", p.EnvNames, "secrets", p.SecretNames, "grantEnv", p.GrantEnv, "attachments", len(atts),
		"effort", p.ReasoningEffort, "fast", p.Fast, "deadline", deadline.Format(time.RFC3339))
	out := toPrompt(p) // before follow: the turn's goroutine updates p
	s.follow(p, lt, 0)
	return out, nil
}

// environment merges caller env and named secrets; values are never logged
// or stored.
func (s *Service) environment(req api.PromptRequest) (map[string]string, error) {
	env := map[string]string{}
	for k, v := range req.Env {
		if !envName.MatchString(k) {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "env name %q is invalid", k).WithDetail("field", "env")
		}
		env[k] = v
	}
	for _, name := range req.Secrets {
		if !envName.MatchString(name) {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "secret name %q is invalid", name).WithDetail("field", "secrets")
		}
		if s.cfg.Secrets == nil {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "no daemon secrets are configured (secretsFile)").WithDetail("field", "secrets")
		}
		v, ok, err := s.cfg.Secrets(name)
		if err != nil {
			return nil, domain.Errorf(domain.CodeInternal, "reading daemon secrets: %v", err)
		}
		if !ok {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "no daemon secret named %q", name).WithDetail("field", "secrets")
		}
		env[name] = v
	}
	return env, nil
}

func (s *Service) pickConversation(tx *state.Tx, principal domain.PrincipalID, t lifecycle.GuestTarget, req api.PromptRequest, now time.Time) (*state.ConversationRecord, error) {
	if req.ConversationID != "" {
		c, err := tx.GetConversation(req.ConversationID)
		if err != nil || c.Owner != principal || c.VMID != t.Key.VMID {
			return nil, domain.Errorf(domain.CodeNotFound, "conversation %s not found", req.ConversationID)
		}
		if req.Harness != "" {
			if ad, err := Lookup(req.Harness); err != nil || ad.Name() != c.Harness {
				return nil, domain.Errorf(domain.CodeInvalidArgument, "conversation %s uses harness %s", c.ID, c.Harness).WithDetail("field", "harness")
			}
		}
		return c, nil
	}
	ad, err := Lookup(req.Harness)
	if err != nil {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "%v", err).WithDetail("field", "harness")
	}
	if !req.New {
		recent, err := tx.RecentConversations(principal, t.Key.VMID, 50)
		if err != nil {
			return nil, err
		}
		for _, c := range recent {
			if c.Harness == ad.Name() {
				return c, nil
			}
		}
	}
	return &state.ConversationRecord{ID: newID("conv_"), VMID: t.Key.VMID, Owner: principal, Harness: ad.Name(),
		Model: req.Model, Cwd: req.Cwd, CreatedAt: now, UpdatedAt: now}, nil
}

// argv runs the harness directly when its command is a path, else through
// a login shell so per-user installs (~/.local/bin) resolve.
func (s *Service) argv(ad Adapter, t Turn) []string {
	cmd := s.cfg.Commands[ad.Name()]
	if cmd == "" {
		cmd = ad.DefaultCommand()
	}
	if filepath.IsAbs(cmd) {
		return append([]string{cmd}, ad.Args(t)...)
	}
	return append([]string{"sh", "-lc", `exec "$0" "$@"`, cmd}, ad.Args(t)...)
}

func missingMessage(harness, detail string) string {
	return fmt.Sprintf("harness %s is not installed in this VM's image (image capability harness.%s): %s", harness, harness, detail)
}

// follow streams a turn's output into events in the background. skip is
// the number of events already stored (recovery re-derives and skips them).
func (s *Service) follow(p *state.PromptRecord, lt *liveTurn, skip int64) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run(p, lt, skip)
	}()
}

const stderrTail = 4 << 10

func (s *Service) run(p *state.PromptRecord, lt *liveTurn, skip int64) {
	ctx := s.ctx
	defer func() {
		s.mu.Lock()
		if s.live[p.ID] == lt {
			delete(s.live, p.ID)
		}
		s.mu.Unlock()
	}()
	ad := lt.ad
	streaming := ad.Capabilities().Steer == domain.SteerNative
	parser := ad.NewParser()
	var seq int64
	if p.Notice != nil {
		seq = 1 // the notice Prompt stored
	}
	var line, stderr bytes.Buffer
	var final *api.Process
	jsonSeen := false

	emit := func(evs []Event) error {
		var docs [][]byte
		for _, e := range evs {
			seq++
			if seq <= skip {
				continue
			}
			docs = append(docs, s.eventDoc(p, seq, e))
		}
		if streaming {
			s.endInput(p, lt, parser.Results())
		}
		if len(docs) == 0 {
			return nil
		}
		err := s.cfg.DB.Tx(context.WithoutCancel(ctx), func(tx *state.Tx) error {
			for i, d := range docs {
				if err := tx.AppendPromptEvent(p.ID, seq-int64(len(docs))+int64(i)+1, d); err != nil {
					return err
				}
			}
			cur, err := tx.GetPrompt(p.ID)
			if err != nil {
				return err
			}
			cur.Events = seq
			if cur.State == string(api.PromptStarting) {
				cur.State = string(api.PromptRunning)
			}
			*p = *cur
			return tx.PutPrompt(cur)
		})
		s.notify()
		return err
	}
	lines := func(b []byte) error {
		line.Write(b)
		for {
			i := bytes.IndexByte(line.Bytes(), '\n')
			if i < 0 {
				if line.Len() > 8<<20 { // a runaway line: keep it raw
					l := bytes.Clone(line.Bytes())
					line.Reset()
					return emit([]Event{rawEvent(l)})
				}
				return nil
			}
			l := bytes.TrimRight(bytes.Clone(line.Next(i+1)), "\r\n")
			if len(bytes.TrimSpace(l)) == 0 {
				continue
			}
			jsonSeen = jsonSeen || json.Valid(l)
			if err := emit(parser.Line(l)); err != nil {
				return err
			}
		}
	}
	send := func(m api.ProcessStreamMessage) error {
		switch m.Type {
		case api.StreamStdout, api.StreamStderr:
			b, err := base64.StdEncoding.DecodeString(m.Data)
			if err != nil {
				return err
			}
			if m.Type == api.StreamStderr {
				stderr.Write(b)
				if stderr.Len() > 2*stderrTail {
					tail := bytes.Clone(stderr.Bytes()[stderr.Len()-stderrTail:])
					stderr.Reset()
					stderr.Write(tail)
				}
				return nil
			}
			return lines(b)
		case api.StreamExit:
			final = m.Process
		case api.StreamError:
			if m.Error != nil {
				return fmt.Errorf("%s", m.Error.Message)
			}
		}
		return nil
	}
	deliver := func(st guestexec.Stream) {
		if st.Stdin == nil {
			return // recovery: the prompt was delivered by the previous run
		}
		lt.mu.Lock()
		if _, err := st.Stdin.Write(lt.input); err != nil {
			s.cfg.Log.Warn("prompt stdin", "prompt", p.ID, "err", err)
		}
		lt.sent = 1
		if streaming {
			lt.stdin = st.Stdin // closed once every message has its result
		} else {
			_ = st.Stdin.Close()
		}
		lt.mu.Unlock()
		_ = s.cfg.DB.Tx(context.WithoutCancel(ctx), func(tx *state.Tx) error {
			cur, err := tx.GetPrompt(p.ID)
			if err != nil || cur.State != string(api.PromptStarting) {
				return err
			}
			cur.State, cur.Messages = string(api.PromptRunning), 1
			return tx.PutPrompt(cur)
		})
	}

	err := s.cfg.Procs.Attach(ctx, p.Owner, string(p.VMID), p.ProcessID, 0, send, deliver)
	lt.mu.Lock()
	if lt.stdin != nil {
		_ = lt.stdin.Close()
		lt.stdin = nil
	}
	lt.mu.Unlock()
	if ctx.Err() != nil {
		return // daemon shutdown: the turn keeps running; Start recovers it
	}
	if line.Len() > 0 {
		_ = emit(parser.Line(bytes.TrimSpace(line.Bytes())))
		line.Reset()
	}
	if err != nil || final == nil {
		msg := "the turn's process stream was lost"
		if err != nil {
			msg += ": " + err.Error()
		}
		s.finish(ctx, p, parser, &Error{Code: ErrLost, Message: msg}, nil)
		return
	}
	s.finish(ctx, p, parser, s.outcome(p, parser, *final, jsonSeen, stderr.String()), final)
}

// outcome maps a finished harness process to the turn's error (nil on
// success).
func (s *Service) outcome(p *state.PromptRecord, parser Parser, proc api.Process, jsonSeen bool, stderr string) *Error {
	s.mu.Lock()
	stop := s.stops[p.ID]
	s.mu.Unlock()
	tail := strings.TrimSpace(stderr)
	exit := -1
	if proc.ExitCode != nil {
		exit = *proc.ExitCode
	}
	switch {
	case stop != nil && stop.Code == stopEnded:
		// The service ended a streaming harness that had answered every
		// message: its own report decides.
		if r := parser.Result(); r != nil && r.Failed {
			return &Error{Code: ErrHarnessFailed, Message: r.Message}
		}
		return nil
	case stop != nil:
		return stop
	case proc.TimedOut:
		return &Error{Code: ErrTimeout, Message: fmt.Sprintf("the turn reached its deadline (turnTimeoutSeconds) and %s was killed", p.Harness)}
	case exit == 127 && !jsonSeen:
		return &Error{Code: ErrHarnessMissing, Message: missingMessage(p.Harness, tail)}
	case parser.Result() != nil && parser.Result().Failed:
		return &Error{Code: ErrHarnessFailed, Message: parser.Result().Message}
	case exit != 0:
		msg := fmt.Sprintf("%s exited with status %d", p.Harness, exit)
		if exit < 0 {
			msg = fmt.Sprintf("%s was killed (%s)", p.Harness, proc.Signal)
		}
		if tail != "" {
			msg += ": " + lastLines(tail, 5)
		}
		return &Error{Code: ErrHarnessExit, Message: msg}
	}
	return nil
}

func lastLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

// finish records turn.completed and frees the conversation.
func (s *Service) finish(ctx context.Context, p *state.PromptRecord, parser Parser, turnErr *Error, _ *api.Process) {
	ctx = context.WithoutCancel(ctx)
	now := s.cfg.Now().UTC()
	ev := Event{Type: EventTurnCompleted, Status: string(api.PromptSucceeded), Error: turnErr}
	state_ := api.PromptSucceeded
	if turnErr != nil {
		state_ = api.PromptFailed
		if turnErr.Code == ErrCancelled {
			state_ = api.PromptCancelled
		}
		ev.Status = string(state_)
	}
	if parser != nil && parser.Result() != nil {
		ev.Usage = parser.Result().Usage
	}
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		cur, err := tx.GetPrompt(p.ID)
		if err != nil {
			return err
		}
		if cur.Terminal() {
			return nil
		}
		cur.Events++
		if err := tx.AppendPromptEvent(cur.ID, cur.Events, s.eventDoc(cur, cur.Events, ev)); err != nil {
			return err
		}
		cur.State, cur.FinishedAt = string(state_), &now
		if turnErr != nil {
			cur.Error, _ = json.Marshal(api.PromptError{Code: turnErr.Code, Message: turnErr.Message})
		}
		if ev.Usage != nil {
			cur.Usage, _ = json.Marshal(toUsage(ev.Usage))
		}
		if err := tx.PutPrompt(cur); err != nil {
			return err
		}
		conv, err := tx.GetConversation(cur.ConversationID)
		if err != nil {
			return err
		}
		if parser != nil && parser.Session() != "" {
			conv.SessionID = parser.Session()
		}
		if conv.ActivePromptID == cur.ID {
			conv.ActivePromptID = ""
		}
		conv.UpdatedAt = now
		*p = *cur
		return tx.PutConversation(conv)
	})
	if err != nil {
		s.cfg.Log.Error("finish prompt", "prompt", p.ID, "err", err)
	}
	s.mu.Lock()
	delete(s.stops, p.ID)
	s.mu.Unlock()
	s.cfg.Log.Info("prompt finished", "prompt", p.ID, "state", state_)
	s.notify()
}

func (s *Service) eventDoc(p *state.PromptRecord, seq int64, e Event) []byte {
	out := api.PromptEvent{Seq: seq, Time: s.cfg.Now().UTC(), PromptID: p.ID, ConversationID: p.ConversationID,
		Type: e.Type, SessionID: e.SessionID, Role: e.Role, Text: e.Text, ToolCallID: e.ToolCallID, ToolName: e.ToolName,
		Output: e.Output, IsError: e.IsError, Status: e.Status, Usage: toUsage(e.Usage)}
	if len(e.Input) > 0 {
		out.Input = e.Input
	}
	if len(e.Raw) > 0 {
		out.Raw = e.Raw
	}
	if e.Error != nil {
		out.Error = &api.PromptError{Code: e.Error.Code, Message: e.Error.Message}
	}
	b, _ := json.Marshal(out)
	return b
}

func toUsage(u *Usage) *api.PromptUsage {
	if u == nil {
		return nil
	}
	return &api.PromptUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedInputTokens: u.CachedInputTokens, CostUSD: u.CostUSD}
}

func toPrompt(p *state.PromptRecord) api.Prompt {
	out := api.Prompt{ID: p.ID, ConversationID: p.ConversationID, VMID: p.VMID, Harness: p.Harness, Model: p.Model,
		State: api.PromptState(p.State), ProcessID: p.ProcessID, EventCursor: p.Events, CreatedAt: p.CreatedAt, FinishedAt: p.FinishedAt,
		TurnDeadlineAt: p.TurnDeadline}
	if len(p.Error) > 0 {
		out.Error = new(api.PromptError)
		_ = json.Unmarshal(p.Error, out.Error)
	}
	if len(p.Usage) > 0 {
		out.Usage = new(api.PromptUsage)
		_ = json.Unmarshal(p.Usage, out.Usage)
	}
	return out
}

func toConversation(c *state.ConversationRecord) api.Conversation {
	return api.Conversation{ID: c.ID, VMID: c.VMID, Harness: c.Harness, Model: c.Model, Cwd: c.Cwd, SessionID: c.SessionID,
		ActivePromptID: c.ActivePromptID, Turns: c.Turns, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// prompt loads id if it belongs to principal's VM ref.
func (s *Service) prompt(ctx context.Context, principal domain.PrincipalID, ref, id string) (*state.PromptRecord, error) {
	vm, err := s.cfg.VMs.GetVM(ctx, principal, ref)
	if err != nil {
		return nil, err
	}
	var p *state.PromptRecord
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) { p, err = tx.GetPrompt(id); return err })
	if err != nil || p.Owner != principal || p.VMID != vm.ID {
		return nil, domain.Errorf(domain.CodeNotFound, "prompt %s not found", id)
	}
	return p, nil
}

// Get returns a turn.
// PromptSecretNames are the daemon secrets prompt id was started with (a
// restart steer passes them to the next turn).
func (s *Service) PromptSecretNames(ctx context.Context, principal domain.PrincipalID, ref, id string) ([]string, error) {
	p, err := s.prompt(ctx, principal, ref, id)
	if err != nil {
		return nil, err
	}
	return p.SecretNames, nil
}

func (s *Service) Get(ctx context.Context, principal domain.PrincipalID, ref, id string) (api.Prompt, error) {
	p, err := s.prompt(ctx, principal, ref, id)
	if err != nil {
		return api.Prompt{}, err
	}
	return toPrompt(p), nil
}

// Events returns events after seq; with wait > 0 it long-polls until one
// arrives or the turn ends.
func (s *Service) Events(ctx context.Context, principal domain.PrincipalID, ref, id string, after int64, wait time.Duration) (api.PromptEventList, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		changed := s.watch()
		p, err := s.prompt(ctx, principal, ref, id)
		if err != nil {
			return api.PromptEventList{}, err
		}
		var docs []json.RawMessage
		if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) { docs, err = tx.PromptEvents(id, after, 500); return err }); err != nil {
			return api.PromptEventList{}, err
		}
		if len(docs) > 0 || p.Terminal() || wait <= 0 {
			out := api.PromptEventList{Prompt: toPrompt(p), Items: make([]api.PromptEvent, 0, len(docs)), NextCursor: fmt.Sprint(after)}
			for _, d := range docs {
				var e api.PromptEvent
				if err := json.Unmarshal(d, &e); err != nil {
					return api.PromptEventList{}, err
				}
				out.Items = append(out.Items, e)
			}
			if n := len(out.Items); n > 0 {
				out.NextCursor = fmt.Sprint(out.Items[n-1].Seq)
			}
			return out, nil
		}
		select {
		case <-changed:
		case <-deadline.C:
			wait = 0
		case <-ctx.Done():
			return api.PromptEventList{}, ctx.Err()
		case <-s.ctx.Done():
			wait = 0
		}
	}
}

// Cancel sends SIGTERM to a running turn's harness; a turn still
// starting is signalled as soon as its process exists.
func (s *Service) Cancel(ctx context.Context, principal domain.PrincipalID, ref, id string) (api.Prompt, error) {
	p, err := s.prompt(ctx, principal, ref, id)
	if err != nil {
		return api.Prompt{}, err
	}
	if p.Terminal() {
		return api.Prompt{}, domain.Errorf(domain.CodeInvalidState, "prompt %s is %s", id, p.State)
	}
	if err := s.stop(ctx, p, &Error{Code: ErrCancelled, Message: "cancelled"}); err != nil {
		return api.Prompt{}, err
	}
	return toPrompt(p), nil
}

// stopEnded marks a streaming turn the service ended after its last
// result (an internal stop reason, never reported).
const stopEnded = "ended"

// stop records why the service ends p (the turn's outcome reports it),
// then sends SIGTERM. The reason is recorded before the process ID is
// read, under the lock Prompt takes to publish it, so a turn still
// starting is signalled by whichever side comes second.
func (s *Service) stop(ctx context.Context, p *state.PromptRecord, why *Error) error {
	s.mu.Lock()
	if s.stops[p.ID] == nil || s.stops[p.ID].Code == stopEnded {
		s.stops[p.ID] = why
	}
	proc := p.ProcessID
	if lt := s.live[p.ID]; lt != nil && lt.processID != "" {
		proc = lt.processID
	}
	s.mu.Unlock()
	if proc == "" {
		return nil
	}
	if _, err := s.cfg.Procs.Signal(ctx, p.Owner, string(p.VMID), proc, "TERM"); err != nil && !domain.IsCode(err, domain.CodeInvalidState) {
		return err
	}
	return nil
}

// Conversations lists principal's conversations on ref.
// Pages are in stable (createdAt, id) order; cursor is the previous page's
// nextCursor and is bound to (principal, VM).
func (s *Service) Conversations(ctx context.Context, principal domain.PrincipalID, ref string, limit int, cursor string) ([]api.Conversation, string, error) {
	vm, err := s.cfg.VMs.GetVM(ctx, principal, ref)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > api.MaxListLimit {
		limit = api.DefaultListLimit
	}
	var cs []*state.ConversationRecord
	var next string
	page := state.Page{Limit: limit, Cursor: cursor, Filter: string(principal) + "|" + string(vm.ID)}
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		cs, next, err = tx.ListConversations(principal, vm.ID, page)
		return err
	}); err != nil {
		return nil, "", err
	}
	out := make([]api.Conversation, 0, len(cs))
	for _, c := range cs {
		out = append(out, toConversation(c))
	}
	return out, next, nil
}

// Conversation returns one conversation.
func (s *Service) Conversation(ctx context.Context, principal domain.PrincipalID, ref, id string) (api.Conversation, error) {
	vm, err := s.cfg.VMs.GetVM(ctx, principal, ref)
	if err != nil {
		return api.Conversation{}, err
	}
	var c *state.ConversationRecord
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) { c, err = tx.GetConversation(id); return err })
	if err != nil || c.Owner != principal || c.VMID != vm.ID {
		return api.Conversation{}, domain.Errorf(domain.CodeNotFound, "conversation %s not found", id)
	}
	return toConversation(c), nil
}

// Start recovers turns a previous daemon run was following: running turns
// are followed again (stored events are re-derived and skipped); turns
// whose prompt text may never have reached the harness are killed and
// failed.
func (s *Service) Start(ctx context.Context) error {
	var ps []*state.PromptRecord
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) { ps, err = tx.UnfinishedPrompts(); return err }); err != nil {
		return err
	}
	for _, p := range ps {
		ad, err := Lookup(p.Harness)
		switch {
		case err != nil || p.ProcessID == "":
			s.finish(ctx, p, nil, &Error{Code: ErrLost, Message: "the daemon restarted before the turn started"}, nil)
		case p.State == string(api.PromptStarting):
			_, _ = s.cfg.Procs.Signal(ctx, p.Owner, string(p.VMID), p.ProcessID, "KILL")
			s.finish(ctx, p, nil, &Error{Code: ErrLost, Message: "the daemon restarted before the prompt was delivered"}, nil)
		default:
			s.cfg.Log.Info("recovering prompt", "prompt", p.ID, "events", p.Events)
			lt := &liveTurn{ad: ad, processID: p.ProcessID, sent: max(p.Messages, 1)}
			s.mu.Lock()
			s.live[p.ID] = lt
			s.mu.Unlock()
			s.follow(p, lt, p.Events)
		}
	}
	return nil
}
