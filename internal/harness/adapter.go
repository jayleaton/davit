// Package harness runs coding-agent CLIs (Claude Code, Codex, OpenCode, pi)
// inside a VM
// as conversations of turns (SPEC §8 M4 "prompt"). A turn is one harness
// process started through the guest process API: detached, prompt text on
// stdin, machine-readable output on stdout. Adapters build the argv and
// normalize the harness's own JSON lines into a small stable event schema,
// keeping every raw line alongside. Davit never re-implements a model
// API: the harness is the agent, and its native session ID is what makes
// --resume continue a conversation.
//
// Harnesses are not installed at prompt time. A missing binary fails the
// turn with unsupported_capability naming the image capability
// (harness.claude, harness.codex, harness.opencode, harness.pi).
package harness

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"davit/internal/domain"
)

// Turn is what an adapter needs to build one harness invocation.
type Turn struct {
	// Command is the harness executable: a bare name is resolved by a login
	// shell in the guest (so ~/.local/bin installs work), an absolute path
	// runs directly.
	Command string
	Model   string
	// Session is the native session to resume, or "" for the first turn.
	Session string
	// ReasoningEffort and Fast are PromptOptions, already checked against
	// Capabilities.
	ReasoningEffort string
	Fast            bool
	// Images are guest paths of image attachments, for harnesses that take
	// images as files on the command line.
	Images []string
}

// Message is one user message on a harness's stdin.
type Message struct {
	Text   string
	Images []Image
}

// Image is an image attachment's bytes, for harnesses that take images
// inline.
type Image struct {
	MediaType string
	Data      []byte
}

// Adapter knows one harness CLI.
type Adapter interface {
	Name() string
	// DefaultCommand is the executable name inside the guest.
	DefaultCommand() string
	// Args are the arguments after the command; the prompt goes to stdin.
	Args(t Turn) []string
	NewParser() Parser
	// Capabilities are the controls the harness CLI really has
	// (HostCapabilities.harnesses).
	Capabilities() domain.HarnessCapabilities
	// Input encodes one message for stdin. With Steer native, stdin stays
	// open for further messages and the harness reports one result per
	// message (Parser.Results); otherwise stdin is closed after the first.
	Input(m Message) []byte
	// InlineImages: image attachments travel in Input as bytes rather than
	// in Turn.Images as paths.
	InlineImages() bool
}

// Parser turns harness stdout lines into events. It is not safe for
// concurrent use.
type Parser interface {
	// Line normalizes one stdout line. Unrecognized lines yield one "raw"
	// event so nothing is dropped.
	Line(line []byte) []Event
	// Session is the native session ID seen so far.
	Session() string
	// Result is the harness's own end-of-turn report, if any.
	Result() *Result
	// Results counts the end-of-message reports seen (one per Input
	// message for a streaming harness).
	Results() int
}

// Result is a harness's final report for a turn.
type Result struct {
	Failed  bool
	Message string // error text when Failed
	Usage   *Usage
}

// Event types (stable API).
const (
	EventConversationStarted = "conversation.started"
	EventMessageDelta        = "message.delta"
	EventToolCall            = "tool.call"
	EventToolResult          = "tool.result"
	EventTurnCompleted       = "turn.completed"
	EventRaw                 = "raw"
)

// Event is one normalized harness event. Seq, time and IDs are filled in
// by the service.
type Event struct {
	Type string `json:"type"`
	// SessionID is the harness-native session (conversation.started).
	SessionID string `json:"sessionId,omitempty"`
	// Role and Text carry assistant output (message.delta).
	Role string `json:"role,omitempty"`
	Text string `json:"text,omitempty"`
	// Tool calls and results.
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     string          `json:"output,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
	// Turn end (turn.completed).
	Status string `json:"status,omitempty"` // succeeded | failed | cancelled
	Usage  *Usage `json:"usage,omitempty"`
	Error  *Error `json:"error,omitempty"`
	// Raw is the harness line this event came from.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Usage is token accounting as reported by the harness.
type Usage struct {
	InputTokens       int64   `json:"inputTokens"`
	OutputTokens      int64   `json:"outputTokens"`
	CachedInputTokens int64   `json:"cachedInputTokens,omitempty"`
	CostUSD           float64 `json:"costUsd,omitempty"`
}

// Error is a turn failure.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Turn error codes (Event.Error.Code). They are harness-level, distinct
// from API error codes.
const (
	ErrHarnessMissing = "harness_missing" // binary not in the image
	ErrHarnessFailed  = "harness_failed"  // the harness reported an error
	ErrHarnessExit    = "harness_exit"    // non-zero exit without a report
	ErrCancelled      = "cancelled"
	ErrLost           = "lost"         // the turn's process or output was lost
	ErrTimeout        = "turn_timeout" // turnTimeoutSeconds elapsed; the harness was killed
	ErrAttachment     = "attachment"   // an attachment could not be placed in the guest
)

// grantRouter is implemented by adapters whose CLI does not read the
// standard provider base URL variables (grants.go).
type grantRouter interface {
	// routeGrants returns the grant variables in the form the CLI reads;
	// ok is false when the CLI cannot be pointed at the credential proxy,
	// and then the VM's grants are not applied to its turns.
	routeGrants(env map[string]string) (out map[string]string, ok bool)
}

var adapters = map[string]Adapter{"claude": claude{}, "codex": codex{}, "opencode": opencode{}, "pi": pi{}}

// Names lists the supported harnesses.
func Names() []string { return []string{"claude", "codex", "opencode", "pi"} }

// Capabilities reports every harness's controls, for HostCapabilities.
func Capabilities() []domain.HarnessCapabilities {
	var out []domain.HarnessCapabilities
	for _, n := range Names() {
		out = append(out, adapters[n].Capabilities())
	}
	return out
}

// imageTypes are the image media types every harness CLI takes natively.
var imageTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// fileTypes are attachments placed in the guest and named in the prompt
// text: the harness reads them with its own file tools.
var fileTypes = []string{"application/pdf", "text/plain", "text/markdown", "text/csv", "application/json"}

func attachmentTypes() []string { return slices.Concat(imageTypes, fileTypes) }

func isImage(mediaType string) bool { return slices.Contains(imageTypes, mediaType) }

// Lookup returns the adapter for name (aliases: claude-code).
func Lookup(name string) (Adapter, error) {
	switch strings.ToLower(name) {
	case "", "claude", "claude-code":
		return adapters["claude"], nil
	case "codex", "opencode", "pi":
		return adapters[strings.ToLower(name)], nil
	}
	return nil, fmt.Errorf("unknown harness %q (supported: %s)", name, strings.Join(Names(), ", "))
}

// rawJSON keeps line as raw JSON; a non-JSON line is kept as a string.
func rawJSON(line []byte) json.RawMessage {
	if json.Valid(line) {
		return json.RawMessage(append([]byte(nil), line...))
	}
	b, _ := json.Marshal(string(line))
	return b
}

func rawEvent(line []byte) Event { return Event{Type: EventRaw, Raw: rawJSON(line)} }
