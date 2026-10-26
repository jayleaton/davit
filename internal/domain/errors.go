package domain

import (
	"errors"
	"fmt"
	"syscall"
)

// Code is a stable, machine-readable error code. Codes, not messages, are the
// contract (SPEC §4); transports map them to HTTP status and CLI exit codes.
type Code string

const (
	CodeInvalidArgument       Code = "invalid_argument"
	CodeUnauthenticated       Code = "unauthenticated"
	CodePermissionDenied      Code = "permission_denied"
	CodeNotFound              Code = "not_found"
	CodeOperationInProgress   Code = "operation_in_progress"
	CodeIdempotencyConflict   Code = "idempotency_conflict"
	CodeInvalidState          Code = "invalid_state"
	CodeStopRequired          Code = "stop_required"
	CodeRevisionMismatch      Code = "revision_mismatch"
	CodePayloadTooLarge       Code = "payload_too_large"
	CodeUnsupportedCapability Code = "unsupported_capability"
	CodeResourceExhausted     Code = "resource_exhausted"
	CodeUnavailable           Code = "unavailable"
	CodeNotImplemented        Code = "not_implemented"
	CodeTimeout               Code = "timeout"
	CodeInternal              Code = "internal"
)

// Error is the typed error carried across every boundary. Message is for
// humans and may change; Code is stable. Details must never contain secrets or
// host paths.
type Error struct {
	Code        Code           `json:"code"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	Details     map[string]any `json:"details,omitempty"`
	OperationID OperationID    `json:"operationId,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// Is matches on Code so errors.Is(err, &Error{Code: CodeNotFound}) works.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Errorf builds a non-retryable *Error.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Retryable: code == CodeUnavailable}
}

// WithDetail returns e with one detail key set.
func (e *Error) WithDetail(k string, v any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[k] = v
	return e
}

// ErrDiskFull marks a host write that failed for lack of space; it maps to
// resource_exhausted. Errors wrapping ENOSPC or EDQUOT map the same way.
var ErrDiskFull = errors.New("host disk is full")

// IsDiskFull reports whether err is a host out-of-space failure.
func IsDiskFull(err error) bool {
	return errors.Is(err, ErrDiskFull) || errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// AsError converts any error to *Error. Host out-of-space failures become a
// retryable resource_exhausted; other unknown errors become internal with a
// redacted message.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var de *Error
	if errors.As(err, &de) {
		return de
	}
	if IsDiskFull(err) {
		return &Error{Code: CodeResourceExhausted, Message: "host disk is full; free space and retry", Retryable: true,
			Details: map[string]any{"resource": "disk"}}
	}
	return &Error{Code: CodeInternal, Message: "internal error"}
}

// CodeOf returns the code of err, or CodeInternal.
func CodeOf(err error) Code { return AsError(err).Code }

// IsCode reports whether err carries code.
func IsCode(err error, code Code) bool {
	var de *Error
	return errors.As(err, &de) && de.Code == code
}
