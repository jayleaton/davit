//go:build linux

package userspace

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

func TestCheckPeer(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fds[1])
	f := os.NewFile(uintptr(fds[0]), "pair")
	defer f.Close()
	c, err := net.FileConn(f)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	uc := c.(*net.UnixConn)
	if err := checkPeer(uc, os.Getuid()); err != nil {
		t.Errorf("same uid rejected: %v", err)
	}
	if err := checkPeer(uc, os.Getuid()+1); err == nil {
		t.Error("other uid accepted")
	}
}

// testStack is a Stack without a netstack: enough for policy, DNS and
// control logic.
func testStack(t *testing.T, mode string, allow []string) *Stack {
	t.Helper()
	st := &Stack{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ms, err := newModeState(mode, allow)
	if err != nil {
		t.Fatal(err)
	}
	st.pol.Store(ms)
	return st
}

func query(t *testing.T, name string) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// fakeExchange answers every A question with addr.
func fakeExchange(addr netip.Addr) func([]byte) ([]byte, error) {
	return func(q []byte) ([]byte, error) {
		var p dnsmessage.Parser
		h, err := p.Start(q)
		if err != nil {
			return nil, err
		}
		qs, _ := p.AllQuestions()
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true})
		_ = b.StartQuestions()
		_ = b.Question(qs[0])
		_ = b.StartAnswers()
		_ = b.AResource(dnsmessage.ResourceHeader{Name: qs[0].Name, Class: dnsmessage.ClassINET, TTL: 300}, dnsmessage.AResource{A: addr.As4()})
		return b.Finish()
	}
}

func rcode(t *testing.T, resp []byte) dnsmessage.RCode {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil {
		t.Fatalf("bad response: %v", err)
	}
	return h.RCode
}

func TestAllowlistDNSPins(t *testing.T) {
	ip := netip.MustParseAddr("192.0.2.10")
	ex := fakeExchange(ip)
	st := testStack(t, ModeAllowlist, []string{"allowed.test", "198.51.100.0/24"})
	if st.allowed(st.pol.Load(), "tcp", netip.AddrPortFrom(ip, 443)) {
		t.Fatal("unpinned address allowed")
	}
	if !st.allowed(st.pol.Load(), "tcp", netip.AddrPortFrom(netip.MustParseAddr("198.51.100.9"), 443)) {
		t.Error("allowlisted CIDR denied")
	}
	if resp := st.answer(query(t, "other.test."), ex); resp == nil || rcode(t, resp) != dnsmessage.RCodeNameError {
		t.Errorf("other.test: want NXDOMAIN, got %v", resp)
	}
	if st.allowed(st.pol.Load(), "tcp", netip.AddrPortFrom(ip, 443)) {
		t.Fatal("refused name pinned its address")
	}
	if resp := st.answer(query(t, "www.allowed.test."), ex); resp == nil || rcode(t, resp) != dnsmessage.RCodeSuccess {
		t.Fatalf("allowed name: %v", resp)
	}
	if !st.allowed(st.pol.Load(), "tcp", netip.AddrPortFrom(ip, 443)) {
		t.Fatal("answered address not pinned")
	}
	// Allowlist change keeps pins of names still allowed only.
	if err := st.apply(ModeAllowlist, []string{"allowed.test", "x.test"}); err != nil {
		t.Fatal(err)
	}
	if !st.allowed(st.pol.Load(), "tcp", netip.AddrPortFrom(ip, 443)) {
		t.Error("pin of a still-allowed name dropped")
	}
	if err := st.apply(ModeAllowlist, []string{"x.test"}); err != nil {
		t.Fatal(err)
	}
	if st.allowed(st.pol.Load(), "tcp", netip.AddrPortFrom(ip, 443)) {
		t.Error("pin of a removed name kept")
	}
	if err := st.apply(ModeOff, nil); err != nil {
		t.Fatal(err)
	}
	if resp := st.answer(query(t, "allowed.test."), ex); resp != nil {
		t.Error("off mode answered DNS")
	}
	if err := st.apply(ModeFiltered, nil); err != nil {
		t.Fatal(err)
	}
	if resp := st.answer(query(t, "anything.test."), ex); resp == nil || rcode(t, resp) != dnsmessage.RCodeSuccess {
		t.Error("filtered mode did not forward DNS")
	}
	if !st.allowed(st.pol.Load(), "tcp", netip.AddrPortFrom(ip, 443)) {
		t.Error("filtered mode denied an internet address")
	}
}

// TestControlSetPolicy drives policy changes through the control socket and
// checks that flows the new policy denies are closed.
func TestControlSetPolicy(t *testing.T) {
	st := testStack(t, "", nil)
	name := fmt.Sprintf("davit-test/%d/%d", os.Getpid(), time.Now().UnixNano())
	if err := st.listenControl(name); err != nil {
		t.Fatal(err)
	}
	defer st.ctl.Close()
	var closedA, closedB atomic.Int32
	a := &flow{proto: "tcp", dst: netip.AddrPortFrom(netip.MustParseAddr("192.0.2.10"), 443), close: func() { closedA.Add(1) }}
	b := &flow{proto: "tcp", dst: netip.AddrPortFrom(netip.MustParseAddr("198.51.100.9"), 443), close: func() { closedB.Add(1) }}
	if !st.track(a) || !st.track(b) {
		t.Fatal("filtered mode denied a flow")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := SetPolicy(ctx, name, ModeAllowlist, []string{"198.51.100.0/24"}); err != nil {
		t.Fatal(err)
	}
	if closedA.Load() != 1 || closedB.Load() != 0 {
		t.Errorf("after allowlist: closed a=%d b=%d, want 1 0", closedA.Load(), closedB.Load())
	}
	if err := SetPolicy(ctx, name, "bogus", nil); err == nil {
		t.Error("invalid mode accepted")
	}
	if err := SetPolicy(ctx, name, ModeAllowlist, []string{"bad entry"}); err == nil {
		t.Error("invalid allow entry accepted")
	}
	if err := SetPolicy(ctx, name, ModeOff, nil); err != nil {
		t.Fatal(err)
	}
	if closedB.Load() != 1 {
		t.Error("off mode left a flow open")
	}
	if st.track(&flow{proto: "tcp", dst: b.dst, close: func() {}}) {
		t.Error("off mode admitted a new flow")
	}
	if err := SetPolicy(ctx, "davit-test/none", ModeOff, nil); err == nil {
		t.Error("SetPolicy to a missing socket succeeded")
	}
}
