package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"davit/internal/network"
)

func TestStopAttemptsValidated(t *testing.T) {
	for n, ok := range map[int]bool{-1: false, 0: true, 1: true, 10: true, 11: false} {
		c := Defaults()
		c.DataDir, c.Socket, c.StopAttempts = "/data", "/run/s.sock", n
		if err := c.Validate(); (err == nil) != ok {
			t.Errorf("stopAttempts %d: %v", n, err)
		}
	}
}

func TestNetworkApply(t *testing.T) {
	n := Network{LANServices: []string{"tcp/192.168.1.10:5432", "udp/192.168.1.20:5353"}, GuestSubnets: []string{"172.17.0.0/16"}}
	c := network.DefaultConfig()
	if err := n.Apply(&c); err != nil {
		t.Fatal(err)
	}
	if len(c.Egress.Services) != 2 || c.Egress.Services[0].String() != "tcp/192.168.1.10:5432" ||
		len(c.GuestSubnets) != 1 || c.GuestSubnets[0] != netip.MustParsePrefix("172.17.0.0/16") {
		t.Fatalf("applied %+v / %v", c.Egress.Services, c.GuestSubnets)
	}
	for _, bad := range []Network{
		{LANServices: []string{"tcp/169.254.169.254:80"}},
		{LANServices: []string{"192.168.1.10:5432"}},
		{GuestSubnets: []string{"172.17.0.0"}},
		{GuestSubnets: []string{"10.0.2.0/24"}},
	} {
		c := network.DefaultConfig()
		if err := bad.Apply(&c); err == nil {
			t.Errorf("Apply(%+v) accepted", bad)
		}
	}
}

func TestReversePolicyRefusesInternal(t *testing.T) {
	p, err := Network{ReverseHostServices: []string{"127.0.0.1:5432", "192.168.1.10:5432"}}.ReversePolicy()
	if err != nil || len(p.HostServices) != 2 {
		t.Fatalf("ReversePolicy = %v, %v", p, err)
	}
	for _, bad := range []string{"169.254.169.254:80", "[fd00:ec2::254]:80", "[fe80::1]:22", "224.0.0.251:5353", "0.0.0.0:80"} {
		if _, err := (Network{ReverseHostServices: []string{bad}}).ReversePolicy(); err == nil {
			t.Errorf("ReversePolicy accepted %s", bad)
		}
	}
}

func TestHarnessDefaultTurnTimeout(t *testing.T) {
	dir := t.TempDir()
	load := func(v string) (Daemon, error) {
		p := filepath.Join(dir, "c.json")
		if err := os.WriteFile(p, []byte(`{"harness":{"defaultTurnTimeout":`+v+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err == nil {
			err = c.Validate()
		}
		return c, err
	}
	if c, err := load(`"90m"`); err != nil || time.Duration(c.Harness.DefaultTurnTimeout) != 90*time.Minute {
		t.Fatalf("90m: %v %v", c.Harness.DefaultTurnTimeout, err)
	}
	for _, bad := range []string{`"25h"`, `"500ms"`, `"-1m"`} {
		if _, err := load(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if Defaults().Harness.DefaultTurnTimeout != 0 { // 0: the harness default, 1h
		t.Fatal("default changed")
	}
}
