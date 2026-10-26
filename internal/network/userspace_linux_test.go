package network

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"davit/internal/domain"
	"davit/internal/network/nethelper"
	"davit/internal/network/userspace"
)

// TestMain lets the test binary act as the davit binary for the helper
// subcommands (selfExe is the test binary), plus a probe client.
func TestMain(m *testing.M) {
	nethelper.Serve()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "probe":
			switch os.Args[2] {
			case "dns":
				os.Exit(probeDNS(os.Args[3]))
			case "hold":
				os.Exit(probeHold(os.Args[3]))
			}
			os.Exit(probe(os.Args[2], os.Args[3]))
		}
	}
	os.Exit(m.Run())
}

// probe sends "ping\n" over network ("tcp"/"udp") to addr and prints the
// reply line, or the error.
func probe(network, addr string) int {
	c, err := net.DialTimeout(network, addr, 3*time.Second)
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("ping\n")); err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	fmt.Print(line)
	return 0
}

// probeDNS asks 1.1.1.1:53 (inside the netns every port-53 flow reaches
// the stack) for name's A record and prints the address, "NXDOMAIN", or
// the error.
func probeDNS(name string) int {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x5157, RecursionDesired: true})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	c, err := net.Dial("udp", "1.1.1.1:53")
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n := 0
	if _, err = c.Write(q); err == nil {
		n, err = c.Read(buf)
	}
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	var p dnsmessage.Parser
	h, err := p.Start(buf[:n])
	if err == nil {
		err = p.SkipAllQuestions()
	}
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	if h.RCode == dnsmessage.RCodeNameError {
		fmt.Println("NXDOMAIN")
		return 0
	}
	var a dnsmessage.AResource
	rh, err := p.AnswerHeader()
	if err == nil && rh.Type != dnsmessage.TypeA {
		err = fmt.Errorf("answer type %v", rh.Type)
	}
	if err == nil {
		a, err = p.AResource()
	}
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	fmt.Println(netip.AddrFrom4(a.A))
	return 0
}

// probeHold sends "ping\n" over TCP to addr, prints the reply line, then
// waits up to 10s for the connection to end and prints "CLOSED" or "OPEN".
func probeHold(addr string) int {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping\n")); err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	r := bufio.NewReader(c)
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	line, err := r.ReadString('\n')
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	fmt.Print(line)
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = io.Copy(io.Discard, r)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		fmt.Println("OPEN")
		return 1
	}
	fmt.Println("CLOSED")
	return 0
}

// hostIPv4 is a non-loopback IPv4 address of this host.
func hostIPv4(t *testing.T) netip.Addr {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() && !ip.IsLoopback() {
				return ip.Unmap()
			}
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return netip.Addr{}
}

// echoTCP serves "tcp:LINE" replies to one line on ip; hold keeps each
// connection open until the client ends it.
func echoTCP(t *testing.T, ip netip.Addr, hold bool) string {
	l, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				line, _ := r.ReadString('\n')
				fmt.Fprintf(c, "tcp:%s", line)
				if hold {
					_, _ = io.Copy(io.Discard, r)
				}
			}()
		}
	}()
	return l.Addr().String()
}

// echoUDP answers each datagram on ip with "udp:" + datagram.
func echoUDP(t *testing.T, ip netip.Addr) string {
	u, err := net.ListenPacket("udp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := u.ReadFrom(buf)
			if err != nil {
				return
			}
			u.WriteTo(append([]byte("udp:"), buf[:n]...), from)
		}
	}()
	return u.LocalAddr().String()
}

// fakeDNS points the stacks created by the test at a host DNS server that
// answers every A query with ip (TTL 300).
func fakeDNS(t *testing.T, ip netip.Addr) {
	u, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := u.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true})
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			if q.Type == dnsmessage.TypeA {
				_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 300}, dnsmessage.AResource{A: ip.As4()})
			}
			if resp, err := b.Finish(); err == nil {
				u.WriteTo(resp, from)
			}
		}
	}()
	userspaceResolvers = []string{u.LocalAddr().String()}
	t.Cleanup(func() { userspaceResolvers = nil })
}

// runProbe runs the probe client inside h's namespaces.
func runProbe(h Handle, args ...string) string {
	exe, _ := os.Executable()
	argv := append(append(EnterArgv(h), exe, "probe"), args...)
	out, _ := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// createEgress creates a rootless network with the userspace stack under
// policy; the host's own IPv4 address stands in for the internet.
func createEgress(t *testing.T, ip netip.Addr, policy domain.Egress) (*Rootless, Handle) {
	t.Helper()
	policy, err := policy.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Egress.Allow = []netip.Prefix{netip.PrefixFrom(ip, 32)}
	r := newRootlessOrSkip(t, cfg)
	ctx := context.Background()
	h, err := r.Create(ctx, "vm_egress", 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Destroy(ctx, h) })
	if !h.Egress.Enabled || h.Egress.Backend != BackendUserspace {
		t.Fatalf("egress = %+v", h.Egress)
	}
	return r, h
}

// TestUserspaceEgressOff: off mode lets nothing out, DNS included, even to
// an otherwise allowed host.
func TestUserspaceEgressOff(t *testing.T) {
	ip := hostIPv4(t)
	fakeDNS(t, ip)
	addr := echoTCP(t, ip, false)
	_, h := createEgress(t, ip, domain.Egress{Mode: domain.EgressOff})
	if got := runProbe(h, "tcp", addr); !strings.HasPrefix(got, "ERR") {
		t.Errorf("TCP in off mode: %q, want blocked", got)
	}
	if got := runProbe(h, "dns", "allowed.test."); !strings.HasPrefix(got, "ERR") {
		t.Errorf("DNS in off mode: %q, want no answer", got)
	}
}

// TestUserspaceEgressAllowlist: only allowlisted names resolve, and only
// the addresses they resolved to (or allowlisted CIDRs) are reachable.
func TestUserspaceEgressAllowlist(t *testing.T) {
	ip := hostIPv4(t)
	fakeDNS(t, ip)
	addr := echoTCP(t, ip, false)
	_, h := createEgress(t, ip, domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"Allowed.TEST."}})
	if got := runProbe(h, "tcp", addr); !strings.HasPrefix(got, "ERR") {
		t.Errorf("TCP to an unpinned address: %q, want refused", got)
	}
	if got := runProbe(h, "dns", "other.test."); got != "NXDOMAIN" {
		t.Errorf("other.test: %q, want NXDOMAIN", got)
	}
	if got := runProbe(h, "tcp", addr); !strings.HasPrefix(got, "ERR") {
		t.Errorf("TCP after a refused lookup: %q, want refused", got)
	}
	if got := runProbe(h, "dns", "allowed.test."); got != ip.String() {
		t.Fatalf("allowed.test: %q, want %s", got, ip)
	}
	if got := runProbe(h, "tcp", addr); got != "tcp:ping" {
		t.Errorf("TCP to a pinned address: %q", got)
	}
}

// TestUserspaceEgressAllowlistCIDR: a CIDR entry admits its addresses
// without DNS, and names stay refused.
func TestUserspaceEgressAllowlistCIDR(t *testing.T) {
	ip := hostIPv4(t)
	fakeDNS(t, ip)
	addr := echoTCP(t, ip, false)
	_, h := createEgress(t, ip, domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{ip.String()}})
	if got := runProbe(h, "tcp", addr); got != "tcp:ping" {
		t.Errorf("TCP to an allowlisted CIDR: %q", got)
	}
	if got := runProbe(h, "dns", "allowed.test."); got != "NXDOMAIN" {
		t.Errorf("allowed.test with a CIDR-only list: %q, want NXDOMAIN", got)
	}
}

// TestUserspaceEgressSetPolicy switches a live stack to off (closing an
// established flow, refusing new ones) and back to filtered.
func TestUserspaceEgressSetPolicy(t *testing.T) {
	ip := hostIPv4(t)
	addr := echoTCP(t, ip, false)
	holdAddr := echoTCP(t, ip, true)
	_, h := createEgress(t, ip, domain.Egress{Mode: domain.EgressFiltered})
	if h.EgressControl == "" {
		t.Fatal("no egress control socket on the handle")
	}
	exe, _ := os.Executable()
	argv := append(EnterArgv(h), exe, "probe", "hold", holdAddr)
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	defer cmd.Process.Kill()
	lines := bufio.NewScanner(out)
	if !lines.Scan() || lines.Text() != "tcp:ping" {
		t.Fatalf("held flow: %q", lines.Text())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := userspace.SetPolicy(ctx, h.EgressControl, string(domain.EgressOff), nil); err != nil {
		t.Fatal(err)
	}
	if !lines.Scan() || lines.Text() != "CLOSED" {
		t.Errorf("held flow after off: %q, want CLOSED", lines.Text())
	}
	t.Logf("held flow closed %v after SetPolicy", time.Since(start))
	if got := runProbe(h, "tcp", addr); !strings.HasPrefix(got, "ERR") {
		t.Errorf("new TCP after off: %q, want refused", got)
	}
	if err := userspace.SetPolicy(ctx, h.EgressControl, string(domain.EgressFiltered), nil); err != nil {
		t.Fatal(err)
	}
	if got := runProbe(h, "tcp", addr); got != "tcp:ping" {
		t.Errorf("TCP after back to filtered: %q", got)
	}
}

// TestUserspaceEgress sends real traffic from inside a rootless VM netns
// through the userspace stack to host sockets. The host's own IPv4 address
// is explicitly allowed for the test (it stands in for "the internet");
// metadata and other deny-listed addresses must be refused.
func TestUserspaceEgress(t *testing.T) {
	ip := hostIPv4(t)
	cfg := DefaultConfig()
	cfg.Egress.Allow = []netip.Prefix{netip.PrefixFrom(ip, 32)}
	r := newRootlessOrSkip(t, cfg)
	ctx := context.Background()
	h, err := r.Create(ctx, "vm_egress", 1, domain.Egress{Mode: domain.EgressFiltered})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Destroy(ctx, h)
	if !h.Egress.Enabled || h.Egress.Backend != BackendUserspace {
		t.Fatalf("egress = %+v", h.Egress)
	}

	allowed := echoTCP(t, ip, false)
	udpAddr := echoUDP(t, ip)
	run := func(network, addr string) string { return runProbe(h, network, addr) }
	if got := run("tcp", allowed); got != "tcp:ping" {
		t.Errorf("allowed TCP: %q", got)
	}
	if got := run("udp", udpAddr); got != "udp:ping" {
		t.Errorf("allowed UDP: %q", got)
	}
	for _, addr := range []string{"169.254.169.254:80", "100.100.100.100:443", "10.0.2.2:80"} {
		if got := run("tcp", addr); !strings.HasPrefix(got, "ERR") {
			t.Errorf("denied %s: %q, want blocked", addr, got)
		}
	}
	if err := r.Destroy(ctx, h); err != nil {
		t.Fatal(err)
	}
	if alive(h.EgressProc) {
		t.Error("egress process survived Destroy")
	}
}

// TestUserspaceEgressExitsWithHolder: if only the holder dies (daemon gone,
// VM torn down by someone else), the stack exits on its own. Its TAP fd
// would otherwise keep the namespace alive forever.
func TestUserspaceEgressExitsWithHolder(t *testing.T) {
	r := newRootlessOrSkip(t, DefaultConfig())
	ctx := context.Background()
	h, err := r.Create(ctx, "vm_orphan", 1, domain.Egress{Mode: domain.EgressFiltered})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Destroy(ctx, h)
	if !alive(h.EgressProc) {
		t.Fatalf("no egress process: %+v", h.Egress)
	}
	if err := stopProcess(h.Holder, stopGrace); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && alive(h.EgressProc); time.Sleep(10 * time.Millisecond) {
	}
	if alive(h.EgressProc) {
		t.Fatal("egress process outlived its namespace holder")
	}
}

// TestUserspaceEgressLANService: the userspace stack admits an
// operator-permitted LAN service on exactly its transport and port (the
// host's own LAN address, outside HostAddrs here, stands in for a LAN host)
// and, in allowlist mode, only when the VM's list has the address too.
func TestUserspaceEgressLANService(t *testing.T) {
	ip := hostIPv4(t)
	if !containsAddr(DefaultDeny(), ip) {
		t.Skipf("host address %s is not in the deny list, so it is no LAN address", ip)
	}
	svc, other, udp := echoTCP(t, ip, false), echoTCP(t, ip, false), echoUDP(t, ip)
	cfg := DefaultConfig()
	cfg.Egress.Services = []Service{{Proto: "tcp", Addr: ip, Port: netip.MustParseAddrPort(svc).Port()}}
	r := newRootlessOrSkip(t, cfg)
	ctx := context.Background()
	h, err := r.Create(ctx, "vm_lan", 1, domain.Egress{Mode: domain.EgressFiltered})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Destroy(ctx, h)
	if h.Egress.Backend != BackendUserspace {
		t.Skipf("egress backend %q, not the userspace stack", h.Egress.Backend)
	}
	check := func(what string, want map[string]bool) {
		t.Helper()
		for addr, w := range want {
			proto := "tcp"
			if addr == udp {
				proto = "udp"
			}
			if got := runProbe(h, proto, addr) == proto+":ping"; got != w {
				t.Errorf("%s: %s %s reachable = %v, want %v", what, proto, addr, got, w)
			}
		}
	}
	check("filtered", map[string]bool{svc: true, other: false, udp: false, "169.254.169.254:80": false, "100.100.100.100:443": false})
	for _, c := range []struct {
		allow []string
		want  bool
	}{{[]string{"192.0.2.0/24"}, false}, {[]string{ip.String() + "/32"}, true}} {
		if h, err = r.SetEgress(ctx, h, domain.Egress{Mode: domain.EgressAllowlist, Allow: c.allow}); err != nil {
			t.Fatal(err)
		}
		check(fmt.Sprintf("allowlist %v", c.allow), map[string]bool{svc: c.want, other: false, udp: false})
	}
}
