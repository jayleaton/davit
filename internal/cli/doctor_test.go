package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"davit/internal/config"
	"davit/internal/preflight"
)

func doctorSession() (*session, *bytes.Buffer) {
	var b bytes.Buffer
	return &session{out: &printer{stdout: &b, stderr: &b}}, &b
}

var doctorSample = []preflight.Check{
	{ID: "config", Title: "Configuration", Status: preflight.Pass, Detail: "/c.json"},
	{ID: "kvm", Title: "KVM access", Status: preflight.Fail, Detail: "/dev/kvm is not writable by this user (group kvm)",
		Hint: "add alice to group kvm, then log in again", Sudo: []string{"sudo usermod -aG kvm alice", "sudo systemctl restart user@1000.service"}},
	{ID: "tun", Title: "TUN/TAP device", Status: preflight.Warn, Detail: "cannot open /dev/net/tun",
		Hint: "load the tun module", Sudo: []string{"sudo modprobe tun"}},
	{ID: "disk_space", Title: "Free disk space", Status: preflight.Warn, Detail: "8.0GiB free", Hint: "less than 10GiB free"},
	{ID: "tailnet_auth", Title: "Tailnet enrollment", Status: preflight.Skip, Detail: "tailnet gateway off (optional)"},
}

func TestDoctorFixesOutput(t *testing.T) {
	s, out := doctorSession()
	printFixes(s, doctorSample)
	want := `# kvm: /dev/kvm is not writable by this user (group kvm)
sudo usermod -aG kvm alice
sudo systemctl restart user@1000.service
# tun: cannot open /dev/net/tun
sudo modprobe tun
`
	if out.String() != want {
		t.Fatalf("--fixes:\n%s", out)
	}
	s, out = doctorSession()
	printFixes(s, doctorSample[3:])
	if out.Len() != 0 {
		t.Fatalf("--fixes with nothing to run as root must print nothing: %q", out)
	}
}

func TestDoctorSummaryOutput(t *testing.T) {
	counts := map[preflight.Status]int{preflight.Pass: 1, preflight.Fail: 1, preflight.Warn: 2, preflight.Skip: 1}
	s, out := doctorSession()
	printDoctorSummary(s, doctorSample, counts)
	want := `davit doctor: 1 problem, 2 warnings
✗ KVM access: /dev/kvm is not writable by this user (group kvm)
  fix: sudo usermod -aG kvm alice && sudo systemctl restart user@1000.service
! TUN/TAP device: cannot open /dev/net/tun
  fix: sudo modprobe tun
! Free disk space: 8.0GiB free
  fix: less than 10GiB free
run davit doctor -v for details
`
	if out.String() != want {
		t.Fatalf("summary:\n%s", out)
	}

	s, out = doctorSession()
	printDoctorSummary(s, []preflight.Check{doctorSample[0], doctorSample[3]}, map[preflight.Status]int{preflight.Pass: 15, preflight.Warn: 1})
	if want := "davit doctor: ready (15 passed, 1 warning)\n! Free disk space: 8.0GiB free\n  fix: less than 10GiB free\n"; out.String() != want {
		t.Fatalf("ready with a warning (nothing hidden, so no -v line):\n%s", out)
	}

	s, out = doctorSession()
	printDoctorVerbose(s, doctorSample, counts)
	for _, want := range []string{"PASS  config", "SKIP  tailnet_auth", "fix: add alice to group kvm", "run: sudo modprobe tun"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("-v lacks %q:\n%s", want, out)
		}
	}
}

func TestServiceCheck(t *testing.T) {
	unit := filepath.Join(t.TempDir(), "davit.service")
	p := serviceProbes{
		unit: unit, user: "alice",
		active: func(context.Context) (string, error) { return "active", nil },
		linger: func(context.Context, string) bool { return true },
	}
	ctx := context.Background()
	if c := serviceCheck(ctx, p); c.Status != preflight.Skip || c.Hint != "davit init --service" {
		t.Fatalf("not installed: %+v", c)
	}
	os.WriteFile(unit, []byte("[Unit]\n"), 0o644)
	if c := serviceCheck(ctx, p); c.Status != preflight.Pass || c.Sudo != nil {
		t.Fatalf("active and lingering: %+v", c)
	}
	p.linger = func(_ context.Context, u string) bool { return u != "alice" }
	c := serviceCheck(ctx, p)
	if c.Status != preflight.Warn || !slices.Equal(c.Sudo, []string{"sudo loginctl enable-linger alice"}) ||
		!strings.Contains(c.Detail, "stops when you log out and does not start at boot") || c.Hint != c.Sudo[0] {
		t.Fatalf("lingering off: %+v", c)
	}
	p.user, p.linger = "o'brien", func(context.Context, string) bool { return false }
	if c := serviceCheck(ctx, p); !slices.Equal(c.Sudo, []string{`sudo loginctl enable-linger 'o'\''brien'`}) {
		t.Fatalf("user names are shell-quoted: %q", c.Sudo)
	}
	p.user = "alice"
	p.active = func(context.Context) (string, error) { return "failed", errors.New("exit status 3") }
	if c := serviceCheck(ctx, p); c.Status != preflight.Warn || !strings.Contains(c.Hint, "enable --now") || !strings.Contains(c.Hint, "enable-linger") {
		t.Fatalf("failed and not lingering: %+v", c)
	}
	p.active = func(context.Context) (string, error) { return "", errors.New("Failed to connect to bus") }
	if c := serviceCheck(ctx, p); c.Status != preflight.Warn || c.Hint == "" {
		t.Fatalf("no user bus: %+v", c)
	}
}

// TestHostChecksHaveFixes: every failing or warning CLI-side check carries
// a one-line fix (the preflight classifiers are swept in their package).
func TestHostChecksHaveFixes(t *testing.T) {
	bad := config.Defaults()
	bad.Runtime = ""
	fake := config.Defaults()
	fake.Runtime = "fake"
	checks := []preflight.Check{configCheck(config.Defaults(), ""), configCheck(bad, "/c.json"), runtimeCheck(fake)}
	unit := filepath.Join(t.TempDir(), "davit.service")
	os.WriteFile(unit, nil, 0o644)
	for _, active := range []string{"", "inactive"} {
		for _, user := range []string{"", "alice"} {
			checks = append(checks, serviceCheck(context.Background(), serviceProbes{unit: unit, user: user,
				active: func(context.Context) (string, error) { return active, errors.New("x") },
				linger: func(context.Context, string) bool { return false }}))
		}
	}
	if c := runtimeCheck(fake); c.Status != preflight.Warn || !strings.Contains(c.Detail, "development only") {
		t.Errorf("fake runtime: %+v", c)
	}
	for _, c := range checks {
		if (c.Status == preflight.Fail || c.Status == preflight.Warn) && (c.Fix() == "" || strings.Contains(c.Fix(), "\n")) {
			t.Errorf("%s %s without a one-line fix: %+v", c.ID, c.Status, c)
		}
	}
}
