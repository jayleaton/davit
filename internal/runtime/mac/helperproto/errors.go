package helperproto

import "fmt"

// Code is a typed error code.
type Code string

const (
	CodeMalformed       Code = "malformed_frame"  // framing or JSON broken: close the connection
	CodeTooLarge        Code = "frame_too_large"  // over MaxFrameSize: close the connection
	CodeVersion         Code = "version_mismatch" // no common protocol version
	CodeUnsupported     Code = "unsupported"      // unknown message type or unavailable operation
	CodeInvalidArgument Code = "invalid_argument" // a field fails validation
	CodeInvalidState    Code = "invalid_state"    // not valid in the VM's current state
	CodeConflict        Code = "conflict"         // Create with a different configuration
	CodeTimeout         Code = "timeout"          // a deadline passed; the VM state is unchanged
	CodeRefused         Code = "refused"          // the guest refused or did not accept a vsock dial
	CodeUnauthorized    Code = "unauthorized"     // peer credentials do not match the helper's user
	CodeVZ              Code = "vz_error"         // Virtualization.framework reported an error
	CodeInternal        Code = "internal"
)

// Error is a typed protocol error, carried in a frame's error member.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// Retryable says the same request may succeed later unchanged.
	Retryable bool `json:"retryable,omitempty"`
}

func (e *Error) Error() string { return "helper: " + string(e.Code) + ": " + e.Message }

// Errorf returns an *Error.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsCode reports whether err is an *Error with code.
func IsCode(err error, code Code) bool {
	e, ok := err.(*Error)
	return ok && e.Code == code
}
