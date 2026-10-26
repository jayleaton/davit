package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"davit/internal/buildinfo"
	"davit/packaging"
)

// Files the AppArmor check reads. None of them needs root except the
// profile list, which is usually root-only; then "loaded" is unknown.
const (
	AppArmorUsernsSysctl = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
	appArmorSelfLabel    = "/proc/self/attr/apparmor/current" // Linux 5.8+
	appArmorSelfLabelOld = "/proc/self/attr/current"
	appArmorProfiles     = "/sys/kernel/security/apparmor/profiles"
)

// AppArmorFacts is what the AppArmor userns check observed.
type AppArmorFacts struct {
	// Restriction is kernel.apparmor_restrict_unprivileged_userns; -1 when
	// the knob does not exist (mainline kernels, non-Ubuntu distros).
	Restriction int
	// Binary is the resolved path of this executable ("" if unknown).
	Binary string
	// Profile is the profile name `init --apparmor` installs for Binary
	// (AppArmorProfileFor), one per binary path.
	Profile string
	// Label is this process's AppArmor profile name ("unconfined",
	// "davit", ...); "" when it cannot be read.
	Label string
	// Loaded is 1 when a profile named Profile is loaded, 0 when not, and
	// -1 when the kernel's profile list is not readable (non-root).
	Loaded int
}

// Attached reports whether this process runs under the davit profile, the
// one direct proof that a loaded profile attaches to this binary's path.
func (f AppArmorFacts) Attached() bool { return f.Profile != "" && f.Label == f.Profile }

// ParseAppArmorLabel turns a /proc/<pid>/attr/current value such as
// "davit (unconfined)\n" into the profile name ("davit").
func ParseAppArmorLabel(s string) string {
	s = strings.TrimRight(s, "\x00\n ")
	if i := strings.LastIndex(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		s = s[:i]
	}
	return s
}

// AppArmorProfileFor is the name of the profile `init --apparmor` installs
// for the binary at the resolved path bin ("" when bin is unknown).
func AppArmorProfileFor(bin string) string {
	if bin == "" {
		return ""
	}
	return packaging.AppArmorProfileName(buildinfo.Name, bin)
}

// SelfBinary is the resolved path of the running executable, the path an
// AppArmor profile has to name; "" if it cannot be determined.
func SelfBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return ""
	}
	return exe
}

// ReadAppArmorFacts gathers AppArmorFacts through read (os.ReadFile in
// production) for this process, which runs binary under profile (its
// AppArmorProfileFor name) if one is attached.
func ReadAppArmorFacts(read func(string) ([]byte, error), binary, profile string) AppArmorFacts {
	f := AppArmorFacts{Restriction: -1, Binary: binary, Profile: profile, Loaded: -1}
	if b, err := read(AppArmorUsernsSysctl); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			f.Restriction = v
		}
	}
	for _, p := range []string{appArmorSelfLabel, appArmorSelfLabelOld} {
		if b, err := read(p); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			f.Label = ParseAppArmorLabel(string(b))
			break
		}
	}
	if b, err := read(appArmorProfiles); err == nil {
		f.Loaded = 0
		for _, line := range strings.Split(string(b), "\n") {
			if line != "" && ParseAppArmorLabel(line) == profile {
				f.Loaded = 1
				break
			}
		}
	}
	return f
}

// ClassifyAppArmor grades whether AppArmor lets this binary create user
// namespaces. Skip when the host has no restriction; Pass when this process
// runs under the davit profile; otherwise the fix is `init --apparmor`.
func ClassifyAppArmor(f AppArmorFacts, required bool) Check {
	c := Check{ID: "apparmor", Title: "AppArmor user namespace profile"}
	bin := "this binary"
	if f.Binary != "" {
		bin = f.Binary
	}
	switch {
	case f.Restriction == -1:
		c.Status, c.Detail = Skip, "no AppArmor user namespace restriction on this kernel; no profile needed"
		return c
	case f.Restriction != 1:
		c.Status, c.Detail = Skip, fmt.Sprintf("kernel.apparmor_restrict_unprivileged_userns=%d; no profile needed", f.Restriction)
		return c
	case f.Attached():
		c.Status, c.Detail = Pass, fmt.Sprintf("%s runs under AppArmor profile %q, which allows user namespaces", bin, f.Profile)
		return c
	}
	c.Status = severity(required)
	c.Hint = fmt.Sprintf("`%s init --apparmor` prints a profile for %s and the one-time root commands that load it; then restart the daemon", buildinfo.Name, bin)
	if f.Label == "" {
		c.Detail = "AppArmor restricts unprivileged user namespaces (kernel.apparmor_restrict_unprivileged_userns=1) and this process's AppArmor label is unreadable"
		return c
	}
	c.Detail = fmt.Sprintf("AppArmor restricts unprivileged user namespaces (kernel.apparmor_restrict_unprivileged_userns=1) and %s runs as %q, not under a %q profile", bin, f.Label, f.Profile)
	switch f.Loaded {
	case 1:
		c.Detail += fmt.Sprintf("; profile %q is loaded, but this process started before it was (restart the daemon)", f.Profile)
	case 0:
		c.Detail += fmt.Sprintf("; no profile named %q is loaded", f.Profile)
	default:
		c.Detail += "; whether one is loaded for another path is unknown (the kernel's profile list is root-only)"
	}
	return c
}
