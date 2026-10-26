package network

import (
	"bufio"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"

	"davit/internal/domain"
)

// netnsEcho answers "PROTO:LINE" on every "tcp/ADDR" and "udp/ADDR" it is
// given and prints "ready" once all listen (probe's peer).
func netnsEcho(specs []string) int {
	for _, spec := range specs {
		proto, addr, _ := strings.Cut(spec, "/")
		switch proto {
		case "tcp":
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
						line, _ := bufio.NewReader(c).ReadString('\n')
						fmt.Fprintf(c, "tcp:%s", line)
					}()
				}
			}()
		case "udp":
			u, err := net.ListenPacket("udp", addr)
			if err != nil {
				fmt.Println(err)
				return 1
			}
			go func() {
				buf := make([]byte, 1500)
				for {
					n, from, err := u.ReadFrom(buf)
					if err != nil {
						return
					}
					_, _ = u.WriteTo(append([]byte("udp:"), buf[:n]...), from)
				}
			}()
		}
	}
	fmt.Println("ready")
	select {}
}

// target is a destination a guest probes ("tcp/ADDR:PORT").
type target string

func (tg target) split() (string, string) {
	proto, addr, _ := strings.Cut(string(tg), "/")
	return proto, addr
}

// reach probes every target from h concurrently (a dropped TCP SYN costs
// the probe's whole timeout) and reports which answered.
func (n *ruleNet) reach(h Handle, targets []target) map[target]bool {
	exe, _ := os.Executable()
	out := map[target]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, tg := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			proto, addr := tg.split()
			argv := append(EnterArgv(h), exe, "probe", proto, addr)
			got, _ := exec.Command(argv[0], argv[1:]...).Output()
			mu.Lock()
			out[tg] = strings.TrimSpace(string(got)) == proto+":ping"
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// expectReach checks h's reachability of every target in want.
func (n *ruleNet) expectReach(what string, h Handle, want map[target]bool) {
	n.t.Helper()
	got := n.reach(h, slicesOf(want))
	for tg, w := range want {
		if got[tg] != w {
			n.t.Errorf("%s: %s reachable = %v, want %v", what, tg, got[tg], w)
		}
	}
}

func slicesOf(m map[target]bool) []target {
	var out []target
	for k := range maps.Keys(m) {
		out = append(out, k)
	}
	return out
}

// lanWorld adds LAN-service and blocked-target listeners to n's world:
// 192.168.7.20 is a LAN host with a permitted service on tcp and udp 5432
// and an unpermitted one on tcp 5433; 10.200.0.6 is another VM's
// (privileged) uplink address; 203.0.113.9 is a host-management address.
func lanWorld(n *ruleNet) {
	n.t.Helper()
	for _, a := range []string{"192.168.7.20", "10.200.0.6"} {
		n.run(n.world, "", "ip", "addr", "add", a+"/32", "dev", "lo")
	}
	var specs []string
	for _, tg := range lanTargets {
		specs = append(specs, string(tg))
	}
	n.daemon(n.world, append([]string{"netns-echo"}, specs...)...)
}

const (
	svcTCP    target = "tcp/192.168.7.20:5432" // permitted LAN service
	svcUDP    target = "udp/192.168.7.20:5432" // permitted LAN service
	lanOther  target = "tcp/192.168.7.20:5433" // same LAN host, other port
	lanHost   target = "tcp/192.168.1.1:5432"  // other LAN host, same port
	hostMgmt  target = "tcp/203.0.113.9:22"    // host address, even though listed as a service
	metadata  target = "tcp/169.254.169.254:80"
	tailnet   target = "tcp/100.100.100.100:443"
	otherVM   target = "tcp/10.200.0.6:80"
	internet  target = "tcp/203.0.113.1:443"
	allowedDB target = "tcp/203.0.113.50:5432" // in the VM allowlist below
)

var lanTargets = []target{svcTCP, svcUDP, lanOther, lanHost, hostMgmt, metadata, tailnet, otherVM, internet, allowedDB}

// lanConfig permits the two LAN services and (to prove it cannot) the host
// address 203.0.113.9:22.
func lanConfig() Config {
	cfg := DefaultConfig(netip.MustParsePrefix("203.0.113.9/32"))
	cfg.Egress.Services = []Service{
		{Proto: "tcp", Addr: netip.MustParseAddr("192.168.7.20"), Port: 5432},
		{Proto: "udp", Addr: netip.MustParseAddr("192.168.7.20"), Port: 5432},
		{Proto: "tcp", Addr: netip.MustParseAddr("203.0.113.9"), Port: 22},
	}
	return cfg
}

// blocked are the targets no mode or service may reach.
var blocked = map[target]bool{lanOther: false, lanHost: false, hostMgmt: false, metadata: false, tailnet: false, otherVM: false}

func with(base map[target]bool, extra map[target]bool) map[target]bool {
	out := maps.Clone(base)
	maps.Copy(out, extra)
	return out
}

// TestGuestRulesetLANServices: operator-permitted LAN services are reachable
// on exactly their address, transport and port; the rest of the LAN, the
// host, metadata, the tailnet and other VMs stay blocked in every mode.
func TestGuestRulesetLANServices(t *testing.T) {
	n := newRuleNet(t)
	lanWorld(n)
	cfg := lanConfig()

	n.load(cfg, domain.Egress{Mode: domain.EgressFiltered})
	n.expectReach("filtered", n.guest, with(blocked, map[target]bool{svcTCP: true, svcUDP: true, internet: true, allowedDB: true}))

	// Allowlist: a service also needs the address on the VM's own list.
	n.load(cfg, domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"203.0.113.50/32"}})
	n.expectReach("allowlist without the service", n.guest, with(blocked, map[target]bool{svcTCP: false, svcUDP: false, internet: false, allowedDB: true}))
	n.load(cfg, domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"192.168.7.20/32", "203.0.113.9/32", "10.200.0.0/16"}})
	n.expectReach("allowlist with the service", n.guest, with(blocked, map[target]bool{svcTCP: true, svcUDP: true, internet: false, allowedDB: false}))

	n.load(cfg, domain.Egress{Mode: domain.EgressOff})
	n.expectReach("off", n.guest, with(blocked, map[target]bool{svcTCP: false, svcUDP: false, internet: false, allowedDB: false}))

	// Without the operator's permission the LAN host is just LAN.
	n.load(DefaultConfig(netip.MustParsePrefix("203.0.113.9/32")), domain.Egress{Mode: domain.EgressFiltered})
	n.expectReach("filtered without services", n.guest, with(blocked, map[target]bool{svcTCP: false, svcUDP: false, internet: true}))
}

// TestGuestBridgeEgress: traffic from a bridge inside the guest (Docker's
// containers) gets exactly the guest's own policy, whether the guest
// masquerades it (Docker's default) or routes it (Config.GuestSubnets); a
// routed subnet the operator did not declare cannot leave at all.
//
//	container: eth0 172.17.0.2/16 ── guest: br0 172.17.0.1/16, eth0 172.16.0.2 ── VM netns ── world
func TestGuestBridgeEgress(t *testing.T) {
	n := newRuleNet(t)
	lanWorld(n)
	ctr := childNetNS(t, n.vm)
	n.run(n.guest, "", "ip", "link", "add", "br0", "type", "veth", "peer", "name", "eth0", "netns", strconv.Itoa(ctr.Holder.PID))
	n.run(n.guest, "", "ip", "addr", "add", "172.17.0.1/16", "dev", "br0")
	n.run(n.guest, "", "ip", "link", "set", "dev", "br0", "up")
	n.run(n.guest, "", "sysctl", "-q", "-w", "net.ipv4.ip_forward=1")
	n.run(ctr, "", "ip", "link", "set", "dev", "lo", "up")
	n.run(ctr, "", "ip", "addr", "add", "172.17.0.2/16", "dev", "eth0")
	n.run(ctr, "", "ip", "link", "set", "dev", "eth0", "up")
	n.run(ctr, "", "ip", "route", "add", "default", "via", "172.17.0.1")

	cfg := lanConfig()
	want := with(blocked, map[target]bool{svcTCP: true, svcUDP: true, internet: true, allowedDB: true})
	same := func(what string) {
		t.Helper()
		n.expectReach(what+" (guest)", n.guest, want)
		n.expectReach(what+" (container)", ctr, want)
	}

	// Docker's default: the guest masquerades its bridge.
	n.run(n.guest, `table ip docker {
	chain post {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "eth0" ip saddr 172.17.0.0/16 masquerade
	}
}
`, "nft", "-f", "-")
	n.load(cfg, domain.Egress{Mode: domain.EgressFiltered})
	same("masqueraded bridge, filtered")
	n.load(cfg, domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"203.0.113.50/32"}})
	want = with(blocked, map[target]bool{svcTCP: false, svcUDP: false, internet: false, allowedDB: true})
	same("masqueraded bridge, allowlist")

	// A routed bridge (no masquerade in the guest).
	n.run(n.guest, "", "nft", "delete", "table", "ip", "docker")
	n.load(cfg, domain.Egress{Mode: domain.EgressFiltered})
	none := map[target]bool{}
	for _, tg := range lanTargets {
		none[tg] = false
	}
	n.expectReach("undeclared routed bridge", ctr, none)

	routed := cfg
	routed.GuestSubnets = []netip.Prefix{netip.MustParsePrefix("172.17.0.0/16")}
	if err := routed.Validate(); err != nil {
		t.Fatal(err)
	}
	// Rootless.Create adds this return route for every GuestSubnets entry.
	n.run(n.vm, "", "ip", "route", "add", "172.17.0.0/16", "via", "172.16.0.2", "dev", "tap0")
	n.load(routed, domain.Egress{Mode: domain.EgressFiltered})
	want = with(blocked, map[target]bool{svcTCP: true, svcUDP: true, internet: true, allowedDB: true})
	same("declared routed bridge, filtered")
	n.load(routed, domain.Egress{Mode: domain.EgressOff})
	n.expectReach("declared routed bridge, off", ctr, none)
	// Anti-spoofing still holds for everything else.
	if n.ping("-I", "10.99.0.2", "203.0.113.1") {
		t.Error("spoofed source address was forwarded")
	}
}
