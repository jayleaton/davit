package guestproto

import "fmt"

// ErrorCode is a stable, machine-readable protocol error code. Codes, not
// messages, are the contract; messages are for humans and may change.
type ErrorCode string

const (
	// CodeUnsupported answers an unknown message type or feature. Peers
	// must reply with it instead of dropping the session (forward compat).
	CodeUnsupported     ErrorCode = "unsupported"
	CodeInvalidArgument ErrorCode = "invalid_argument"
	CodeUnauthenticated ErrorCode = "unauthenticated"
	// CodePermissionDenied: the guest user may not access the path.
	CodePermissionDenied ErrorCode = "permission_denied"
	CodeVersionMismatch  ErrorCode = "version_mismatch"
	CodeNotFound         ErrorCode = "not_found"
	CodeFrameTooLarge    ErrorCode = "frame_too_large"
	CodeMalformedFrame   ErrorCode = "malformed_frame"
	CodeUnexpectedType   ErrorCode = "unexpected_type"
	CodeSequenceGap      ErrorCode = "sequence_gap"
	CodeInternal         ErrorCode = "internal"
)

// Error is the typed protocol error. It is also the wire form of a frame's
// "error" member: {"code":..., "message":...}. Messages must never contain
// secrets or host paths.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *Error) Error() string { return "guestproto: " + string(e.Code) + ": " + e.Message }

// Is matches on Code so errors.Is(err, ErrFrameTooLarge) matches any error
// with that code, including one received from the peer.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Errorf builds an *Error.
func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Sentinels for errors.Is. Returned errors carry more specific messages.
var (
	ErrFrameTooLarge   = &Error{Code: CodeFrameTooLarge, Message: "frame exceeds MaxFrameSize"}
	ErrMalformedFrame  = &Error{Code: CodeMalformedFrame, Message: "malformed frame"}
	ErrUnsupported     = &Error{Code: CodeUnsupported, Message: "unsupported"}
	ErrInvalid         = &Error{Code: CodeInvalidArgument, Message: "invalid argument"}
	ErrUnauthenticated = &Error{Code: CodeUnauthenticated, Message: "proof verification failed"}
	ErrVersionMismatch = &Error{Code: CodeVersionMismatch, Message: "no common protocol version"}
	ErrUnexpectedType  = &Error{Code: CodeUnexpectedType, Message: "unexpected message type"}
	ErrSequenceGap     = &Error{Code: CodeSequenceGap, Message: "stream sequence gap"}
)

func invalidf(format string, args ...any) *Error {
	return Errorf(CodeInvalidArgument, format, args...)
}
