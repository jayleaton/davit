package onboarding

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"

	"davit/internal/buildinfo"
	"davit/internal/config"
	"davit/internal/preflight"
	"davit/packaging"
)

// AppArmorOptions configure AppArmor (`init --apparmor`).
type AppArmorOptions struct {
	// ConfigPath locates the config dir; the rendered profile is written to
	// <config dir>/apparmor/<name>. Default config.DefaultConfigPath.
	ConfigPath string
	// Binary is the davit binary the profile attaches to; default this
	// executable. Symlinks are resolved: AppArmor matches the real path.
	Binary string
	// ReadFile reads /proc and /sys for the current state; default os.ReadFile.
	ReadFile func(string) ([]byte, error)
}

// AppArmorResult is what `init --apparmor` reports. Commands are for the
// user to run once as root; nothing here runs them.
type AppArmorResult struct {
	Binary      string `json:"binary"`
	ProfileName string `json:"profileName"`
	// ProfileFile is the rendered profile, written as this user.
	ProfileFile string `json:"profileFile"`
	// InstallPath is where Commands install it.
	InstallPath string `json:"installPath"`
	Profile     string `json:"profile"`
	// Restricted: kernel.apparmor_restrict_unprivileged_userns=1 here.
	Restricted bool `json:"restricted"`
	// Attached: Binary is this executable and already runs under the profile.
	Attached       bool     `json:"attached"`
	Commands       []string `json:"commands"`
	RemoveCommands []string `json:"removeCommands"`
	Warnings       []string `json:"warnings"`
	NextSteps      []string `json:"nextSteps"`
}

// AppArmor renders the userns profile for the davit binary, writes it to
// a user-owned file and returns the one-time root commands that install and
// load it (and remove it again). It never runs anything as root and never
// writes outside the config dir.
func AppArmor(o AppArmorOptions) (*AppArmorResult, error) {
	if goruntime.GOOS != "linux" {
		return nil, fmt.Errorf("init --apparmor: AppArmor exists on Linux only")
	}
	if o.ConfigPath == "" {
		o.ConfigPath = config.DefaultConfigPath()
	}
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	self := preflight.SelfBinary()
	bin := o.Binary
	if bin == "" {
		if bin = self; bin == "" {
			return nil, errors.New("init --apparmor: cannot determine this executable's path; pass --apparmor-binary")
		}
	}
	if !filepath.IsAbs(bin) || !filepath.IsAbs(o.ConfigPath) {
		return nil, fmt.Errorf("init --apparmor: binary %q and config path %q must be absolute", bin, o.ConfigPath)
	}
	resolved, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return nil, fmt.Errorf("init --apparmor: %w", err)
	}
	if fi, err := os.Stat(resolved); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("init --apparmor: %s is not a regular file", resolved)
	}
	name := preflight.AppArmorProfileFor(resolved)
	profile, err := packaging.AppArmorProfile(packaging.AppArmorParams{Name: name, Command: buildinfo.Name, Binary: resolved})
	if err != nil {
		return nil, fmt.Errorf("init --apparmor: %w", err)
	}
	dir := filepath.Join(filepath.Dir(o.ConfigPath), "apparmor")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("init --apparmor: %w", err)
	}
	file := filepath.Join(dir, name)
	if err := writeFileAtomic(file, []byte(profile), 0o644); err != nil {
		return nil, fmt.Errorf("init --apparmor: %w", err)
	}
	install, remove := packaging.AppArmorCommands(file, name)
	facts := preflight.ReadAppArmorFacts(o.ReadFile, resolved, name)
	r := &AppArmorResult{
		Binary: resolved, ProfileName: name, ProfileFile: file, InstallPath: packaging.AppArmorDir + "/" + name,
		Profile: profile, Restricted: facts.Restriction == 1, Attached: facts.Attached() && resolved == self,
		Commands: install, RemoveCommands: remove, Warnings: []string{}, NextSteps: []string{},
	}
	if !r.Restricted {
		r.Warnings = append(r.Warnings, "this host does not restrict unprivileged user namespaces (kernel.apparmor_restrict_unprivileged_userns is not 1); the profile is not needed here")
	}
	if r.Attached {
		r.Warnings = append(r.Warnings, "this binary already runs under the "+name+" profile; reinstall only after moving the binary")
	}
	if userWritable(resolved) || userWritable(filepath.Dir(resolved)) {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"%s or its directory is writable by you, so any program running as you can replace it and inherit the profile's user-namespace permission; for a tighter setup install it root-owned first (sudo install -m 0755 %s /usr/local/bin/%s) and re-run /usr/local/bin/%s init --apparmor",
			resolved, packaging.ShellQuote(resolved), buildinfo.Name, buildinfo.Name))
	}
	r.NextSteps = append(r.NextSteps,
		fmt.Sprintf("After loading the profile, restart the daemon so it starts under it: systemctl --user restart %s (or restart `%s daemon`)", buildinfo.Name, buildinfo.Name),
		fmt.Sprintf("Check it: %s doctor (apparmor and userns pass)", buildinfo.Name))
	return r, nil
}
