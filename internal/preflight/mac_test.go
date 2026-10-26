package preflight

import (
	"errors"
	"testing"

	hp "davit/internal/runtime/mac/helperproto"
)

func TestMacOSCheck(t *testing.T) {
	for _, c := range []struct {
		arch, version string
		required      bool
		want          Status
	}{
		{"arm64", "14.6.1", true, Pass},
		{"arm64", "15.0", true, Pass},
		{"arm64", "13.6", true, Fail},
		{"arm64", "13.6", false, Warn},
		{"amd64", "14.6", true, Fail},
		{"arm64", "", true, Fail},
	} {
		if got := MacOSCheck(c.arch, c.version, c.required); got.Status != c.want {
			t.Errorf("%s %q required=%v: %s (%s)", c.arch, c.version, c.required, got.Status, got.Detail)
		}
	}
}

func TestMacHelperChecks(t *testing.T) {
	good := hp.ProbeResult{MinVersion: 1, MaxVersion: 1, HelperVersion: "1.0.0", Build: "b", VZSupported: true, Entitled: true,
		Limits: hp.Limits{MaxCPUs: 8, MinMemoryBytes: 1 << 27, MaxMemoryBytes: 1 << 34}}
	status := func(cs []Check) (Status, Status) { return cs[0].Status, cs[1].Status }
	if h, v := status(MacHelperChecks("/h", good, nil, true)); h != Pass || v != Pass {
		t.Fatalf("good helper: %s %s", h, v)
	}
	if h, v := status(MacHelperChecks("/h", hp.ProbeResult{}, errors.New("no such file"), true)); h != Fail || v != Skip {
		t.Fatalf("missing helper: %s %s", h, v)
	}
	if h, _ := status(MacHelperChecks("/h", hp.ProbeResult{}, errors.New("no such file"), false)); h != Warn {
		t.Fatalf("missing helper, fake runtime: %s", h)
	}
	newer := good
	newer.MinVersion, newer.MaxVersion = 2, 3
	if h, _ := status(MacHelperChecks("/h", newer, nil, true)); h != Fail {
		t.Fatalf("protocol mismatch: %s", h)
	}
	for name, mut := range map[string]func(*hp.ProbeResult){
		"unsupported": func(p *hp.ProbeResult) { p.VZSupported = false },
		"unentitled":  func(p *hp.ProbeResult) { p.Entitled = false },
	} {
		p := good
		mut(&p)
		if _, v := status(MacHelperChecks("/h", p, nil, true)); v != Fail {
			t.Errorf("%s: %s", name, v)
		}
	}
}

func TestMacSignatureCheck(t *testing.T) {
	bad := errors.New("exit status 1")
	if c := MacSignatureCheck(nil, "", nil, "", true); c.Status != Pass {
		t.Fatalf("signed and notarized: %s", c.Status)
	}
	// An ad-hoc development build: valid signature, Gatekeeper refuses.
	if c := MacSignatureCheck(nil, "", bad, "/h: rejected\nsource=no usable signature", true); c.Status != Warn || c.Detail == "" {
		t.Fatalf("ad-hoc: %+v", c)
	}
	if c := MacSignatureCheck(bad, "/h: invalid signature (code or signature have been modified)", nil, "", true); c.Status != Fail {
		t.Fatalf("broken signature: %+v", c)
	}
}
