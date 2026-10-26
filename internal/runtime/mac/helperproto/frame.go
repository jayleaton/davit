package helperproto

import (
	"encoding/binary"
	"encoding/json"
	"io"
)

const (
	// ProtocolVersion is the newest version this package speaks;
	// MinProtocolVersion the oldest.
	ProtocolVersion    = 1
	MinProtocolVersion = 1

	// HeaderSize is the fixed frame header length.
	HeaderSize = 8
	// MaxFrameSize bounds a frame's payload. Control messages are small;
	// guest bytes never travel in frames.
	MaxFrameSize = 64 << 10

	magic0, magic1 = 'S', 'H'
	framingVersion = 1
)

// MsgType names a message.
type MsgType string

const (
	TypeHello  MsgType = "hello"
	TypeCreate MsgType = "create"
	TypeBoot   MsgType = "boot"
	TypeState  MsgType = "state"
	TypePause  MsgType = "pause"
	TypeResume MsgType = "resume"
	TypeStop   MsgType = "stop"
	TypeKill   MsgType = "kill"
	TypeWatch  MsgType = "watch"
	TypeEvent  MsgType = "event"
	TypeDial   MsgType = "dial"
)

// Frame is one protocol message.
type Frame struct {
	// V is the protocol version: MinProtocolVersion on Hello, the
	// negotiated version afterwards.
	V uint16 `json:"v"`
	// ID correlates a request and its reply (non-zero); events carry the
	// Watch request's ID.
	ID uint64 `json:"id,omitempty"`
	// Type names the message.
	Type MsgType `json:"type"`
	// Seq numbers events of one Watch from 1.
	Seq uint64 `json:"seq,omitempty"`
	// Body is the message payload.
	Body json.RawMessage `json:"body,omitempty"`
	// Error makes the frame an error reply to ID.
	Error *Error `json:"error,omitempty"`
}

// WriteFrame encodes f and writes it with one Write call. Nothing is written
// if f is invalid or too large.
func WriteFrame(w io.Writer, f *Frame) error {
	if f == nil || f.V == 0 || f.Type == "" {
		return Errorf(CodeMalformed, "frame needs a version and a type")
	}
	payload, err := json.Marshal(f)
	if err != nil {
		return Errorf(CodeMalformed, "encode: %v", err)
	}
	if len(payload) > MaxFrameSize {
		return Errorf(CodeTooLarge, "payload of %d bytes exceeds %d", len(payload), MaxFrameSize)
	}
	buf := make([]byte, HeaderSize+len(payload))
	buf[0], buf[1], buf[2], buf[3] = magic0, magic1, framingVersion, 0
	binary.BigEndian.PutUint32(buf[4:], uint32(len(payload)))
	copy(buf[HeaderSize:], payload)
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one frame. It returns io.EOF only at a frame boundary and
// io.ErrUnexpectedEOF for a truncated frame; any other error means the
// connection is unsynchronised.
func ReadFrame(r io.Reader) (*Frame, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != magic0 || hdr[1] != magic1 {
		return nil, Errorf(CodeMalformed, "bad magic %q", hdr[:2])
	}
	if hdr[2] != framingVersion || hdr[3] != 0 {
		return nil, Errorf(CodeMalformed, "unsupported framing %d flags %#x", hdr[2], hdr[3])
	}
	n := binary.BigEndian.Uint32(hdr[4:])
	if n > MaxFrameSize {
		return nil, Errorf(CodeTooLarge, "payload of %d bytes exceeds %d", n, MaxFrameSize)
	}
	if n < 2 {
		return nil, Errorf(CodeMalformed, "payload of %d bytes", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	var f Frame
	if err := json.Unmarshal(payload, &f); err != nil {
		return nil, Errorf(CodeMalformed, "decode: %v", err)
	}
	if f.V == 0 || f.Type == "" {
		return nil, Errorf(CodeMalformed, "frame without version or type")
	}
	return &f, nil
}

// NewFrame builds a frame with body marshalled from v (nil for none).
func NewFrame(version uint16, id uint64, t MsgType, v any) (*Frame, error) {
	f := &Frame{V: version, ID: id, Type: t}
	if v != nil {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, Errorf(CodeMalformed, "encode %s: %v", t, err)
		}
		f.Body = b
	}
	return f, nil
}

// Decode unmarshals f's body into v and validates it when v has Validate.
func (f *Frame) Decode(v any) error {
	if len(f.Body) == 0 {
		return Errorf(CodeInvalidArgument, "%s: missing body", f.Type)
	}
	if err := json.Unmarshal(f.Body, v); err != nil {
		return Errorf(CodeInvalidArgument, "%s: %v", f.Type, err)
	}
	if val, ok := v.(interface{ Validate() error }); ok {
		return val.Validate()
	}
	return nil
}

// Negotiate picks the highest version in both [min, max] ranges.
func Negotiate(min, max uint16) (uint16, error) {
	lo, hi := uint16(MinProtocolVersion), uint16(ProtocolVersion)
	if min > max || max < lo || min > hi {
		return 0, Errorf(CodeVersion, "peer speaks %d..%d, this side %d..%d", min, max, lo, hi)
	}
	return minU16(max, hi), nil
}

func minU16(a, b uint16) uint16 {
	if a < b {
		return a
	}
	return b
}

// File and socket names inside a helper's directory.
const (
	RecordFile    = "helper.json"
	ExitFile      = "exit.json"
	ControlSocket = "ctl.sock"
	StreamSocket  = "str.sock"
	ConsoleLog    = "console.log"
)
