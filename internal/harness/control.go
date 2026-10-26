package harness

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

// Turn controls beyond create/get/cancel (parity v1 G5): the VM-wide event
// feed, scoped interrupt, steering, and the active-turn lease that holds a
// VM's auto-stop.

// endInput closes a streaming harness's stdin once every message it was
// sent has its result, so the harness exits. A recovered turn has no
// stdin (the guest keeps the previous daemon's pipe open), so the harness
// is ended with SIGTERM instead and its own report decides the outcome.
func (s *Service) endInput(p *state.PromptRecord, lt *liveTurn, results int) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if lt.done || lt.sent == 0 || results < lt.sent {
		return
	}
	lt.done = true
	if lt.stdin != nil {
		_ = lt.stdin.Close()
		lt.stdin = nil
		return
	}
	if err := s.stop(context.WithoutCancel(s.ctx), p, &Error{Code: stopEnded}); err != nil {
		s.cfg.Log.Warn("end answered turn", "prompt", p.ID, "err", err)
	}
}

// feedCursor is the opaque listVMPromptEvents cursor: a feed position
// bound to one VM.
const feedCursorPrefix = "pf1."

func encodeFeedCursor(vm domain.VMID, pos int64) string {
	return feedCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(string(vm)+":"+strconv.FormatInt(pos, 10)))
}

func decodeFeedCursor(c string, vm domain.VMID) (int64, error) {
	if c == "" {
		return 0, nil
	}
	bad := domain.Errorf(domain.CodeInvalidArgument, "after is not a prompt feed cursor of this VM").WithDetail("field", "after")
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(c, feedCursorPrefix))
	if err != nil || !strings.HasPrefix(c, feedCursorPrefix) {
		return 0, bad
	}
	v, n, ok := strings.Cut(string(raw), ":")
	pos, err := strconv.ParseInt(n, 10, 64)
	if !ok || err != nil || pos < 0 || domain.VMID(v) != vm {
		return 0, bad
	}
	return pos, nil
}

// Feed returns principal's prompt events on ref after cursor, across turns
// and conversations in the order they were recorded (an absent cursor
// replays from the oldest). With wait > 0 it long-polls until an event
// arrives. NextCursor is always set: pass it back to continue, including
// after reconnecting.
func (s *Service) Feed(ctx context.Context, principal domain.PrincipalID, ref, after, conversation string, limit int, wait time.Duration) (api.PromptFeed, error) {
	vm, err := s.cfg.VMs.GetVM(ctx, principal, ref)
	if err != nil {
		return api.PromptFeed{}, err
	}
	pos, err := decodeFeedCursor(after, vm.ID)
	if err != nil {
		return api.PromptFeed{}, err
	}
	if conversation != "" {
		if _, err := s.Conversation(ctx, principal, string(vm.ID), conversation); err != nil {
			return api.PromptFeed{}, err
		}
	}
	if limit <= 0 || limit > api.MaxListLimit {
		limit = api.DefaultListLimit
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		changed := s.watch()
		var evs []state.FeedEvent
		if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
			evs, err = tx.PromptFeed(principal, vm.ID, conversation, pos, limit)
			return err
		}); err != nil {
			return api.PromptFeed{}, err
		}
		if len(evs) > 0 || wait <= 0 {
			out := api.PromptFeed{Items: make([]api.PromptFeedItem, 0, len(evs)), NextCursor: encodeFeedCursor(vm.ID, pos)}
			for _, e := range evs {
				var ev api.PromptEvent
				if err := json.Unmarshal(e.Doc, &ev); err != nil {
					return api.PromptFeed{}, err
				}
				out.NextCursor = encodeFeedCursor(vm.ID, e.Pos)
				out.Items = append(out.Items, api.PromptFeedItem{Cursor: out.NextCursor, Event: ev})
			}
			return out, nil
		}
		select {
		case <-changed:
		case <-deadline.C:
			wait = 0
		case <-ctx.Done():
			return api.PromptFeed{}, ctx.Err()
		case <-s.ctx.Done():
			wait = 0
		}
	}
}

// InterruptWait bounds how long Interrupt waits for the turns it
// cancelled to end.
const InterruptWait = 10 * time.Second

// Interrupt cancels the active turn of one conversation, or every active
// turn of principal's on ref (all). It waits (up to InterruptWait) for
// those turns to end and returns them; none active is an empty list.
func (s *Service) Interrupt(ctx context.Context, principal domain.PrincipalID, ref string, req api.InterruptRequest) (api.InterruptResult, error) {
	if req.All == (req.ConversationID != "") {
		return api.InterruptResult{}, domain.Errorf(domain.CodeInvalidArgument, "give exactly one of conversationId and all").WithDetail("field", "conversationId")
	}
	vm, err := s.cfg.VMs.GetVM(ctx, principal, ref)
	if err != nil {
		return api.InterruptResult{}, err
	}
	if req.ConversationID != "" {
		if _, err := s.Conversation(ctx, principal, string(vm.ID), req.ConversationID); err != nil {
			return api.InterruptResult{}, err
		}
	}
	var active []*state.PromptRecord
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		ps, err := tx.UnfinishedPrompts()
		for _, p := range ps {
			if p.Owner == principal && p.VMID == vm.ID && (req.All || p.ConversationID == req.ConversationID) {
				active = append(active, p)
			}
		}
		return err
	}); err != nil {
		return api.InterruptResult{}, err
	}
	for _, p := range active {
		if err := s.stop(ctx, p, &Error{Code: ErrCancelled, Message: "interrupted"}); err != nil {
			return api.InterruptResult{}, err
		}
	}
	out := api.InterruptResult{Cancelled: make([]api.Prompt, 0, len(active))}
	for _, p := range active {
		got, err := s.settle(ctx, p.ID, InterruptWait)
		if err != nil {
			return api.InterruptResult{}, err
		}
		out.Cancelled = append(out.Cancelled, toPrompt(got))
	}
	return out, nil
}

// settle waits up to d for prompt id to end and returns its record.
func (s *Service) settle(ctx context.Context, id string, d time.Duration) (*state.PromptRecord, error) {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		changed := s.watch()
		var p *state.PromptRecord
		if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) { p, err = tx.GetPrompt(id); return err }); err != nil {
			return nil, err
		}
		if p.Terminal() {
			return p, nil
		}
		select {
		case <-changed:
		case <-deadline.C:
			return p, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ctx.Done():
			return p, nil
		}
	}
}

// SteerRestartWait bounds how long a restart steer waits for the turn it
// cancelled to end (then the harness is killed and waited for as long
// again).
const SteerRestartWait = 15 * time.Second

// Steer sends text to running turn id. A harness that takes input while it
// runs (steer native) gets it in the running turn. Otherwise, or when that
// turn can no longer take input, a harness that resumes sessions (steer
// restart) has the turn cancelled and a new turn of the same conversation
// started with text; the result says which happened. A harness with
// neither answers unsupported_capability.
//
// key is the optional Idempotency-Key, scoped to the turn: a repeat with
// the same text returns the first result without delivering again (also
// once a restart ended the turn); other text is idempotency_conflict.
func (s *Service) Steer(ctx context.Context, principal domain.PrincipalID, ref, id, key, text string) (api.SteerResult, error) {
	if strings.TrimSpace(text) == "" {
		return api.SteerResult{}, domain.Errorf(domain.CodeInvalidArgument, "steer text is required").WithDetail("field", "text")
	}
	unlock := s.steerLock(id)
	defer unlock()
	p, err := s.prompt(ctx, principal, ref, id)
	if err != nil {
		return api.SteerResult{}, err
	}
	sum := sha256.Sum256([]byte(text))
	steer := state.SteerRecord{TextHash: hex.EncodeToString(sum[:])}
	if key != "" {
		if res, ok, err := s.steerReplay(ctx, principal, ref, p, key, steer.TextHash); ok || err != nil {
			return res, err
		}
	}
	ad, err := Lookup(p.Harness)
	if err != nil {
		return api.SteerResult{}, domain.Errorf(domain.CodeInternal, "prompt %s: %v", id, err)
	}
	support := ad.Capabilities().Steer
	switch {
	case support == domain.SteerUnsupported:
		return api.SteerResult{}, domain.Errorf(domain.CodeUnsupportedCapability, "harness %s takes no input during a turn and cannot restart one", p.Harness).
			WithDetail("capability", "harnesses[].steer").WithDetail("harness", p.Harness)
	case p.Terminal():
		return api.SteerResult{}, domain.Errorf(domain.CodeInvalidState, "prompt %s is %s; send a new prompt", id, p.State).WithDetail("promptId", id)
	case p.State == string(api.PromptStarting):
		e := domain.Errorf(domain.CodeOperationInProgress, "prompt %s is still starting", id).WithDetail("promptId", id)
		e.Retryable = true
		return api.SteerResult{}, e
	}
	if support == domain.SteerNative {
		steer.Delivery, steer.PromptID = string(domain.SteerNative), p.ID
		ok, err := s.steerNative(ctx, p, ad, text, key, steer)
		if err != nil {
			return api.SteerResult{}, err
		}
		if ok {
			cur, err := s.prompt(ctx, principal, ref, id)
			if err != nil {
				return api.SteerResult{}, err
			}
			return api.SteerResult{Delivery: domain.SteerNative, Prompt: toPrompt(cur)}, nil
		}
	}
	return s.steerRestart(ctx, principal, ref, p, key, text, steer)
}

// steerLock serializes steers of one turn (so a keyed repeat sees the
// first one's record).
func (s *Service) steerLock(id string) func() {
	s.mu.Lock()
	m := s.steering[id]
	if m == nil {
		m = &sync.Mutex{}
		s.steering[id] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// steerReplay answers a repeat of keyed steer key on p: the recorded
// result, or idempotency_conflict for other text. A restart whose record
// was not written (the daemon stopped in between) is found by its turn's
// derived key.
func (s *Service) steerReplay(ctx context.Context, principal domain.PrincipalID, ref string, p *state.PromptRecord, key, textHash string) (api.SteerResult, bool, error) {
	rec, ok := p.Steers[key]
	if !ok {
		var next *state.PromptRecord
		if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
			next, err = tx.PromptByKey(principal, p.VMID, restartKey(p.ID, key))
			return err
		}); err != nil || next == nil {
			return api.SteerResult{}, false, err
		}
		rec = state.SteerRecord{TextHash: textHash, Delivery: string(domain.SteerRestart), PromptID: next.ID}
	}
	if rec.TextHash != textHash {
		return api.SteerResult{}, true, domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key was used to steer prompt %s with other text", p.ID).
			WithDetail("promptId", p.ID)
	}
	cur, err := s.prompt(ctx, principal, ref, rec.PromptID)
	if err != nil {
		return api.SteerResult{}, true, err
	}
	return api.SteerResult{Delivery: domain.SteerSupport(rec.Delivery), Prompt: toPrompt(cur)}, true, nil
}

// restartKey is the createPrompt key of the turn a keyed restart steer
// starts.
func restartKey(prompt, key string) string { return "steer:" + prompt + ":" + key }

// recordSteer stores keyed steer rec on prompt id.
func recordSteer(tx *state.Tx, id, key string, rec state.SteerRecord) error {
	if key == "" {
		return nil
	}
	cur, err := tx.GetPrompt(id)
	if err != nil {
		return err
	}
	if cur.Steers == nil {
		cur.Steers = map[string]state.SteerRecord{}
	}
	cur.Steers[key] = rec
	return tx.PutPrompt(cur)
}

// steerNative writes text to the running harness; false means the turn no
// longer takes input (its stdin is gone or already closed).
func (s *Service) steerNative(ctx context.Context, p *state.PromptRecord, ad Adapter, text, key string, rec state.SteerRecord) (bool, error) {
	s.mu.Lock()
	lt := s.live[p.ID]
	s.mu.Unlock()
	if lt == nil {
		return false, nil
	}
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if lt.stdin == nil || lt.done {
		return false, nil
	}
	if _, err := lt.stdin.Write(ad.Input(Message{Text: text})); err != nil {
		s.cfg.Log.Warn("steer stdin", "prompt", p.ID, "err", err)
		return false, nil
	}
	lt.sent++
	err := s.cfg.DB.Tx(context.WithoutCancel(ctx), func(tx *state.Tx) error {
		cur, err := tx.GetPrompt(p.ID)
		if err != nil {
			return err
		}
		cur.Messages = lt.sent
		if err := tx.PutPrompt(cur); err != nil {
			return err
		}
		return recordSteer(tx, p.ID, key, rec)
	})
	s.cfg.Log.Info("prompt steered", "prompt", p.ID, "delivery", domain.SteerNative, "messages", lt.sent)
	return true, err
}

// steerRestart cancels p, waits for it to end, and starts the next turn of
// its conversation with text, the same options and a fresh deadline.
func (s *Service) steerRestart(ctx context.Context, principal domain.PrincipalID, ref string, p *state.PromptRecord, key, text string,
	rec state.SteerRecord) (api.SteerResult, error) {
	// The caller's env values were never stored: only this daemon run
	// still has them.
	var env map[string]string
	s.mu.Lock()
	if lt := s.live[p.ID]; lt != nil {
		env = lt.env
	}
	s.mu.Unlock()
	if len(p.EnvNames) > 0 && env == nil {
		return api.SteerResult{}, domain.Errorf(domain.CodeInvalidState,
			"prompt %s was given env values the daemon no longer holds (not stored); cancel it and send a new prompt with env", p.ID).
			WithDetail("promptId", p.ID)
	}
	if err := s.stop(ctx, p, &Error{Code: ErrCancelled, Message: "cancelled to restart the conversation with a steering message"}); err != nil {
		return api.SteerResult{}, err
	}
	done, err := s.settle(ctx, p.ID, SteerRestartWait)
	if err == nil && !done.Terminal() {
		_, _ = s.cfg.Procs.Signal(ctx, p.Owner, string(p.VMID), done.ProcessID, "KILL")
		done, err = s.settle(ctx, p.ID, SteerRestartWait)
	}
	if err != nil {
		return api.SteerResult{}, err
	}
	if !done.Terminal() {
		return api.SteerResult{}, domain.Errorf(domain.CodeUnavailable, "prompt %s did not stop; steer not delivered", p.ID).WithDetail("promptId", p.ID)
	}
	req := api.PromptRequest{Text: text, ConversationID: p.ConversationID, Env: env, Secrets: p.SecretNames}
	if p.ReasoningEffort != "" || p.Fast {
		req.Options = &api.PromptOptions{ReasoningEffort: p.ReasoningEffort, Fast: p.Fast}
	}
	var conv *state.ConversationRecord
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) { conv, err = tx.GetConversation(p.ConversationID); return err }); err != nil {
		return api.SteerResult{}, err
	}
	if conv.SessionID == "" {
		// The harness never reported a session to resume: the new turn
		// carries the interrupted request too.
		req.Text = p.Text + "\n\n" + text
	}
	nextKey := restartKey(p.ID, newID(""))
	if key != "" {
		nextKey = restartKey(p.ID, key)
	}
	next, err := s.Prompt(ctx, principal, ref, nextKey, req)
	if err != nil {
		return api.SteerResult{}, err
	}
	rec.Delivery, rec.PromptID = string(domain.SteerRestart), next.ID
	if err := s.cfg.DB.Tx(context.WithoutCancel(ctx), func(tx *state.Tx) error { return recordSteer(tx, p.ID, key, rec) }); err != nil {
		return api.SteerResult{}, err
	}
	s.cfg.Log.Info("prompt steered", "prompt", p.ID, "delivery", domain.SteerRestart, "next", next.ID)
	return api.SteerResult{Delivery: domain.SteerRestart, Prompt: next}, nil
}

// TurnLease reports when the active-turn lease on vm ends: the latest
// deadline of its unfinished turns, or the zero time when none is active.
// Lifecycle does not auto-stop a VM before then. It reads the database, so
// the lease survives a daemon restart.
func (s *Service) TurnLease(ctx context.Context, vm domain.VMID) (time.Time, error) {
	var until time.Time
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		ps, err := tx.UnfinishedPrompts()
		for _, p := range ps {
			if p.VMID == vm && p.TurnDeadline != nil && p.TurnDeadline.After(until) {
				until = *p.TurnDeadline
			}
		}
		return err
	})
	return until, err
}

// checkControls checks a turn's options and attachments against the
// harness's capabilities.
func checkControls(c domain.HarnessCapabilities, o *api.PromptOptions, atts []attachment) error {
	if o != nil {
		if o.ReasoningEffort != "" && !slices.Contains(c.ReasoningEfforts, o.ReasoningEffort) {
			return domain.Errorf(domain.CodeInvalidArgument, "harness %s takes reasoningEffort %s", c.Name, strings.Join(c.ReasoningEfforts, ", ")).
				WithDetail("field", "options.reasoningEffort")
		}
		if o.Fast && !c.Fast {
			return domain.Errorf(domain.CodeUnsupportedCapability, "harness %s has no fast mode", c.Name).
				WithDetail("capability", "harnesses[].fast").WithDetail("harness", c.Name)
		}
	}
	for _, a := range atts {
		if !slices.Contains(c.AttachmentTypes, a.MediaType) {
			return domain.Errorf(domain.CodeUnsupportedCapability, "harness %s does not take %s attachments (it takes %s)", c.Name, a.MediaType,
				strings.Join(c.AttachmentTypes, ", ")).WithDetail("capability", "harnesses[].attachmentTypes").WithDetail("harness", c.Name)
		}
	}
	return nil
}

func fieldErr(i int, format string, args ...any) error {
	return domain.Errorf(domain.CodeInvalidArgument, "attachments[%d]: %s", i, fmt.Sprintf(format, args...)).
		WithDetail("field", fmt.Sprintf("attachments[%d]", i))
}
