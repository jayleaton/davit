package userspace

import (
	"net/netip"
	"testing"
)

func TestPolicyOrder(t *testing.T) {
	pf := netip.MustParsePrefix
	svc := netip.MustParseAddr("192.168.1.10")
	p := Policy{
		Deny:      []netip.Prefix{pf("10.0.0.0/8"), pf("169.254.0.0/16"), pf("100.64.0.0/10"), pf("192.168.0.0/16")},
		Allow:     []netip.Prefix{pf("10.1.2.0/24"), pf("192.0.2.7/32")},
		HostAddrs: []netip.Prefix{pf("10.1.2.3/32"), pf("192.0.2.7/32"), pf("192.168.1.1/32")},
		Services:  []Service{{Proto: "tcp", Addr: svc, Port: 5432}, {Proto: "tcp", Addr: netip.MustParseAddr("192.168.1.1"), Port: 22}},
	}
	for _, c := range []struct {
		proto, dst string
		want       bool
	}{
		{"tcp", "93.184.216.34:443", true},    // internet
		{"tcp", "10.9.9.9:443", false},        // deny list
		{"tcp", "10.1.2.4:443", true},         // explicit allow beats deny
		{"tcp", "10.1.2.3:443", false},        // host address beats allow
		{"tcp", "192.0.2.7:443", false},       // host address beats allow
		{"tcp", "169.254.169.254:80", false},  // metadata
		{"tcp", "100.101.102.103:443", false}, // tailnet CGNAT
		{"tcp", "0.0.0.0:443", false},         // unspecified
		{"tcp", "[::ffff:10.9.9.9]:443", false},
		{"tcp", "192.168.1.10:5432", true},          // LAN service
		{"tcp", "[::ffff:192.168.1.10]:5432", true}, // unmapped first
		{"udp", "192.168.1.10:5432", false},         // other transport
		{"tcp", "192.168.1.10:5433", false},         // other port
		{"tcp", "192.168.1.11:5432", false},         // other address
		{"tcp", "192.168.1.1:22", false},            // host address beats a service
		// IPv6 forms carrying a denied IPv4 are checked as that IPv4 (SEC-9).
		{"tcp", "[64:ff9b::a09:909]:443", false},  // NAT64 of 10.9.9.9
		{"tcp", "[2002:a09:909::1]:443", false},   // 6to4 of 10.9.9.9
		{"tcp", "[64:ff9b::a9fe:a9fe]:80", false}, // NAT64 of 169.254.169.254
	} {
		if got := p.Allows(c.proto, netip.MustParseAddrPort(c.dst)); got != c.want {
			t.Errorf("Allows(%s, %s) = %v, want %v", c.proto, c.dst, got, c.want)
		}
	}
}
