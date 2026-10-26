// Package privileged is the boundary of the narrow root network helper
// (SPEC §5, §7). The helper accepts only the typed requests below, never
// shell strings, validates them, and turns them into a fixed plan of argv
// commands (ip, sysctl, nft) whose text is produced by pure functions here.
//
// This build is render-only: the Executor interface is how a real root
// helper would run a plan, and DryRun records plans for tests and docs.
// Nothing in this package invokes sudo or executes commands itself.
//
// Host layout per VM (index i from the 10.200.0.0/16 uplink pool, one /30
// each): a named netns "slw-<id>" holding tap0 (172.16.0.1/30, guest
// 172.16.0.2) and the veth end "up0" (10.200.x.(4i+2)); the host veth end
// "slw<id>h" (10.200.x.(4i+1)). <id> is network.ShortID(vmID, generation).
// The interface name prefix "slw" is reserved for davit on the host.
package privileged

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"davit/internal/domain"
	"davit/internal/network"
	"davit/internal/network/netnsdns"
)

// UplinkPool is the address pool for host<->netns veth /30 links.
var UplinkPool = netip.MustParsePrefix("10.200.0.0/16")

// MaxIndex is the largest per-VM pool index; index 0 is reserved so the zero
// value is never valid. 16384 /30 blocks fit in the /16.
const MaxIndex = 1<<(32-16-2) - 1

// HostTable is the host nftables table owned by the helper.
const HostTable = "davit"

// IfPrefix is the reserved host interface name prefix of VM veths.
const IfPrefix = "slw"

// Request is one typed helper request: EnsureHost, CreateVMNet,
// SetVMEgress or DestroyVMNet.
type Request interface{ isRequest() }

// EnsureHost prepares host-wide state once (idempotent): IPv4 forwarding and
// the host nftables table with NAT out of Uplink.
type EnsureHost struct {
	Uplink string `json:"uplink"`
}

// CreateVMNet provisions the netns, veth, TAP and rules for one VM run.
type CreateVMNet struct {
	VMID       domain.VMID `json:"vmId"`
	Generation int64       `json:"generation"`
	Index      int         `json:"index"` // pool slot, 1..MaxIndex, unique per live VM
	// TapOwner, if nonzero, is the UID allowed to open tap0 (the jailer's
	// runtime UID); zero leaves the TAP root-only.
	TapOwner uint32 `json:"tapOwner,omitempty"`
	// Egress is the VM's egress policy; the zero value means filtered.
	Egress domain.Egress `json:"egress"`
}

// SetVMEgress replaces the egress policy of a live VM network.
type SetVMEgress struct {
	VMID       domain.VMID   `json:"vmId"`
	Generation int64         `json:"generation"`
	Index      int           `json:"index"`
	Egress     domain.Egress `json:"egress"`
}

// DestroyVMNet removes exactly what CreateVMNet made for the same
// (VMID, Generation, Index). It is idempotent.
type DestroyVMNet struct {
	VMID       domain.VMID `json:"vmId"`
	Generation int64       `json:"generation"`
	Index      int         `json:"index"`
}

func (EnsureHost) isRequest()   {}
func (CreateVMNet) isRequest()  {}
func (SetVMEgress) isRequest()  {}
func (DestroyVMNet) isRequest() {}

// Command is one argv the helper runs, optionally with stdin. IgnoreError
// marks cleanup steps whose target may already be gone. Daemon marks a
// long-running process (the allowlist resolver): the helper starts it in
// the background once it is ready, supervises it, and stops the VM's
// daemons before running its SetVMEgress or DestroyVMNet plan.
type Command struct {
	Argv        []string `json:"argv"`
	Stdin       string   `json:"stdin,omitempty"`
	IgnoreError bool     `json:"ignoreError,omitempty"`
	Daemon      bool     `json:"daemon,omitempty"`
}

// String renders the command for logs and docs (stdin is summarized).
func (c Command) String() string {
	s := strings.Join(c.Argv, " ")
	if c.Stdin != "" {
		s += " <<ruleset"
	}
	if c.Daemon {
		s += " &"
	}
	return s
}

// DavitExe is the davit binary the helper runs the resolver from.
var DavitExe = "davit"

// egressOf validates a request's policy; the zero value means filtered.
func egressOf(e domain.Egress) (domain.Egress, error) {
	if e.Mode == "" && len(e.Allow) == 0 {
		return domain.DefaultEgress(), nil
	}
	n, err := e.Normalize()
	if err != nil {
		return domain.Egress{}, fmt.Errorf("%w: %v", network.ErrInvalid, err)
	}
	return n, nil
}

// egressPlan installs the guest table for policy inside netns (atomically
// replacing any previous one) and, in allowlist mode, starts the in-netns
// resolver. Upstream is netnsdns.Fallback: the host table denies LAN
// resolvers to VM netns traffic anyway.
func egressPlan(netns string, cfg network.Config, policy domain.Egress, replace bool) []Command {
	ruleset := network.RenderGuestRuleset(cfg, policy, false)
	if replace {
		ruleset = network.RenderGuestReplace(cfg, policy, false)
	}
	plan := []Command{{Argv: []string{"ip", "netns", "exec", netns, "nft", "-f", "-"}, Stdin: ruleset}}
	if policy.Mode == domain.EgressAllowlist {
		rc, _ := json.Marshal(netnsdns.Config{
			Listen: netip.AddrPortFrom(cfg.TapIP, 53), Upstreams: []netip.AddrPort{netnsdns.Fallback},
			Allow: policy.Allow, Table: network.GuestTable, Set: network.PinnedSet,
		})
		plan = append(plan, Command{Argv: []string{"ip", "netns", "exec", netns, DavitExe, netnsdns.Cmd, string(rc)}, Daemon: true})
	}
	return plan
}

// checkConfig validates cfg for this helper: network.Config.Validate, and
// no guest subnet may overlap the uplink pool (the netns routes those back
// to the guest).
func checkConfig(cfg network.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	for _, sn := range cfg.GuestSubnets {
		if sn.Overlaps(UplinkPool) {
			return fmt.Errorf("%w: guest subnet %s overlaps the uplink pool %s", network.ErrInvalid, sn, UplinkPool)
		}
	}
	return nil
}

// Names are the host-visible names for one VM run.
type Names struct {
	NetNS    string // ip netns name
	HostVeth string // host end of the veth pair
}

// NamesFor derives names from (vmID, generation); see network.ShortID.
func NamesFor(vmID domain.VMID, generation int64) Names {
	id := network.ShortID(vmID, generation)
	return Names{NetNS: IfPrefix + "-" + id, HostVeth: IfPrefix + id + "h"}
}

// UplinkAddrs returns the host and netns ends of the /30 for index.
func UplinkAddrs(index int) (host, ns netip.Prefix, err error) {
	if index < 1 || index > MaxIndex {
		return host, ns, fmt.Errorf("%w: index %d outside 1..%d", network.ErrInvalid, index, MaxIndex)
	}
	base := UplinkPool.Addr().As4()
	off := uint32(index) * 4
	v := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3]) + off
	at := func(n uint32) netip.Addr {
		return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	}
	return netip.PrefixFrom(at(v+1), 30), netip.PrefixFrom(at(v+2), 30), nil
}

func validateKey(vmID domain.VMID, generation int64, index int) error {
	if err := network.ValidateVMID(vmID); err != nil {
		return err
	}
	if generation < 1 {
		return fmt.Errorf("%w: generation %d must be >= 1", network.ErrInvalid, generation)
	}
	_, _, err := UplinkAddrs(index)
	return err
}

func validateUplink(uplink string) error {
	if err := network.ValidateIfName(uplink); err != nil {
		return err
	}
	if strings.HasPrefix(uplink, IfPrefix) {
		return fmt.Errorf("%w: uplink %q uses the reserved prefix %q", network.ErrInvalid, uplink, IfPrefix)
	}
	return nil
}

// PlanEnsureHost returns the host setup commands.
func PlanEnsureHost(req EnsureHost, policy network.EgressPolicy) ([]Command, error) {
	if err := validateUplink(req.Uplink); err != nil {
		return nil, err
	}
	return []Command{
		{Argv: []string{"sysctl", "-q", "-w", "net.ipv4.ip_forward=1"}},
		{Argv: []string{"nft", "-f", "-"}, Stdin: RenderHostRuleset(req.Uplink, policy)},
	}, nil
}

// PlanCreate returns the commands that provision one VM network. cfg gives
// the guest-side constants and egress policy; cfg.Uplink names the veth end
// inside the netns.
func PlanCreate(req CreateVMNet, cfg network.Config) ([]Command, error) {
	if err := validateKey(req.VMID, req.Generation, req.Index); err != nil {
		return nil, err
	}
	if err := checkConfig(cfg); err != nil {
		return nil, err
	}
	policy, err := egressOf(req.Egress)
	if err != nil {
		return nil, err
	}
	n := NamesFor(req.VMID, req.Generation)
	host, ns, _ := UplinkAddrs(req.Index)
	ipns := func(args ...string) Command {
		return Command{Argv: append([]string{"ip", "-n", n.NetNS}, args...)}
	}
	tap := []string{"tuntap", "add", "dev", cfg.TapName, "mode", "tap"}
	if req.TapOwner != 0 {
		tap = append(tap, "user", strconv.FormatUint(uint64(req.TapOwner), 10))
	}
	plan := []Command{
		{Argv: []string{"ip", "netns", "add", n.NetNS}},
		{Argv: []string{"ip", "link", "add", n.HostVeth, "type", "veth", "peer", "name", cfg.Uplink, "netns", n.NetNS}},
		{Argv: []string{"ip", "addr", "add", host.String(), "dev", n.HostVeth}},
		{Argv: []string{"ip", "link", "set", "dev", n.HostVeth, "up"}},
		ipns("link", "set", "dev", "lo", "up"),
		ipns("addr", "add", ns.String(), "dev", cfg.Uplink),
		ipns("link", "set", "dev", cfg.Uplink, "up"),
		ipns("route", "add", "default", "via", host.Addr().String(), "dev", cfg.Uplink),
		ipns(tap...),
		ipns("addr", "add", netip.PrefixFrom(cfg.TapIP, cfg.Prefix).String(), "dev", cfg.TapName),
		ipns("link", "set", "dev", cfg.TapName, "mtu", strconv.Itoa(cfg.MTU), "up"),
		{Argv: []string{"ip", "netns", "exec", n.NetNS, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1"}},
	}
	for _, s := range cfg.GuestSubnets {
		plan = append(plan, ipns("route", "add", s.String(), "via", cfg.GuestIP.String(), "dev", cfg.TapName))
	}
	plan = append(plan, egressPlan(n.NetNS, cfg, policy, false)...)
	// Registering the veth last opens the host forward path only once the
	// netns ruleset is in place; unregistered slw* veths are dropped.
	return append(plan, Command{Argv: []string{"nft", "-f", "-"}, Stdin: fmt.Sprintf("add element inet %s vm_src { %q . %s }\n", HostTable, n.HostVeth, ns.Addr())}), nil
}

// PlanSetEgress returns the commands that switch a live VM network to
// req.Egress: the guest table is replaced in one nft transaction and, in
// allowlist mode, a resolver with the new list is started. The helper stops
// the VM's previous resolver (a Daemon of an earlier plan) first, so pins
// start empty under the new policy. On error the helper re-runs the plan of
// the previous policy.
func PlanSetEgress(req SetVMEgress, cfg network.Config) ([]Command, error) {
	if err := validateKey(req.VMID, req.Generation, req.Index); err != nil {
		return nil, err
	}
	if err := checkConfig(cfg); err != nil {
		return nil, err
	}
	policy, err := egressOf(req.Egress)
	if err != nil {
		return nil, err
	}
	return egressPlan(NamesFor(req.VMID, req.Generation).NetNS, cfg, policy, true), nil
}

// PlanDestroy returns the idempotent cleanup commands for one VM network.
// Deleting the netns also deletes tap0 and both veth ends once no process
// (the VMM) holds the namespace; the explicit link delete covers a netns
// that vanished earlier. Run it after the VMM has exited.
func PlanDestroy(req DestroyVMNet) ([]Command, error) {
	if err := validateKey(req.VMID, req.Generation, req.Index); err != nil {
		return nil, err
	}
	n := NamesFor(req.VMID, req.Generation)
	_, ns, _ := UplinkAddrs(req.Index)
	return []Command{
		{Argv: []string{"nft", "-f", "-"}, Stdin: fmt.Sprintf("delete element inet %s vm_src { %q . %s }\n", HostTable, n.HostVeth, ns.Addr()), IgnoreError: true},
		{Argv: []string{"ip", "netns", "del", n.NetNS}, IgnoreError: true},
		{Argv: []string{"ip", "link", "del", "dev", n.HostVeth}, IgnoreError: true},
	}, nil
}

// RenderHostRuleset returns the `nft -f -` input for the host table. It is
// written as incremental commands so re-running it (EnsureHost on every
// helper start) atomically replaces chains and policy sets while keeping the
// per-VM vm_src elements of running VMs.
//
// Forward policy for traffic from VM veths (slw*): established/related
// accept; source must be the (veth, uplink address) pair registered for that
// VM (anti-spoofing, and unregistered VMs are dropped); only the uplink may
// be the output (no VM<->VM, no tailscale0/docker0); then host addresses
// drop, LAN services and explicit allows accept, the deny list (plus the
// whole uplink pool) drops, the internet is accepted. New connections into VM veths are
// dropped. VM->host input is dropped: the host offers no DHCP/DNS to VMs
// (the guest is statically addressed and uses public resolvers).
func RenderHostRuleset(uplink string, policy network.EgressPolicy) string {
	t := "inet " + HostTable
	deny := append(append([]netip.Prefix(nil), policy.Deny...), UplinkPool)
	var b strings.Builder
	fmt.Fprintf(&b, "add table %s\n", t)
	fmt.Fprintf(&b, "add set %s vm_src { type ifname . ipv4_addr; }\n", t)
	fmt.Fprintf(&b, "add set %s %s { type ipv4_addr . inet_proto . inet_service; }\n", t, network.ServiceSet)
	fmt.Fprintf(&b, "flush set %s %s\n", t, network.ServiceSet)
	if len(policy.Services) > 0 {
		fmt.Fprintf(&b, "add element %s %s { %s }\n", t, network.ServiceSet, strings.Join(network.ServiceElements(policy.Services), ", "))
	}
	for _, s := range []struct {
		name string
		list []netip.Prefix
	}{{"host", policy.HostAddrs}, {"allow", policy.Allow}, {"deny", deny}} {
		v4, v6 := network.SplitFamilies(s.list)
		for _, fam := range []struct {
			suffix, typ string
			list        []netip.Prefix
		}{{"4", "ipv4_addr", v4}, {"6", "ipv6_addr", v6}} {
			name := s.name + fam.suffix
			fmt.Fprintf(&b, "add set %s %s { type %s; flags interval; auto-merge; }\n", t, name, fam.typ)
			fmt.Fprintf(&b, "flush set %s %s\n", t, name)
			if len(fam.list) > 0 {
				elems := make([]string, len(fam.list))
				for i, p := range fam.list {
					elems[i] = p.Masked().String()
				}
				fmt.Fprintf(&b, "add element %s %s { %s }\n", t, name, strings.Join(elems, ", "))
			}
		}
	}
	wild := strconv.Quote(IfPrefix + "*")
	chains := []struct {
		name, hook string
		rules      []string
	}{
		// vm_out first: a jump target must exist before the rule using it.
		{"vm_out", "", append([]string{
			"ct state established,related accept",
			"ct state invalid drop",
			"iifname . ip saddr != @vm_src drop",
			"oifname != " + strconv.Quote(uplink) + " drop",
		}, append(network.PolicyRules(), "accept")...)},
		{"forward", "type filter hook forward priority filter; policy accept;", []string{
			"iifname " + wild + " jump vm_out",
			"oifname " + wild + " ct state established,related accept",
			"oifname " + wild + " drop",
		}},
		{"input", "type filter hook input priority filter; policy accept;", []string{
			"iifname " + wild + " drop",
		}},
		{"postrouting", "type nat hook postrouting priority srcnat; policy accept;", []string{
			fmt.Sprintf("oifname %q ip saddr %s masquerade", uplink, UplinkPool),
		}},
	}
	for _, c := range chains {
		if c.hook != "" {
			fmt.Fprintf(&b, "add chain %s %s { %s }\n", t, c.name, c.hook)
		} else {
			fmt.Fprintf(&b, "add chain %s %s\n", t, c.name)
		}
		fmt.Fprintf(&b, "flush chain %s %s\n", t, c.name)
		for _, r := range c.rules {
			fmt.Fprintf(&b, "add rule %s %s %s\n", t, c.name, r)
		}
	}
	return b.String()
}

// Executor runs one planned command as root. A real implementation lives
// in the root helper process; it must exec Argv directly (no shell).
type Executor interface {
	Run(ctx context.Context, c Command) error
}

// DryRun records commands instead of running them.
type DryRun struct {
	Commands []Command
}

// Run records c.
func (d *DryRun) Run(_ context.Context, c Command) error {
	d.Commands = append(d.Commands, c)
	return nil
}

// Helper validates typed requests and runs their plans through an Executor.
type Helper struct {
	cfg  network.Config
	exec Executor
}

// NewHelper returns a helper for the guest config/policy cfg. cfg comes from
// the helper's own (root-owned) configuration, not from requests.
func NewHelper(cfg network.Config, exec Executor) (*Helper, error) {
	if err := checkConfig(cfg); err != nil {
		return nil, err
	}
	return &Helper{cfg: cfg, exec: exec}, nil
}

// Do dispatches one typed request. Create returns the network handle for
// the runtime; the other requests return a zero Handle.
func (h *Helper) Do(ctx context.Context, req Request) (network.Handle, error) {
	switch r := req.(type) {
	case EnsureHost:
		plan, err := PlanEnsureHost(r, h.cfg.Egress)
		if err != nil {
			return network.Handle{}, err
		}
		return network.Handle{}, h.run(ctx, plan)
	case CreateVMNet:
		return h.create(ctx, r)
	case SetVMEgress:
		plan, err := PlanSetEgress(r, h.cfg)
		if err != nil {
			return network.Handle{}, err
		}
		return network.Handle{}, h.run(ctx, plan)
	case DestroyVMNet:
		plan, err := PlanDestroy(r)
		if err != nil {
			return network.Handle{}, err
		}
		return network.Handle{}, h.run(ctx, plan)
	default:
		return network.Handle{}, fmt.Errorf("%w: unknown request %T", network.ErrInvalid, req)
	}
}

func (h *Helper) create(ctx context.Context, r CreateVMNet) (network.Handle, error) {
	plan, err := PlanCreate(r, h.cfg)
	if err != nil {
		return network.Handle{}, err
	}
	if err := h.run(ctx, plan); err != nil {
		undo, _ := PlanDestroy(DestroyVMNet{VMID: r.VMID, Generation: r.Generation, Index: r.Index})
		return network.Handle{}, errors.Join(err, h.run(context.WithoutCancel(ctx), undo))
	}
	policy, _ := egressOf(r.Egress) // validated by PlanCreate
	return network.Handle{
		VMID: r.VMID, Generation: r.Generation, Mode: network.ModePrivileged, Policy: policy,
		NetNSPath: "/run/netns/" + NamesFor(r.VMID, r.Generation).NetNS,
		TapName:   h.cfg.TapName, GuestIP: h.cfg.GuestIP, TapIP: h.cfg.TapIP,
		Prefix: h.cfg.Prefix, GuestMAC: h.cfg.GuestMAC, MTU: h.cfg.MTU,
		Egress: network.Egress{Enabled: true, Backend: network.BackendVeth, Filtered: true},
	}, nil
}

func (h *Helper) run(ctx context.Context, plan []Command) error {
	for _, c := range plan {
		if err := h.exec.Run(ctx, c); err != nil && !c.IgnoreError {
			return fmt.Errorf("privileged: %s: %w", c, err)
		}
	}
	return nil
}
