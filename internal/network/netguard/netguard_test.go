package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestPolicyRefusesInternalAddresses(t *testing.T) {
	always := []string{
		"169.254.169.254", "169.254.0.1", "fe80::1", "fd00:ec2::254", "0.0.0.0", "::",
		"224.0.0.1", "255.255.255.255", "ff02::1",
		"::ffff:169.254.169.254", // IPv4-mapped metadata
		"64:ff9b::a9fe:a9fe",     // NAT64 of 169.254.169.254
		"2002:a9fe:a9fe::1",      // 6to4 of 169.254.169.254
	}
	private := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "100.64.0.1", "100.100.100.100",
		"fd7a:115c:a1e0::1", "fc00::1", "198.18.0.1", "192.0.0.8",
		"::ffff:10.0.0.1", "64:ff9b::a00:1", "2002:a00:1::1",
	}
	public := []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, p := range []Policy{{}, {AllowPrivate: true}} {
		for _, s := range always {
			if err := p.Check(netip.MustParseAddr(s)); err == nil {
				t.Errorf("AllowPrivate=%v accepted %s", p.AllowPrivate, s)
			}
		}
		for _, s := range private {
			if err := p.Check(netip.MustParseAddr(s)); (err == nil) != p.AllowPrivate {
				t.Errorf("AllowPrivate=%v %s: %v", p.AllowPrivate, s, err)
			}
		}
		for _, s := range public {
			if err := p.Check(netip.MustParseAddr(s)); err != nil {
				t.Errorf("AllowPrivate=%v refused public %s: %v", p.AllowPrivate, s, err)
			}
		}
	}
}

// TestTransportChecksAtConnect: the check runs on the address actually
// dialed, so a public-looking name that resolves (or rebinds) to loopback
// is refused, and no environment proxy is used.
func TestTransportChecksAtConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("reached")) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	for _, host := range []string{"127.0.0.1", "localhost"} {
		_, err := (&http.Client{Transport: Policy{}.Transport()}).Get("http://" + net.JoinHostPort(host, port) + "/")
		if err == nil || !strings.Contains(err.Error(), "refused by policy") {
			t.Errorf("%s: %v, want refused by policy", host, err)
		}
	}
	res, err := (&http.Client{Transport: Policy{AllowPrivate: true}.Transport()}).Get(srv.URL)
	if err != nil {
		t.Fatalf("allowed private: %v", err)
	}
	res.Body.Close()
	if _, err := (Policy{}).Dialer().DialContext(context.Background(), "tcp", srv.Listener.Addr().String()); err == nil {
		t.Fatal("dialer reached loopback")
	}
}
