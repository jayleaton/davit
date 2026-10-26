package network

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"davit/internal/domain"
)

// GuestTable is the nftables table installed inside every VM netns.
const GuestTable = "davit_guest"

// Allowlist sets of the guest table (nft-only backends in allowlist mode):
// VMAllowSet holds the allowlist's CIDRs, PinnedSet the addresses the
// in-netns resolver pinned (elements with timeouts and the name as comment).
const (
	PinnedSet  = "pinned4"
	VMAllowSet = "vmallow4"
)

// allowMark is the conntrack mark of flows admitted under an allowlist
// ruleset. Only marked flows stay established across a switch to
// allowlist, so tightening the policy also cuts existing connections.
const allowMark = "0x00000001"

// RenderGuestRuleset returns the `nft -f -` input installed inside a VM's
// own network namespace (both modes) for egress policy policy. stackEnforces
// is true when the userspace backend serves egress: it answers DNS itself
// and enforces the allowlist on every flow.
//
// Per mode, for traffic forwarded from the guest TAP to the uplink:
//   - off: everything is dropped (DNS too), established flows included.
//   - filtered: the egress policy (host addresses drop, LAN services and
//     Config allow accept, deny list drop, internet accept). With stackEnforces, DNS
//     (udp/tcp 53) to any address is accepted first: the stack answers it.
//   - allowlist with stackEnforces: as filtered.
//   - allowlist otherwise: the destination must also be in @vmallow4 (the
//     allowlist's CIDRs) or @pinned4 (filled at runtime by the in-netns
//     resolver), and guest DNS to any address is DNAT'd to that resolver at
//     TapIP:53, the only input the guest may reach.
//
// Config.TapServices are host services, not egress: in every mode (off
// included) the input chain accepts guest TCP to TapIP on exactly those
// ports. The rule matches every packet of such a flow, so off mode needs no
// established accept for it.
//
// Guest->netns input is dropped otherwise and the guest address is
// masqueraded out of the uplink. The forward policy is drop, so a VM without
// an egress backend still has a closed, well-defined ruleset. Only the
// guest's own address and Config.GuestSubnets may leave (anti-spoofing;
// anything else would escape the masquerade), and both get the same policy. Guest egress is IPv4-only today; the IPv6 policy sets are
// rendered so enabling IPv6 later only removes the nfproto drop.
func RenderGuestRuleset(cfg Config, policy domain.Egress, stackEnforces bool) string {
	nftOnlyAllow := policy.Mode == domain.EgressAllowlist && !stackEnforces
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n", GuestTable)
	writePolicySets(&b, cfg.Egress)
	if nftOnlyAllow {
		var cidrs []netip.Prefix
		for _, a := range policy.Allow {
			if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Is4() {
				cidrs = append(cidrs, p)
			}
		}
		b.WriteString(FormatNftSet("\t", VMAllowSet, false, cidrs))
		fmt.Fprintf(&b, "\tset %s {\n\t\ttype ipv4_addr\n\t\tflags timeout\n\t}\n", PinnedSet)
	}
	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority filter; policy drop;\n")
	var rules []string
	guest := []string{
		fmt.Sprintf("iifname != %q drop", cfg.TapName),
		fmt.Sprintf("oifname != %q drop", cfg.Uplink),
		"meta nfproto ipv6 drop",
		fmt.Sprintf("ip saddr != %s drop", guestSources(cfg)),
	}
	switch {
	case policy.Mode == domain.EgressOff:
		rules = []string{fmt.Sprintf("iifname %q drop", cfg.TapName)}
	case nftOnlyAllow:
		rules = append([]string{
			"ct state invalid drop",
			fmt.Sprintf("iifname %q oifname %q ct state established,related accept", cfg.Uplink, cfg.TapName),
		}, guest...)
		rules = append(rules,
			"ct state established,related ct mark "+allowMark+" accept",
			"ip daddr @host4 drop", "ip6 daddr @host6 drop",
			fmt.Sprintf("ip daddr != @%s ip daddr != @%s drop", VMAllowSet, PinnedSet),
			strings.TrimSuffix(serviceRule, " accept")+" ct mark set "+allowMark+" accept",
			"ip daddr @allow4 ct mark set "+allowMark+" accept", "ip6 daddr @allow6 accept",
			"ip daddr @deny4 drop", "ip6 daddr @deny6 drop",
			"ct mark set "+allowMark+" accept")
	default:
		rules = append([]string{"ct state established,related accept", "ct state invalid drop"}, guest...)
		if stackEnforces {
			rules = append(rules, "udp dport 53 accept", "tcp dport 53 accept")
		}
		rules = append(append(rules, PolicyRules()...), "accept")
	}
	for _, r := range rules {
		b.WriteString("\t\t" + r + "\n")
	}
	b.WriteString("\t}\n")
	if nftOnlyAllow {
		fmt.Fprintf(&b, `	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		iifname %[1]q udp dport 53 dnat ip to %[2]s:53
		iifname %[1]q tcp dport 53 dnat ip to %[2]s:53
	}
`, cfg.TapName, cfg.TapIP)
	}
	fmt.Fprintf(&b, "\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n")
	if policy.Mode != domain.EgressOff {
		fmt.Fprintf(&b, "\t\tiifname %q ct state established,related accept\n", cfg.TapName)
	}
	if nftOnlyAllow {
		for _, proto := range []string{"udp", "tcp"} {
			fmt.Fprintf(&b, "\t\tiifname %q ip saddr %s ip daddr %s %s dport 53 accept\n", cfg.TapName, guestSources(cfg), cfg.TapIP, proto)
		}
	}
	if len(cfg.TapServices) > 0 {
		fmt.Fprintf(&b, "\t\tiifname %q ip saddr %s ip daddr %s tcp dport %s accept\n", cfg.TapName, guestSources(cfg), cfg.TapIP, tapPorts(cfg.TapServices))
	}
	fmt.Fprintf(&b, `		iifname %[1]q drop
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname %[2]q ip saddr %[3]s masquerade
	}
}
`, cfg.TapName, cfg.Uplink, guestSources(cfg))
	return b.String()
}

// RenderGuestReplace wraps RenderGuestRuleset so that one `nft -f -`
// transaction atomically replaces the guest table whether or not it exists
// (the first line creates it empty if missing). Packets never see a moment
// without a ruleset, and a failed load leaves the old table in force.
func RenderGuestReplace(cfg Config, policy domain.Egress, stackEnforces bool) string {
	return fmt.Sprintf("table inet %[1]s {}\ndelete table inet %[1]s\n", GuestTable) +
		RenderGuestRuleset(cfg, policy, stackEnforces)
}

// tapPorts renders Config.TapServices as an nft port match: one port bare,
// several as a sorted anonymous set.
func tapPorts(ports []uint16) string {
	ps := slices.Clone(ports)
	slices.Sort(ps)
	ss := make([]string, len(ps))
	for i, p := range ps {
		ss[i] = strconv.Itoa(int(p))
	}
	if len(ss) == 1 {
		return ss[0]
	}
	return "{ " + strings.Join(ss, ", ") + " }"
}

// guestSources is the guest-side source match: the guest address, or an
// anonymous set of it and Config.GuestSubnets.
func guestSources(cfg Config) string {
	if len(cfg.GuestSubnets) == 0 {
		return cfg.GuestIP.String()
	}
	srcs := []string{cfg.GuestIP.String()}
	for _, s := range cfg.GuestSubnets {
		srcs = append(srcs, s.Masked().String())
	}
	return "{ " + strings.Join(srcs, ", ") + " }"
}

// writePolicySets renders host4/host6, svc4, allow4/allow6, deny4/deny6
// inside a table block (one tab indent).
func writePolicySets(b *strings.Builder, p EgressPolicy) {
	for _, s := range []struct {
		name string
		list []netip.Prefix
	}{{"host", p.HostAddrs}, {"allow", p.Allow}, {"deny", p.Deny}} {
		if s.name == "allow" {
			b.WriteString(formatServiceSet("\t", p.Services))
		}
		v4, v6 := SplitFamilies(s.list)
		b.WriteString(FormatNftSet("\t", s.name+"4", false, v4))
		b.WriteString(FormatNftSet("\t", s.name+"6", true, v6))
	}
}

// PolicyRules returns the ordered destination checks shared by the guest and
// host tables: host addresses always drop, LAN services (svc4) and explicit
// allows accept, the deny list drops.
func PolicyRules() []string {
	return []string{
		"ip daddr @host4 drop", "ip6 daddr @host6 drop",
		serviceRule,
		"ip daddr @allow4 accept", "ip6 daddr @allow6 accept",
		"ip daddr @deny4 drop", "ip6 daddr @deny6 drop",
	}
}

// FormatNftSet renders one interval set declaration of IPv4 or IPv6
// prefixes. auto-merge lets user-supplied prefixes overlap. An empty set has
// no elements line (an empty `elements = { }` is a syntax error).
func FormatNftSet(indent, name string, ipv6 bool, prefixes []netip.Prefix) string {
	typ := "ipv4_addr"
	if ipv6 {
		typ = "ipv6_addr"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%sset %s {\n%s\ttype %s\n%s\tflags interval\n%s\tauto-merge\n", indent, name, indent, typ, indent, indent)
	if len(prefixes) > 0 {
		elems := make([]string, len(prefixes))
		for i, p := range prefixes {
			elems[i] = p.Masked().String()
		}
		fmt.Fprintf(&b, "%s\telements = { %s }\n", indent, strings.Join(elems, ", "))
	}
	fmt.Fprintf(&b, "%s}\n", indent)
	return b.String()
}

// SplitFamilies splits prefixes into IPv4 and IPv6 lists. IPv4-mapped IPv6
// prefixes stay IPv6 (they are matched as ip6 daddr).
func SplitFamilies(ps []netip.Prefix) (v4, v6 []netip.Prefix) {
	for _, p := range ps {
		if p.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	return v4, v6
}
