package netnsdns

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestUpstreams(t *testing.T) {
	got := Upstreams([]byte("# comment\nnameserver 127.0.0.53\nnameserver 192.168.1.1\nnameserver ::1\nnameserver fd00::1\nsearch lan\nnameserver 9.9.9.9 \n"))
	want := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.1:53"), netip.MustParseAddrPort("9.9.9.9:53")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Upstreams = %v, want %v", got, want)
	}
	if got := Upstreams([]byte("nameserver 127.0.0.53\n")); !reflect.DeepEqual(got, []netip.AddrPort{Fallback}) {
		t.Errorf("loopback-only Upstreams = %v, want fallback", got)
	}
}

func TestPinBatchSanitizes(t *testing.T) {
	a := netip.MustParseAddr("203.0.113.7")
	want := "add element inet t s { 203.0.113.7 timeout 60s comment \"www.github.com\" }\n" +
		"delete element inet t s { 203.0.113.7 }\n" +
		"add element inet t s { 203.0.113.7 timeout 60s comment \"www.github.com\" }\n"
	if got := PinBatch("t", "s", a, 60, "WWW.GitHub.com."); got != want {
		t.Errorf("PinBatch =\n%s", got)
	}
	for _, bad := range []string{`x" } ; flush ruleset ; add x { "a.github.com`, "a b.github.com", "\x00.github.com", strings.Repeat("a", 121)} {
		if c := CommentName(bad); c != "" {
			t.Errorf("CommentName(%q) = %q, want empty", bad, c)
		}
		if b := PinBatch("t", "s", a, 60, bad); strings.Contains(b, "comment") || strings.Count(b, "\n") != 3 {
			t.Errorf("PinBatch with %q:\n%s", bad, b)
		}
	}
}

// upstream is a fake resolver on 127.0.0.1 answering every A query with
// addr and ttl, over UDP and TCP.
type upstream struct {
	addr    netip.AddrPort
	queries atomic.Int32
}

func newUpstream(t *testing.T, ip [4]byte, ttl uint32) *upstream {
	u := &upstream{}
	answer := func(q []byte) []byte {
		u.queries.Add(1)
		var p dnsmessage.Parser
		h, err := p.Start(q)
		if err != nil {
			return nil
		}
		qq, err := p.Question()
		if err != nil {
			return nil
		}
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true})
		_ = b.StartQuestions()
		_ = b.Question(qq)
		_ = b.StartAnswers()
		_ = b.AResource(dnsmessage.ResourceHeader{Name: qq.Name, Class: dnsmessage.ClassINET, TTL: ttl}, dnsmessage.AResource{A: ip})
		out, _ := b.Finish()
		return out
	}
	// UDP and TCP on one port: a free UDP port may be taken for TCP by any
	// other process on the host, so try again with another.
	var pc net.PacketConn
	var l net.Listener
	for try := 0; ; try++ {
		var err error
		if pc, err = net.ListenPacket("udp4", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		u.addr = netip.MustParseAddrPort(pc.LocalAddr().String())
		if l, err = net.Listen("tcp4", u.addr.String()); err == nil {
			break
		}
		pc.Close()
		if !errors.Is(err, syscall.EADDRINUSE) || try == 20 {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { pc.Close(); l.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(answer(buf[:n]), from)
		}
	}()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			q, err := readTCPMsg(c)
			if err == nil {
				_ = writeTCPMsg(c, answer(q))
			}
			c.Close()
		}
	}()
	return u
}

func query(name string) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 77, RecursionDesired: true})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	return q
}

func rcode(t *testing.T, resp []byte) dnsmessage.RCode {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil || h.ID != 77 || !h.Response {
		t.Fatalf("bad response %x: %v", resp, err)
	}
	return h.RCode
}

type pinLog struct {
	mu      sync.Mutex
	batches []string
	err     error
}

func (l *pinLog) pin(_ context.Context, batch string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.batches = append(l.batches, batch)
	return l.err
}

func newServer(t *testing.T, up *upstream, allow ...string) (*Server, *pinLog) {
	t.Helper()
	s, err := New(Config{Listen: netip.MustParseAddrPort("127.0.0.1:53"), Upstreams: []netip.AddrPort{up.addr},
		Allow: allow, Table: "davit_guest", Set: "pinned4"})
	if err != nil {
		t.Fatal(err)
	}
	l := &pinLog{}
	s.pin = l.pin
	return s, l
}

func TestHandle(t *testing.T) {
	up := newUpstream(t, [4]byte{203, 0, 113, 7}, 5)
	s, pins := newServer(t, up, "github.com", "203.0.113.0/24")
	ctx := context.Background()

	resp := s.Handle(ctx, query("api.github.com."), false)
	if rcode(t, resp) != dnsmessage.RCodeSuccess {
		t.Fatalf("allowed name: %x", resp)
	}
	if len(pins.batches) != 1 || !strings.Contains(pins.batches[0], `203.0.113.7 timeout 60s comment "api.github.com"`) {
		t.Errorf("pins = %q, want 203.0.113.7 pinned for MinPin", pins.batches)
	}
	if got := rcode(t, s.Handle(ctx, query("evil.org."), false)); got != dnsmessage.RCodeNameError {
		t.Errorf("disallowed name: %v, want NXDOMAIN", got)
	}
	if up.queries.Load() != 1 {
		t.Errorf("upstream saw %d queries, want 1 (disallowed names are not forwarded)", up.queries.Load())
	}
	if s.Handle(ctx, []byte{1, 2, 3}, false) != nil {
		t.Error("malformed query answered")
	}
	if got := rcode(t, s.Handle(ctx, query("github.com."), true)); got != dnsmessage.RCodeSuccess {
		t.Errorf("allowed name over TCP: %v", got)
	}
	pins.err = errors.New("nft failed")
	if got := rcode(t, s.Handle(ctx, query("github.com."), false)); got != dnsmessage.RCodeServerFailure {
		t.Errorf("failed pin: %v, want SERVFAIL (never an answer that is not pinned)", got)
	}
}

func TestPinsClampAndNeverShrink(t *testing.T) {
	up := newUpstream(t, [4]byte{203, 0, 113, 8}, 86400)
	s, pins := newServer(t, up, "example.com")
	ctx := context.Background()
	s.Handle(ctx, query("a.example.com."), false)
	if len(pins.batches) != 1 || !strings.Contains(pins.batches[0], "timeout 3600s") {
		t.Fatalf("pins = %q, want MaxPin", pins.batches)
	}
	short := newUpstream(t, [4]byte{203, 0, 113, 8}, 1)
	s.cfg.Upstreams = []netip.AddrPort{short.addr}
	s.Handle(ctx, query("b.example.com."), false)
	if len(pins.batches) != 2 || strings.Contains(pins.batches[1], "timeout 60s") {
		t.Errorf("a shorter TTL shrank the pin: %q", pins.batches[1])
	}
}

func TestUpstreamFailureIsServfail(t *testing.T) {
	dead, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	addr := netip.MustParseAddrPort(dead.LocalAddr().String())
	dead.Close()
	s, err := New(Config{Listen: addr, Upstreams: []netip.AddrPort{addr}, Allow: []string{"x.org"}, Table: "t", Set: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if got := rcode(t, s.Handle(context.Background(), query("x.org."), false)); got != dnsmessage.RCodeServerFailure {
		t.Errorf("unreachable upstream: %v, want SERVFAIL", got)
	}
}

func TestServe(t *testing.T) {
	up := newUpstream(t, [4]byte{203, 0, 113, 9}, 30)
	s, _ := newServer(t, up, "example.com")
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- s.Serve(ctx, pc, l) }()

	c, _ := net.Dial("udp4", pc.LocalAddr().String())
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write(query("www.example.com."))
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil || rcode(t, buf[:n]) != dnsmessage.RCodeSuccess {
		t.Errorf("UDP: %v", err)
	}
	c.Close()
	tc, _ := net.Dial("tcp4", l.Addr().String())
	_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
	for _, name := range []string{"example.com.", "nope.org."} { // two messages, one connection
		q := query(name)
		_, _ = tc.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...))
		resp, err := readTCPMsg(tc)
		if err != nil {
			t.Fatalf("TCP %s: %v", name, err)
		}
		want := dnsmessage.RCodeSuccess
		if name == "nope.org." {
			want = dnsmessage.RCodeNameError
		}
		if got := rcode(t, resp); got != want {
			t.Errorf("TCP %s: %v, want %v", name, got, want)
		}
	}
	tc.Close()
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve after cancel: %v", err)
	}
}
