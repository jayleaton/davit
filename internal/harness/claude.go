package harness

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"davit/internal/domain"
)

// claude drives Claude Code: `claude -p --input-format stream-json
// --output-format stream-json --verbose [--model M] [--effort E]
// [--settings {"fastMode":true}] [--resume SESSION]`. Messages are
// stream-json user lines on stdin, so images travel inline and a message
// written during the turn reaches the running harness (steer native); the
// harness answers each message with a result line and exits when stdin
// closes. Permission prompts are bypassed: the VM is the sandbox.
type claude struct{}

func (claude) Name() string           { return "claude" }
func (claude) DefaultCommand() string { return "claude" }

func (claude) Capabilities() domain.HarnessCapabilities {
	return domain.HarnessCapabilities{Name: "claude", Steer: domain.SteerNative, AttachmentTypes: attachmentTypes(),
		ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}, Fast: true}
}

func (claude) InlineImages() bool { return true }

func (claude) Args(t Turn) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.ReasoningEffort != "" {
		args = append(args, "--effort", t.ReasoningEffort)
	}
	if t.Fast {
		args = append(args, "--settings", `{"fastMode":true}`)
	}
	if t.Session != "" {
		args = append(args, "--resume", t.Session)
	}
	return args
}

// claudeBlock is a user message content block (text or base64 image).
type claudeBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source,omitempty"`
}

func (claude) Input(m Message) []byte {
	blocks := []claudeBlock{{Type: "text", Text: m.Text}}
	for _, im := range m.Images {
		b := claudeBlock{Type: "image"}
		b.Source = &struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		}{"base64", im.MediaType, base64.StdEncoding.EncodeToString(im.Data)}
		blocks = append(blocks, b)
	}
	type msg struct {
		Role    string        `json:"role"`
		Content []claudeBlock `json:"content"`
	}
	line, _ := json.Marshal(struct {
		Type    string `json:"type"`
		Message msg    `json:"message"`
	}{"user", msg{"user", blocks}})
	return append(line, '\n')
}

func (claude) NewParser() Parser { return &claudeParser{} }

type claudeParser struct {
	session string
	result  *Result
	results int
}

func (p *claudeParser) Session() string { return p.session }
func (p *claudeParser) Result() *Result { return p.result }
func (p *claudeParser) Results() int    { return p.results }

// claudeLine is the subset of stream-json this parser reads.
type claudeLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Message   struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		} `json:"content"`
	} `json:"message"`
	IsError bool    `json:"is_error"`
	Result  string  `json:"result"`
	CostUSD float64 `json:"total_cost_usd"`
	Usage   *struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		CacheReadInputToken int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func (p *claudeParser) Line(line []byte) []Event {
	var l claudeLine
	if err := json.Unmarshal(line, &l); err != nil || l.Type == "" {
		return []Event{rawEvent(line)}
	}
	raw := rawJSON(line)
	var out []Event
	if l.SessionID != "" && p.session == "" {
		p.session = l.SessionID
		out = append(out, Event{Type: EventConversationStarted, SessionID: l.SessionID, Raw: raw})
		if l.Type == "system" {
			return out
		}
	}
	switch l.Type {
	case "assistant":
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				if c.Text != "" {
					out = append(out, Event{Type: EventMessageDelta, Role: "assistant", Text: c.Text, Raw: raw})
				}
			case "tool_use":
				out = append(out, Event{Type: EventToolCall, ToolCallID: c.ID, ToolName: c.Name, Input: c.Input, Raw: raw})
			}
		}
	case "user":
		for _, c := range l.Message.Content {
			if c.Type == "tool_result" {
				out = append(out, Event{Type: EventToolResult, ToolCallID: c.ToolUseID, Output: toolText(c.Content), IsError: c.IsError, Raw: raw})
			}
		}
	case "result":
		r := &Result{Failed: l.IsError || strings.HasPrefix(l.Subtype, "error")}
		if r.Failed {
			r.Message = l.Result
			if r.Message == "" {
				r.Message = l.Subtype
			}
		}
		if l.Usage != nil {
			r.Usage = &Usage{InputTokens: l.Usage.InputTokens, OutputTokens: l.Usage.OutputTokens,
				CachedInputTokens: l.Usage.CacheReadInputToken, CostUSD: l.CostUSD}
		}
		// A steered turn has one result per message: tokens add up, cost
		// is the process's running total, and the turn failed if any
		// message did.
		if prev := p.result; prev != nil {
			if prev.Failed && !r.Failed {
				r.Failed, r.Message = true, prev.Message
			}
			if prev.Usage != nil {
				if r.Usage == nil {
					r.Usage = &Usage{}
				}
				r.Usage.InputTokens += prev.Usage.InputTokens
				r.Usage.OutputTokens += prev.Usage.OutputTokens
				r.Usage.CachedInputTokens += prev.Usage.CachedInputTokens
				r.Usage.CostUSD = max(r.Usage.CostUSD, prev.Usage.CostUSD)
			}
		}
		p.result = r
		p.results++
		// The service emits turn.completed once the process exits; keep the
		// line itself as raw.
		out = append(out, Event{Type: EventRaw, Raw: raw})
	}
	if len(out) == 0 {
		out = append(out, Event{Type: EventRaw, Raw: raw})
	}
	return out
}

// toolText flattens a tool_result content: a string or text blocks.
func toolText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			b.WriteString(x.Text)
		}
		return b.String()
	}
	return string(raw)
}
