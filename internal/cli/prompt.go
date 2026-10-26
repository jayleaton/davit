package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// prompt runs one turn of a coding-agent harness (Claude Code, Codex)
// inside the VM and follows its events.

type promptOpts struct {
	harness, resume, model, cwd string
	newConv, noWait             bool
	env, secrets                multiFlag
	attach, attachGuest         multiFlag
	effort                      string
	fast                        bool
	turnTimeout                 time.Duration
	// prompt-events and interrupt
	conversation, after string
	follow, all         bool
}

func promptFlags(fs *flag.FlagSet, o *opts) {
	p := &o.prompt
	fs.StringVar(&p.harness, "harness", "", "claude (default), codex, opencode or pi")
	fs.StringVar(&p.harness, "provider", "", "same as --harness")
	fs.BoolVar(&p.newConv, "new", false, "start a new conversation")
	fs.StringVar(&p.resume, "resume", "", "continue conversation CONV")
	fs.StringVar(&p.model, "model", "", "model for the harness")
	fs.StringVar(&p.cwd, "cwd", "", "guest working directory of a new conversation")
	fs.Var(&p.env, "env", "pass NAME from this environment to the harness (repeatable; the value enters the VM)")
	fs.Var(&p.secrets, "secret", "pass daemon secret NAME as an env var (repeatable; the value enters the VM)")
	fs.BoolVar(&p.noWait, "no-wait", false, "return once the turn is accepted")
	fs.StringVar(&o.key, "idempotency-key", "", "deduplicate separate invocations")
	fs.Var(&p.attach, "attach", "attach local FILE (repeatable; sent inline, placed in the VM; capabilities list the types)")
	fs.Var(&p.attachGuest, "attach-guest", "attach the VM's file PATH (repeatable)")
	fs.StringVar(&p.effort, "effort", "", "reasoning effort (capabilities harnesses[].reasoningEfforts)")
	fs.BoolVar(&p.fast, "fast", false, "the harness's fast mode")
	fs.DurationVar(&p.turnTimeout, "turn-timeout", 0, "kill the turn after D (default 1h); the turn holds auto-stop until then")
}

// MaxAttachBytes bounds one --attach file (it travels inline in the JSON
// request).
const MaxAttachBytes = 32 << 20

// attachments reads --attach files and names --attach-guest paths, typed
// by extension (else by content, for local files).
func (p promptOpts) attachments() ([]api.PromptAttachment, error) {
	var out []api.PromptAttachment
	for _, f := range p.attach {
		st, err := os.Stat(f)
		if err != nil {
			return nil, usageErr("--attach %s: %v", f, err)
		}
		if !st.Mode().IsRegular() || st.Size() > MaxAttachBytes {
			return nil, usageErr("--attach %s: want a regular file of at most %d MiB", f, MaxAttachBytes>>20)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, usageErr("--attach %s: %v", f, err)
		}
		mt := mime.TypeByExtension(filepath.Ext(f))
		if mt == "" {
			mt = http.DetectContentType(b)
		}
		out = append(out, api.PromptAttachment{Name: filepath.Base(f), MediaType: mt, Data: base64.StdEncoding.EncodeToString(b)})
	}
	for _, g := range p.attachGuest {
		mt := mime.TypeByExtension(path.Ext(g))
		if mt == "" {
			return nil, usageErr("--attach-guest %s: cannot tell its media type from the extension", g)
		}
		out = append(out, api.PromptAttachment{Name: path.Base(g), MediaType: mt, Path: g})
	}
	return out, nil
}

func runPrompt(ctx context.Context, s *session, o *opts, args []string) error {
	p := o.prompt
	var ref, text string
	switch len(args) {
	case 1:
		text = args[0]
	case 2:
		ref, text = args[0], args[1]
	default:
		return usageErr("usage: %s prompt [ID] TEXT (TEXT - reads stdin)", buildinfo.Name)
	}
	if ref == "" {
		id, err := s.currentID(nil)
		if err != nil {
			return err
		}
		ref = id
	}
	if text == "-" {
		b, err := io.ReadAll(io.LimitReader(s.io.Stdin, 1<<20))
		if err != nil {
			return usageErr("reading the prompt from stdin: %v", err)
		}
		text = string(b)
	}
	if p.newConv && p.resume != "" {
		return usageErr("--new and --resume are exclusive")
	}
	body := api.PromptRequest{Text: text, Harness: p.harness, New: p.newConv, ConversationID: p.resume,
		Model: p.model, Cwd: p.cwd, Secrets: p.secrets, TurnTimeoutSeconds: int64(p.turnTimeout / time.Second)}
	if p.turnTimeout < 0 || (p.turnTimeout > 0 && p.turnTimeout < time.Second) {
		return usageErr("--turn-timeout must be at least 1s")
	}
	if p.effort != "" || p.fast {
		body.Options = &api.PromptOptions{ReasoningEffort: p.effort, Fast: p.fast}
	}
	atts, err := p.attachments()
	if err != nil {
		return err
	}
	body.Attachments = atts
	for _, name := range p.env {
		if strings.Contains(name, "=") {
			return usageErr("--env takes a variable NAME from your environment, not NAME=VALUE (values never go on the command line)")
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			return usageErr("--env %s: not set in this environment", name)
		}
		if body.Env == nil {
			body.Env = map[string]string{}
		}
		body.Env[name] = v
	}
	var pr api.Prompt
	// Generated once and reused on transport retries, as for exec.
	hdr := map[string]string{api.HeaderIdempotencyKey: o.idempotencyKey()}
	if err := s.cl.do(ctx, http.MethodPost, vmPath(ref)+"/prompts", nil, body, hdr, &pr); err != nil {
		return err
	}
	s.out.emit("prompt.started", "", 0, pr, nil)
	if p.noWait {
		s.out.result("", pr)
		s.out.human("%s (conversation %s)", pr.ID, pr.ConversationID)
		return nil
	}
	return s.followPrompt(ctx, ref, pr)
}

// followPrompt long-polls the turn's events until turn.completed. Ctrl-C
// cancels the turn (and keeps following until it ends).
func (s *session) followPrompt(ctx context.Context, ref string, pr api.Prompt) error {
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigc)
	interrupted := false
	path := vmPath(ref) + "/prompts/" + url.PathEscape(pr.ID)
	var after int64
	for {
		select {
		case <-sigc:
			if !interrupted {
				interrupted = true
				s.out.diag("cancelling %s…", pr.ID)
				_ = s.cl.do(ctx, http.MethodDelete, path, nil, nil, nil, nil)
			}
		default:
		}
		var page api.PromptEventList
		q := url.Values{"after": {strconv.FormatInt(after, 10)}, "waitMs": {"1000"}}
		if err := s.cl.get(ctx, path+"/events", q, &page); err != nil {
			return err
		}
		for _, e := range page.Items {
			after = e.Seq
			if s.g.json {
				s.out.emit(e.Type, "", e.Seq, e, nil)
				continue
			}
			switch e.Type {
			case api.PromptEventMessageDelta:
				fmt.Fprint(s.io.Stdout, e.Text)
				if !strings.HasSuffix(e.Text, "\n") {
					fmt.Fprintln(s.io.Stdout)
				}
			case api.PromptEventToolCall:
				fmt.Fprintf(s.io.Stderr, "› %s %s\n", e.ToolName, summarize(e.Input))
			case api.PromptEventToolResult:
				if e.IsError {
					fmt.Fprintf(s.io.Stderr, "  ✗ %s\n", firstLine(e.Output))
				}
			}
		}
		final := page.Prompt
		if final.State == api.PromptStarting || final.State == api.PromptRunning || len(page.Items) > 0 {
			continue
		}
		s.out.result("", final)
		if final.Usage != nil {
			s.out.diag("%s: %d in / %d out tokens (conversation %s)", final.State, final.Usage.InputTokens, final.Usage.OutputTokens, final.ConversationID)
		}
		if interrupted || final.State == api.PromptCancelled {
			return &exitStatus{code: ExitInterrupt}
		}
		if final.State == api.PromptFailed && final.Error != nil {
			return turnError(final)
		}
		return nil
	}
}

// turnError maps a failed turn to a CLI error. The result line was already
// printed, so --json readers see the prompt; the error is the exit status.
func turnError(p api.Prompt) error {
	code := domain.CodeInternal
	switch p.Error.Code {
	case "harness_missing":
		code = domain.CodeUnsupportedCapability
	case "lost":
		code = domain.CodeUnavailable
	case "attachment":
		code = domain.CodeInvalidArgument
	case "turn_timeout":
		code = domain.CodeTimeout
	}
	return &exitStatus{code: ExitCode(code), msg: fmt.Sprintf("turn %s failed (%s): %s", p.ID, p.Error.Code, p.Error.Message)}
}

func summarize(v any) string {
	if v == nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok {
		for _, k := range []string{"command", "file_path", "path", "pattern", "query", "url"} {
			if s, ok := m[k].(string); ok {
				return firstLine(s)
			}
		}
	}
	b, _ := json.Marshal(v)
	return firstLine(string(b))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if len(s) > 120 {
		s = s[:117] + "…"
	}
	return s
}

func runConversations(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 1, "conversations [ID]"); err != nil {
		return err
	}
	id, err := s.currentID(args)
	if err != nil {
		return err
	}
	q := url.Values{}
	if o.limit != 0 {
		q.Set("limit", strconv.Itoa(o.limit))
	}
	if o.cursor != "" {
		q.Set("cursor", o.cursor)
	}
	var list api.ConversationList
	if err := s.cl.get(ctx, vmPath(id)+"/conversations", q, &list); err != nil {
		return err
	}
	for _, c := range list.Items {
		s.out.emit("item", "", 0, c, nil)
		active := ""
		if c.ActivePromptID != "" {
			active = " running " + c.ActivePromptID
		}
		s.out.human("%s  %-6s  %d turns  session %s  %s%s", c.ID, c.Harness, c.Turns, orDash(c.SessionID), c.UpdatedAt.Local().Format(time.DateTime), active)
	}
	if list.NextCursor != "" {
		s.out.diag("more: --cursor %s", list.NextCursor)
	}
	s.out.result("", map[string]any{"nextCursor": list.NextCursor, "count": len(list.Items)})
	return nil
}

// promptControlFlags are the flags of prompt-events, interrupt and steer.
func promptControlFlags(fs *flag.FlagSet, o *opts) {
	p := &o.prompt
	fs.StringVar(&p.conversation, "conversation", "", "only conversation CONV")
	fs.StringVar(&p.after, "after", "", "feed cursor to continue from (prompt-events)")
	fs.BoolVar(&p.follow, "follow", false, "keep waiting for new events until Ctrl-C (prompt-events)")
	fs.IntVar(&o.limit, "limit", 0, "page size (prompt-events, max 200)")
	fs.BoolVar(&p.all, "all", false, "every active turn on the VM (interrupt)")
	fs.BoolVar(&p.noWait, "no-wait", false, "do not follow the turn (steer)")
	fs.StringVar(&o.key, "idempotency-key", "", "deduplicate separate invocations (steer)")
}

// runPromptEvents prints the VM-wide prompt feed (every turn, every
// conversation) after a cursor; --follow long-polls until Ctrl-C. The last
// cursor is printed so a later call can continue.
func runPromptEvents(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 1, "prompt-events [ID]"); err != nil {
		return err
	}
	id, err := s.currentID(args)
	if err != nil {
		return err
	}
	p := o.prompt
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	cursor := p.after
	for {
		q := url.Values{}
		if cursor != "" {
			q.Set("after", cursor)
		}
		if p.conversation != "" {
			q.Set("conversationId", p.conversation)
		}
		if o.limit != 0 {
			q.Set("limit", strconv.Itoa(o.limit))
		}
		if p.follow {
			q.Set("waitMs", "25000")
		}
		var feed api.PromptFeed
		if err := s.cl.get(ctx, vmPath(id)+"/prompt-events", q, &feed); err != nil {
			if p.follow && ctx.Err() != nil {
				break
			}
			return err
		}
		for _, it := range feed.Items {
			e := it.Event
			if s.g.json {
				s.out.emit(e.Type, "", e.Seq, it, nil)
				continue
			}
			line := e.Type
			switch e.Type {
			case api.PromptEventMessageDelta:
				line = firstLine(e.Text)
			case api.PromptEventToolCall:
				line = "› " + e.ToolName + " " + summarize(e.Input)
			case api.PromptEventTurnCompleted:
				line = "turn " + e.Status
			case api.PromptEventRaw:
				continue
			}
			s.out.human("%s %s  %s", e.PromptID, e.Time.Local().Format(time.TimeOnly), line)
		}
		cursor = feed.NextCursor
		if !p.follow && (len(feed.Items) == 0 || o.limit != 0) {
			break
		}
	}
	s.out.diag("continue with: --after %s", cursor)
	s.out.result("", map[string]any{"nextCursor": cursor})
	return nil
}

// runInterrupt cancels one conversation's active turn or all of the VM's.
func runInterrupt(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 1, "interrupt [ID] --conversation CONV|--all"); err != nil {
		return err
	}
	p := o.prompt
	if p.all == (p.conversation != "") {
		return usageErr("give exactly one of --conversation CONV and --all")
	}
	id, err := s.currentID(args)
	if err != nil {
		return err
	}
	var res api.InterruptResult
	body := api.InterruptRequest{ConversationID: p.conversation, All: p.all}
	if err := s.cl.do(ctx, http.MethodPost, vmPath(id)+"/interrupt", nil, body, nil, &res); err != nil {
		return err
	}
	for _, pr := range res.Cancelled {
		s.out.human("%s  %s  (conversation %s)", pr.ID, pr.State, pr.ConversationID)
	}
	if len(res.Cancelled) == 0 {
		s.out.human("no active turn")
	}
	s.out.result("", res)
	return nil
}

// runSteer sends a message to a running turn and follows the turn that
// carries it: the same turn (native) or the turn that replaced it
// (restart), as the server reports.
func runSteer(ctx context.Context, s *session, o *opts, args []string) error {
	var ref, id, text string
	switch len(args) {
	case 2:
		id, text = args[0], args[1]
	case 3:
		ref, id, text = args[0], args[1], args[2]
	default:
		return usageErr("usage: %s steer [ID] PROMPT TEXT", buildinfo.Name)
	}
	if ref == "" {
		cur, err := s.currentID(nil)
		if err != nil {
			return err
		}
		ref = cur
	}
	var res api.SteerResult
	path := vmPath(ref) + "/prompts/" + url.PathEscape(id) + "/steer"
	// Generated once and reused on transport retries: the message is
	// delivered once.
	hdr := map[string]string{api.HeaderIdempotencyKey: o.idempotencyKey()}
	if err := s.cl.do(ctx, http.MethodPost, path, nil, api.SteerRequest{Text: text}, hdr, &res); err != nil {
		return err
	}
	s.out.emit("prompt.steered", "", 0, res, nil)
	if res.Delivery == domain.SteerRestart {
		s.out.diag("%s cannot take input mid-turn: %s was cancelled and %s continues the conversation", res.Prompt.Harness, id, res.Prompt.ID)
	}
	if o.prompt.noWait {
		s.out.result("", res)
		s.out.human("%s %s", res.Delivery, res.Prompt.ID)
		return nil
	}
	return s.followPrompt(ctx, ref, res.Prompt)
}
