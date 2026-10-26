package network

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"davit/internal/domain"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

// golden compares got with testdata/name, rewriting it under -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create)", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run with -update after review)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func testConfig() Config {
	cfg := DefaultConfig(netip.MustParsePrefix("192.168.1.10/32"))
	cfg.Egress.Allow = []netip.Prefix{netip.MustParsePrefix("192.168.7.7/32")}
	cfg.Egress.Services = []Service{
		{Proto: "tcp", Addr: netip.MustParseAddr("192.168.7.20"), Port: 5432},
		{Proto: "udp", Addr: netip.MustParseAddr("10.0.0.5"), Port: 5353},
	}
	cfg.TapServices = []uint16{15080}
	return cfg
}

// Rootless serves host services on the TAP; None has no TAP.
var _ TapListener = (*Rootless)(nil)

func TestGuestRulesetGolden(t *testing.T) {
	cfg := testConfig()
	off := domain.Egress{Mode: domain.EgressOff}
	filtered := domain.Egress{Mode: domain.EgressFiltered}
	allow := domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"203.0.113.0/24", "github.com"}}
	golden(t, "guest.nft", RenderGuestRuleset(cfg, filtered, false))
	golden(t, "guest_stack.nft", RenderGuestRuleset(cfg, filtered, true))
	golden(t, "guest_off.nft", RenderGuestRuleset(cfg, off, false))
	golden(t, "guest_allowlist.nft", RenderGuestRuleset(cfg, allow, false))
	// Several tap services render as one sorted set.
	multi := cfg
	multi.TapServices = []uint16{15081, 8080, 15080}
	if want := `iifname "tap0" ip saddr 172.16.0.2 ip daddr 172.16.0.1 tcp dport { 8080, 15080, 15081 } accept`; !strings.Contains(RenderGuestRuleset(multi, off, false), want) {
		t.Errorf("multi tap services: want %q in\n%s", want, RenderGuestRuleset(multi, off, false))
	}
	routed := cfg
	routed.GuestSubnets = []netip.Prefix{netip.MustParsePrefix("172.17.0.0/16")}
	golden(t, "guest_routed.nft", RenderGuestRuleset(routed, filtered, false))
	golden(t, "guest_routed_allowlist.nft", RenderGuestRuleset(routed, allow, false))
	// The userspace stack enforces the allowlist itself and answers DNS.
	if RenderGuestRuleset(cfg, allow, true) != RenderGuestRuleset(cfg, filtered, true) {
		t.Error("allowlist with the stack differs from filtered with the stack")
	}
	if RenderGuestRuleset(cfg, off, true) != RenderGuestRuleset(cfg, off, false) {
		t.Error("off depends on the backend")
	}
	want := "table inet davit_guest {}\ndelete table inet davit_guest\n" + RenderGuestRuleset(cfg, allow, false)
	if got := RenderGuestReplace(cfg, allow, false); got != want {
		t.Errorf("RenderGuestReplace =\n%s", got)
	}
}

func TestValidateVMID(t *testing.T) {
	for _, ok := range []string{"vm_k3abcdefghijklmnop", "a", "vm-1", strings.Repeat("a", 63)} {
		if err := ValidateVMID(domain.VMID(ok)); err != nil {
			t.Errorf("ValidateVMID(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "vm 1", "vm;rm", "VM1", "_vm", "-vm", "vm/../x", "vm\n", strings.Repeat("a", 64), "vm$(x)"} {
		if err := ValidateVMID(domain.VMID(bad)); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateVMID(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestValidateIfName(t *testing.T) {
	for _, bad := range []string{"", "eth 0", "a/b", "x;y", "sixteen-chars-xx", ".", "-x"} {
		if err := ValidateIfName(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ValidateIfName(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	if err := ValidateIfName("enp3s0.100"); err != nil {
		t.Error(err)
	}
}

func TestShortIDFitsIfName(t *testing.T) {
	a, b := ShortID("vm_a", 1), ShortID("vm_a", 2)
	if a == b || a != ShortID("vm_a", 1) || len(a) != 10 {
		t.Fatalf("ShortID not deterministic/distinct by generation: %q %q", a, b)
	}
	// Longest derived interface name: "slw" + id + "h".
	if err := ValidateIfName("slw" + a + "h"); err != nil {
		t.Fatal(err)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	if _, isTL := Provider(None{}).(TapListener); isTL {
		t.Error("None implements TapListener")
	}
	ok := DefaultConfig()
	ok.TapServices = []uint16{15080, 8080}
	if err := ok.Validate(); err != nil {
		t.Errorf("tap services: %v", err)
	}
	cases := map[string]func(*Config){
		"tap name":      func(c *Config) { c.TapName = "tap 0" },
		"same names":    func(c *Config) { c.Uplink = c.TapName },
		"guest outside": func(c *Config) { c.GuestIP = netip.MustParseAddr("172.16.0.9") },
		"guest == tap":  func(c *Config) { c.GuestIP = c.TapIP },
		"ipv6 guest":    func(c *Config) { c.GuestIP = netip.MustParseAddr("fd00::2") },
		"multicast mac": func(c *Config) { c.GuestMAC = "01:00:5e:00:00:01" },
		"mtu":           func(c *Config) { c.MTU = 70000 },
		"bad prefix":    func(c *Config) { c.Egress.Allow = []netip.Prefix{{}} },
		"relative dir":  func(c *Config) { c.StateDir = "var/net" },
		"bad service": func(c *Config) {
			c.Egress.Services = []Service{{Proto: "tcp", Addr: netip.MustParseAddr("169.254.169.254"), Port: 80}}
		},
		"public guest subnet":   func(c *Config) { c.GuestSubnets = []netip.Prefix{netip.MustParsePrefix("8.8.8.0/24")} },
		"guest subnet on tap":   func(c *Config) { c.GuestSubnets = []netip.Prefix{netip.MustParsePrefix("172.16.0.0/16")} },
		"guest subnet uplink":   func(c *Config) { c.GuestSubnets = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16")} },
		"unmasked guest subnet": func(c *Config) { c.GuestSubnets = []netip.Prefix{netip.MustParsePrefix("172.17.0.1/16")} },
		"tap service port 0":    func(c *Config) { c.TapServices = []uint16{15080, 0} },
		"tap service port 53":   func(c *Config) { c.TapServices = []uint16{53} },
		"duplicate tap service": func(c *Config) { c.TapServices = []uint16{15080, 8080, 15080} },
	}
	for name, mutate := range cases {
		cfg := DefaultConfig()
		mutate(&cfg)
		if err := cfg.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid", name, err)
		}
	}
}

func TestDefaultDenyCoversSensitiveRanges(t *testing.T) {
	deny := DefaultDeny()
	for _, a := range []string{"10.1.2.3", "172.31.0.1", "192.168.0.1", "100.100.100.100", "169.254.169.254",
		"127.0.0.1", "224.0.0.251", "fd7a:115c:a1e0::1", "fe80::1", "::1", "::ffff:10.0.0.1"} {
		addr := netip.MustParseAddr(a)
		if !containsAddr(deny, addr) {
			t.Errorf("%s not denied by default", a)
		}
	}
	if containsAddr(deny, netip.MustParseAddr("1.1.1.1")) {
		t.Error("internet address denied")
	}
}

// TestDefaultDenyCoversEmbeddedV4: IPv6 forms that carry a private IPv4
// (NAT64, 6to4, Teredo) and the remaining IPv4 special ranges are denied
// too, so they cannot be used once guest IPv6 is enabled (SEC-9).
func TestDefaultDenyCoversEmbeddedV4(t *testing.T) {
	deny := DefaultDeny()
	for _, a := range []string{"64:ff9b::a00:1", "64:ff9b:1::a00:1", "2002:a00:1::1", "2001:0:a00:1::1", "192.0.0.8", "198.18.0.1"} {
		if !containsAddr(deny, netip.MustParseAddr(a)) {
			t.Errorf("%s not denied by default", a)
		}
	}
}

func containsAddr(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func TestEgressArgv(t *testing.T) {
	cfg := DefaultConfig()
	got := PastaArgv(cfg, "/proc/42/ns/user", "/proc/42/ns/net", "/tmp/p/pid")
	want := []string{"pasta", "--quiet", "--config-net", "--ipv4-only", "--no-map-gw",
		"--tcp-ports", "none", "--udp-ports", "none", "--tcp-ns", "none", "--udp-ns", "none",
		"--ns-ifname", "up0", "--pid", "/tmp/p/pid", "--userns", "/proc/42/ns/user", "--netns", "/proc/42/ns/net"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PastaArgv =\n%q\nwant\n%q", got, want)
	}
	got = Slirp4netnsArgv("/proc/42/ns/user", "/proc/42/ns/net", "up0", 3)
	want = []string{"slirp4netns", "--configure", "--mtu=65520", "--disable-host-loopback", "--ready-fd=3",
		"--netns-type=path", "--userns-path=/proc/42/ns/user", "/proc/42/ns/net", "up0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Slirp4netnsArgv =\n%q\nwant\n%q", got, want)
	}
}

func TestEnterArgv(t *testing.T) {
	h := Handle{Mode: ModeRootless, NetNSPath: "/proc/7/ns/net", UserNSPath: "/proc/7/ns/user"}
	want := []string{"nsenter", "--preserve-credentials", "--user=/proc/7/ns/user", "--net=/proc/7/ns/net", "--"}
	if got := EnterArgv(h); !reflect.DeepEqual(got, want) {
		t.Errorf("rootless EnterArgv = %q", got)
	}
	h = Handle{Mode: ModePrivileged, NetNSPath: "/run/netns/slw-x"}
	if got := EnterArgv(h); !reflect.DeepEqual(got, []string{"nsenter", "--net=/run/netns/slw-x", "--"}) {
		t.Errorf("privileged EnterArgv = %q", got)
	}
	if got := EnterArgv(Handle{Mode: ModeNone}); got != nil {
		t.Errorf("none EnterArgv = %q", got)
	}
}

func TestNoneProviderAndHandleJSON(t *testing.T) {
	var p Provider = None{}
	if _, err := p.Create(context.Background(), "bad id", 1, domain.Egress{Mode: domain.EgressFiltered}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Create(bad id) = %v", err)
	}
	h, err := p.Create(context.Background(), "vm_x", 1, domain.Egress{Mode: domain.EgressFiltered})
	if err != nil || h.Mode != ModeNone || h.Egress.Enabled {
		t.Fatalf("Create = %+v, %v", h, err)
	}
	full := Handle{VMID: "vm_x", Generation: 3, Mode: ModeRootless, NetNSPath: "/proc/1/ns/net",
		Holder: &Process{PID: 1, StartTime: 9}, TapName: "tap0", GuestIP: DefaultGuestIP, TapIP: DefaultTapIP,
		Prefix: 30, GuestMAC: DefaultGuestMAC, MTU: 1500, Egress: Egress{Reason: "x"}}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var back Handle
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, full) {
		t.Fatalf("round trip: %+v, %v\n%s", back, err, b)
	}
}
