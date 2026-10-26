// Package rfbtest is a fake RFB (VNC) server standing in for a guest's
// x11vnc, and a minimal RFB client standing in for a viewer, for hermetic
// desktop tests. Import it from tests only.
package rfbtest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// Server is a fake VNC server: security None (or, with Auth, VNC
// authentication only), a Width x Height 32bpp framebuffer, and a Raw
// update of the whole screen for every FramebufferUpdateRequest. It counts
// the input messages it receives.
type Server struct {
	Name          string
	Width, Height uint16
	// Minor is the protocol version it announces (3, 7 or 8; default 8).
	Minor int
	// Auth makes it offer only VNC authentication (type 2).
	Auth bool

	Keys, Pointers, CutTexts, Updates atomic.Int64

	ln   net.Listener
	mu   sync.Mutex
	conn map[net.Conn]struct{}
}

// Listen serves on 127.0.0.1 (a fake guest's loopback) and returns its port.
func (s *Server) Listen() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	s.ln = ln
	s.conn = map[net.Conn]struct{}{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conn[c] = struct{}{}
			s.mu.Unlock()
			go func() {
				s.serve(c)
				c.Close()
				s.mu.Lock()
				delete(s.conn, c)
				s.mu.Unlock()
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// Close stops the server and its connections.
func (s *Server) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conn {
		c.Close()
	}
}

// Conns is the number of open connections.
func (s *Server) Conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conn)
}

func (s *Server) serve(c net.Conn) {
	minor := s.Minor
	if minor == 0 {
		minor = 8
	}
	r := bufio.NewReader(c)
	fmt.Fprintf(c, "RFB 003.%03d\n", minor)
	v := make([]byte, 12)
	if _, err := io.ReadFull(r, v); err != nil {
		return
	}
	sec := byte(1)
	if s.Auth {
		sec = 2
	}
	if minor == 3 {
		binary.Write(c, binary.BigEndian, uint32(sec))
	} else {
		c.Write([]byte{1, sec})
		if _, err := r.ReadByte(); err != nil {
			return
		}
	}
	if s.Auth {
		c.Write(make([]byte, 16)) // challenge; the gateway should never get here
		io.ReadFull(r, make([]byte, 16))
		binary.Write(c, binary.BigEndian, uint32(1))
		return
	}
	if minor == 8 {
		binary.Write(c, binary.BigEndian, uint32(0))
	}
	if _, err := r.ReadByte(); err != nil { // ClientInit
		return
	}
	init := make([]byte, 24)
	binary.BigEndian.PutUint16(init[0:], s.Width)
	binary.BigEndian.PutUint16(init[2:], s.Height)
	copy(init[4:], []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0})
	binary.BigEndian.PutUint32(init[20:], uint32(len(s.Name)))
	c.Write(append(init, s.Name...))
	for {
		t, err := r.ReadByte()
		if err != nil {
			return
		}
		switch t {
		case 0:
			io.ReadFull(r, make([]byte, 19))
		case 2:
			h := make([]byte, 3)
			io.ReadFull(r, h)
			io.ReadFull(r, make([]byte, 4*int(binary.BigEndian.Uint16(h[1:]))))
		case 3:
			io.ReadFull(r, make([]byte, 9))
			s.Updates.Add(1)
			if err := s.frame(c); err != nil {
				return
			}
		case 4:
			io.ReadFull(r, make([]byte, 7))
			s.Keys.Add(1)
		case 5:
			io.ReadFull(r, make([]byte, 5))
			s.Pointers.Add(1)
		case 6:
			h := make([]byte, 7)
			io.ReadFull(r, h)
			io.ReadFull(r, make([]byte, binary.BigEndian.Uint32(h[3:])))
			s.CutTexts.Add(1)
		default:
			return
		}
	}
}

// frame sends one Raw FramebufferUpdate of the whole screen.
func (s *Server) frame(w io.Writer) error {
	n := int(s.Width) * int(s.Height) * 4
	msg := make([]byte, 4+12+n)
	msg[0] = 0
	binary.BigEndian.PutUint16(msg[2:], 1)
	binary.BigEndian.PutUint16(msg[8:], s.Width)
	binary.BigEndian.PutUint16(msg[10:], s.Height)
	// encoding 0 (Raw) at msg[12:16]; pixels: a solid blue-ish frame.
	for i := 16; i < len(msg); i += 4 {
		msg[i], msg[i+1], msg[i+2] = 0xff, 0x40, 0x10
	}
	_, err := w.Write(msg)
	return err
}

// Client is a minimal RFB 3.8 viewer over any byte stream.
type Client struct {
	conn          io.ReadWriter
	r             *bufio.Reader
	Name          string
	Width, Height uint16
}

// Handshake runs the viewer side against a server offering security None.
func Handshake(c io.ReadWriter) (*Client, error) {
	r := bufio.NewReader(c)
	v := make([]byte, 12)
	if _, err := io.ReadFull(r, v); err != nil {
		return nil, err
	}
	if string(v) != "RFB 003.008\n" {
		return nil, fmt.Errorf("rfbtest: server version %q", v)
	}
	if _, err := io.WriteString(c, "RFB 003.008\n"); err != nil {
		return nil, err
	}
	n, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	types := make([]byte, n)
	if _, err := io.ReadFull(r, types); err != nil {
		return nil, err
	}
	if n != 1 || types[0] != 1 {
		return nil, fmt.Errorf("rfbtest: security types %v, want only None", types)
	}
	c.Write([]byte{1})
	var res uint32
	if err := binary.Read(r, binary.BigEndian, &res); err != nil || res != 0 {
		return nil, fmt.Errorf("rfbtest: security result %d %v", res, err)
	}
	c.Write([]byte{1}) // shared
	init := make([]byte, 24)
	if _, err := io.ReadFull(r, init); err != nil {
		return nil, err
	}
	name := make([]byte, binary.BigEndian.Uint32(init[20:]))
	if _, err := io.ReadFull(r, name); err != nil {
		return nil, err
	}
	return &Client{conn: c, r: r, Name: string(name), Width: binary.BigEndian.Uint16(init[0:]), Height: binary.BigEndian.Uint16(init[2:])}, nil
}

// RequestUpdate asks for a full-screen update.
func (c *Client) RequestUpdate() error {
	m := make([]byte, 10)
	m[0] = 3
	binary.BigEndian.PutUint16(m[6:], c.Width)
	binary.BigEndian.PutUint16(m[8:], c.Height)
	_, err := c.conn.Write(m)
	return err
}

// ReadFrame reads one FramebufferUpdate with Raw rects and returns its
// first pixel's bytes.
func (c *Client) ReadFrame() ([]byte, error) {
	h := make([]byte, 4)
	if _, err := io.ReadFull(c.r, h); err != nil {
		return nil, err
	}
	if h[0] != 0 {
		return nil, fmt.Errorf("rfbtest: message type %d, want FramebufferUpdate", h[0])
	}
	var first []byte
	for range binary.BigEndian.Uint16(h[2:]) {
		rh := make([]byte, 12)
		if _, err := io.ReadFull(c.r, rh); err != nil {
			return nil, err
		}
		if enc := int32(binary.BigEndian.Uint32(rh[8:])); enc != 0 {
			return nil, fmt.Errorf("rfbtest: encoding %d, want Raw", enc)
		}
		px := make([]byte, int(binary.BigEndian.Uint16(rh[4:]))*int(binary.BigEndian.Uint16(rh[6:]))*4)
		if _, err := io.ReadFull(c.r, px); err != nil {
			return nil, err
		}
		if first == nil && len(px) >= 4 {
			first = px[:4]
		}
	}
	if first == nil {
		return nil, errors.New("rfbtest: empty update")
	}
	return first, nil
}

// Key sends a key press and release.
func (c *Client) Key(keysym uint32) error {
	for _, down := range []byte{1, 0} {
		m := make([]byte, 8)
		m[0], m[1] = 4, down
		binary.BigEndian.PutUint32(m[4:], keysym)
		if _, err := c.conn.Write(m); err != nil {
			return err
		}
	}
	return nil
}

// Pointer sends a pointer event.
func (c *Client) Pointer(x, y uint16, buttons byte) error {
	m := []byte{5, buttons, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(m[2:], x)
	binary.BigEndian.PutUint16(m[4:], y)
	_, err := c.conn.Write(m)
	return err
}

// CutText sends clipboard text.
func (c *Client) CutText(s string) error {
	m := make([]byte, 8, 8+len(s))
	m[0] = 6
	binary.BigEndian.PutUint32(m[4:], uint32(len(s)))
	_, err := c.conn.Write(append(m, s...))
	return err
}
