//go:build linux

package main_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/cli"
	"davit/internal/domain"
)

func turnText(t *testing.T, r run) string {
	t.Helper()
	var b strings.Builder
	for i, l := range r.lines {
		if l.Event == api.PromptEventMessageDelta {
			var e api.PromptEvent
			json.Unmarshal(r.raw[i], &e)
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

func finalPrompt(t *testing.T, r run) api.Prompt {
	t.Helper()
	if l := r.last(); l.Event != "result" {
		t.Fatalf("last event %q %+v (stderr %q)", l.Event, l.Error, r.stderr)
	}
	var p api.Prompt
	json.Unmarshal(r.raw[len(r.raw)-1], &p)
	return p
}

// TestPromptEndToEnd drives conversations through the real binary against
// fake claude/codex scripts run by the fake runtime's in-process guest.
func TestPromptEndToEnd(t *testing.T) {
	fakes, _ := filepath.Abs("../../internal/harness/testdata")
	secrets := filepath.Join(t.TempDir(), "secrets.json")
	os.WriteFile(secrets, []byte(`{"FAKE_TOKEN":"from-secrets-file"}`), 0o600)
	h := newHost(t)
	h.guestAgent = true
	h.extraConfig = fmt.Sprintf(`"harness":{"commands":{"claude":%q,"codex":%q},"secretsFile":%q}`,
		filepath.Join(fakes, "fake-claude"), filepath.Join(fakes, "fake-codex"), secrets)
	h.start(20*time.Millisecond, 0)
	h.waited(h.cli("new", "box"), domain.OpCreate)

	r := h.cli("prompt", "box", "hello")
	p1 := finalPrompt(t, r)
	if r.code != 0 || r.lines[0].Event != "prompt.started" || p1.State != api.PromptSucceeded || !strings.Contains(turnText(t, r), "echo: hello resumed=none") {
		t.Fatalf("turn 1: exit %d %+v text %q", r.code, p1, turnText(t, r))
	}
	var sawTool, sawStart bool
	var seq int64
	for _, l := range r.lines[1 : len(r.lines)-1] {
		if l.Seq <= seq {
			t.Fatalf("event seq %d after %d", l.Seq, seq)
		}
		seq = l.Seq
		sawTool = sawTool || l.Event == api.PromptEventToolCall
		sawStart = sawStart || l.Event == api.PromptEventConversationStarted
	}
	if !sawTool || !sawStart || r.lines[len(r.lines)-2].Event != api.PromptEventTurnCompleted {
		t.Fatalf("events %+v", r.lines)
	}

	// The next turn resumes the native session; a daemon secret by name.
	r = h.cli("prompt", "box", "again", "--secret", "FAKE_TOKEN")
	if p2 := finalPrompt(t, r); p2.ConversationID != p1.ConversationID || !strings.Contains(turnText(t, r), "resumed=sess-") ||
		!strings.Contains(turnText(t, r), "token=from-secrets-file") {
		t.Fatalf("turn 2: %+v %q", p2, turnText(t, r))
	}

	// Human mode prints the assistant text; --env passes a caller variable.
	cmd := exec.Command(bin, "prompt", "box", "--env", "FAKE_TOKEN", "--resume", p1.ConversationID, "human")
	cmd.Env = append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "FAKE_TOKEN=from-caller-env")
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "echo: human resumed=sess-") || !strings.Contains(string(out), "token=from-caller-env") {
		t.Fatalf("human prompt: %v %q", err, out)
	}

	// Another harness gets its own conversation.
	if r := h.cli("prompt", "box", "--harness", "codex", "hi"); finalPrompt(t, r).Harness != "codex" || !strings.Contains(turnText(t, r), "echo: hi") {
		t.Fatalf("codex turn: %+v", r.lines)
	}
	r = h.cli("conversations", "box")
	if r.code != 0 || len(r.lines) != 3 {
		t.Fatalf("conversations: %+v", r.lines)
	}
	// Conversations list in stable (createdAt, id) order, so the newest is last.
	var oldest, newest api.Conversation
	json.Unmarshal(r.raw[0], &oldest)
	json.Unmarshal(r.raw[1], &newest)
	if oldest.Harness != "claude" || newest.Harness != "codex" {
		t.Fatalf("conversations in createdAt order: %+v then %+v", oldest, newest)
	}

	// One turn at a time per conversation.
	slow := finalPrompt(t, h.cli("prompt", "box", "--resume", p1.ConversationID, "--no-wait", "slow"))
	h.expectError(h.cli("prompt", "box", "--resume", p1.ConversationID, "too soon"), cli.ExitConflict, domain.CodeOperationInProgress)

	// Steer the running Claude turn: native, followed to its end.
	r = h.cli("steer", "box", slow.ID, "nudge")
	if p := finalPrompt(t, r); r.code != 0 || p.ID != slow.ID || p.State != api.PromptSucceeded || !strings.Contains(turnText(t, r), "echo: nudge") {
		t.Fatalf("steer: exit %d %+v %q (stderr %q)", r.code, p, turnText(t, r), r.stderr)
	}
	var steered api.SteerResult
	json.Unmarshal(r.raw[0], &steered)
	if r.lines[0].Event != "prompt.steered" || steered.Delivery != domain.SteerNative {
		t.Fatalf("steer event %+v", steered)
	}

	// Attachments and options reach the harness.
	png := filepath.Join(t.TempDir(), "shot.png")
	os.WriteFile(png, []byte("\x89PNG\r\n\x1a\n"), 0o644)
	r = h.cli("prompt", "box", "--new", "--attach", png, "--effort", "high", "--fast", "--turn-timeout", "10m", "look")
	if p := finalPrompt(t, r); p.State != api.PromptSucceeded || p.TurnDeadlineAt == nil ||
		!strings.Contains(turnText(t, r), "effort=high fast=yes images=1") || !strings.Contains(turnText(t, r), "/shot.png (image/png)") {
		t.Fatalf("attachment turn: %+v %q", p, turnText(t, r))
	}
	h.expectError(h.cli("prompt", "box", "--new", "--effort", "ludicrous", "x"), cli.ExitUsage, domain.CodeInvalidArgument)

	// The VM-wide feed replays every turn; its cursor continues it.
	r = h.cli("prompt-events", "box")
	var feedEnd struct{ NextCursor string }
	json.Unmarshal(r.raw[len(r.raw)-1], &feedEnd)
	if r.code != 0 || len(r.lines) < 10 || feedEnd.NextCursor == "" {
		t.Fatalf("prompt-events: exit %d, %d lines", r.code, len(r.lines))
	}
	if r := h.cli("prompt-events", "box", "--after", feedEnd.NextCursor); r.code != 0 || len(r.lines) != 1 {
		t.Fatalf("prompt-events after the end: %+v", r.lines)
	}

	// Interrupt every active turn.
	bg := finalPrompt(t, h.cli("prompt", "box", "--new", "--no-wait", "slow"))
	r = h.cli("interrupt", "box", "--all")
	var ir api.InterruptResult
	json.Unmarshal(r.raw[len(r.raw)-1], &ir)
	if r.code != 0 || len(ir.Cancelled) != 1 || ir.Cancelled[0].ID != bg.ID || ir.Cancelled[0].State != api.PromptCancelled {
		t.Fatalf("interrupt: exit %d %+v", r.code, ir)
	}
	h.expectError(h.cli("interrupt", "box"), cli.ExitUsage, domain.CodeInvalidArgument)

	// A failed turn: the result still prints, the exit status reports it.
	r = h.cli("prompt", "box", "--new", "please fail")
	if p := finalPrompt(t, r); r.code != cli.ExitGeneral || p.State != api.PromptFailed || p.Error.Code != "harness_failed" {
		t.Fatalf("failed turn: exit %d %+v", r.code, p)
	}
	// A world-readable secrets file is refused.
	os.Chmod(secrets, 0o644)
	h.expectError(h.cli("prompt", "box", "--new", "x", "--secret", "FAKE_TOKEN"), cli.ExitGeneral, domain.CodeInternal)
	h.expectError(h.cli("prompt", "box", "--env", "DAVIT_TEST_UNSET_VAR", "x"), cli.ExitUsage, domain.CodeInvalidArgument)
}
