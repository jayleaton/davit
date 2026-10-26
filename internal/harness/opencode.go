package harness

import (
	"encoding/json"

	"davit/internal/domain"
)

// opencode drives OpenCode (opencode-ai 1.19.3): `opencode run --format json
// --auto [-m PROVIDER/MODEL] [--variant E] [-s SESSION] [-f FILE]...`,
// prompt on stdin (run takes piped stdin as the message when none is
// given). --auto approves every permission request: the VM is the sandbox.
// Images are --file paths, which OpenCode reads with its Read tool and
// attaches to the message. run takes no input while it works, so a steer
// restarts (cancel, then -s SESSION with the message). Built from the CLI
// source (packages/opencode/src/cli/cmd/run.ts at v1.19.3), not yet run
// against a model: experimental.
type opencode struct{}

func (opencode) Name() string           { return "opencode" }
func (opencode) DefaultCommand() string { return "opencode" }

// Capabilities: --variant names are per provider (Anthropic high|max,
// OpenAI none..xhigh, Google low|high); the list is their union and the
// provider rejects or ignores the rest. There is no fast mode.
func (opencode) Capabilities() domain.HarnessCapabilities {
	return domain.HarnessCapabilities{Name: "opencode", Steer: domain.SteerRestart, AttachmentTypes: attachmentTypes(),
		ReasoningEfforts: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, Experimental: true}
}

func (opencode) InlineImages() bool { return false }

func (opencode) Input(m Message) []byte { return []byte(m.Text) }

func (opencode) Args(t Turn) []string {
	args := []string{"run", "--format", "json", "--auto"}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if t.ReasoningEffort != "" {
		args = append(args, "--variant", t.ReasoningEffort)
	}
	if t.Session != "" {
		args = append(args, "--session", t.Session)
	}
	for _, img := range t.Images {
		args = append(args, "--file", img)
	}
	return args
}

// routeGrants: OpenCode takes a provider's endpoint only from config
// (provider.<id>.options.baseURL), so the proxy base URLs travel as
// OPENCODE_CONFIG_CONTENT, which OpenCode merges over the user's config.
// Its AI SDK clients expect /v1 in the base URL.
func (opencode) routeGrants(env map[string]string) (map[string]string, bool) {
	providers := map[string]any{}
	if u := env["ANTHROPIC_BASE_URL"]; u != "" {
		providers["anthropic"] = map[string]any{"options": map[string]string{"baseURL": u + "/v1"}}
	}
	if u := env["OPENAI_BASE_URL"]; u != "" {
		providers["openai"] = map[string]any{"options": map[string]string{"baseURL": u}}
	}
	if len(providers) > 0 {
		b, _ := json.Marshal(map[string]any{"provider": providers})
		env["OPENCODE_CONFIG_CONTENT"] = string(b)
	}
	return env, true
}

func (opencode) NewParser() Parser { return &opencodeParser{} }

type opencodeParser struct {
	session string
	result  *Result
	usage   *Usage
}

func (p *opencodeParser) Session() string { return p.session }
func (p *opencodeParser) Result() *Result { return p.result }

// Results is 0 or 1: one prompt per process.
func (p *opencodeParser) Results() int {
	if p.result == nil {
		return 0
	}
	return 1
}

// opencodeLine is one `run --format json` line: {type, timestamp,
// sessionID} plus a message part or an error.
type opencodeLine struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      *struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		CallID string `json:"callID"`
		Tool   string `json:"tool"`
		State  *struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
			Error  string          `json:"error"`
		} `json:"state"`
		Cost   float64 `json:"cost"`
		Tokens *struct {
			Input  int64 `json:"input"`
			Output int64 `json:"output"`
			Cache  struct {
				Read int64 `json:"read"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

func (p *opencodeParser) Line(line []byte) []Event {
	var l opencodeLine
	if err := json.Unmarshal(line, &l); err != nil || l.Type == "" {
		return []Event{rawEvent(line)}
	}
	raw := rawJSON(line)
	var out []Event
	// No session event: every line names the session.
	if l.SessionID != "" && p.session == "" {
		p.session = l.SessionID
		out = append(out, Event{Type: EventConversationStarted, SessionID: l.SessionID, Raw: raw})
	}
	part := l.Part
	switch {
	case l.Type == "text" && part != nil && part.Text != "":
		out = append(out, Event{Type: EventMessageDelta, Role: "assistant", Text: part.Text, Raw: raw})
	case l.Type == "tool_use" && part != nil && part.State != nil:
		// Emitted once the tool has finished: call and result together.
		failed := part.State.Status == "error"
		output := part.State.Output
		if failed {
			output = part.State.Error
		}
		out = append(out,
			Event{Type: EventToolCall, ToolCallID: part.CallID, ToolName: part.Tool, Input: part.State.Input, Raw: raw},
			Event{Type: EventToolResult, ToolCallID: part.CallID, ToolName: part.Tool, Output: output, IsError: failed, Raw: raw})
	case l.Type == "step_finish" && part != nil:
		// Usage is per step: the turn's is the sum.
		if p.usage == nil {
			p.usage = &Usage{}
		}
		if t := part.Tokens; t != nil {
			p.usage.InputTokens += t.Input
			p.usage.OutputTokens += t.Output
			p.usage.CachedInputTokens += t.Cache.Read
		}
		p.usage.CostUSD += part.Cost
		if p.result == nil {
			p.result = &Result{}
		}
		p.result.Usage = p.usage
	case l.Type == "error":
		msg := "opencode reported an error"
		if e := l.Error; e != nil {
			msg = e.Name
			if e.Data.Message != "" {
				msg = e.Data.Message
			}
		}
		if p.result == nil || !p.result.Failed {
			p.result = &Result{Failed: true, Message: msg, Usage: p.usage}
		} else {
			p.result.Message += "\n" + msg
		}
	}
	if len(out) == 0 {
		out = append(out, Event{Type: EventRaw, Raw: raw})
	}
	return out
}
