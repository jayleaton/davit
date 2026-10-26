// Package userspace is the rootless guest egress backend that needs nothing
// installed on the host: a gVisor TCP/IP stack (gvisor.dev/gvisor, Apache-2.0)
// serving the uplink TAP inside a VM's network namespace and turning guest
// flows into ordinary sockets of an unprivileged process in the host network
// namespace (like pasta or slirp4netns, but built into the davit binary).
//
// It runs as its own process (`davit netstack-egress`), started in its own
// session, so guest egress survives daemon restarts exactly like the VMM and
// the namespace holder do. Topology inside the VM netns:
//
//	guest eth0 --- tap0 (VMM) ==kernel forward + nft filter/masquerade==> up0 (TAP) --- this stack --- host sockets
//
// The stack exits when the namespace holder exits (watched with a pidfd):
// its own open TAP fd would otherwise keep the namespace alive forever.
//
// Policy is enforced here at connect time with the same semantics as the
// netns nftables ruleset (host addresses always denied, then explicit
// allows, then the deny list, then the internet), so it holds even where nft
// is missing. DNS (UDP/TCP port 53, any destination) is answered through the
// host's resolvers, message by message. Only IPv4 TCP and UDP are carried;
// ICMP is not.
//
// On top of that policy the VM's egress mode applies (Config.Mode): off
// carries nothing (DNS included); allowlist answers DNS only for allowed
// names (NXDOMAIN otherwise), pins the answered addresses for their TTL
// (dnsallow) before the guest sees the answer, and admits only pinned
// addresses and allowlisted CIDRs. SetPolicy changes the mode live over
// Config.Control and closes guest flows the new policy denies.
package userspace

import (
	"net/netip"
)

// Policy is the destination filter (network.EgressPolicy semantics).
type Policy struct {
	Deny      []netip.Prefix `json:"deny"`
	Allow     []netip.Prefix `json:"allow,omitempty"`
	HostAddrs []netip.Prefix `json:"hostAddrs,omitempty"`
	// Services are operator-permitted LAN services (network.Service).
	Services []Service `json:"services,omitempty"`
}

// Service is one permitted LAN service: proto ("tcp"/"udp"), address, port.
type Service struct {
	Proto string     `json:"proto"`
	Addr  netip.Addr `json:"addr"`
	Port  uint16     `json:"port"`
}

// Allows reports whether a guest flow over proto ("tcp"/"udp") may reach
// dst: host addresses never, LAN services and explicit allows always, the
// deny list never, everything else yes.
func (p Policy) Allows(proto string, dst netip.AddrPort) bool {
	a := dst.Addr().Unmap()
	if !a.IsValid() || a.IsUnspecified() {
		return false
	}
	if contains(p.HostAddrs, a) {
		return false
	}
	for _, s := range p.Services {
		if s.Proto == proto && s.Addr == a && s.Port == dst.Port() {
			return true
		}
	}
	if contains(p.Allow, a) {
		return true
	}
	if v4, ok := embeddedV4(a); ok && (contains(p.HostAddrs, v4) || contains(p.Deny, v4)) {
		return false // NAT64 / 6to4 reaching a denied IPv4 (SEC-9)
	}
	return !contains(p.Deny, a)
}

var (
	nat64  = netip.MustParsePrefix("64:ff9b::/96")
	sixto4 = netip.MustParsePrefix("2002::/16")
)

// embeddedV4 is the IPv4 address a NAT64 or 6to4 address reaches.
func embeddedV4(a netip.Addr) (netip.Addr, bool) {
	b := a.As16()
	switch {
	case !a.Is6():
		return netip.Addr{}, false
	case nat64.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case sixto4.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

func contains(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Egress modes (domain.EgressMode).
const (
	ModeOff       = "off"
	ModeFiltered  = "filtered"
	ModeAllowlist = "allowlist"
)

// Config configures one egress stack. It travels as JSON on the command
// line of `davit netstack-egress`.
type Config struct {
	// Gateway is the stack's own address on the uplink (the netns default
	// route points at it); Peer is the netns side of the uplink.
	Gateway netip.Prefix `json:"gateway"`
	Peer    netip.Addr   `json:"peer"`
	MTU     int          `json:"mtu"`
	Policy  Policy       `json:"policy"`
	// Resolvers are host DNS servers (host:port); empty reads the host's
	// /etc/resolv.conf at start.
	Resolvers []string `json:"resolvers,omitempty"`
	// Mode is the VM egress mode: "off", "filtered" or "allowlist"
	// (domain.EgressMode); empty means filtered.
	Mode string `json:"mode,omitempty"`
	// Allow is the normalized allowlist (domain.Egress.Allow) for allowlist
	// mode: domain names and IPv4 CIDRs.
	Allow []string `json:"allow,omitempty"`
	// Control is the abstract unix socket name the stack listens on for
	// live policy changes (SetPolicy); only the same UID may use it.
	Control string `json:"control,omitempty"`
	// Holder is the VM netns holder process; the stack exits when it does.
	// (An open TAP fd keeps its network namespace alive, so the stack must
	// not wait for the TAP to disappear.)
	Holder Holder `json:"holder"`
}

// Holder identifies a process by PID and start time (/proc/<pid>/stat
// field 22), so a reused PID is never mistaken for it.
type Holder struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"startTime"`
}

// Uplink addressing inside every VM netns (slirp-style 10.0.2.0/24; the
// guest never sees it, and the deny list covers it).
var (
	DefaultGateway = netip.MustParsePrefix("10.0.2.2/24")
	DefaultPeer    = netip.MustParsePrefix("10.0.2.15/24")
)

// File descriptors of `davit netstack-egress` (os/exec ExtraFiles start
// at 3) and `davit netns-tapfd`.
const (
	FDTap   = 3
	FDReady = 4
	FDSock  = 3
)

// Subcommands of the davit binary that this package implements.
const (
	CmdEgress = "netstack-egress"
	CmdTapFD  = "netns-tapfd"
)
