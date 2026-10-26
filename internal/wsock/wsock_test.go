package wsock

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// serve runs fn on every upgraded server conn, then closes it. A bad
// handshake gets a 400 JSON body; /nope never upgrades and answers 404 JSON.
func serve(t *testing.T, fn func(*Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/nope" {
			writeJSON(w, http.StatusNotFound, "no such process")
			return
		}
		c, err := Upgrade(w, r)
		if errors.Is(err, ErrBadHandshake) {
			writeJSON(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			t.Errorf("Upgrade: %v", err)
			return
		}
		defer c.Close(CloseNormal, "")
		fn(c)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func echo(c *Conn) {
	for {
		typ, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		if err := c.WriteMessage(typ, data); err != nil {
			return
		}
	}
}

func dialRaw(t *testing.T, srv *httptest.Server) net.Conn {
	t.Helper()
	nc, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return nc
}

func dial(t *testing.T, srv *httptest.Server) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := Client(ctx, dialRaw(t, srv), "davit", "/attach?stream=1", http.Header{"X-Test": {"1"}})
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_ = c.NetConn().SetDeadline(time.Now().Add(10 * time.Second))
	t.Cleanup(func() { c.Close(CloseNormal, "") })
	return c
}

// rawFrame builds one frame, masked with a fixed key when mask is set.
func rawFrame(fin bool, op byte, payload []byte, mask bool) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	b := []byte{b0}
	var m byte
	if mask {
		m = 0x80
	}
	switch {
	case len(payload) <= 125:
		b = append(b, m|byte(len(payload)))
	case len(payload) <= 0xffff:
		b = append(b, m|126)
		b = binary.BigEndian.AppendUint16(b, uint16(len(payload)))
	default:
		b = append(b, m|127)
		b = binary.BigEndian.AppendUint64(b, uint64(len(payload)))
	}
	p := append([]byte(nil), payload...)
	if mask {
		key := [4]byte{1, 2, 3, 4}
		b = append(b, key[:]...)
		maskBytes(p, key)
	}
	return append(b, p...)
}

// readRawFrame reads one unmasked server frame.
func readRawFrame(t *testing.T, br *bufio.Reader) (op byte, payload []byte) {
	t.Helper()
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		t.Fatal(err)
	}
	if h[1]&0x80 != 0 || h[1]&0x7f > 125 {
		t.Fatalf("unexpected frame header % x", h)
	}
	payload = make([]byte, h[1]&0x7f)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatal(err)
	}
	return h[0] & 0x0f, payload
}

func wantClose(t *testing.T, err error, code int) {
	t.Helper()
	var ce *CloseError
	if !errors.As(err, &ce) || !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want CloseError", err)
	}
	if ce.Code != code {
		t.Fatalf("close code = %d, want %d", ce.Code, code)
	}
}

func TestEchoRoundTrip(t *testing.T) {
	c := dial(t, serve(t, echo))
	if err := c.WriteMessage(TextMessage, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := c.ReadMessage()
	if err != nil || typ != TextMessage || string(data) != "hello" {
		t.Fatalf("got %d %q %v", typ, data, err)
	}
	big := make([]byte, 300<<10)
	_, _ = rand.Read(big)
	if err := c.WriteMessage(BinaryMessage, big); err != nil {
		t.Fatal(err)
	}
	typ, data, err = c.ReadMessage()
	if err != nil || typ != BinaryMessage || !bytes.Equal(data, big) {
		t.Fatalf("binary echo: type %d len %d err %v", typ, len(data), err)
	}
}

func TestFragmentedMessage(t *testing.T) {
	c := dial(t, serve(t, echo))
	var raw []byte
	raw = append(raw, rawFrame(false, opText, []byte("hel"), true)...)
	raw = append(raw, rawFrame(true, opPing, []byte("p"), true)...) // interleaved control frame
	raw = append(raw, rawFrame(false, opContinuation, []byte("lo "), true)...)
	raw = append(raw, rawFrame(true, opContinuation, []byte("world"), true)...)
	if _, err := c.NetConn().Write(raw); err != nil {
		t.Fatal(err)
	}
	typ, data, err := c.ReadMessage() // skips the pong
	if err != nil || typ != TextMessage || string(data) != "hello world" {
		t.Fatalf("got %d %q %v", typ, data, err)
	}
}

func TestPingGetsPong(t *testing.T) {
	c := dial(t, serve(t, echo))
	if _, err := c.NetConn().Write(rawFrame(true, opPing, []byte("abc"), true)); err != nil {
		t.Fatal(err)
	}
	op, p := readRawFrame(t, c.br)
	if op != opPong || string(p) != "abc" {
		t.Fatalf("got op %d %q, want pong abc", op, p)
	}
}

func TestReadLimit(t *testing.T) {
	errc := make(chan error, 1)
	c := dial(t, serve(t, func(c *Conn) {
		c.SetReadLimit(1024)
		_, _, err := c.ReadMessage()
		errc <- err
	}))
	if err := c.WriteMessage(BinaryMessage, make([]byte, 2048)); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.ReadMessage()
	wantClose(t, err, CloseMessageTooBig)
	if err := <-errc; !errors.Is(err, ErrReadLimit) {
		t.Fatalf("server err = %v, want ErrReadLimit", err)
	}
}

func TestUnmaskedClientFrameRejected(t *testing.T) {
	errc := make(chan error, 1)
	c := dial(t, serve(t, func(c *Conn) {
		_, _, err := c.ReadMessage()
		errc <- err
	}))
	if _, err := c.NetConn().Write(rawFrame(true, opText, []byte("hi"), false)); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.ReadMessage()
	wantClose(t, err, CloseProtocolError)
	if err := <-errc; !errors.Is(err, ErrProtocol) {
		t.Fatalf("server err = %v, want ErrProtocol", err)
	}
}

func TestBadHandshake(t *testing.T) {
	srv := serve(t, echo)

	// Plain GET: Upgrade rejects without writing; the handler's 400 goes out.
	resp, err := http.Get(srv.URL + "/attach")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body["error"], "bad handshake") {
		t.Fatalf("got %d %v", resp.StatusCode, body)
	}

	// Client exposes a non-101 answer and its JSON body.
	c, resp, err := Client(context.Background(), dialRaw(t, srv), "davit", "/nope", nil)
	if c != nil || !errors.Is(err, ErrBadHandshake) || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("Client = %v, %v, %v", c, resp, err)
	}
	body = nil
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body["error"] != "no such process" {
		t.Fatalf("body %v, err %v", body, err)
	}
}

func TestClientContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // accept and never answer
		nc, err := ln.Accept()
		if err == nil {
			defer nc.Close()
			_, _ = io.Copy(io.Discard, nc)
		}
	}()
	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := Client(ctx, nc, "davit", "/", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestCloseHandshake(t *testing.T) {
	t.Run("client initiates", func(t *testing.T) {
		errc := make(chan error, 1)
		c := dial(t, serve(t, func(c *Conn) {
			_, _, err := c.ReadMessage()
			errc <- err
		}))
		if err := c.Close(4001, "bye"); err != nil {
			t.Fatal(err)
		}
		err := <-errc
		wantClose(t, err, 4001)
		if ce := err.(*CloseError); ce.Reason != "bye" {
			t.Fatalf("reason %q", ce.Reason)
		}
		if err := c.WriteMessage(TextMessage, nil); !errors.Is(err, ErrClosed) {
			t.Fatalf("write after close = %v", err)
		}
	})
	t.Run("server initiates", func(t *testing.T) {
		c := dial(t, serve(t, func(c *Conn) { c.Close(CloseGoingAway, "shutdown") }))
		_, _, err := c.ReadMessage()
		wantClose(t, err, CloseGoingAway)
	})
}

func TestConcurrentWrites(t *testing.T) {
	const writers, perWriter = 16, 50
	c := dial(t, serve(t, echo))
	msg := func(g, i int) string {
		return fmt.Sprintf("%02d:%03d:", g, i) + strings.Repeat(string(rune('a'+g)), 1000+i*37)
	}
	var wg sync.WaitGroup
	for g := range writers {
		wg.Go(func() {
			for i := range perWriter {
				if err := c.WriteMessage(TextMessage, []byte(msg(g, i))); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	seen := make(map[string]bool)
	for range writers * perWriter {
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var g, i int
		if _, err := fmt.Sscanf(string(data[:7]), "%02d:%03d", &g, &i); err != nil || string(data) != msg(g, i) {
			t.Fatalf("corrupt message (len %d): %.20q", len(data), data)
		}
		seen[string(data[:7])] = true
	}
	wg.Wait()
	if len(seen) != writers*perWriter {
		t.Fatalf("got %d distinct messages, want %d", len(seen), writers*perWriter)
	}
}
