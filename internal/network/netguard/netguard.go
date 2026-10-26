// Package netguard refuses outbound connections the daemon makes on a
// guest's or a configuration's behalf (credential-proxy upstreams, webhook
// deliveries) to internal addresses. The check runs on the address actually
// dialed, after resolution, so a public-looking name that resolves or
// rebinds to an internal address is refused too, and no environment proxy
// is used.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrRefused marks a destination refused by policy.
var ErrRefused = errors.New("netguard: destination refused by policy")

// Policy decides which destinations may be dialed.
type Policy struct {
	// AllowPrivate allows loopback, RFC 1918, CGNAT (the tailnet), IPv6 ULA
	// and other non-public ranges. Link-local (cloud metadata), unspecified,
	// multicast and broadcast addresses are refused whatever it says.
	AllowPrivate bool
}

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// always are refused even with AllowPrivate.
var always = prefixes(
	"0.0.0.0/8",      // "this network", unspecified
	"169.254.0.0/16", // link-local, cloud metadata 169.254.169.254
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // reserved, broadcast
	"::/128",
	"fe80::/10",         // link-local
	"fd00:ec2::254/128", // AWS metadata (IPv6)
	"ff00::/8",          // multicast
)

// private are refused unless AllowPrivate.
var private = prefixes(
	"10.0.0.0/8",
	"100.64.0.0/10", // CGNAT, Tailscale tailnet addresses
	"127.0.0.0/8",
	"172.16.0.0/12",
	"192.0.0.0/24", // IETF protocol assignments
	"192.168.0.0/16",
	"198.18.0.0/15", // benchmarking
	"::1/128",
	"fc00::/7", // ULA, incl. the tailnet's fd7a:115c:a1e0::/48
)

var (
	nat64  = netip.MustParsePrefix("64:ff9b::/96")
	sixto4 = netip.MustParsePrefix("2002::/16")
)

// embedded is the IPv4 address an IPv6 address carries (IPv4-mapped, NAT64,
// 6to4), so it is checked like the IPv4 address it reaches.
func embedded(a netip.Addr) netip.Addr {
	a = a.Unmap()
	if !a.Is6() {
		return a
	}
	b := a.As16()
	switch {
	case nat64.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	case sixto4.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})
	}
	return a
}

// Check reports whether a may be dialed.
func (p Policy) Check(a netip.Addr) error {
	if !a.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrRefused)
	}
	for _, addr := range []netip.Addr{a.Unmap(), embedded(a)} {
		for _, pf := range always {
			if pf.Contains(addr) {
				return fmt.Errorf("%w: %s is link-local, metadata, multicast or unspecified", ErrRefused, a)
			}
		}
		if !p.AllowPrivate {
			for _, pf := range private {
				if pf.Contains(addr) {
					return fmt.Errorf("%w: %s is a private, loopback or tailnet address (network.allowPrivateUpstreams)", ErrRefused, a)
				}
			}
		}
	}
	return nil
}

// Dialer dials TCP and refuses destinations p refuses, checked on the
// resolved address of each connection attempt.
func (p Policy) Dialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrRefused, err)
			}
			return p.Check(ap.Addr())
		},
	}
}

// Transport is an http.Transport that dials through Dialer and ignores
// proxy settings from the environment.
func (p Policy) Transport() *http.Transport {
	d := p.Dialer()
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0,
		ExpectContinueTimeout: time.Second,
	}
}
