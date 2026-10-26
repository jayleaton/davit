package harness

import (
	"bufio"
	"os"
	"slices"
	"strings"
	"testing"
)

func parse(t *testing.T, harness, fixture string) (Parser, []Event) {
	t.Helper()
	a, err := Lookup(harness)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("testdata/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p := a.NewParser()
	var evs []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		evs = append(evs, p.Line(sc.Bytes())...)
	}
	return p, evs
}

func types(evs []Event) []string {
	var out []string
	for _, e := range evs {
		if e.Type != EventRaw {
			out = append(out, e.Type)
		}
	}
	return out
}

func text(evs []Event) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Type == EventMessageDelta {
			b.WriteString(e.Text + "|")
		}
	}
	return b.String()
}

func TestClaudeNormalization(t *testing.T) {
	p, evs := parse(t, "claude", "claude.jsonl")
	want := []string{EventConversationStarted, EventMessageDelta, EventToolCall, EventToolResult, EventMessageDelta}
	if got := types(evs); !slices.Equal(got, want) {
		t.Fatalf("types %v, want %v", got, want)
	}
	if p.Session() != "9f1c2b7e-5d3a-4c1e-8f7a-1b2c3d4e5f60" || evs[0].SessionID != p.Session() {
		t.Fatalf("session %q", p.Session())
	}
	if text(evs) != "Let me look at the files.|There are two files: README.md and main.go.|" {
		t.Fatalf("text %q", text(evs))
	}
	call, res := evs[2], evs[3]
	if call.ToolName != "Bash" || call.ToolCallID != "toolu_01" || !strings.Contains(string(call.Input), `"ls"`) ||
		res.ToolCallID != "toolu_01" || res.Output != "README.md\nmain.go\n" || res.IsError {
		t.Fatalf("tool events %+v %+v", call, res)
	}
	r := p.Result()
	if r == nil || r.Failed || r.Usage.InputTokens != 52 || r.Usage.OutputTokens != 42 || r.Usage.CachedInputTokens != 30 || r.Usage.CostUSD != 0.0123 {
		t.Fatalf("result %+v %+v", r, r.Usage)
	}
	for _, e := range evs {
		if len(e.Raw) == 0 {
			t.Fatalf("event %s lost its raw line", e.Type)
		}
	}
	_, errEvs := parse(t, "claude", "claude_error.jsonl")
	pe, _ := parse(t, "claude", "claude_error.jsonl")
	if r := pe.Result(); r == nil || !r.Failed || !strings.Contains(r.Message, "Invalid API key") || len(errEvs) != 2 {
		t.Fatalf("error result %+v", r)
	}
}

func TestCodexNormalization(t *testing.T) {
	p, evs := parse(t, "codex", "codex.jsonl")
	want := []string{EventConversationStarted, EventToolCall, EventToolResult, EventToolCall, EventToolResult, EventMessageDelta}
	if got := types(evs); !slices.Equal(got, want) {
		t.Fatalf("types %v, want %v", got, want)
	}
	if p.Session() != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Fatalf("session %q", p.Session())
	}
	var results []Event
	for _, e := range evs {
		if e.Type == EventToolResult {
			results = append(results, e)
		}
	}
	if results[0].IsError || results[0].ToolName != "shell" || !results[1].IsError {
		t.Fatalf("tool results %+v", results)
	}
	if r := p.Result(); r == nil || r.Failed || r.Usage.InputTokens != 2400 || r.Usage.CachedInputTokens != 1800 {
		t.Fatalf("result %+v", r)
	}
	pe, _ := parse(t, "codex", "codex_error.jsonl")
	if r := pe.Result(); r == nil || !r.Failed || r.Message != "401 Unauthorized" {
		t.Fatalf("error result %+v", r)
	}
}

func TestOpenCodeNormalization(t *testing.T) {
	p, evs := parse(t, "opencode", "opencode.jsonl")
	want := []string{EventConversationStarted, EventMessageDelta, EventToolCall, EventToolResult, EventToolCall, EventToolResult, EventMessageDelta}
	if got := types(evs); !slices.Equal(got, want) {
		t.Fatalf("types %v, want %v", got, want)
	}
	if p.Session() != "ses_3f2a9c1b7ffeYb2k9Q4tTn0aZx" || evs[0].SessionID != p.Session() {
		t.Fatalf("session %q", p.Session())
	}
	if text(evs) != "Let me look at the files.|There are two files: README.md and main.go.|" {
		t.Fatalf("text %q", text(evs))
	}
	call, res, bad := evs[2], evs[3], evs[5]
	if call.ToolName != "bash" || call.ToolCallID != "toolu_01" || !strings.Contains(string(call.Input), `"ls"`) ||
		res.ToolCallID != "toolu_01" || res.Output != "README.md\nmain.go\n" || res.IsError ||
		!bad.IsError || bad.Output != "File not found: /nope" {
		t.Fatalf("tool events %+v %+v %+v", call, res, bad)
	}
	// Usage is per step; the turn's is the sum.
	r := p.Result()
	if r == nil || r.Failed || r.Usage.InputTokens != 2500 || r.Usage.OutputTokens != 42 || r.Usage.CachedInputTokens != 1500 ||
		r.Usage.CostUSD < 0.0059 || r.Usage.CostUSD > 0.0061 {
		t.Fatalf("result %+v %+v", r, r.Usage)
	}
	for _, e := range evs {
		if len(e.Raw) == 0 {
			t.Fatalf("event %s lost its raw line", e.Type)
		}
	}
	pe, errEvs := parse(t, "opencode", "opencode_error.jsonl")
	if r := pe.Result(); r == nil || !r.Failed || r.Message != "invalid x-api-key" || len(errEvs) != 1 {
		t.Fatalf("error result %+v %v", r, types(errEvs))
	}
}

func TestPiNormalization(t *testing.T) {
	p, evs := parse(t, "pi", "pi.jsonl")
	want := []string{EventConversationStarted, EventMessageDelta, EventToolCall, EventToolResult, EventToolCall, EventToolResult, EventMessageDelta}
	if got := types(evs); !slices.Equal(got, want) {
		t.Fatalf("types %v, want %v", got, want)
	}
	if p.Session() != "8d0f5a2e-6b1c-4f7e-9a3d-2c4b6e8f0a1b" || evs[0].SessionID != p.Session() {
		t.Fatalf("session %q", p.Session())
	}
	// Streamed deltas stay raw; message_end carries the text.
	if text(evs) != "Let me look at the files.|There are two files: README.md and main.go.|" {
		t.Fatalf("text %q", text(evs))
	}
	var tools []Event
	for _, e := range evs {
		if e.Type == EventToolCall || e.Type == EventToolResult {
			tools = append(tools, e)
		}
	}
	if tools[0].ToolName != "bash" || tools[0].ToolCallID != "call_01" || !strings.Contains(string(tools[0].Input), `"ls"`) ||
		tools[1].Output != "README.md\nmain.go\n" || tools[1].IsError || !tools[3].IsError || tools[3].Output != "ENOENT: /nope" {
		t.Fatalf("tool events %+v", tools)
	}
	r := p.Result()
	if r == nil || r.Failed || r.Usage.InputTokens != 2500 || r.Usage.OutputTokens != 42 || r.Usage.CachedInputTokens != 1500 ||
		r.Usage.CostUSD < 0.0085 || r.Usage.CostUSD > 0.0086 {
		t.Fatalf("result %+v %+v", r, r.Usage)
	}
	// JSON mode exits 0 on a model error: the stop reason fails the turn.
	// A retried error is superseded by the retry's outcome.
	pe, _ := parse(t, "pi", "pi_error.jsonl")
	if r := pe.Result(); r == nil || !r.Failed || !strings.Contains(r.Message, "invalid x-api-key") || strings.Contains(r.Message, "overloaded") {
		t.Fatalf("error result %+v", r)
	}
	retried := pi{}.NewParser()
	for _, l := range []string{`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"503"}}`,
		`{"type":"auto_retry_start","attempt":1}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stopReason":"stop"}}`} {
		retried.Line([]byte(l))
	}
	if r := retried.Result(); r == nil || r.Failed {
		t.Fatalf("retried result %+v", r)
	}
}

// TestArgs pins each harness's real CLI flags (claude --help, codex exec
// --help, OpenCode's run.ts, pi's docs/cli.md): options, images and resume.
func TestArgs(t *testing.T) {
	c, _ := Lookup("claude-code")
	if got := strings.Join(c.Args(Turn{Model: "opus", Session: "s1"}), " "); got !=
		"-p --input-format stream-json --output-format stream-json --verbose --dangerously-skip-permissions --model opus --resume s1" {
		t.Fatalf("claude args %q", got)
	}
	if got := strings.Join(c.Args(Turn{ReasoningEffort: "xhigh", Fast: true}), " "); !strings.HasSuffix(got, `--effort xhigh --settings {"fastMode":true}`) {
		t.Fatalf("claude options %q", got)
	}
	x, _ := Lookup("codex")
	if got := strings.Join(x.Args(Turn{}), " "); !strings.HasPrefix(got, "exec --json") || !strings.HasSuffix(got, " -") {
		t.Fatalf("codex new args %q", got)
	}
	if got := strings.Join(x.Args(Turn{Session: "t1", Model: "gpt-5"}), " "); !strings.HasPrefix(got, "exec resume --json") || !strings.HasSuffix(got, "--model gpt-5 t1 -") {
		t.Fatalf("codex resume args %q", got)
	}
	// --image takes several values, so it comes before the next flag;
	// options are config overrides.
	got := strings.Join(x.Args(Turn{Session: "t1", Images: []string{"/a.png", "/b.jpg"}, ReasoningEffort: "high", Fast: true}), " ")
	if got != `exec resume --image /a.png --image /b.jpg --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox `+
		`-c model_reasoning_effort="high" -c service_tier="fast" t1 -` {
		t.Fatalf("codex options %q", got)
	}
	// Claude messages are stream-json user lines with inline images.
	in := string(c.Input(Message{Text: "look", Images: []Image{{MediaType: "image/png", Data: []byte("PNG")}}}))
	if in != `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"look"},`+
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"UE5H"}}]}}`+"\n" {
		t.Fatalf("claude input %q", in)
	}
	if string(x.Input(Message{Text: "look"})) != "look" {
		t.Fatal("codex input is the prompt text")
	}
	// OpenCode: run, JSON events, approvals bypassed; images are --file.
	o, _ := Lookup("OpenCode")
	if got := strings.Join(o.Args(Turn{}), " "); got != "run --format json --auto" {
		t.Fatalf("opencode new args %q", got)
	}
	if got := strings.Join(o.Args(Turn{Model: "anthropic/claude-sonnet-4-5", ReasoningEffort: "high", Session: "ses_1", Images: []string{"/a.png", "/b.jpg"}}), " "); got !=
		"run --format json --auto --model anthropic/claude-sonnet-4-5 --variant high --session ses_1 --file /a.png --file /b.jpg" {
		t.Fatalf("opencode options %q", got)
	}
	// pi: JSON mode; images are @path arguments.
	pa, _ := Lookup("pi")
	if got := strings.Join(pa.Args(Turn{}), " "); got != "--mode json" {
		t.Fatalf("pi new args %q", got)
	}
	if got := strings.Join(pa.Args(Turn{Model: "anthropic/claude-sonnet-4-5", ReasoningEffort: "xhigh", Session: "s1", Images: []string{"/a.png"}}), " "); got !=
		"--mode json --model anthropic/claude-sonnet-4-5 --thinking xhigh --session-id s1 @/a.png" {
		t.Fatalf("pi options %q", got)
	}
	for _, a := range []Adapter{o, pa} {
		if string(a.Input(Message{Text: "look"})) != "look" || a.InlineImages() || !a.Capabilities().Experimental || a.Capabilities().Fast {
			t.Fatalf("%s: input, images or capabilities", a.Name())
		}
	}
	if c.Capabilities().Experimental || x.Capabilities().Experimental {
		t.Fatal("claude and codex are not experimental")
	}
	if _, err := Lookup("aider"); err == nil {
		t.Fatal("unknown harness accepted")
	}
	// Garbage lines are kept, not dropped.
	if evs := c.NewParser().Line([]byte("not json")); len(evs) != 1 || evs[0].Type != EventRaw || string(evs[0].Raw) != `"not json"` {
		t.Fatalf("garbage line: %+v", evs)
	}
}

// TestGrantRouting: OpenCode takes the proxy base URLs as config content;
// pi cannot be pointed at the proxy, so it gets no grant variables.
func TestGrantRouting(t *testing.T) {
	env := map[string]string{"ANTHROPIC_API_KEY": "ph", "ANTHROPIC_BASE_URL": "http://p", "OPENAI_BASE_URL": "http://p/v1"}
	out, ok := opencode{}.routeGrants(env)
	if !ok || out["OPENCODE_CONFIG_CONTENT"] != `{"provider":{"anthropic":{"options":{"baseURL":"http://p/v1"}},"openai":{"options":{"baseURL":"http://p/v1"}}}}` ||
		out["ANTHROPIC_API_KEY"] != "ph" {
		t.Fatalf("opencode %v %v", out, ok)
	}
	if out, ok := (opencode{}).routeGrants(map[string]string{"GITHUB_TOKEN": "ph"}); !ok || out["OPENCODE_CONFIG_CONTENT"] != "" {
		t.Fatalf("opencode without a provider grant %v", out)
	}
	if out, ok := (pi{}).routeGrants(env); ok || out != nil {
		t.Fatalf("pi %v %v", out, ok)
	}
}
