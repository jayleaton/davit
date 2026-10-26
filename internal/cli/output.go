package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"davit/internal/domain"
)

// SchemaVersion is the JSONL envelope version.
const SchemaVersion = 1

// Envelope is one --json stdout line (SPEC §3).
type Envelope struct {
	SchemaVersion int                `json:"schemaVersion"`
	Event         string             `json:"event"`
	Time          time.Time          `json:"time"`
	RequestID     string             `json:"requestId"`
	OperationID   domain.OperationID `json:"operationId,omitempty"`
	Seq           int64              `json:"seq,omitempty"`
	Data          any                `json:"data,omitempty"`
	Error         *ErrorBody         `json:"error,omitempty"`
}

// ErrorBody is the envelope error. Origin tells a client-side failure
// (usage, transport, wait timeout) from a server or operation error.
type ErrorBody struct {
	Code        domain.Code        `json:"code"`
	Message     string             `json:"message"`
	Retryable   bool               `json:"retryable"`
	Details     map[string]any     `json:"details,omitempty"`
	Origin      string             `json:"origin"` // "client" | "server" | "operation"
	RequestID   string             `json:"requestId,omitempty"`
	OperationID domain.OperationID `json:"operationId,omitempty"`
}

// cliError is a failure carrying its exit code.
type cliError struct{ ErrorBody }

func (e *cliError) Error() string { return e.Message }

func clientErr(code domain.Code, format string, args ...any) *cliError {
	return &cliError{ErrorBody{Code: code, Message: fmt.Sprintf(format, args...), Origin: "client"}}
}

func usageErr(format string, args ...any) *cliError {
	return clientErr(domain.CodeInvalidArgument, format, args...)
}

// Exit codes (SPEC §3).
const (
	ExitOK         = 0
	ExitGeneral    = 1
	ExitUsage      = 2
	ExitAuthn      = 3
	ExitAuthz      = 4
	ExitNotFound   = 5
	ExitConflict   = 6
	ExitTransport  = 7
	ExitExhaustion = 8
)

// ExitCode maps an error code to the CLI exit status.
func ExitCode(c domain.Code) int {
	switch c {
	case domain.CodeInvalidArgument:
		return ExitUsage
	case domain.CodeUnauthenticated:
		return ExitAuthn
	case domain.CodePermissionDenied:
		return ExitAuthz
	case domain.CodeNotFound:
		return ExitNotFound
	case domain.CodeOperationInProgress, domain.CodeIdempotencyConflict, domain.CodeInvalidState,
		domain.CodeStopRequired, domain.CodeRevisionMismatch, domain.CodeUnsupportedCapability, domain.CodeNotImplemented:
		return ExitConflict
	case domain.CodeTimeout, domain.CodeUnavailable:
		return ExitTransport
	case domain.CodeResourceExhausted, domain.CodePayloadTooLarge:
		return ExitExhaustion
	}
	return ExitGeneral
}

// printer writes JSONL envelopes or human text. In JSON mode stdout holds
// only envelopes; human diagnostics always go to stderr.
type printer struct {
	mu        sync.Mutex
	json      bool
	stdout    io.Writer
	stderr    io.Writer
	requestID string
}

func (p *printer) emit(event string, op domain.OperationID, seq int64, data any, e *ErrorBody) {
	if !p.json {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	b, err := json.Marshal(Envelope{SchemaVersion: SchemaVersion, Event: event, Time: time.Now().UTC(),
		RequestID: p.requestID, OperationID: op, Seq: seq, Data: data, Error: e})
	if err != nil {
		b, _ = json.Marshal(Envelope{SchemaVersion: SchemaVersion, Event: "error", Time: time.Now().UTC(), RequestID: p.requestID,
			Error: &ErrorBody{Code: domain.CodeInternal, Message: "cannot encode output", Origin: "client"}})
	}
	p.stdout.Write(append(b, '\n'))
}

func (p *printer) result(op domain.OperationID, data any) { p.emit("result", op, 0, data, nil) }

// human prints a line for people (stdout) in non-JSON mode.
func (p *printer) human(format string, args ...any) {
	if !p.json {
		fmt.Fprintf(p.stdout, format+"\n", args...)
	}
}

// diag prints a diagnostic to stderr in human mode only.
func (p *printer) diag(format string, args ...any) {
	if !p.json {
		fmt.Fprintf(p.stderr, format+"\n", args...)
	}
}

// fail reports err and returns the exit code.
func (p *printer) fail(err error) int {
	ce, ok := err.(*cliError)
	if !ok {
		ce = &cliError{ErrorBody{Code: domain.CodeInternal, Message: err.Error(), Origin: "client"}}
	}
	if ce.RequestID == "" {
		ce.RequestID = p.requestID
	}
	if p.json {
		p.emit("error", ce.OperationID, 0, nil, &ce.ErrorBody)
	} else {
		fmt.Fprintf(p.stderr, "error: %s (%s)\n", ce.Message, ce.Code)
	}
	return ExitCode(ce.Code)
}
