package onboarding

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"davit/internal/preflight"
	"davit/packaging"
)

func sysctl(v string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if p == preflight.AppArmorUsernsSysctl && v != "" {
			return []byte(v + "\n"), nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestAppArmorRendersProfileAndRootCommands(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("AppArmor is Linux-only")
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "my bin")
	bin := filepath.Join(binDir, "davit")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "davit-link")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config", "davit", "config.json")

	r, err := AppArmor(AppArmorOptions{ConfigPath: cfg, Binary: link, ReadFile: sysctl("1")})
	if err != nil {
		t.Fatal(err)
	}
	name := packaging.AppArmorProfileName("davit", bin)
	want, _ := packaging.AppArmorProfile(packaging.AppArmorParams{Name: name, Command: "davit", Binary: bin})
	if r.Binary != bin || r.Profile != want || !strings.Contains(r.Profile, `"`+bin+`"`) {
		t.Fatalf("profile must attach to the resolved, quoted path %s:\n%s", bin, r.Profile)
	}
	file := filepath.Join(dir, "config", "davit", "apparmor", name)
	got, err := os.ReadFile(file)
	if err != nil || string(got) != want || r.ProfileFile != file || mode(t, file) != 0o644 {
		t.Fatalf("profile file %s: %v %q", file, err, got)
	}
	if !r.Restricted || r.Attached || r.ProfileName != name || r.InstallPath != "/etc/apparmor.d/"+name {
		t.Fatalf("%+v", r)
	}
	wantCmds := []string{
		"sudo install -m 0644 -o root -g root " + packaging.ShellQuote(file) + " /etc/apparmor.d/" + name,
		"sudo apparmor_parser -r /etc/apparmor.d/" + name,
	}
	if strings.Join(r.Commands, "\n") != strings.Join(wantCmds, "\n") || len(r.RemoveCommands) != 2 {
		t.Fatalf("commands:\n%s\nremove: %v", strings.Join(r.Commands, "\n"), r.RemoveCommands)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "writable by you") {
		t.Fatalf("a user-writable binary must be called out: %v", r.Warnings)
	}
	b, _ := json.Marshal(r)
	for _, k := range []string{`"commands"`, `"removeCommands"`, `"profileFile"`, `"installPath"`, `"profile"`, `"restricted":true`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("JSON lacks %s: %s", k, b)
		}
	}

	// Re-running is idempotent; on a host without the restriction it says
	// the profile is not needed.
	r, err = AppArmor(AppArmorOptions{ConfigPath: cfg, Binary: bin, ReadFile: sysctl("")})
	if err != nil || r.Restricted || !strings.Contains(strings.Join(r.Warnings, "\n"), "not needed") {
		t.Fatalf("%v %+v", err, r)
	}

	for _, bad := range []string{"davit", filepath.Join(dir, "missing"), binDir, filepath.Join(dir, "a*b")} {
		if bad == filepath.Join(dir, "a*b") {
			if err := os.WriteFile(bad, nil, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := AppArmor(AppArmorOptions{ConfigPath: cfg, Binary: bad, ReadFile: sysctl("1")}); err == nil {
			t.Errorf("binary %q accepted", bad)
		}
	}
}
