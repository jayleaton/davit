package domain

import (
	"net/netip"
	"slices"
	"strings"
)

// EgressMode is a VM's internet egress policy. Guests are untrusted, so the
// mode is chosen by the owner, not the guest.
type EgressMode string

const (
	// EgressOff blocks all guest traffic leaving the VM (DNS included).
	EgressOff EgressMode = "off"
	// EgressFiltered allows the public internet; private/LAN, host,
	// link-local/metadata and CGNAT/tailnet ranges stay blocked. Default.
	EgressFiltered EgressMode = "filtered"
	// EgressAllowlist allows only Allow entries (on top of the filtered
	// rules): domain names, resolved through the built-in DNS resolver which
	// pins the answers' addresses for their TTL, and CIDRs. DNS queries for
	// other names are answered NXDOMAIN.
	EgressAllowlist EgressMode = "allowlist"
)

// MaxEgressAllow bounds Egress.Allow.
const MaxEgressAllow = 256

// Egress is a VM's egress policy.
type Egress struct {
	Mode EgressMode `json:"mode"`
	// Allow lists domain names ("github.com" also matches every subdomain;
	// "*.example.com" only subdomains) and IPv4 CIDRs/addresses. Only used in
	// allowlist mode; CIDRs never reach ranges the filtered mode blocks.
	Allow []string `json:"allow,omitempty"`
}

// DefaultEgress is the built-in default (config can override it per host).
func DefaultEgress() Egress { return Egress{Mode: EgressFiltered} }

// Valid reports whether m is a known mode.
func (m EgressMode) Valid() bool {
	return m == EgressOff || m == EgressFiltered || m == EgressAllowlist
}

// Normalize validates e and returns it canonicalized: lowercase names
// without a trailing dot, CIDRs masked, duplicates removed, sorted. Allow
// is dropped unless the mode is allowlist.
func (e Egress) Normalize() (Egress, error) {
	if !e.Mode.Valid() {
		return Egress{}, Errorf(CodeInvalidArgument, "egress mode %q must be off, filtered or allowlist", e.Mode).WithDetail("field", "egress.mode")
	}
	if e.Mode != EgressAllowlist {
		if len(e.Allow) > 0 {
			return Egress{}, Errorf(CodeInvalidArgument, "egress allow entries need mode allowlist").WithDetail("field", "egress.allow")
		}
		return Egress{Mode: e.Mode}, nil
	}
	if len(e.Allow) > MaxEgressAllow {
		return Egress{}, Errorf(CodeInvalidArgument, "at most %d egress allow entries", MaxEgressAllow).WithDetail("field", "egress.allow")
	}
	out := Egress{Mode: e.Mode, Allow: []string{}}
	for _, a := range e.Allow {
		n, err := NormalizeAllowEntry(a)
		if err != nil {
			return Egress{}, err
		}
		out.Allow = append(out.Allow, n)
	}
	slices.Sort(out.Allow)
	out.Allow = slices.Compact(out.Allow)
	return out, nil
}

// NormalizeAllowEntry canonicalizes one allow entry: an IPv4 address or
// CIDR (masked, always with a prefix length), or a domain name, optionally
// with a leading "*." wildcard label.
func NormalizeAllowEntry(a string) (string, error) {
	bad := func(why string) error {
		return Errorf(CodeInvalidArgument, "egress allow entry %q: %s", a, why).WithDetail("field", "egress.allow")
	}
	s := strings.ToLower(strings.TrimSpace(a))
	if s == "" {
		return "", bad("empty")
	}
	if p, err := netip.ParsePrefix(s); err == nil {
		if !p.Addr().Is4() {
			return "", bad("only IPv4 CIDRs (guest egress is IPv4)")
		}
		return p.Masked().String(), nil
	}
	if ip, err := netip.ParseAddr(s); err == nil {
		if !ip.Is4() {
			return "", bad("only IPv4 addresses (guest egress is IPv4)")
		}
		return netip.PrefixFrom(ip, 32).String(), nil
	}
	s = strings.TrimSuffix(s, ".")
	name := strings.TrimPrefix(s, "*.")
	if len(name) > 253 || !strings.Contains(name, ".") {
		return "", bad("want a domain name with at least two labels, or an IPv4 CIDR")
	}
	for _, l := range strings.Split(name, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", bad("invalid domain label")
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return "", bad("invalid character in domain name")
			}
		}
	}
	return s, nil
}
