// Package network provisions per-VM guest networking (SPEC §5 "Images and
// networking", §7 security).
//
// Every VM gets its own network namespace containing an identical TAP device
// and an identical guest address (172.16.0.2/30, TAP side 172.16.0.1), so a
// restored fork needs no re-addressing. Host<->guest control traffic (SSH,
// exec, agent) uses vsock and never depends on this package: networking here
// exists only for guest egress, which is filtered by a deny list (private,
// link-local, metadata, CGNAT/tailnet, loopback, multicast, host addresses).
//
// Providers:
//   - Rootless (Linux): a user+net namespace held by a small holder process,
//     created without root. Egress is attached through pasta or slirp4netns
//     when present, else through the built-in userspace stack
//     (internal/network/userspace).
//   - Privileged (subpackage privileged): the typed request model and the
//     exact ip/nft commands a narrow root helper runs. Render-only here.
//   - None: no guest network at all (every OS).
package network

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"

	"davit/internal/domain"
)

// Mode selects how a VM's network is provisioned.
type Mode string

const (
	// ModeRootless uses an unprivileged user+net namespace (Linux).
	ModeRootless Mode = "rootless"
	// ModePrivileged uses a root-owned named netns, veth uplink and host
	// nftables NAT, provisioned by the privileged helper.
	ModePrivileged Mode = "privileged"
	// ModeNone gives the guest no network device; vsock still works.
	ModeNone Mode = "none"
)

// Egress backend names reported in Egress.Backend and Capabilities.
const (
	BackendPasta       = "pasta"
	BackendSlirp4netns = "slirp4netns"
	BackendVeth        = "veth-nat" // privileged mode
	// BackendUserspace is the built-in rootless backend: a gVisor TCP/IP
	// stack in a `davit netstack-egress` process (package userspace). It
	// needs nothing beyond what rootless mode needs, so it is the fallback
	// when pasta and slirp4netns are missing.
	BackendUserspace = "userspace"
)

// ErrUnsupported is returned by providers that cannot run on this OS or host.
var ErrUnsupported = errors.New("network: unsupported on this host")

// ErrInvalid wraps every input-validation failure.
var ErrInvalid = errors.New("network: invalid input")

// Defaults shared by every VM. They are constants on purpose: forks restored
// from one snapshot must find the same device name, address and MAC.
var (
	DefaultTapName  = "tap0"
	DefaultUplink   = "up0" // egress-side interface inside the VM netns
	DefaultGuestIP  = netip.MustParseAddr("172.16.0.2")
	DefaultTapIP    = netip.MustParseAddr("172.16.0.1")
	DefaultPrefix   = 30
	DefaultGuestMAC = "06:00:AC:10:00:02" // locally administered, unicast
	DefaultMTU      = 1500
)

// DefaultDeny is the destination deny list applied to guest egress unless a
// prefix is listed in Config.Allow. It covers RFC 1918 private ranges, CGNAT
// 100.64.0.0/10 (Tailscale tailnet addresses), link-local including cloud
// metadata 169.254.169.254, loopback, "this network", multicast, reserved,
// IPv6 ULA/link-local/loopback/multicast and IPv4-mapped IPv6.
func DefaultDeny() []netip.Prefix {
	return mustPrefixes(
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.0.0.0/24", // IETF protocol assignments
		"192.168.0.0/16",
		"198.18.0.0/15", // benchmarking
		"224.0.0.0/4",
		"240.0.0.0/4",
		"::/128",
		"::1/128",
		"::ffff:0:0/96",
		"64:ff9b::/96",   // NAT64 (embeds an IPv4)
		"64:ff9b:1::/48", // local-use NAT64
		"2001::/32",      // Teredo
		"2002::/16",      // 6to4 (embeds an IPv4)
		"fc00::/7",
		"fe80::/10",
		"ff00::/8",
	)
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// EgressPolicy is the destination filter for guest-originated traffic.
// Evaluation order: HostAddrs drop (never overridable), Services accept,
// Allow accept, Deny drop, everything else (the internet) accept.
type EgressPolicy struct {
	// Deny lists blocked destinations; DefaultDeny when built by DefaultConfig.
	Deny []netip.Prefix `json:"deny"`
	// Allow lists explicit admin exceptions to Deny: whole prefixes, every
	// port.
	Allow []netip.Prefix `json:"allow,omitempty"`
	// Services are operator-permitted LAN services: one address, transport
	// and port each (ParseService). Everything else on the LAN stays denied.
	Services []Service `json:"services,omitempty"`
	// HostAddrs are host-management addresses (the host's own LAN/tailnet
	// IPs). They are always dropped, even if covered by Allow.
	HostAddrs []netip.Prefix `json:"hostAddrs,omitempty"`
}

// Config is the per-host network configuration. The guest-facing fields are
// identical for every VM.
type Config struct {
	// StateDir, when set (absolute path), is where the rootless provider
	// makes one directory per handle and runs that handle's helper processes
	// with it as cwd, so Reap can find processes leaked by a crashed daemon.
	// Empty keeps the old behaviour (no directories, nothing to reap).
	StateDir string `json:"stateDir,omitempty"`

	TapName  string       `json:"tapName"`
	Uplink   string       `json:"uplink"` // egress interface name inside the VM netns
	GuestIP  netip.Addr   `json:"guestIp"`
	TapIP    netip.Addr   `json:"tapIp"`
	Prefix   int          `json:"prefix"`
	GuestMAC string       `json:"guestMac"`
	MTU      int          `json:"mtu"`
	Egress   EgressPolicy `json:"egress"`
	// GuestSubnets are private IPv4 subnets the guest routes behind its own
	// address without masquerading them (a guest Docker bridge with
	// masquerading off, nested VMs). Traffic from them gets the same egress
	// policy as the guest's own; the VM netns routes them back via the guest.
	// Empty (the default): only the guest address may send, which covers
	// Docker's default bridge (it masquerades to the guest address).
	GuestSubnets []netip.Prefix `json:"guestSubnets,omitempty"`
	// TapServices are TCP ports on TapIP that the host serves to the guest
	// (e.g. the credential proxy at http://172.16.0.1:PORT). They are host
	// services, not egress: the guest table accepts them from the guest's
	// sources in every egress mode, off included, and nothing else from the
	// guest reaches the VM netns. The listening socket lives in the VM netns
	// (TapListener). Each port must be nonzero, unique and not 53 (guest
	// DNS).
	TapServices []uint16 `json:"tapServices,omitempty"`
}

// DefaultConfig returns the standard guest addressing and the default deny
// list. hostAddrs are added to EgressPolicy.HostAddrs.
func DefaultConfig(hostAddrs ...netip.Prefix) Config {
	return Config{
		TapName:  DefaultTapName,
		Uplink:   DefaultUplink,
		GuestIP:  DefaultGuestIP,
		TapIP:    DefaultTapIP,
		Prefix:   DefaultPrefix,
		GuestMAC: DefaultGuestMAC,
		MTU:      DefaultMTU,
		Egress:   EgressPolicy{Deny: DefaultDeny(), HostAddrs: append([]netip.Prefix(nil), hostAddrs...)},
	}
}

// Validate checks the config and returns an ErrInvalid-wrapped error.
func (c Config) Validate() error {
	if err := ValidateIfName(c.TapName); err != nil {
		return fmt.Errorf("tap name: %w", err)
	}
	if err := ValidateIfName(c.Uplink); err != nil {
		return fmt.Errorf("uplink: %w", err)
	}
	if c.TapName == c.Uplink {
		return fmt.Errorf("%w: tap and uplink share name %q", ErrInvalid, c.TapName)
	}
	if !c.GuestIP.Is4() || !c.TapIP.Is4() {
		return fmt.Errorf("%w: guest and tap addresses must be IPv4", ErrInvalid)
	}
	if c.Prefix < 8 || c.Prefix > 30 {
		return fmt.Errorf("%w: prefix /%d outside /8../30", ErrInvalid, c.Prefix)
	}
	p := netip.PrefixFrom(c.TapIP, c.Prefix).Masked()
	if c.GuestIP == c.TapIP || !p.Contains(c.GuestIP) {
		return fmt.Errorf("%w: guest %s and tap %s not distinct hosts of %s", ErrInvalid, c.GuestIP, c.TapIP, p)
	}
	mac, err := net.ParseMAC(c.GuestMAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 {
		return fmt.Errorf("%w: guest MAC %q must be a unicast 48-bit MAC", ErrInvalid, c.GuestMAC)
	}
	if c.StateDir != "" && !filepath.IsAbs(c.StateDir) {
		return fmt.Errorf("%w: state dir %q must be absolute", ErrInvalid, c.StateDir)
	}
	if c.MTU < 576 || c.MTU > 65520 {
		return fmt.Errorf("%w: MTU %d outside 576..65520", ErrInvalid, c.MTU)
	}
	for _, set := range [][]netip.Prefix{c.Egress.Deny, c.Egress.Allow, c.Egress.HostAddrs} {
		for _, pfx := range set {
			if !pfx.IsValid() {
				return fmt.Errorf("%w: invalid prefix in egress policy", ErrInvalid)
			}
		}
	}
	for _, s := range c.Egress.Services {
		if err := s.validate(); err != nil {
			return fmt.Errorf("%w: LAN service %s: %v", ErrInvalid, s, err)
		}
	}
	if err := c.validateTapServices(); err != nil {
		return err
	}
	return c.validateGuestSubnets()
}

func (c Config) validateTapServices() error {
	seen := make(map[uint16]bool, len(c.TapServices))
	for _, p := range c.TapServices {
		switch {
		case p == 0:
			return fmt.Errorf("%w: tap service port 0", ErrInvalid)
		case p == 53:
			return fmt.Errorf("%w: tap service port 53 is reserved for guest DNS", ErrInvalid)
		case seen[p]:
			return fmt.Errorf("%w: tap service port %d listed twice", ErrInvalid, p)
		}
		seen[p] = true
	}
	return nil
}

// privateV4 are the ranges a guest subnet must lie in.
var privateV4 = mustPrefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16")

func (c Config) validateGuestSubnets() error {
	link := netip.PrefixFrom(c.TapIP, c.Prefix).Masked()
	for _, s := range c.GuestSubnets {
		if !s.IsValid() || !s.Addr().Is4() || s != s.Masked() {
			return fmt.Errorf("%w: guest subnet %s must be a masked IPv4 prefix", ErrInvalid, s)
		}
		if !slices.ContainsFunc(privateV4, func(p netip.Prefix) bool { return p.Bits() <= s.Bits() && p.Contains(s.Addr()) }) {
			return fmt.Errorf("%w: guest subnet %s must be inside 10/8, 172.16/12 or 192.168/16", ErrInvalid, s)
		}
		for _, used := range []netip.Prefix{link, UserspaceUplink} {
			if s.Overlaps(used) {
				return fmt.Errorf("%w: guest subnet %s overlaps %s, used by the VM netns", ErrInvalid, s, used)
			}
		}
	}
	return nil
}

// UserspaceUplink is the uplink subnet inside every VM netns when the
// userspace backend serves egress (userspace.DefaultGateway).
var UserspaceUplink = netip.MustParsePrefix("10.0.2.0/24")

// Egress reports whether the guest can reach the internet and why not.
type Egress struct {
	Enabled bool   `json:"enabled"`
	Backend string `json:"backend,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Filtered is true when the deny list is enforced: by the netns
	// nftables ruleset, and/or by the userspace backend on every flow.
	Filtered bool `json:"filtered"`
}

// Process identifies a helper process without trusting the PID alone: the
// start time (clock ticks since boot, /proc/<pid>/stat field 22) must match.
type Process struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"startTime"`
}

// Handle describes one provisioned per-VM network. It round-trips through
// JSON so lifecycle can persist it and Destroy it after a daemon restart.
type Handle struct {
	VMID       domain.VMID `json:"vmId"`
	Generation int64       `json:"generation"`
	Mode       Mode        `json:"mode"`
	// NetNSPath is the path of the VM's network namespace (empty in ModeNone):
	// /proc/<holder>/ns/net (rootless) or /run/netns/<name> (privileged).
	NetNSPath string `json:"netnsPath,omitempty"`
	// UserNSPath is the owning user namespace (rootless only).
	UserNSPath string `json:"usernsPath,omitempty"`
	// Holder keeps the rootless namespaces alive.
	Holder *Process `json:"holder,omitempty"`
	// Policy is the egress policy in force (domain.Egress).
	Policy domain.Egress `json:"policy"`
	// EgressControl is the userspace stack's control socket (abstract unix
	// socket name), if that backend serves egress.
	EgressControl string `json:"egressControl,omitempty"`
	// DNSProc is the in-netns allowlist DNS resolver of the nft-only
	// backends, if one runs.
	DNSProc *Process `json:"dnsProc,omitempty"`
	// EgressProc is the pasta/slirp4netns process, if any.
	EgressProc *Process `json:"egressProc,omitempty"`
	// StateDir is the handle's own directory under Config.StateDir (rootless
	// with a state dir only); its helper processes run with it as cwd.
	StateDir string     `json:"stateDir,omitempty"`
	TapName  string     `json:"tapName,omitempty"`
	GuestIP  netip.Addr `json:"guestIp,omitzero"`
	TapIP    netip.Addr `json:"tapIp,omitzero"`
	Prefix   int        `json:"prefix,omitempty"`
	GuestMAC string     `json:"guestMac,omitempty"`
	MTU      int        `json:"mtu,omitempty"`
	Egress   Egress     `json:"egress"`
}

// ModeStatus reports whether a mode can be used here.
type ModeStatus struct {
	Mode   Mode   `json:"mode"`
	Usable bool   `json:"usable"`
	Reason string `json:"reason,omitempty"`
}

// BackendStatus reports whether an egress backend can be used here.
type BackendStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// Capabilities is the result of Provider.Probe.
type Capabilities struct {
	Modes  []ModeStatus    `json:"modes"`
	Egress []BackendStatus `json:"egress"`
	// Filter reports whether the egress deny list can be enforced.
	Filter BackendStatus `json:"filter"`
}

// Provider provisions per-VM networks.
type Provider interface {
	// Probe reports usable modes/backends and why the others are not.
	Probe(ctx context.Context) Capabilities
	// Create provisions the network for one VM run with egress policy
	// policy (domain.Egress, already normalized).
	Create(ctx context.Context, vmID domain.VMID, generation int64, policy domain.Egress) (Handle, error)
	// SetEgress applies a new egress policy to a live network and returns
	// the updated handle (persist it). On error the previous policy is
	// still in force.
	SetEgress(ctx context.Context, h Handle, policy domain.Egress) (Handle, error)
	// Enter returns the argv prefix that runs a command (the VMM) inside the
	// handle's namespaces, e.g. append(prefix, "firecracker", ...). An empty
	// prefix means "run directly". It fails if the namespace is gone.
	Enter(h Handle) ([]string, error)
	// Destroy releases everything Create made. It is idempotent.
	Destroy(ctx context.Context, h Handle) error
}

// Reaper is implemented by providers whose helper processes can outlive the
// daemon. Lifecycle type-asserts a Provider to it at daemon start.
type Reaper interface {
	// Reap kills helper processes the provider started that belong to no
	// handle in keep, waits for them to exit, removes their state and
	// returns how many processes it killed. Call it once at daemon start,
	// before any Create: it would race Creates in flight.
	Reap(ctx context.Context, keep []Handle) (int, error)
}

// TapListener is implemented by providers that can serve a host TCP
// service to one VM's guest at TapIP:port (a port listed in
// Config.TapServices). It is optional: callers type-assert a Provider to it
// like Reaper; None does not implement it.
type TapListener interface {
	// ListenTCP returns a listener bound to TapIP:port inside h's network
	// namespace, so only that VM's guest can connect to it, while the caller
	// accepts on it from its own namespace. Close it when the VM's network
	// is destroyed: an open listener keeps the namespace alive.
	ListenTCP(ctx context.Context, h Handle, port uint16) (net.Listener, error)
}

var vmIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// ValidateVMID accepts 1-63 chars of [a-z0-9_-] starting with [a-z0-9]
// (davit IDs look like "vm_k3..."). VM IDs end up in host-visible names,
// so anything else is rejected rather than escaped.
func ValidateVMID(vmID domain.VMID) error {
	if !vmIDRe.MatchString(string(vmID)) {
		return fmt.Errorf("%w: vm id %q must match %s", ErrInvalid, vmID, vmIDRe)
	}
	return nil
}

var ifNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`)

// ValidateIfName accepts Linux interface names of at most 15 bytes (IFNAMSIZ
// minus NUL) from a conservative character set.
func ValidateIfName(name string) error {
	if !ifNameRe.MatchString(name) {
		return fmt.Errorf("%w: interface name %q must match %s", ErrInvalid, name, ifNameRe)
	}
	return nil
}

// ShortID derives a deterministic 10-hex-char ID from (vmID, generation).
// Linux interface names are limited to 15 bytes, so host-visible names are
// built from this hash ("slw" + ShortID + suffix = 14 bytes) instead of the
// VM ID. 40 bits make accidental collisions negligible at host scale, and
// creating an interface or netns that already exists fails loudly rather
// than sharing state. Including the generation keeps a stale run's cleanup
// from touching its replacement.
func ShortID(vmID domain.VMID, generation int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d", vmID, generation))
	return hex.EncodeToString(sum[:5])
}

func validateKey(vmID domain.VMID, generation int64) error {
	if err := ValidateVMID(vmID); err != nil {
		return err
	}
	if generation < 1 {
		return fmt.Errorf("%w: generation %d must be >= 1", ErrInvalid, generation)
	}
	return nil
}

// EnterArgv is the pure argv prefix for running a command inside h's
// namespaces. Rootless: nsenter into the user namespace (where the caller is
// mapped to root and gains CAP_NET_ADMIN over the netns) and the netns;
// --preserve-credentials is required because setgroups is denied in a
// userns mapped by an unprivileged user. Privileged: nsenter into the named
// netns only (the caller is already root or the jailer).
func EnterArgv(h Handle) []string {
	switch h.Mode {
	case ModeRootless:
		return []string{"nsenter", "--preserve-credentials", "--user=" + h.UserNSPath, "--net=" + h.NetNSPath, "--"}
	case ModePrivileged:
		return []string{"nsenter", "--net=" + h.NetNSPath, "--"}
	default:
		return nil
	}
}

// None is the provider for ModeNone: no network device, vsock only.
type None struct{}

// Probe reports that ModeNone is always usable.
func (None) Probe(context.Context) Capabilities {
	return Capabilities{
		Modes:  []ModeStatus{{Mode: ModeNone, Usable: true}},
		Filter: BackendStatus{Name: "none", Reason: "no guest network"},
	}
}

// Create returns a handle with no namespace and egress disabled.
func (None) Create(_ context.Context, vmID domain.VMID, generation int64, policy domain.Egress) (Handle, error) {
	if err := validateKey(vmID, generation); err != nil {
		return Handle{}, err
	}
	return Handle{VMID: vmID, Generation: generation, Mode: ModeNone, Policy: policy,
		Egress: Egress{Reason: "network mode none; control plane over vsock unaffected"}}, nil
}

// SetEgress records the policy; there is no guest network to change.
func (None) SetEgress(_ context.Context, h Handle, policy domain.Egress) (Handle, error) {
	h.Policy = policy
	return h, nil
}

// Enter returns an empty prefix: the VMM runs in the caller's namespaces.
func (None) Enter(Handle) ([]string, error) { return nil, nil }

// Destroy is a no-op.
func (None) Destroy(context.Context, Handle) error { return nil }

// Reap is a no-op: ModeNone starts no processes.
func (None) Reap(context.Context, []Handle) (int, error) { return 0, nil }
