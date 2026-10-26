package guestproto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

const (
	// ProtocolVersion is the newest protocol version this package speaks.
	ProtocolVersion uint16 = 1
	// MinProtocolVersion is the oldest protocol version this package speaks.
	MinProtocolVersion uint16 = 1

	// HeaderSize is the fixed frame header length in bytes.
	HeaderSize = 8
	// MaxFrameSize bounds a frame's payload (the JSON envelope). Larger
	// frames are rejected on encode and decode; the decoder never allocates
	// a claimed length above it.
	MaxFrameSize = 1 << 20
	// MinFrameSize is the smallest acceptable payload ("{}").
	MinFrameSize = 2
	// MaxChunkSize bounds raw bytes in one stream chunk so that its base64
	// form plus the envelope stays well under MaxFrameSize.
	MaxChunkSize = 512 << 10

	magic0         = 'S'
	magic1         = 'W'
	framingVersion = 1
)

// Frame is one protocol message: the JSON envelope carried in a frame's
// payload. Body holds the message-specific JSON; use Encode/Decode rather
// than filling it by hand.
type Frame struct {
	// V is the sender's protocol version (never 0). Hello/HelloAck always
	// carry MinProtocolVersion so any peer can parse them; afterwards V is the
	// negotiated version and receivers reject frames that differ.
	V uint16 `json:"v"`
	// ID correlates a request with its response. 0 means "no response
	// expected" (stream frames, notifications).
	ID uint64 `json:"id,omitempty"`
	// Type names the message; see the MsgType constants.
	Type MsgType `json:"type"`
	// Stream is the stream a frame belongs to. A stream opened by a request
	// uses that request's ID as its stream ID.
	Stream uint64 `json:"stream,omitempty"`
	// Seq is monotonic per stream starting at 1 (0 = not a stream frame).
	Seq uint64 `json:"seq,omitempty"`
	// Body is the message payload as raw JSON.
	Body json.RawMessage `json:"body,omitempty"`
	// Error, when set, makes this frame an error response to ID.
	Error *Error `json:"error,omitempty"`
}

// WriteFrame encodes f and writes header+payload with a single Write call.
// Nothing is written if f is invalid or oversize.
func WriteFrame(w io.Writer, f *Frame) error {
	buf, err := marshalFrame(f)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

func marshalFrame(f *Frame) ([]byte, error) {
	if f == nil || f.V == 0 || f.Type == "" {
		return nil, Errorf(CodeMalformedFrame, "frame needs non-zero v and a type")
	}
	payload, err := json.Marshal(f)
	if err != nil {
		return nil, Errorf(CodeMalformedFrame, "encode envelope: %v", err)
	}
	if len(payload) > MaxFrameSize {
		return nil, Errorf(CodeFrameTooLarge, "payload %d bytes exceeds %d", len(payload), MaxFrameSize)
	}
	buf := make([]byte, HeaderSize+len(payload))
	buf[0], buf[1], buf[2], buf[3] = magic0, magic1, framingVersion, 0
	binary.BigEndian.PutUint32(buf[4:HeaderSize], uint32(len(payload)))
	copy(buf[HeaderSize:], payload)
	return buf, nil
}

// ReadFrame reads exactly one frame. It returns io.EOF only when r ends
// cleanly at a frame boundary and io.ErrUnexpectedEOF when a frame is
// truncated. Any other error leaves the stream unsynchronised: close it.
func ReadFrame(r io.Reader) (*Frame, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err // io.EOF at boundary, io.ErrUnexpectedEOF mid-header
	}
	if hdr[0] != magic0 || hdr[1] != magic1 {
		return nil, Errorf(CodeMalformedFrame, "bad magic %#x%02x", hdr[0], hdr[1])
	}
	if hdr[2] != framingVersion || hdr[3] != 0 {
		return nil, Errorf(CodeMalformedFrame, "unsupported framing version %d flags %#x", hdr[2], hdr[3])
	}
	n := binary.BigEndian.Uint32(hdr[4:])
	if n > MaxFrameSize {
		return nil, Errorf(CodeFrameTooLarge, "claimed payload %d bytes exceeds %d", n, MaxFrameSize)
	}
	if n < MinFrameSize {
		return nil, Errorf(CodeMalformedFrame, "payload %d bytes is too short", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	f := new(Frame)
	if err := json.Unmarshal(payload, f); err != nil {
		return nil, Errorf(CodeMalformedFrame, "decode envelope: %v", err)
	}
	if f.V == 0 || f.Type == "" {
		return nil, Errorf(CodeMalformedFrame, "envelope needs non-zero v and a type")
	}
	return f, nil
}

// Conn frames messages over a byte stream (a vsock connection in
// production). It is safe for one concurrent reader plus any number of
// concurrent writers; each frame is written atomically.
type Conn struct {
	rw      io.ReadWriter
	rmu     sync.Mutex
	wmu     sync.Mutex
	version atomic.Uint32 // negotiated version; 0 until SetVersion
}

// NewConn wraps rw. Transport dialing and closing belong to the caller.
func NewConn(rw io.ReadWriter) *Conn { return &Conn{rw: rw} }

// WriteFrame writes one frame atomically with respect to other writers.
func (c *Conn) WriteFrame(f *Frame) error {
	buf, err := marshalFrame(f)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.rw.Write(buf)
	return err
}

// ReadFrame reads the next frame; see the package-level ReadFrame.
func (c *Conn) ReadFrame() (*Frame, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	return ReadFrame(c.rw)
}

// SetVersion records the version chosen by the handshake. Send stamps it on
// every non-handshake frame afterwards.
func (c *Conn) SetVersion(v uint16) { c.version.Store(uint32(v)) }

// Version returns the negotiated version, or 0 before the handshake.
func (c *Conn) Version() uint16 { return uint16(c.version.Load()) }

// Send encodes body as a request/response frame with the given ID and
// writes it.
func (c *Conn) Send(id uint64, body Message) error {
	f, err := Encode(id, body)
	if err != nil {
		return err
	}
	if v := c.Version(); v != 0 && !isHandshake(f.Type) {
		f.V = v
	}
	return c.WriteFrame(f)
}

// SeqTracker enforces per-stream sequence order on a receiver: duplicates
// (replay after reconnect) are reported so they can be dropped, gaps are
// errors. The zero value is ready; it is not safe for concurrent use.
type SeqTracker struct {
	last map[uint64]uint64
}

// Resume declares that the next accepted seq on stream is after+1, e.g. when
// attaching to a process stream from a cursor.
func (t *SeqTracker) Resume(stream, after uint64) {
	if t.last == nil {
		t.last = map[uint64]uint64{}
	}
	t.last[stream] = after
}

// Accept checks f's (stream, seq). dup is true for an already-seen seq.
func (t *SeqTracker) Accept(f *Frame) (dup bool, err error) {
	if f.Stream == 0 || f.Seq == 0 {
		return false, Errorf(CodeMalformedFrame, "frame %q is not a stream frame", f.Type)
	}
	if t.last == nil {
		t.last = map[uint64]uint64{}
	}
	last := t.last[f.Stream]
	switch {
	case f.Seq <= last:
		return true, nil
	case f.Seq != last+1:
		return false, Errorf(CodeSequenceGap, "stream %d: got seq %d after %d", f.Stream, f.Seq, last)
	}
	t.last[f.Stream] = f.Seq
	return false, nil
}
