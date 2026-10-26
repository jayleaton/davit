package network

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
)

// serveLines answers "tcp:LINE" on every connection l accepts (probe's
// peer, like netnsEcho) until l is closed.
func serveLines(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			line, _ := bufio.NewReader(c).ReadString('\n')
			fmt.Fprintf(c, "tcp:%s", line)
		}()
	}
}

// TestRootlessListenTCP: the listener ListenTCP returns is bound to
// 172.16.0.1:PORT inside the VM's own netns (reachable there, accepted by
// this process in the host netns), and neither the host netns nor another
// VM's netns reaches it.
func TestRootlessListenTCP(t *testing.T) {
	const port = 15080
	cfg := DefaultConfig()
	cfg.TapServices = []uint16{port}
	r := newRootlessOrSkip(t, cfg)
	ctx := context.Background()
	var hs []Handle
	for _, id := range []domain.VMID{"vm_tapsvc", "vm_tapother"} {
		h, err := r.Create(ctx, id, 1, domain.Egress{Mode: domain.EgressOff})
		if err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
		t.Cleanup(func() { _ = r.Destroy(ctx, h) })
		hs = append(hs, h)
	}
	h, other := hs[0], hs[1]

	if _, err := r.ListenTCP(ctx, h, port+1); !errors.Is(err, ErrInvalid) {
		t.Errorf("ListenTCP(port not in TapServices) = %v, want ErrInvalid", err)
	}
	if _, err := r.ListenTCP(ctx, Handle{Mode: ModeNone}, port); !errors.Is(err, ErrInvalid) {
		t.Errorf("ListenTCP(mode none) = %v, want ErrInvalid", err)
	}

	l, err := r.ListenTCP(ctx, h, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() }) // runs before Destroy: it pins the netns
	if got := l.Addr().String(); got != "172.16.0.1:15080" {
		t.Errorf("listener address = %s", got)
	}
	go serveLines(l)

	addr := netip.AddrPortFrom(DefaultTapIP, port).String()
	if got := runProbe(h, "tcp", addr); got != "tcp:ping" {
		t.Errorf("from the VM netns: %q, want tcp:ping", got)
	}
	if got := runProbe(other, "tcp", addr); !strings.HasPrefix(got, "ERR") {
		t.Errorf("from another VM's netns: %q, want no connection", got)
	}
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Error("the host netns reached the VM netns listener")
	}
	// The port is taken in that netns only.
	if l2, err := r.ListenTCP(ctx, h, port); err == nil {
		l2.Close()
		t.Error("second ListenTCP on the same port succeeded")
	}
	l3, err := r.ListenTCP(ctx, other, port)
	if err != nil {
		t.Fatalf("ListenTCP in another VM's netns: %v", err)
	}
	l3.Close()

	if err := r.Destroy(ctx, other); err != nil {
		t.Fatal(err)
	}
	if l4, err := r.ListenTCP(ctx, other, port); err == nil {
		l4.Close()
		t.Error("ListenTCP after Destroy succeeded")
	}
}

// TestGuestRulesetTapServices: the guest reaches a TapServices port on
// 172.16.0.1 in every egress mode (off included) and nothing else on the
// VM netns: not another TCP port, not UDP on the same port, not ICMP.
func TestGuestRulesetTapServices(t *testing.T) {
	const (
		svc    target = "tcp/172.16.0.1:15080"
		other  target = "tcp/172.16.0.1:15081"
		svcUDP target = "udp/172.16.0.1:15080"
	)
	n := newRuleNet(t)
	n.daemon(n.vm, "netns-echo", string(svc), string(other), string(svcUDP))
	cfg := DefaultConfig(netip.MustParsePrefix("203.0.113.9/32"))
	cfg.TapServices = []uint16{15080}

	for _, policy := range []domain.Egress{
		{Mode: domain.EgressOff},
		{Mode: domain.EgressFiltered},
		{Mode: domain.EgressAllowlist, Allow: []string{"203.0.113.0/28"}},
	} {
		what := string(policy.Mode)
		n.load(cfg, policy)
		n.expectReach(what, n.guest, map[target]bool{svc: true, other: false, svcUDP: false})
		n.expect(what, map[string]bool{
			"172.16.0.1":      false, // ICMP to the netns
			"203.0.113.1":     policy.Mode != domain.EgressOff,
			"203.0.113.9":     false, // host address
			"169.254.169.254": false,
		})
	}
	n.load(DefaultConfig(netip.MustParsePrefix("203.0.113.9/32")), domain.Egress{Mode: domain.EgressFiltered})
	n.expectReach("filtered without tap services", n.guest, map[target]bool{svc: false, other: false})
}
