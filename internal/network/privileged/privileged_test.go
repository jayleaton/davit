package privileged

import (
	"context"
	"errors"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"davit/internal/domain"
	"davit/internal/network"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

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

func testConfig() network.Config {
	cfg := network.DefaultConfig(netip.MustParsePrefix("192.168.1.10/32"))
	cfg.Egress.Services = []network.Service{{Proto: "tcp", Addr: netip.MustParseAddr("192.168.7.20"), Port: 5432}}
	cfg.GuestSubnets = []netip.Prefix{netip.MustParsePrefix("172.17.0.0/16")}
	cfg.TapServices = []uint16{15080}
	return cfg
}

func render(plan []Command) string {
	var b strings.Builder
	for _, c := range plan {
		b.WriteString(c.String())
		if c.IgnoreError {
			b.WriteString("  # ignore error")
		}
		b.WriteString("\n")
		if c.Stdin != "" {
			b.WriteString(c.Stdin)
			b.WriteString("ruleset\n")
		}
	}
	return b.String()
}

// TestHelperPlansGolden pins exactly what root would execute for VM index 1
// with uplink eth0 (the example in docs/network-netns.md).
func TestHelperPlansGolden(t *testing.T) {
	rec := &DryRun{}
	h, err := NewHelper(testConfig(), rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.Do(ctx, EnsureHost{Uplink: "eth0"}); err != nil {
		t.Fatal(err)
	}
	golden(t, "ensure_host.txt", render(rec.Commands))

	rec.Commands = nil
	nh, err := h.Do(ctx, CreateVMNet{VMID: "vm_example", Generation: 1, Index: 1})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "create_vm.txt", render(rec.Commands))
	if nh.Mode != network.ModePrivileged || nh.NetNSPath != "/run/netns/"+NamesFor("vm_example", 1).NetNS ||
		nh.TapName != "tap0" || nh.GuestIP != network.DefaultGuestIP || !nh.Egress.Enabled {
		t.Errorf("handle = %+v", nh)
	}

	rec.Commands = nil
	if _, err := h.Do(ctx, DestroyVMNet{VMID: "vm_example", Generation: 1, Index: 1}); err != nil {
		t.Fatal(err)
	}
	golden(t, "destroy_vm.txt", render(rec.Commands))
	for _, c := range rec.Commands {
		if !c.IgnoreError {
			t.Errorf("destroy step %q is not idempotent", c)
		}
	}
}

// TestEgressPlansGolden pins the per-mode plans: an allowlist VM gets the
// allowlist table plus the resolver daemon before it is registered, and
// SetVMEgress replaces the table atomically.
func TestEgressPlansGolden(t *testing.T) {
	rec := &DryRun{}
	h, err := NewHelper(testConfig(), rec)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	allow := domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"github.com", "203.0.113.0/24"}}
	nh, err := h.Do(ctx, CreateVMNet{VMID: "vm_example", Generation: 1, Index: 1, Egress: allow})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "create_vm_allowlist.txt", render(rec.Commands))
	if nh.Policy.Mode != domain.EgressAllowlist || len(nh.Policy.Allow) != 2 || nh.Policy.Allow[0] != "203.0.113.0/24" {
		t.Errorf("handle policy = %+v, want the normalized allowlist", nh.Policy)
	}
	if n := len(rec.Commands); !rec.Commands[n-2].Daemon || !strings.HasPrefix(rec.Commands[n-1].Stdin, "add element inet davit vm_src") {
		t.Errorf("resolver must start before the VM is registered: %v", rec.Commands)
	}
	for _, e := range []domain.Egress{{Mode: domain.EgressOff}, allow} {
		rec.Commands = nil
		if _, err := h.Do(ctx, SetVMEgress{VMID: "vm_example", Generation: 1, Index: 1, Egress: e}); err != nil {
			t.Fatal(err)
		}
		golden(t, "set_egress_"+string(e.Mode)+".txt", render(rec.Commands))
	}
}

func TestUplinkAddrs(t *testing.T) {
	for _, tc := range []struct {
		index      int
		host, peer string
	}{{1, "10.200.0.5/30", "10.200.0.6/30"}, {64, "10.200.1.1/30", "10.200.1.2/30"}, {MaxIndex, "10.200.255.253/30", "10.200.255.254/30"}} {
		h, p, err := UplinkAddrs(tc.index)
		if err != nil || h.String() != tc.host || p.String() != tc.peer {
			t.Errorf("UplinkAddrs(%d) = %s %s %v, want %s %s", tc.index, h, p, err, tc.host, tc.peer)
		}
	}
}

func TestRejectsBadRequests(t *testing.T) {
	h, err := NewHelper(testConfig(), &DryRun{})
	if err != nil {
		t.Fatal(err)
	}
	bad := map[string]Request{
		"vmid space":        CreateVMNet{VMID: "vm 1", Generation: 1, Index: 1},
		"vmid semicolon":    CreateVMNet{VMID: "vm;rm -rf /", Generation: 1, Index: 1},
		"vmid empty":        DestroyVMNet{VMID: "", Generation: 1, Index: 1},
		"generation 0":      CreateVMNet{VMID: "vm_a", Generation: 0, Index: 1},
		"index 0":           CreateVMNet{VMID: "vm_a", Generation: 1, Index: 0},
		"index negative":    DestroyVMNet{VMID: "vm_a", Generation: 1, Index: -1},
		"index over pool":   CreateVMNet{VMID: "vm_a", Generation: 1, Index: MaxIndex + 1},
		"uplink empty":      EnsureHost{Uplink: ""},
		"uplink injection":  EnsureHost{Uplink: `eth0" accept; #`},
		"uplink too long":   EnsureHost{Uplink: "abcdefghijklmnop"},
		"uplink slash":      EnsureHost{Uplink: "eth0/x"},
		"uplink reserved":   EnsureHost{Uplink: "slw0"},
		"uplink whitespace": EnsureHost{Uplink: "eth 0"},
		"egress mode":       CreateVMNet{VMID: "vm_a", Generation: 1, Index: 1, Egress: domain.Egress{Mode: "open"}},
		"egress allow":      SetVMEgress{VMID: "vm_a", Generation: 1, Index: 1, Egress: domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"x\" accept"}}},
		"egress key":        SetVMEgress{VMID: "vm a", Generation: 1, Index: 1, Egress: domain.Egress{Mode: domain.EgressOff}},
	}
	for name, req := range bad {
		rec := &DryRun{}
		h.exec = rec
		if _, err := h.Do(context.Background(), req); !errors.Is(err, network.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
		if len(rec.Commands) != 0 {
			t.Errorf("%s: rejected request still produced commands: %v", name, rec.Commands)
		}
	}
	if _, err := h.Do(context.Background(), nil); !errors.Is(err, network.ErrInvalid) {
		t.Errorf("nil request: %v", err)
	}
}

type failAt struct {
	DryRun
	n int
}

func (f *failAt) Run(ctx context.Context, c Command) error {
	_ = f.DryRun.Run(ctx, c)
	if len(f.Commands) == f.n {
		return errors.New("boom")
	}
	return nil
}

// A failed create is rolled back with the destroy plan.
func TestCreateRollsBack(t *testing.T) {
	ex := &failAt{n: 3}
	h, err := NewHelper(testConfig(), ex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Do(context.Background(), CreateVMNet{VMID: "vm_a", Generation: 1, Index: 2}); err == nil {
		t.Fatal("create succeeded")
	}
	undo, _ := PlanDestroy(DestroyVMNet{VMID: "vm_a", Generation: 1, Index: 2})
	got := ex.Commands[3:]
	if render(got) != render(undo) {
		t.Errorf("rollback =\n%s\nwant\n%s", render(got), render(undo))
	}
}

func TestGuestSubnetOutsideUplinkPool(t *testing.T) {
	cfg := testConfig()
	cfg.GuestSubnets = []netip.Prefix{netip.MustParsePrefix("10.200.4.0/24")}
	if _, err := NewHelper(cfg, &DryRun{}); !errors.Is(err, network.ErrInvalid) {
		t.Errorf("guest subnet in the uplink pool: NewHelper = %v, want ErrInvalid", err)
	}
}
