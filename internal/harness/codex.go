package harness

import (
	"encoding/json"
	"strconv"

	"davit/internal/domain"
)

// codex drives Codex: `codex exec [resume] [--image F]... --json
// --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox
// [--model M] [-c model_reasoning_effort=E] [-c service_tier="fast"]
// [SESSION] -`, prompt on stdin. Approvals and Codex's own sandbox are
// bypassed: the VM is the sandbox. `codex exec` reads one prompt and takes
// no input while it runs, so a steer cancels the turn and resumes the
// session with the message (steer restart).
type codex struct{}

func (codex) Name() string           { return "codex" }
func (codex) DefaultCommand() string { return "codex" }

func (codex) Capabilities() domain.HarnessCapabilities {
	return domain.HarnessCapabilities{Name: "codex", Steer: domain.SteerRestart, AttachmentTypes: attachmentTypes(),
		ReasoningEfforts: []string{"none", "minimal", "low", "medium", "high", "xhigh"}, Fast: true}
}

func (codex) InlineImages() bool { return false }

func (codex) Input(m Message) []byte { return []byte(m.Text) }

func (codex) Args(t Turn) []string {
	args := []string{"exec"}
	if t.Session != "" {
		args = append(args, "resume")
	}
	// --image takes several values: the next flag ends the list.
	for _, img := range t.Images {
		args = append(args, "--image", img)
	}
	args = append(args, "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox")
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.ReasoningEffort != "" {
		args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(t.ReasoningEffort))
	}
	if t.Fast {
		args = append(args, "-c", `service_tier="fast"`)
	}
	if t.Session != "" {
		args = append(args, t.Session)
	}
	return append(args, "-")
}

func (codex) NewParser() Parser { return &codexParser{called: map[string]bool{}} }

type codexParser struct {
	session string
	result  *Result
	called  map[string]bool // item IDs already reported as tool.call
}

func (p *codexParser) Session() string { return p.session }
func (p *codexParser) Result() *Result { return p.result }

// Results is 0 or 1: one prompt per process.
func (p *codexParser) Results() int {
	if p.result == nil {
		return 0
	}
	return 1
}

type codexItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	ExitCode         *int            `json:"exit_code"`
	Status           string          `json:"status"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Changes          json.RawMessage `json:"changes"`
	Query            string          `json:"query"`
	Message          string          `json:"message"`
}

type codexLine struct {
	Type     string     `json:"type"`
	ThreadID string     `json:"thread_id"`
	Item     *codexItem `json:"item"`
	Usage    *struct {
		InputTokens       int64 `json:"input_tokens"`
		CachedInputTokens int64 `json:"cached_input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// toolInfo maps tool-like items to a name and input; ok is false for
// non-tool items.
func toolInfo(it *codexItem) (name string, input json.RawMessage, ok bool) {
	switch it.Type {
	case "command_execution":
		b, _ := json.Marshal(map[string]string{"command": it.Command})
		return "shell", b, true
	case "mcp_tool_call":
		return it.Server + "." + it.Tool, it.Arguments, true
	case "file_change":
		return "file_change", it.Changes, true
	case "web_search":
		b, _ := json.Marshal(map[string]string{"query": it.Query})
		return "web_search", b, true
	}
	return "", nil, false
}

func (p *codexParser) Line(line []byte) []Event {
	var l codexLine
	if err := json.Unmarshal(line, &l); err != nil || l.Type == "" {
		return []Event{rawEvent(line)}
	}
	raw := rawJSON(line)
	var out []Event
	switch l.Type {
	case "thread.started":
		if l.ThreadID != "" {
			p.session = l.ThreadID
			out = append(out, Event{Type: EventConversationStarted, SessionID: l.ThreadID, Raw: raw})
		}
	case "item.started", "item.completed":
		it := l.Item
		if it == nil {
			break
		}
		if it.Type == "agent_message" && l.Type == "item.completed" && it.Text != "" {
			out = append(out, Event{Type: EventMessageDelta, Role: "assistant", Text: it.Text, Raw: raw})
			break
		}
		name, input, ok := toolInfo(it)
		if !ok {
			break
		}
		if !p.called[it.ID] {
			p.called[it.ID] = true
			out = append(out, Event{Type: EventToolCall, ToolCallID: it.ID, ToolName: name, Input: input, Raw: raw})
		}
		if l.Type == "item.completed" {
			failed := it.Status == "failed" || (it.ExitCode != nil && *it.ExitCode != 0)
			out = append(out, Event{Type: EventToolResult, ToolCallID: it.ID, ToolName: name, Output: it.AggregatedOutput, IsError: failed, Raw: raw})
		}
	case "turn.completed":
		r := &Result{}
		if l.Usage != nil {
			r.Usage = &Usage{InputTokens: l.Usage.InputTokens, OutputTokens: l.Usage.OutputTokens, CachedInputTokens: l.Usage.CachedInputTokens}
		}
		p.result = r
	case "turn.failed":
		msg := "turn failed"
		if l.Error != nil && l.Error.Message != "" {
			msg = l.Error.Message
		}
		p.result = &Result{Failed: true, Message: msg}
	case "error":
		if p.result == nil || !p.result.Failed {
			p.result = &Result{Failed: true, Message: l.Message}
		}
	}
	if len(out) == 0 {
		out = append(out, Event{Type: EventRaw, Raw: raw})
	}
	return out
}
