package network

import (
	"errors"
	"net/netip"
	"slices"
	"testing"
)

func TestParseServices(t *testing.T) {
	got, err := ParseServices([]string{"udp/10.0.0.5:5353", " tcp/192.168.1.10:5432", "tcp/192.168.1.10:5432", "tcp/192.168.1.9:22"})
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, v := range got {
		s = append(s, v.String())
	}
	if want := []string{"udp/10.0.0.5:5353", "tcp/192.168.1.9:22", "tcp/192.168.1.10:5432"}; !slices.Equal(s, want) {
		t.Errorf("ParseServices = %q, want %q", s, want)
	}
	if !got[2].Matches("tcp", netip.MustParseAddrPort("[::ffff:192.168.1.10]:5432")) || got[2].Matches("udp", netip.MustParseAddrPort("192.168.1.10:5432")) {
		t.Error("Matches")
	}
	for _, bad := range []string{
		"192.168.1.10:5432",       // no protocol
		"icmp/192.168.1.10:0",     // protocol
		"tcp/192.168.1.0/24:22",   // a prefix, not one address
		"tcp/192.168.1.10",        // no port
		"tcp/192.168.1.10:0",      // port 0
		"udp/192.168.1.1:53",      // DNS is the resolvers' job
		"tcp/[fd00::1]:22",        // IPv6
		"tcp/169.254.169.254:80",  // metadata
		"tcp/100.100.100.100:443", // tailnet
		"tcp/127.0.0.1:22",        // loopback
		"udp/224.0.0.251:5353",    // multicast
		"tcp/lan-db.example:5432", // names are not resolved here
	} {
		if _, err := ParseService(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseService(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}
