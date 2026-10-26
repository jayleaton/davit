package harness

import (
	"encoding/json"
	"strings"

	"davit/internal/domain"
)

// pi drives the pi coding agent (@earendil-works/pi-coding-agent 1.4.0):
// `pi --mode json [--model M] [--thinking E] [--session-id SESSION]
// [@IMAGE]...`, prompt on stdin (json mode prepends piped stdin to the
// first prompt, which is then the whole prompt). pi has no tool approvals.
// Images are @path arguments, which pi attaches as images. --session-id
// opens that exact session of the working directory (the conversation's
// cwd, fixed per conversation). JSON mode takes no input while it runs, so
// a steer restarts; its RPC mode could steer natively but is not used yet.
// Built from the CLI docs and source (docs/cli.md, docs/json.md), not yet
// run against a model: experimental.
type pi struct{}

func (pi) Name() string           { return "pi" }
func (pi) DefaultCommand() string { return "pi" }

// Capabilities: --thinking levels, clamped by pi to what the model
// supports. There is no fast mode.
func (pi) Capabilities() domain.HarnessCapabilities {
	return domain.HarnessCapabilities{Name: "pi", Steer: domain.SteerRestart, AttachmentTypes: attachmentTypes(),
		ReasoningEfforts: []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}, Experimental: true}
}

func (pi) InlineImages() bool { return false }

func (pi) Input(m Message) []byte { return []byte(m.Text) }

func (pi) Args(t Turn) []string {
	args := []string{"--mode", "json"}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.ReasoningEffort != "" {
		args = append(args, "--thinking", t.ReasoningEffort)
	}
	if t.Session != "" {
		args = append(args, "--session-id", t.Session)
	}
	for _, img := range t.Images {
		args = append(args, "@"+img)
	}
	return args
}

// routeGrants: pi's built-in providers carry fixed endpoints and read no
// base URL variable (an override needs models.json or an extension in its
// agent directory), so a placeholder would reach the real provider. Grants
// are not applied to pi turns.
func (pi) routeGrants(map[string]string) (map[string]string, bool) { return nil, false }

func (pi) NewParser() Parser { return &piParser{} }

type piParser struct {
	session string
	result  *Result
}

func (p *piParser) Session() string { return p.session }
func (p *piParser) Result() *Result { return p.result }

// Results is 0 or 1: one prompt per process.
func (p *piParser) Results() int {
	if p.result == nil {
		return 0
	}
	return 1
}

// piLine is the subset of `--mode json` records this parser reads.
type piLine struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"` // session header
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Result     *struct {
		Content json.RawMessage `json:"content"`
	} `json:"result"`
	IsError bool `json:"isError"`
	Message *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		// Assistant messages only.
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
		Usage        *struct {
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			CacheRead int64 `json:"cacheRead"`
			Cost      struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"usage"`
	} `json:"message"`
}

func (p *piParser) Line(line []byte) []Event {
	var l piLine
	if err := json.Unmarshal(line, &l); err != nil || l.Type == "" {
		return []Event{rawEvent(line)}
	}
	raw := rawJSON(line)
	var out []Event
	switch l.Type {
	case "session":
		if l.ID != "" && p.session == "" {
			p.session = l.ID
			out = append(out, Event{Type: EventConversationStarted, SessionID: l.ID, Raw: raw})
		}
	case "tool_execution_start":
		out = append(out, Event{Type: EventToolCall, ToolCallID: l.ToolCallID, ToolName: l.ToolName, Input: l.Args, Raw: raw})
	case "tool_execution_end":
		var output string
		if l.Result != nil {
			output = toolText(l.Result.Content)
		}
		out = append(out, Event{Type: EventToolResult, ToolCallID: l.ToolCallID, ToolName: l.ToolName, Output: output, IsError: l.IsError, Raw: raw})
	case "message_end":
		// The authoritative message; streamed deltas stay raw.
		m := l.Message
		if m == nil || m.Role != "assistant" {
			break
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		json.Unmarshal(m.Content, &blocks)
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				out = append(out, Event{Type: EventMessageDelta, Role: "assistant", Text: b.Text, Raw: raw})
			}
		}
		p.assistant(m.StopReason, m.ErrorMessage)
		if u := m.Usage; u != nil {
			r := p.result
			if r.Usage == nil {
				r.Usage = &Usage{}
			}
			r.Usage.InputTokens += u.Input
			r.Usage.OutputTokens += u.Output
			r.Usage.CachedInputTokens += u.CacheRead
			r.Usage.CostUSD += u.Cost.Total
		}
	case "auto_retry_start":
		// pi retries a failed response itself; the retry decides.
		if p.result != nil {
			p.result.Failed, p.result.Message = false, ""
		}
	}
	if len(out) == 0 {
		out = append(out, Event{Type: EventRaw, Raw: raw})
	}
	return out
}

// assistant records one assistant message's outcome. JSON mode exits 0
// whatever the stop reason, so the stop reason decides the turn.
func (p *piParser) assistant(stop, errMsg string) {
	if p.result == nil {
		p.result = &Result{}
	}
	if stop != "error" && stop != "aborted" {
		return
	}
	msg := strings.TrimSpace(errMsg)
	if msg == "" {
		msg = "pi stopped: " + stop
	}
	p.result.Failed, p.result.Message = true, msg
}
