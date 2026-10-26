package network

import (
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Service is one operator-permitted LAN service: exactly one address, one
// transport and one port (EgressPolicy.Services). It is an exception to the
// deny list narrower than EgressPolicy.Allow, which opens a whole prefix on
// every port. Host addresses stay denied even when listed here.
type Service struct {
	Proto string     `json:"proto"` // "tcp" or "udp"
	Addr  netip.Addr `json:"addr"`
	Port  uint16     `json:"port"`
}

// String renders the service as ParseService reads it ("tcp/10.0.0.5:5432").
func (s Service) String() string {
	return s.Proto + "/" + netip.AddrPortFrom(s.Addr, s.Port).String()
}

// Matches reports whether a guest flow to dst over proto is this service.
func (s Service) Matches(proto string, dst netip.AddrPort) bool {
	return s.Proto == proto && s.Addr == dst.Addr().Unmap() && s.Port == dst.Port()
}

// notService are ranges a LAN service may never be in: they are not LAN
// services but the guest's own plumbing, the host, metadata or the tailnet
// (reach those through the gateway or a reverse connection instead).
var notService = mustPrefixes(
	"0.0.0.0/8",      // this network
	"100.64.0.0/10",  // CGNAT: tailnet peers
	"127.0.0.0/8",    // loopback (of the netns, never the host's)
	"169.254.0.0/16", // link-local, cloud metadata
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved, broadcast
)

// ParseService reads "tcp/192.168.1.10:5432" (or udp). The address must be
// a single unicast IPv4 address outside loopback, link-local/metadata,
// CGNAT/tailnet, multicast and reserved space; port 53 is refused because
// guest DNS is always answered by the built-in resolvers.
func ParseService(s string) (Service, error) {
	bad := func(why string) (Service, error) {
		return Service{}, fmt.Errorf("%w: LAN service %q: %s", ErrInvalid, s, why)
	}
	proto, hostport, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok || (proto != "tcp" && proto != "udp") {
		return bad(`want "tcp/ADDRESS:PORT" or "udp/ADDRESS:PORT"`)
	}
	ap, err := netip.ParseAddrPort(hostport)
	if err != nil {
		return bad("want an IPv4 address and port, e.g. tcp/192.168.1.10:5432")
	}
	svc := Service{Proto: proto, Addr: ap.Addr(), Port: ap.Port()}
	if err := svc.validate(); err != nil {
		return bad(err.Error())
	}
	return svc, nil
}

func (s Service) validate() error {
	switch {
	case s.Proto != "tcp" && s.Proto != "udp":
		return fmt.Errorf("protocol %q must be tcp or udp", s.Proto)
	case !s.Addr.Is4():
		return fmt.Errorf("address %s must be IPv4 (guest egress is IPv4)", s.Addr)
	case s.Port == 0:
		return fmt.Errorf("port 0")
	case s.Port == 53:
		return fmt.Errorf("port 53: guest DNS is always answered by the built-in resolvers")
	}
	for _, p := range notService {
		if p.Contains(s.Addr) {
			return fmt.Errorf("address %s is in %s, which is never a LAN service", s.Addr, p)
		}
	}
	return nil
}

// ParseServices parses, validates, sorts and deduplicates entries.
func ParseServices(entries []string) ([]Service, error) {
	out := make([]Service, 0, len(entries))
	for _, e := range entries {
		s, err := ParseService(e)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Service) int { return strings.Compare(a.sortKey(), b.sortKey()) })
	return slices.Compact(out), nil
}

func (s Service) sortKey() string {
	b := s.Addr.As4()
	return fmt.Sprintf("%03d%03d%03d%03d/%s/%05d", b[0], b[1], b[2], b[3], s.Proto, s.Port)
}

// ServiceSet is the nftables set of LAN services, matched with
// "ip daddr . meta l4proto . th dport @svc4".
const ServiceSet = "svc4"

// formatServiceSet renders the svc4 concatenation set declaration.
func formatServiceSet(indent string, svcs []Service) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%sset %s {\n%s\ttype ipv4_addr . inet_proto . inet_service\n", indent, ServiceSet, indent)
	if len(svcs) > 0 {
		fmt.Fprintf(&b, "%s\telements = { %s }\n", indent, strings.Join(ServiceElements(svcs), ", "))
	}
	fmt.Fprintf(&b, "%s}\n", indent)
	return b.String()
}

// ServiceElements are svc4 elements ("192.168.1.10 . tcp . 5432").
func ServiceElements(svcs []Service) []string {
	out := make([]string, len(svcs))
	for i, s := range svcs {
		out[i] = s.Addr.String() + " . " + s.Proto + " . " + strconv.Itoa(int(s.Port))
	}
	return out
}

// serviceRule accepts flows to a LAN service (svc4).
const serviceRule = "ip daddr . meta l4proto . th dport @" + ServiceSet + " accept"
