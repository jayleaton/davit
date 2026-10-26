// Package wsock is a minimal, bounded RFC 6455 WebSocket implementation for
// the API's process attach stream. It negotiates no extensions and no
// subprotocols, writes every message as a single frame and bounds every read
// by a per-connection message limit.
//
// Text messages are not UTF-8-validated; callers that care must check
// utf8.Valid themselves.
package wsock

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Message types for ReadMessage and WriteMessage.
const (
	TextMessage   = 1
	BinaryMessage = 2
)

// Close codes (RFC 6455 §7.4.1) used by this package.
const (
	CloseNormal        = 1000
	CloseGoingAway     = 1001
	CloseProtocolError = 1002
	CloseNoStatus      = 1005 // never sent; reported when a close frame has no code
	CloseAbnormal      = 1006 // never sent; reported when the peer drops without a close frame
	CloseMessageTooBig = 1009
	CloseInternalError = 1011
)

// DefaultReadLimit is the default maximum assembled message size.
const DefaultReadLimit = 1 << 20

const (
	maxControlPayload = 125
	maxErrorBody      = 64 << 10
	closeWriteTimeout = time.Second
	acceptGUID        = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	opContinuation byte = 0
	opText         byte = 1
	opBinary       byte = 2
	opClose        byte = 8
	opPing         byte = 9
	opPong         byte = 10
)

var (
	// ErrBadHandshake marks an invalid upgrade request (Upgrade, nothing has
	// been written; answer 400) or a non-101 / invalid server answer (Client).
	ErrBadHandshake = errors.New("wsock: bad handshake")
	// ErrProtocol marks a peer protocol violation; the connection was failed
	// with close code 1002.
	ErrProtocol = errors.New("wsock: protocol error")
	// ErrReadLimit means a message exceeded the read limit; the connection was
	// failed with close code 1009.
	ErrReadLimit = errors.New("wsock: message exceeds read limit")
	// ErrClosed is matched by every CloseError and returned after Close.
	ErrClosed = errors.New("wsock: connection closed")
)

// CloseError reports the peer's close frame (or CloseAbnormal when the
// transport ended without one). errors.Is(err, ErrClosed) holds for it.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("wsock: closed (%d)", e.Code)
	}
	return fmt.Sprintf("wsock: closed (%d): %s", e.Code, e.Reason)
}

// Is reports whether target is ErrClosed.
func (e *CloseError) Is(target error) bool { return target == ErrClosed }

// Conn is a WebSocket connection. One goroutine may read at a time;
// WriteMessage and Close are safe for concurrent use.
type Conn struct {
	nc        net.Conn
	br        *bufio.Reader
	server    bool
	readLimit atomic.Int64
	readErr   error // sticky; owned by the reader

	wmu       sync.Mutex
	closeSent bool // guarded by wmu

	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error
}

func newConn(nc net.Conn, br *bufio.Reader, server bool) *Conn {
	c := &Conn{nc: nc, br: br, server: server}
	c.readLimit.Store(DefaultReadLimit)
	return c
}

// Upgrade validates the client handshake, hijacks the connection and answers
// 101. On an invalid handshake it returns an error wrapping ErrBadHandshake
// and writes nothing, so the caller can still write its own response.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	return upgrade(w, r, "")
}

// UpgradeProtocol is Upgrade that selects the first of protocols the client
// offers in Sec-WebSocket-Protocol and echoes it ("" if none matched; a
// client that offered protocols then fails the handshake itself, as RFC 6455
// requires).
func UpgradeProtocol(w http.ResponseWriter, r *http.Request, protocols ...string) (*Conn, string, error) {
	chosen := ""
	for _, want := range protocols {
		if hasToken(r.Header, "Sec-WebSocket-Protocol", want) {
			chosen = want
			break
		}
	}
	c, err := upgrade(w, r, chosen)
	return c, chosen, err
}

func upgrade(w http.ResponseWriter, r *http.Request, protocol string) (*Conn, error) {
	key, err := checkRequest(r)
	if err != nil {
		return nil, err
	}
	nc, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, fmt.Errorf("wsock: hijack: %w", err)
	}
	// net/http may have left server timeouts on the hijacked conn.
	_ = nc.SetDeadline(time.Time{})
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n"
	if protocol != "" {
		resp += "Sec-WebSocket-Protocol: " + protocol + "\r\n"
	}
	resp += "\r\n"
	if _, err := io.WriteString(nc, resp); err != nil {
		nc.Close()
		return nil, fmt.Errorf("wsock: write handshake: %w", err)
	}
	// brw.Reader holds any bytes the server already buffered past the request.
	return newConn(nc, brw.Reader, true), nil
}

func checkRequest(r *http.Request) (string, error) {
	bad := func(msg string) (string, error) {
		return "", fmt.Errorf("%w: %s", ErrBadHandshake, msg)
	}
	if r.Method != http.MethodGet {
		return bad("method must be GET")
	}
	if !hasToken(r.Header, "Connection", "upgrade") {
		return bad("missing Connection: upgrade")
	}
	if !hasToken(r.Header, "Upgrade", "websocket") {
		return bad("missing Upgrade: websocket")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return bad("Sec-WebSocket-Version must be 13")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if b, err := base64.StdEncoding.DecodeString(key); key == "" || err != nil || len(b) != 16 {
		return bad("invalid Sec-WebSocket-Key")
	}
	return key, nil
}

// Client performs the client handshake over an already-dialed conn for GET
// path (which may include a query). Client owns conn: it is closed on any
// error and when ctx ends before the handshake completes. If the server
// answers anything but a valid 101, Client returns the response with up to
// 64 KiB of its body readable from resp.Body and an error wrapping
// ErrBadHandshake.
func Client(ctx context.Context, conn net.Conn, host, path string, header http.Header) (*Conn, *http.Response, error) {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	c, resp, err := clientHandshake(conn, host, path, header)
	if !stop() {
		conn.Close()
		return nil, resp, fmt.Errorf("wsock: handshake: %w", ctx.Err())
	}
	if err != nil {
		conn.Close()
		return nil, resp, err
	}
	return c, resp, nil
}

func clientHandshake(conn net.Conn, host, path string, header http.Header) (*Conn, *http.Response, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, nil, fmt.Errorf("wsock: path %q must start with /", path)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("wsock: %w", err)
	}
	for k, vs := range header {
		req.Header[k] = append([]string(nil), vs...)
	}
	var kb [16]byte
	_, _ = rand.Read(kb[:])
	key := base64.StdEncoding.EncodeToString(kb[:])
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Sec-WebSocket-Version", "13")
	if err := req.Write(conn); err != nil {
		return nil, nil, fmt.Errorf("wsock: write handshake: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, nil, fmt.Errorf("wsock: read handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil, resp, fmt.Errorf("%w: status %s", ErrBadHandshake, resp.Status)
	}
	switch {
	case !hasToken(resp.Header, "Upgrade", "websocket"):
		return nil, resp, fmt.Errorf("%w: missing Upgrade: websocket", ErrBadHandshake)
	case !hasToken(resp.Header, "Connection", "upgrade"):
		return nil, resp, fmt.Errorf("%w: missing Connection: upgrade", ErrBadHandshake)
	case resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key):
		return nil, resp, fmt.Errorf("%w: bad Sec-WebSocket-Accept", ErrBadHandshake)
	}
	return newConn(conn, br, false), resp, nil
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + acceptGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func hasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for part := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// SetReadLimit sets the maximum assembled message size; n <= 0 restores
// DefaultReadLimit (1 MiB).
func (c *Conn) SetReadLimit(n int64) {
	if n <= 0 {
		n = DefaultReadLimit
	}
	c.readLimit.Store(n)
}

// NetConn returns the underlying connection.
func (c *Conn) NetConn() net.Conn { return c.nc }

// ReadMessage returns the next data message, reassembling fragments and
// answering pings. A close frame is answered once and reported as a
// *CloseError; the caller should then Close. Protocol violations and
// oversize messages fail the connection (close 1002 / 1009) and return
// ErrProtocol / ErrReadLimit. Errors are sticky.
func (c *Conn) ReadMessage() (msgType int, data []byte, err error) {
	if c.readErr != nil {
		return 0, nil, c.readErr
	}
	msgType, data, err = c.readMessage()
	if err != nil {
		c.readErr = c.readFailed(err)
		return 0, nil, c.readErr
	}
	return msgType, data, nil
}

func (c *Conn) readFailed(err error) error {
	var ce *CloseError
	switch {
	case errors.As(err, &ce):
		return err
	case errors.Is(err, ErrProtocol):
		c.Close(CloseProtocolError, "protocol error")
		return err
	case errors.Is(err, ErrReadLimit):
		c.Close(CloseMessageTooBig, "message too big")
		return err
	case c.closed.Load():
		return ErrClosed
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return &CloseError{Code: CloseAbnormal}
	}
	return err
}

func protocolError(msg string) error { return fmt.Errorf("%w: %s", ErrProtocol, msg) }

type frameHeader struct {
	fin    bool
	op     byte
	length int64
	masked bool
	key    [4]byte
}

func (c *Conn) readHeader() (frameHeader, error) {
	var h frameHeader
	var b [8]byte
	if _, err := io.ReadFull(c.br, b[:2]); err != nil {
		return h, err
	}
	h.fin = b[0]&0x80 != 0
	if b[0]&0x70 != 0 {
		return h, protocolError("reserved bits set")
	}
	h.op = b[0] & 0x0f
	switch h.op {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
	default:
		return h, protocolError(fmt.Sprintf("unknown opcode %d", h.op))
	}
	h.masked = b[1]&0x80 != 0
	h.length = int64(b[1] & 0x7f)
	switch h.length {
	case 126:
		if _, err := io.ReadFull(c.br, b[:2]); err != nil {
			return h, unexpected(err)
		}
		h.length = int64(binary.BigEndian.Uint16(b[:2]))
	case 127:
		if _, err := io.ReadFull(c.br, b[:8]); err != nil {
			return h, unexpected(err)
		}
		n := binary.BigEndian.Uint64(b[:8])
		if n>>63 != 0 {
			return h, protocolError("payload length has MSB set")
		}
		h.length = int64(n)
	}
	if h.op >= opClose && (!h.fin || h.length > maxControlPayload) {
		return h, protocolError("fragmented or oversize control frame")
	}
	if c.server && !h.masked {
		return h, protocolError("unmasked client frame")
	}
	if !c.server && h.masked {
		return h, protocolError("masked server frame")
	}
	if h.masked {
		if _, err := io.ReadFull(c.br, h.key[:]); err != nil {
			return h, unexpected(err)
		}
	}
	return h, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (c *Conn) readPayload(dst []byte, h frameHeader) error {
	if _, err := io.ReadFull(c.br, dst); err != nil {
		return unexpected(err)
	}
	if h.masked {
		maskBytes(dst, h.key)
	}
	return nil
}

func (c *Conn) readMessage() (int, []byte, error) {
	var typ int
	var buf []byte
	for {
		h, err := c.readHeader()
		if err != nil {
			if typ != 0 {
				err = unexpected(err)
			}
			return 0, nil, err
		}
		if h.op >= opClose {
			p := make([]byte, h.length)
			if err := c.readPayload(p, h); err != nil {
				return 0, nil, err
			}
			switch h.op {
			case opPing:
				// A failed pong surfaces on the next read or write.
				_ = c.writeControl(opPong, p)
			case opClose:
				return 0, nil, c.handleClose(p)
			}
			continue
		}
		if h.op == opContinuation {
			if typ == 0 {
				return 0, nil, protocolError("continuation without a message")
			}
		} else {
			if typ != 0 {
				return 0, nil, protocolError("new message inside a fragmented message")
			}
			typ = int(h.op)
		}
		if int64(len(buf))+h.length > c.readLimit.Load() {
			return 0, nil, ErrReadLimit
		}
		n := len(buf)
		need := n + int(h.length)
		if cap(buf) < need {
			nb := make([]byte, n, need)
			copy(nb, buf)
			buf = nb
		}
		buf = buf[:need]
		if err := c.readPayload(buf[n:], h); err != nil {
			return 0, nil, err
		}
		if h.fin {
			if buf == nil {
				buf = []byte{}
			}
			return typ, buf, nil
		}
	}
}

func (c *Conn) handleClose(p []byte) error {
	ce := &CloseError{Code: CloseNoStatus}
	if len(p) == 1 {
		return protocolError("close payload of 1 byte")
	}
	if len(p) >= 2 {
		ce.Code = int(binary.BigEndian.Uint16(p))
		ce.Reason = string(p[2:])
		if !validCloseCode(ce.Code) {
			return protocolError(fmt.Sprintf("invalid close code %d", ce.Code))
		}
	}
	c.wmu.Lock()
	// Echo the peer's code; best effort, the peer may already be gone.
	_ = c.sendCloseLocked(ce.Code, "")
	c.wmu.Unlock()
	return ce
}

func validCloseCode(code int) bool {
	switch {
	case code >= 3000 && code <= 4999:
		return true
	case code >= 1000 && code <= 1003, code >= 1007 && code <= 1014:
		return true
	}
	return false
}

// WriteMessage writes data as one frame of msgType (TextMessage or
// BinaryMessage). It is safe for concurrent use; set a write deadline on
// NetConn to bound it.
func (c *Conn) WriteMessage(msgType int, data []byte) error {
	if msgType != TextMessage && msgType != BinaryMessage {
		return fmt.Errorf("wsock: invalid message type %d", msgType)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent || c.closed.Load() {
		return ErrClosed
	}
	return c.writeFrame(byte(msgType), data)
}

func (c *Conn) writeControl(op byte, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent {
		return ErrClosed
	}
	return c.writeFrame(op, p)
}

// sendCloseLocked sends at most one close frame; code 0 or CloseNoStatus
// sends an empty payload.
func (c *Conn) sendCloseLocked(code int, reason string) error {
	if c.closeSent {
		return nil
	}
	c.closeSent = true
	var p []byte
	if code != 0 && code != CloseNoStatus {
		if len(reason) > maxControlPayload-2 {
			reason = reason[:maxControlPayload-2]
			for !utf8.ValidString(reason) {
				reason = reason[:len(reason)-1]
			}
		}
		p = binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(reason)), uint16(code))
		p = append(p, reason...)
	}
	return c.writeFrame(opClose, p)
}

// writeFrame writes one final frame; the caller holds wmu.
func (c *Conn) writeFrame(op byte, p []byte) error {
	b := make([]byte, 0, 14+len(p))
	b = append(b, 0x80|op)
	var mask byte
	if !c.server {
		mask = 0x80
	}
	switch {
	case len(p) <= maxControlPayload:
		b = append(b, mask|byte(len(p)))
	case len(p) <= 0xffff:
		b = append(b, mask|126)
		b = binary.BigEndian.AppendUint16(b, uint16(len(p)))
	default:
		b = append(b, mask|127)
		b = binary.BigEndian.AppendUint64(b, uint64(len(p)))
	}
	if c.server {
		b = append(b, p...)
	} else {
		var key [4]byte
		_, _ = rand.Read(key[:])
		b = append(b, key[:]...)
		start := len(b)
		b = append(b, p...)
		maskBytes(b[start:], key)
	}
	_, err := c.nc.Write(b)
	return err
}

func maskBytes(b []byte, key [4]byte) {
	for i := range b {
		b[i] ^= key[i&3]
	}
}

// Close sends a close frame with code and reason (best effort, bounded by a
// 1s write deadline; code 0 sends no status) and closes the connection. It is
// idempotent.
func (c *Conn) Close(code int, reason string) error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		_ = c.nc.SetWriteDeadline(time.Now().Add(closeWriteTimeout))
		c.wmu.Lock()
		_ = c.sendCloseLocked(code, reason)
		c.wmu.Unlock()
		c.closeErr = c.nc.Close()
	})
	return c.closeErr
}
