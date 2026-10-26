package wsock

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// StreamChunk is the largest binary message a Stream writes.
const StreamChunk = 32 << 10

// Stream is a WebSocket carrying one TCP byte stream as a net.Conn: binary
// messages are the bytes, an empty binary message is a half-close (EOF) in
// either direction, and a close frame ends the stream. Text messages are
// ignored.
type Stream struct {
	ws *Conn

	rmu  sync.Mutex
	buf  []byte
	reof bool
	gone atomic.Bool // the peer closed the WebSocket (not just its writing side)

	writeTimeout time.Duration
	closeOnce    sync.Once
	done         chan struct{}
}

// NewStream wraps ws. writeTimeout bounds each write (0: unbounded).
func NewStream(ws *Conn, writeTimeout time.Duration) *Stream {
	return &Stream{ws: ws, writeTimeout: writeTimeout, done: make(chan struct{})}
}

// Read returns the peer's bytes, io.EOF after its half-close or close.
func (s *Stream) Read(p []byte) (int, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	for len(s.buf) == 0 {
		if s.reof {
			return 0, io.EOF
		}
		typ, data, err := s.ws.ReadMessage()
		if err != nil {
			if errors.Is(err, ErrClosed) {
				s.reof = true
				s.gone.Store(true)
				return 0, io.EOF
			}
			return 0, err
		}
		if typ != BinaryMessage {
			continue
		}
		if len(data) == 0 {
			s.reof = true
			continue
		}
		s.buf = data
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *Stream) deadline() {
	if s.writeTimeout > 0 {
		_ = s.ws.NetConn().SetWriteDeadline(time.Now().Add(s.writeTimeout))
	}
}

// Write sends p as binary messages of at most StreamChunk bytes.
func (s *Stream) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += StreamChunk {
		end := min(off+StreamChunk, len(p))
		s.deadline()
		if err := s.ws.WriteMessage(BinaryMessage, p[off:end]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// CloseWrite sends EOF to the peer; reading continues.
func (s *Stream) CloseWrite() error {
	s.deadline()
	return s.ws.WriteMessage(BinaryMessage, nil)
}

// Close ends the stream (close frame) and closes Done.
func (s *Stream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.ws.Close(CloseNormal, "")
		close(s.done)
	})
	return err
}

// PeerClosed reports whether Read ended because the peer closed the
// WebSocket rather than half-closing it.
func (s *Stream) PeerClosed() bool { return s.gone.Load() }

// Done is closed once Close was called.
func (s *Stream) Done() <-chan struct{} { return s.done }

// LocalAddr, RemoteAddr and the deadlines are the underlying connection's.
func (s *Stream) LocalAddr() net.Addr                { return s.ws.NetConn().LocalAddr() }
func (s *Stream) RemoteAddr() net.Addr               { return s.ws.NetConn().RemoteAddr() }
func (s *Stream) SetDeadline(t time.Time) error      { return s.ws.NetConn().SetDeadline(t) }
func (s *Stream) SetReadDeadline(t time.Time) error  { return s.ws.NetConn().SetReadDeadline(t) }
func (s *Stream) SetWriteDeadline(t time.Time) error { return s.ws.NetConn().SetWriteDeadline(t) }
