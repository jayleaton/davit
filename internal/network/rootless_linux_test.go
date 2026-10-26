package network

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"davit/internal/domain"
	"davit/internal/network/userspace"
)

// newRootlessOrSkip returns a provider with egress backends hidden (so the
// test never depends on pasta/slirp4netns being installed) or skips when
// unprivileged user namespaces are unavailable.
func newRootlessOrSkip(t *testing.T, cfg Config) *Rootless {
	t.Helper()
	r, err := NewRootless(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range r.Probe(context.Background()).Modes {
		if m.Mode == ModeRootless && !m.Usable {
			t.Skip("rootless network namespaces unavailable: " + m.Reason)
		}
	}
	r.lookPath = func(name string) (string, error) {
		if name == BackendPasta || name == BackendSlirp4netns {
			return "", exec.ErrNotFound
		}
		return exec.LookPath(name)
	}
	return r
}

func nsOutput(t *testing.T, h Handle, argv ...string) string {
	t.Helper()
	full := append(EnterArgv(h), argv...)
	out, err := exec.Command(full[0], full[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v: %s", argv, err, out)
	}
	return string(out)
}

func TestRootlessLifecycle(t *testing.T) {
	r := newRootlessOrSkip(t, DefaultConfig())
	_, haveNft := exec.LookPath("nft")
	ctx := context.Background()

	if _, err := r.Create(ctx, "vm;reboot", 1, domain.Egress{Mode: domain.EgressFiltered}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Create(bad id) = %v, want ErrInvalid", err)
	}

	var hs []Handle
	for _, id := range []domain.VMID{"vm_one", "vm_two"} {
		h, err := r.Create(ctx, id, 1, domain.Egress{Mode: domain.EgressFiltered})
		if err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
		t.Cleanup(func() { _ = r.Destroy(ctx, h) })
		hs = append(hs, h)
	}

	inodes := map[string]bool{}
	for _, h := range hs {
		if h.TapName != "tap0" || h.GuestIP != DefaultGuestIP || h.GuestMAC != DefaultGuestMAC {
			t.Errorf("%s: handle not using the shared guest identity: %+v", h.VMID, h)
		}
		var links []struct {
			IfName   string `json:"ifname"`
			MTU      int    `json:"mtu"`
			AddrInfo []struct {
				Local     string `json:"local"`
				PrefixLen int    `json:"prefixlen"`
			} `json:"addr_info"`
		}
		if err := json.Unmarshal([]byte(nsOutput(t, h, "ip", "-j", "addr", "show", "dev", "tap0")), &links); err != nil {
			t.Fatal(err)
		}
		if len(links) != 1 || len(links[0].AddrInfo) != 1 || links[0].AddrInfo[0].Local != "172.16.0.1" ||
			links[0].AddrInfo[0].PrefixLen != 30 || links[0].MTU != 1500 {
			t.Errorf("%s: tap0 = %+v, want 172.16.0.1/30 mtu 1500", h.VMID, links)
		}
		ino, err := os.Readlink(h.NetNSPath)
		if err != nil {
			t.Fatal(err)
		}
		inodes[ino] = true
		// No pasta/slirp4netns: the built-in userspace stack serves egress.
		if !h.Egress.Enabled || h.Egress.Backend != BackendUserspace || !h.Egress.Filtered || !alive(h.EgressProc) {
			t.Errorf("%s: egress = %+v proc %+v, want userspace", h.VMID, h.Egress, h.EgressProc)
		}
		if out := nsOutput(t, h, "ip", "-4", "-o", "addr", "show", "dev", "up0"); !strings.Contains(out, "10.0.2.15/24") {
			t.Errorf("%s: up0 = %q", h.VMID, out)
		}
		if haveNft == nil {
			if !h.Egress.Filtered {
				t.Errorf("%s: guest ruleset not installed: %+v", h.VMID, h.Egress)
			}
			nsOutput(t, h, "nft", "list", "table", "inet", GuestTable)
		}
		if _, err := r.Enter(h); err != nil {
			t.Errorf("Enter: %v", err)
		}
	}
	if len(inodes) != 2 {
		t.Fatalf("VMs share a network namespace: %v", inodes)
	}

	for _, h := range hs {
		if err := r.Destroy(ctx, h); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		if alive(h.Holder) || alive(h.EgressProc) {
			t.Errorf("holder %d or egress %+v still alive after Destroy", h.Holder.PID, h.EgressProc)
		}
		if err := r.Destroy(ctx, h); err != nil {
			t.Errorf("second Destroy: %v", err)
		}
		if _, err := r.Enter(h); err == nil {
			t.Error("Enter after Destroy succeeded")
		}
	}
}

// TestRootlessWithoutNft: pasta/slirp4netns would need the ruleset to
// filter, so only the userspace stack (which filters itself) is used.
func TestRootlessWithoutNft(t *testing.T) {
	r := newRootlessOrSkip(t, DefaultConfig())
	r.lookPath = func(name string) (string, error) {
		if name == "nft" || name == BackendPasta || name == BackendSlirp4netns {
			return "", exec.ErrNotFound
		}
		return exec.LookPath(name)
	}
	h, err := r.Create(context.Background(), "vm_nonft", 1, domain.Egress{Mode: domain.EgressFiltered})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Destroy(context.Background(), h)
	if !h.Egress.Enabled || h.Egress.Backend != BackendUserspace || !h.Egress.Filtered {
		t.Fatalf("egress = %+v, want userspace", h.Egress)
	}
}

// ruleNet is a real (rootless) VM namespace for ruleset tests. A veth
// stands in for the TAP so a "guest" netns can originate packets; a third
// netns plays the uplink/internet and has no route back to 172.16.0.2, so
// every successful reply also proves the masquerade.
//
//	guest netns: eth0 172.16.0.2/30 ── VM netns: tap0 172.16.0.1/30, up0 198.51.100.1/24 ── world: eth0 198.51.100.2/24
type ruleNet struct {
	t                *testing.T
	vm, guest, world Handle
	exe              string
}

// worldAddrs are on the world's loopback (the "internet" and denied ranges).
var worldAddrs = []string{"203.0.113.1", "203.0.113.9", "203.0.113.50", "203.0.113.77",
	"192.168.1.1", "192.168.7.7", "169.254.169.254", "100.100.100.100"}

func newRuleNet(t *testing.T) *ruleNet {
	t.Helper()
	newRootlessOrSkip(t, DefaultConfig()) // skip check only
	for _, tool := range []string{"nft", "ping", "sysctl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not found in PATH")
		}
	}
	holder, err := startHolder("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopProcess(holder, stopGrace) })
	pid := strconv.Itoa(holder.PID)
	exe, _ := os.Executable()
	n := &ruleNet{t: t, exe: exe,
		vm: Handle{Mode: ModeRootless, Holder: holder, NetNSPath: "/proc/" + pid + "/ns/net", UserNSPath: "/proc/" + pid + "/ns/user"}}
	n.guest, n.world = childNetNS(t, n.vm), childNetNS(t, n.vm)
	gpid, wpid := strconv.Itoa(n.guest.Holder.PID), strconv.Itoa(n.world.Holder.PID)
	n.run(n.vm, "", "ip", "link", "add", "tap0", "type", "veth", "peer", "name", "eth0", "netns", gpid)
	n.run(n.vm, "", "ip", "link", "add", "up0", "type", "veth", "peer", "name", "eth0", "netns", wpid)
	for _, h := range []Handle{n.vm, n.guest, n.world} {
		n.run(h, "", "ip", "link", "set", "dev", "lo", "up")
	}
	n.run(n.vm, "", "ip", "addr", "add", "172.16.0.1/30", "dev", "tap0")
	n.run(n.vm, "", "ip", "link", "set", "dev", "tap0", "up")
	n.run(n.vm, "", "ip", "addr", "add", "198.51.100.1/24", "dev", "up0")
	n.run(n.vm, "", "ip", "link", "set", "dev", "up0", "up")
	n.run(n.vm, "", "ip", "route", "add", "default", "via", "198.51.100.2")
	n.run(n.vm, "", "ip", "route", "add", "10.99.0.0/24", "via", "172.16.0.2") // return path for the spoof check
	n.run(n.vm, "", "sysctl", "-q", "-w", "net.ipv4.ip_forward=1")

	n.run(n.guest, "", "ip", "addr", "add", "172.16.0.2/30", "dev", "eth0")
	n.run(n.guest, "", "ip", "addr", "add", "10.99.0.2/32", "dev", "lo") // spoof source
	n.run(n.guest, "", "ip", "link", "set", "dev", "eth0", "up")
	n.run(n.guest, "", "ip", "route", "add", "default", "via", "172.16.0.1")

	n.run(n.world, "", "ip", "addr", "add", "198.51.100.2/24", "dev", "eth0")
	n.run(n.world, "", "ip", "link", "set", "dev", "eth0", "up")
	// The world can answer the spoofed source, so only the ruleset stops it.
	n.run(n.world, "", "ip", "route", "add", "10.99.0.0/24", "via", "198.51.100.1")
	for _, a := range worldAddrs {
		n.run(n.world, "", "ip", "addr", "add", a+"/32", "dev", "lo")
	}
	return n
}

func (n *ruleNet) run(h Handle, stdin string, argv ...string) string {
	n.t.Helper()
	full := append(EnterArgv(h), argv...)
	cmd := exec.Command(full[0], full[1:]...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		n.t.Fatalf("%v: %v: %s", argv, err, out)
	}
	return string(out)
}

// load replaces the VM netns guest table with policy's ruleset.
func (n *ruleNet) load(cfg Config, policy domain.Egress) {
	n.t.Helper()
	n.run(n.vm, RenderGuestReplace(cfg, policy, false), "nft", "-f", "-")
}

func (n *ruleNet) ping(args ...string) bool {
	full := append(EnterArgv(n.guest), append([]string{"ping", "-n", "-c1", "-W", "0.5"}, args...)...)
	return exec.Command(full[0], full[1:]...).Run() == nil
}

// expect checks guest reachability of each destination.
func (n *ruleNet) expect(what string, want map[string]bool) {
	n.t.Helper()
	for dst, w := range want {
		if got := n.ping(dst); got != w {
			n.t.Errorf("%s: guest -> %s reachable = %v, want %v", what, dst, got, w)
		}
	}
}

// query sends a DNS A query from the guest and returns "RCODE addr,...".
func (n *ruleNet) query(network, server, name string) string {
	full := append(EnterArgv(n.guest), n.exe, "netns-dns-query", network, server, name)
	out, _ := exec.Command(full[0], full[1:]...).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// daemon starts the test binary's subcommand in h and waits for "ready".
func (n *ruleNet) daemon(h Handle, args ...string) {
	n.t.Helper()
	full := append(append(EnterArgv(h), n.exe), args...)
	cmd := exec.Command(full[0], full[1:]...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		n.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "ready\n" {
		n.t.Fatalf("%v: %q %v", args, line, err)
	}
}

// TestGuestRulesetFilters loads the filtered guest ruleset into a real VM
// namespace and sends traffic through it.
func TestGuestRulesetFilters(t *testing.T) {
	n := newRuleNet(t)
	cfg := DefaultConfig(netip.MustParsePrefix("203.0.113.9/32"))
	cfg.Egress.Allow = []netip.Prefix{netip.MustParsePrefix("192.168.7.7/32")}
	n.run(n.vm, RenderGuestRuleset(cfg, domain.Egress{Mode: domain.EgressFiltered}, false), "nft", "-f", "-")
	n.expect("filtered", map[string]bool{
		"203.0.113.1":     true,  // internet
		"192.168.7.7":     true,  // RFC1918 but explicitly allowed
		"192.168.1.1":     false, // LAN
		"203.0.113.9":     false, // host-management address
		"169.254.169.254": false, // metadata
		"100.100.100.100": false, // tailnet CGNAT
		"172.16.0.1":      false, // the VM netns itself (input)
	})
	if n.ping("-I", "10.99.0.2", "203.0.113.1") {
		t.Error("spoofed source address was forwarded")
	}
}

// TestGuestRulesetModes switches one live VM netns between the modes (each
// load is one atomic nft transaction) and checks what the guest reaches.
func TestGuestRulesetModes(t *testing.T) {
	n := newRuleNet(t)
	cfg := DefaultConfig(netip.MustParsePrefix("203.0.113.9/32"))
	cfg.Egress.Allow = []netip.Prefix{netip.MustParsePrefix("192.168.7.7/32")}
	// A DNS server on the VM side of the TAP: the resolver's address.
	n.daemon(n.vm, "netns-fake-dns", "172.16.0.1:53")

	n.load(cfg, domain.Egress{Mode: domain.EgressOff})
	n.expect("off", map[string]bool{"203.0.113.1": false, "203.0.113.50": false, "172.16.0.1": false})
	if got := n.query("udp", "172.16.0.1:53", "a.example.com"); strings.HasPrefix(got, "NOERROR") {
		t.Errorf("off: DNS answered: %q", got)
	}

	n.load(cfg, domain.Egress{Mode: domain.EgressAllowlist,
		Allow: []string{"169.254.169.254/32", "203.0.113.48/28", "example.com"}})
	n.expect("allowlist", map[string]bool{
		"203.0.113.50":    true,  // in a vmallow4 CIDR
		"203.0.113.1":     false, // internet, not allowlisted
		"203.0.113.77":    false, // not pinned yet
		"192.168.7.7":     false, // host-level allow, but not allowlisted
		"169.254.169.254": false, // allowlisted CIDR inside the deny list
		"172.16.0.1":      false, // only DNS reaches the netns
	})
	n.run(n.vm, "", "nft", "add", "element", "inet", GuestTable, PinnedSet, "{ 203.0.113.77 timeout 60s, 203.0.113.9 timeout 60s }")
	n.expect("allowlist pinned", map[string]bool{"203.0.113.77": true, "203.0.113.9": false /* host address */})
	// DNS to any address, over UDP and TCP, is DNAT'd to the resolver (the
	// world has no 9.9.9.9 and drops nothing: only the DNAT can answer).
	for _, netw := range []string{"udp", "tcp"} {
		for _, srv := range []string{"9.9.9.9:53", "203.0.113.1:53", "172.16.0.1:53"} {
			if got := n.query(netw, srv, "a.example.com"); got != "NOERROR 203.0.113.99" {
				t.Errorf("allowlist DNS %s %s = %q, want the resolver's answer", netw, srv, got)
			}
		}
	}

	n.load(cfg, domain.Egress{Mode: domain.EgressFiltered})
	n.expect("filtered", map[string]bool{"203.0.113.1": true, "203.0.113.77": true, "192.168.1.1": false})
	if got := n.query("udp", "9.9.9.9:53", "a.example.com"); strings.HasPrefix(got, "NOERROR") {
		t.Errorf("filtered: DNS to 9.9.9.9 was redirected: %q", got)
	}
}

// TestGuestRulesetTightenCutsFlows: switching to allowlist stops an
// established flow to a destination the allowlist does not admit (only
// flows admitted under an allowlist ruleset stay established), while one to
// an admitted destination continues.
func TestGuestRulesetTightenCutsFlows(t *testing.T) {
	n := newRuleNet(t)
	cfg := DefaultConfig()
	received := func(dst string, switchTo domain.Egress) int {
		t.Helper()
		n.load(cfg, domain.Egress{Mode: domain.EgressFiltered})
		full := append(EnterArgv(n.guest), "ping", "-n", "-c", "10", "-i", "0.2", "-W", "1", dst)
		cmd := exec.Command(full[0], full[1:]...)
		var out strings.Builder
		cmd.Stdout = &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(700 * time.Millisecond)
		n.load(cfg, switchTo)
		_ = cmd.Wait()
		var got int
		for _, l := range strings.Split(out.String(), "\n") {
			if strings.Contains(l, "bytes from") {
				got++
			}
		}
		return got
	}
	if got := received("203.0.113.1", domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"example.com"}}); got == 0 || got > 6 {
		t.Errorf("flow to a non-allowlisted address: %d/10 replies, want it cut after the switch", got)
	}
	if got := received("203.0.113.1", domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"203.0.113.0/28"}}); got < 9 {
		t.Errorf("flow to an allowlisted CIDR: %d/10 replies, want it to continue", got)
	}
	if got := received("203.0.113.1", domain.Egress{Mode: domain.EgressOff}); got == 0 || got > 6 {
		t.Errorf("flow after off: %d/10 replies, want it cut", got)
	}
}

// TestNetnsDNSResolver runs the real resolver subcommand inside a VM netns
// with the allowlist ruleset: an allowed name is answered from the (fake,
// netns-local) upstream and its address pinned before the guest gets the
// answer; a disallowed name is NXDOMAIN; the resolver exits with the holder.
func TestNetnsDNSResolver(t *testing.T) {
	n := newRuleNet(t)
	n.daemon(n.vm, "netns-fake-dns", "127.0.0.1:5300")
	cfg := DefaultConfig()
	n.load(cfg, domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"example.com"}})
	r := &Rootless{cfg: cfg, lookPath: exec.LookPath, upstreams: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:5300")}}
	p, err := r.startResolver(context.Background(), n.vm, []string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopProcess(p, stopGrace) })

	if n.ping("203.0.113.77") {
		t.Fatal("203.0.113.77 reachable before it was resolved")
	}
	if got := n.query("udp", "9.9.9.9:53", "allowed.example.com"); got != "NOERROR 203.0.113.77" {
		t.Fatalf("allowed name: %q", got)
	}
	set := n.run(n.vm, "", "nft", "-j", "list", "set", "inet", GuestTable, PinnedSet)
	if !strings.Contains(set, `"val": "203.0.113.77"`) || !strings.Contains(set, `"comment": "allowed.example.com"`) {
		t.Fatalf("answer not pinned: %s", set)
	}
	if !strings.Contains(set, `"timeout": 60`) { // fake TTL 5s, clamped to MinPin
		t.Errorf("pin timeout not clamped to 60s: %s", set)
	}
	if !n.ping("203.0.113.77") {
		t.Error("pinned address unreachable")
	}
	if got := n.query("tcp", "1.1.1.1:53", "www.example.com"); got != "NOERROR 203.0.113.99" {
		t.Errorf("allowed name over TCP: %q", got)
	}
	if got := n.query("udp", "9.9.9.9:53", "evil.org"); got != "NXDOMAIN" {
		t.Errorf("disallowed name: %q, want NXDOMAIN", got)
	}
	if strings.Contains(n.run(n.vm, "", "nft", "list", "set", "inet", GuestTable, PinnedSet), "203.0.113.98") {
		t.Error("disallowed name pinned")
	}
	if err := stopProcess(n.vm.Holder, stopGrace); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && alive(p); time.Sleep(10 * time.Millisecond) {
	}
	if alive(p) {
		t.Error("resolver outlived its namespace holder")
	}
}

// TestRootlessSetEgress switches a live rootless network's policy: the
// table is replaced atomically, the userspace stack is told (and a failure
// there keeps the old table), and on an nft-only backend the resolver is
// started, restarted with a new list (carrying over still-allowed pins) and
// stopped.
func TestRootlessSetEgress(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft not found in PATH")
	}
	r := newRootlessOrSkip(t, DefaultConfig())
	r.upstreams = []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:5300")}
	ctx := context.Background()
	h, err := r.Create(ctx, "vm_setegress", 1, domain.Egress{Mode: domain.EgressFiltered})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Destroy(ctx, h) })
	if h.Egress.Backend != BackendUserspace {
		t.Fatalf("egress = %+v", h.Egress)
	}
	var calls []string
	var stackErr error
	setStackPolicy = func(_ context.Context, control, mode string, allow []string) error {
		calls = append(calls, mode+" "+strings.Join(allow, ","))
		return stackErr
	}
	t.Cleanup(func() { setStackPolicy = userspace.SetPolicy })
	forward := func(h Handle) string {
		return nsOutput(t, h, "nft", "list", "chain", "inet", GuestTable, "forward")
	}
	hasPins := func(h Handle) bool {
		full := append(EnterArgv(h), "nft", "list", "set", "inet", GuestTable, PinnedSet)
		return exec.Command(full[0], full[1:]...).Run() == nil
	}

	off := domain.Egress{Mode: domain.EgressOff}
	h, err = r.SetEgress(ctx, h, off)
	if err != nil || h.Policy.Mode != domain.EgressOff {
		t.Fatalf("SetEgress(off) = %+v, %v", h.Policy, err)
	}
	if f := forward(h); !strings.Contains(f, `iifname "tap0" drop`) || strings.Contains(f, "@deny4") {
		t.Errorf("off forward chain:\n%s", f)
	}
	stackErr = errors.New("stack says no")
	if _, err := r.SetEgress(ctx, h, domain.Egress{Mode: domain.EgressFiltered}); err == nil || !strings.Contains(err.Error(), "stack says no") {
		t.Fatalf("SetEgress with failing stack = %v", err)
	}
	if f := forward(h); strings.Contains(f, "@deny4") {
		t.Errorf("failed SetEgress left the new table:\n%s", f)
	}
	stackErr = nil
	if want := []string{"off ", "filtered "}; !slices.Equal(calls, want) {
		t.Errorf("stack calls = %q, want %q", calls, want)
	}

	// From here on, pretend the backend is nft-only (pasta/slirp4netns are
	// not installed): the resolver serves the allowlist.
	h.Egress.Backend = BackendSlirp4netns
	allow := domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"203.0.113.0/24", "example.com"}}
	h, err = r.SetEgress(ctx, h, allow)
	if err != nil {
		t.Fatal(err)
	}
	if !alive(h.DNSProc) || !hasPins(h) || len(calls) != 2 {
		t.Fatalf("allowlist: resolver %+v, pin set %v, stack calls %q", h.DNSProc, hasPins(h), calls)
	}
	if s := nsOutput(t, h, "nft", "list", "set", "inet", GuestTable, VMAllowSet); !strings.Contains(s, "203.0.113.0/24") {
		t.Errorf("vmallow4:\n%s", s)
	}
	first := h.DNSProc
	same, err := r.SetEgress(ctx, h, allow)
	if err != nil || same.DNSProc == nil || *same.DNSProc != *first {
		t.Errorf("re-applying the policy restarted the resolver: %+v %v", same.DNSProc, err)
	}
	nsOutput(t, h, "nft", "add", "element", "inet", GuestTable, PinnedSet,
		`{ 203.0.113.5 timeout 600s comment "www.example.com", 203.0.113.6 timeout 600s comment "gone.org" }`)
	allow2 := domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"example.com", "foo.org"}}
	h, err = r.SetEgress(ctx, h, allow2)
	if err != nil {
		t.Fatal(err)
	}
	if alive(first) || !alive(h.DNSProc) {
		t.Errorf("list change: old resolver alive %v, new %+v", alive(first), h.DNSProc)
	}
	set := nsOutput(t, h, "nft", "list", "set", "inet", GuestTable, PinnedSet)
	if !strings.Contains(set, "203.0.113.5") || strings.Contains(set, "203.0.113.6") {
		t.Errorf("pins after list change (want www.example.com kept, gone.org dropped):\n%s", set)
	}
	if s := nsOutput(t, h, "nft", "list", "set", "inet", GuestTable, VMAllowSet); strings.Contains(s, "203.0.113.0/24") {
		t.Errorf("removed CIDR still in vmallow4:\n%s", s)
	}

	// A resolver that cannot start keeps the previous policy in force.
	selfExe = func() (string, error) { return "", errors.New("no binary") }
	_, err = r.SetEgress(ctx, h, allow)
	selfExe = helperExe
	if err == nil {
		t.Fatal("SetEgress succeeded without a resolver")
	}
	if s := nsOutput(t, h, "nft", "list", "set", "inet", GuestTable, VMAllowSet); strings.Contains(s, "203.0.113.0/24") {
		t.Errorf("failed SetEgress left the new table:\n%s", s)
	}
	if set := nsOutput(t, h, "nft", "list", "set", "inet", GuestTable, PinnedSet); !strings.Contains(set, "203.0.113.5") {
		t.Errorf("failed SetEgress lost the pins:\n%s", set)
	}

	resolver := h.DNSProc
	h, err = r.SetEgress(ctx, h, domain.Egress{Mode: domain.EgressFiltered})
	if err != nil {
		t.Fatal(err)
	}
	if h.DNSProc != nil || alive(resolver) || hasPins(h) || !strings.Contains(forward(h), "@deny4") {
		t.Errorf("filtered: resolver %+v (old alive %v), pin set %v", h.DNSProc, alive(resolver), hasPins(h))
	}
	if h, err = r.SetEgress(ctx, h, allow); err != nil {
		t.Fatal(err)
	}
	if err := r.Destroy(ctx, h); err != nil {
		t.Fatal(err)
	}
	if alive(h.DNSProc) {
		t.Error("resolver survived Destroy")
	}
}

// childNetNS starts a holder for a new netns owned by parent's user
// namespace (so veths can be moved between them without privileges).
func childNetNS(t *testing.T, parent Handle) Handle {
	t.Helper()
	argv := []string{"nsenter", "--preserve-credentials", "--user=" + parent.UserNSPath, "--", "unshare", "--net", "--", "sleep", "infinity"}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	pid := strconv.Itoa(cmd.Process.Pid)
	deadline := time.Now().Add(readyTimeout)
	for {
		if b, _ := os.ReadFile("/proc/" + pid + "/comm"); strings.TrimSpace(string(b)) == "sleep" {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("child netns holder not ready")
		}
		time.Sleep(2 * time.Millisecond)
	}
	p, err := identify(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopProcess(p, stopGrace) })
	return Handle{Mode: ModeRootless, Holder: p, UserNSPath: parent.UserNSPath, NetNSPath: "/proc/" + pid + "/ns/net"}
}

// init runs DNS test helpers inside namespaces (TestMain in
// userspace_linux_test.go serves the resolver subcommand itself).
func init() {
	if len(os.Args) < 3 {
		return
	}
	switch os.Args[1] {
	case "netns-fake-dns":
		os.Exit(netnsFakeDNS(os.Args[2]))
	case "netns-dns-query":
		os.Exit(netnsDNSQuery(os.Args[2], os.Args[3], os.Args[4]))
	case "netns-echo":
		os.Exit(netnsEcho(os.Args[2:]))
	}
}

// netnsFakeDNS serves A answers on addr (UDP and TCP), TTL 5s:
// allowed.example.com is 203.0.113.77, evil.org 203.0.113.98, anything
// else 203.0.113.99. It prints "ready" once listening.
func netnsFakeDNS(addr string) int {
	answer := func(q []byte) []byte {
		var p dnsmessage.Parser
		h, err := p.Start(q)
		if err != nil {
			return nil
		}
		qs, err := p.AllQuestions()
		if err != nil || len(qs) != 1 {
			return nil
		}
		ip := [4]byte{203, 0, 113, 99}
		switch qs[0].Name.String() {
		case "allowed.example.com.":
			ip[3] = 77
		case "evil.org.":
			ip[3] = 98
		}
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true})
		_ = b.StartQuestions()
		_ = b.Question(qs[0])
		_ = b.StartAnswers()
		_ = b.AResource(dnsmessage.ResourceHeader{Name: qs[0].Name, Class: dnsmessage.ClassINET, TTL: 5}, dnsmessage.AResource{A: ip})
		out, _ := b.Finish()
		return out
	}
	u, err := net.ListenPacket("udp", addr)
	if err != nil {
		fmt.Println(err)
		return 1
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Println(err)
		return 1
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var n uint16
				if binary.Read(c, binary.BigEndian, &n) != nil {
					return
				}
				q := make([]byte, n)
				if _, err := io.ReadFull(c, q); err != nil {
					return
				}
				a := answer(q)
				_, _ = c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(a))), a...))
			}()
		}
	}()
	fmt.Println("ready")
	buf := make([]byte, 1500)
	for {
		n, from, err := u.ReadFrom(buf)
		if err != nil {
			return 1
		}
		if a := answer(buf[:n]); a != nil {
			_, _ = u.WriteTo(a, from)
		}
	}
}

// netnsDNSQuery sends an A query for name to server over network ("udp"/"tcp")
// and prints "RCODE addr,..." (e.g. "NOERROR 203.0.113.77", "NXDOMAIN").
func netnsDNSQuery(network, server, name string) int {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 4242, RecursionDesired: true})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	c, err := net.DialTimeout(network, server, 2*time.Second)
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	var resp []byte
	if network == "tcp" {
		_, err = c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...))
		var n uint16
		if err == nil {
			err = binary.Read(c, binary.BigEndian, &n)
		}
		if err == nil {
			resp = make([]byte, n)
			_, err = io.ReadFull(c, resp)
		}
	} else if _, err = c.Write(q); err == nil {
		resp = make([]byte, 1500)
		var n int
		n, err = c.Read(resp)
		resp = resp[:n]
	}
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	_ = p.SkipAllQuestions()
	var addrs []string
	for {
		if _, err := p.AnswerHeader(); err != nil {
			break
		}
		r, err := p.AResource()
		if err != nil {
			break
		}
		addrs = append(addrs, netip.AddrFrom4(r.A).String())
	}
	rc := strings.TrimPrefix(h.RCode.String(), "RCode")
	switch h.RCode {
	case dnsmessage.RCodeSuccess:
		rc = "NOERROR"
	case dnsmessage.RCodeNameError:
		rc = "NXDOMAIN"
	}
	fmt.Println(strings.TrimSpace(rc + " " + strings.Join(addrs, ",")))
	return 0
}
