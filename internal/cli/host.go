package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/config"
	"davit/internal/daemon"
	"davit/internal/domain"
	"davit/internal/onboarding"
	"davit/internal/preflight"
	"davit/packaging"
)

// runDoctor prints host readiness: the daemon's own preflight checks plus
// config, runtime, daemon reachability and the user service. Each check is
// one `item`; any failure ends with an `error` line and exit 6 (unsupported
// host capability). People see a summary line and only the problems, each
// with a one-line fix; -v prints every check. --fixes prints only the root
// commands that fix the problems (for install.sh) and exits 0.
func runDoctor(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 0, "doctor [--config FILE] [-v|--verbose] [--fixes]"); err != nil {
		return err
	}
	cfg, path, err := config.LoadDefault(o.config)
	if err != nil {
		return usageErr("%v", err)
	}
	checks := []preflight.Check{configCheck(cfg, path), runtimeCheck(cfg)}
	checks = append(checks, preflight.Run(ctx, preflight.FromConfig(cfg)).Checks...)
	checks = append(checks, s.daemonCheck(ctx, cfg))
	if goruntime.GOOS == "linux" {
		checks = append(checks, serviceCheck(ctx, defaultServiceProbes()))
	}
	if o.fixes {
		appArmorFix(s, checks, path)
	}

	var failed []string
	counts := map[preflight.Status]int{}
	for _, c := range checks {
		counts[c.Status]++
		if c.Status == preflight.Fail {
			failed = append(failed, c.ID)
		}
		s.out.emit("item", "", 0, c, nil)
	}
	switch {
	case o.fixes:
		printFixes(s, checks)
		s.out.result("", map[string]any{"ok": len(failed) == 0, "counts": counts, "failed": failed})
		return nil
	case o.verbose:
		printDoctorVerbose(s, checks, counts)
	default:
		printDoctorSummary(s, checks, counts)
	}
	if len(failed) > 0 {
		e := clientErr(domain.CodeUnsupportedCapability, "%d host check(s) failed: %s", len(failed), strings.Join(failed, ", "))
		e.Details = map[string]any{"failed": failed, "counts": counts}
		return e
	}
	s.out.result("", map[string]any{"ok": true, "counts": counts})
	return nil
}

// problem reports whether doctor shows c by default.
func problem(c preflight.Check) bool { return c.Status == preflight.Fail || c.Status == preflight.Warn }

// printFixes prints, for each problem with root commands, `# id: detail`
// and the commands one per line; nothing at all when none is needed.
func printFixes(s *session, checks []preflight.Check) {
	for _, c := range checks {
		if !problem(c) || len(c.Sudo) == 0 {
			continue
		}
		s.out.human("# %s: %s", c.ID, oneLine(c.Detail))
		for _, l := range c.Sudo {
			s.out.human("%s", l)
		}
	}
}

// printDoctorSummary prints one summary line, then each failure and
// warning on one line with a one-line fix.
func printDoctorSummary(s *session, checks []preflight.Check, counts map[preflight.Status]int) {
	plural := func(n int, one string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, one)
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	line := fmt.Sprintf("%s doctor: ready (%d passed", buildinfo.Name, counts[preflight.Pass])
	if n := counts[preflight.Warn]; n > 0 {
		line += ", " + plural(n, "warning")
	}
	line += ")"
	if n := counts[preflight.Fail]; n > 0 {
		line = fmt.Sprintf("%s doctor: %s", buildinfo.Name, plural(n, "problem"))
		if w := counts[preflight.Warn]; w > 0 {
			line += ", " + plural(w, "warning")
		}
	}
	s.out.human("%s", line)
	hidden := false
	for _, status := range []preflight.Status{preflight.Fail, preflight.Warn} {
		mark := map[preflight.Status]string{preflight.Fail: "✗", preflight.Warn: "!"}[status]
		for _, c := range checks {
			if c.Status != status {
				continue
			}
			detail, _, cut := strings.Cut(c.Detail, "; ")
			s.out.human("%s %s: %s", mark, c.Title, oneLine(detail))
			if fix := c.Fix(); fix != "" {
				s.out.human("  fix: %s", oneLine(fix))
			}
			hidden = hidden || cut || (len(c.Sudo) > 0 && c.Hint != "")
		}
	}
	if hidden {
		s.out.human("run %s doctor -v for details", buildinfo.Name)
	}
}

// printDoctorVerbose prints every check with its full detail, hint and
// root commands.
func printDoctorVerbose(s *session, checks []preflight.Check, counts map[preflight.Status]int) {
	for _, c := range checks {
		s.out.human("%-5s %-14s %s", strings.ToUpper(string(c.Status)), c.ID, c.Detail)
		if c.Status == preflight.Pass {
			continue
		}
		if c.Hint != "" {
			s.out.human("      %-14s fix: %s", "", c.Hint)
		}
		for _, l := range c.Sudo {
			s.out.human("      %-14s run: %s", "", l)
		}
	}
	s.out.human("\n%d passed, %d warnings, %d failed, %d skipped", counts[preflight.Pass], counts[preflight.Warn], counts[preflight.Fail], counts[preflight.Skip])
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// appArmorFix gives a failing apparmor check the root commands that load
// the profile, rendering it into the config dir (onboarding.AppArmor; the
// preflight package cannot import onboarding). Only --fixes writes it.
func appArmorFix(s *session, checks []preflight.Check, configPath string) {
	for i, c := range checks {
		if c.ID != "apparmor" || !problem(c) {
			continue
		}
		r, err := onboarding.AppArmor(onboarding.AppArmorOptions{ConfigPath: configPath})
		if err != nil {
			s.out.diag("warning: %v", err)
			return
		}
		checks[i].Sudo = r.Commands
		checks[i].Hint += fmt.Sprintf(" (profile rendered to %s)", r.ProfileFile)
	}
}

// runtimeCheck labels the fake runtime, which simulates VMs.
func runtimeCheck(cfg config.Daemon) preflight.Check {
	c := preflight.Check{ID: "runtime", Title: "Runtime", Status: preflight.Pass, Detail: cfg.Runtime}
	if cfg.Runtime == "fake" {
		c.Status, c.Detail = preflight.Warn, "runtime is fake (development only): VMs are simulated"
		c.Hint = buildinfo.Name + " init --force on a KVM host"
	}
	return c
}

// serviceProbes are what the service check reads; tests replace them.
type serviceProbes struct {
	unit   string // the unit file `init --service` installs
	user   string
	active func(ctx context.Context) (string, error) // `systemctl --user is-active` of the unit
	linger func(ctx context.Context, user string) bool
}

func defaultServiceProbes() serviceProbes {
	p := serviceProbes{
		unit:   filepath.Join(onboarding.DefaultUnitDir(), buildinfo.Name+".service"),
		linger: onboarding.Lingering,
		active: func(ctx context.Context) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "systemctl", "--user", "is-active", buildinfo.Name+".service").Output()
			return strings.TrimSpace(string(out)), err
		},
	}
	if u, err := user.Current(); err == nil {
		p.user = u.Username
	}
	return p
}

// serviceCheck grades the systemd --user unit that keeps the daemon
// running: installed, active, and lingering so it survives logout and
// starts at boot.
func serviceCheck(ctx context.Context, p serviceProbes) preflight.Check {
	name := buildinfo.Name + ".service"
	c := preflight.Check{ID: "service", Title: "User service"}
	if _, err := os.Stat(p.unit); err != nil {
		c.Status, c.Detail = preflight.Skip, name+" is not installed; the daemon runs only while started by hand"
		c.Hint = buildinfo.Name + " init --service"
		return c
	}
	state, err := p.active(ctx)
	if state == "" {
		c.Status, c.Detail = preflight.Warn, "cannot ask systemctl --user about "+name
		if err != nil {
			c.Detail += ": " + err.Error()
		}
		c.Hint = "run doctor from a login session of " + cmp.Or(p.user, "this user") + " (not through sudo or su)"
		return c
	}
	c.Status, c.Detail = preflight.Pass, name+" is "+state
	if state != "active" {
		c.Status, c.Hint = preflight.Warn, "systemctl --user enable --now "+name+" (see journalctl --user -u "+name+")"
	}
	if !p.linger(ctx, p.user) {
		c.Status = preflight.Warn
		c.Detail += "; lingering is off: the daemon stops when you log out and does not start at boot"
		fix := "sudo loginctl enable-linger " + packaging.ShellQuote(cmp.Or(p.user, "NAME"))
		if c.Hint == "" {
			c.Hint = fix
		} else {
			c.Hint += ", then " + fix
		}
		if p.user != "" {
			c.Sudo = []string{fix}
		}
	}
	return c
}

func configCheck(cfg config.Daemon, path string) preflight.Check {
	c := preflight.Check{ID: "config", Title: "Configuration"}
	if path == "" {
		c.Status, c.Detail = preflight.Warn, "no config file; using built-in defaults (runtime "+cfg.Runtime+")"
		c.Hint = buildinfo.Name + " init"
		return c
	}
	if err := cfg.Validate(); err != nil {
		c.Status, c.Detail, c.Hint = preflight.Fail, err.Error(), "fix "+path+" or re-run `"+buildinfo.Name+" init --force`"
		return c
	}
	c.Status, c.Detail = preflight.Pass, path+" (runtime "+cfg.Runtime+")"
	return c
}

// daemonCheck asks the daemon serving this config (or --api-url) what it runs.
func (s *session) daemonCheck(ctx context.Context, cfg config.Daemon) preflight.Check {
	c := preflight.Check{ID: "daemon", Title: "Daemon"}
	cl := s.cl
	if !s.g.apiURLSet {
		var err error
		if cl, err = newClient("unix://"+cfg.Socket, s.out.requestID); err != nil {
			c.Status, c.Detail, c.Hint = preflight.Warn, err.Error(), `check "socket" in the config`
			return c
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var caps api.HostCapabilities
	if err := cl.get(ctx, "/capabilities", nil, &caps); err != nil {
		c.Status, c.Detail = preflight.Warn, "not reachable: "+err.Error()
		c.Hint = buildinfo.Name + " daemon   (or: systemctl --user start " + buildinfo.Name + ")"
		return c
	}
	c.Status = preflight.Pass
	c.Detail = "running " + caps.Version + ", runtime " + caps.Runtime.Engine + ", storage " + caps.Storage.Mode
	if caps.Runtime.Engine != cfg.Runtime {
		c.Status, c.Hint = preflight.Warn, "restart the daemon to apply the configured runtime "+cfg.Runtime
	}
	return c
}

// runInit onboards this host; see internal/onboarding.
func runInit(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 0, "init [flags]"); err != nil {
		return err
	}
	if o.apparmor {
		return runInitAppArmor(s, o)
	}
	if o.migrateLegacy {
		return runInitMigrate(ctx, s, o)
	}
	if o.stopVMs {
		return usageErr("--stop-vms needs --migrate-from-slipway")
	}
	if o.apparmorBinary != "" {
		return usageErr("--apparmor-binary needs --apparmor")
	}
	in := onboarding.Options{
		ConfigPath: o.config, DataDir: o.dataDir, Runtime: o.runtime, Storage: o.storage,
		Hostname: o.hostname, ControlURL: o.controlURL, Force: o.force, Service: o.service,
		OwnerLogins: o.ownerLogins, OwnerTags: o.ownerTags,
		SupportedRuntimes: daemon.Runtimes(),
	}
	for _, m := range o.images {
		in.Images = append(in.Images, config.ImageSource{Manifest: m, BlobDir: o.imageBlobs})
	}
	if o.imageBlobs != "" && len(o.images) == 0 {
		return usageErr("--image-blobs needs --image")
	}
	if o.authKeyStdin {
		key, err := io.ReadAll(io.LimitReader(s.io.Stdin, 4096))
		if err != nil {
			return clientErr(domain.CodeInvalidArgument, "reading auth key from stdin: %v", err)
		}
		in.AuthKey = key
	}
	if (in.ConfigPath != "" && !filepath.IsAbs(in.ConfigPath)) || (in.DataDir != "" && !filepath.IsAbs(in.DataDir)) {
		return usageErr("--config and --data-dir must be absolute paths")
	}
	r, err := onboarding.Init(ctx, in)
	if err != nil {
		return clientErr(domain.CodeInvalidArgument, "%v", err)
	}
	s.out.result("", r)
	verb := "kept"
	if r.ConfigWritten {
		verb = "wrote"
	}
	s.out.human("%s %s", verb, r.ConfigPath)
	s.out.human("runtime   %s (%s)", r.Runtime, r.RuntimeReason)
	if r.Runtime == "fake" {
		s.out.human("WARNING: %s", onboarding.FakeRuntimeWarning)
	}
	s.out.human("data      %s", r.DataDir)
	s.out.human("socket    %s", r.Socket)
	if r.AuthKeyWritten {
		s.out.human("auth key  stored in %s (0600)", r.AuthKeyFile)
	}
	if r.TailnetEnabled {
		s.out.human("tailnet   gateway enabled for %s", strings.Join(append(append([]string{}, r.OwnerLogins...), r.OwnerTags...), ", "))
	}
	for _, img := range r.Images {
		s.out.human("image     %s (blobs verified)", img)
	}
	if r.ServiceUnit != "" {
		s.out.human("service   %s (started: %v)", r.ServiceUnit, r.ServiceStarted)
	}
	for _, w := range r.Warnings {
		if !strings.HasPrefix(w, onboarding.FakeRuntimeWarning) { // printed above
			s.out.diag("warning: %s", w)
		}
	}
	s.out.human("\nNext steps:")
	for i, n := range r.NextSteps {
		s.out.human("  %d. %s", i+1, n)
	}
	return nil
}

// runInitMigrate moves a host set up before the slipway -> davit rename
// (onboarding.Migrate); the install that follows is an ordinary init.
func runInitMigrate(ctx context.Context, s *session, o *opts) error {
	if o.config == "" || o.dataDir == "" || !filepath.IsAbs(o.config) || !filepath.IsAbs(o.dataDir) {
		return usageErr("--migrate-from-slipway needs absolute --config and --data-dir (the davit paths)")
	}
	r, err := onboarding.Migrate(ctx, onboarding.MigrateOptions{ConfigPath: o.config, DataDir: o.dataDir, StopVMs: o.stopVMs})
	if err != nil {
		return clientErr(domain.CodeInvalidState, "%v", err)
	}
	s.out.result("", r)
	if !r.Migrated {
		s.out.human("nothing to migrate: %s", r.Reason)
		return nil
	}
	for _, vm := range r.StoppedVMs {
		s.out.human("stopped %s (disk kept)", vm)
	}
	if r.OldUnit != "" {
		s.out.human("retired %s", r.OldUnit)
	}
	s.out.human("moved %s -> %s", r.FromData, r.ToData)
	s.out.human("moved %s -> %s", r.FromConfig, r.ToConfig)
	return nil
}

// runInitAppArmor renders the AppArmor userns profile and prints the root
// commands that install it; see onboarding.AppArmor. It changes nothing
// else, so it refuses the flags of a full init.
func runInitAppArmor(s *session, o *opts) error {
	if o.dataDir != "" || o.runtime != "" || o.storage != "" || o.hostname != "" || o.controlURL != "" ||
		o.authKeyStdin || o.service || o.force || len(o.ownerLogins)+len(o.ownerTags)+len(o.images) > 0 || o.imageBlobs != "" {
		return usageErr("--apparmor runs on its own (only --config and --apparmor-binary apply); run the rest of init separately")
	}
	if (o.config != "" && !filepath.IsAbs(o.config)) || (o.apparmorBinary != "" && !filepath.IsAbs(o.apparmorBinary)) {
		return usageErr("--config and --apparmor-binary must be absolute paths")
	}
	r, err := onboarding.AppArmor(onboarding.AppArmorOptions{ConfigPath: o.config, Binary: o.apparmorBinary})
	if err != nil {
		return clientErr(domain.CodeInvalidArgument, "%v", err)
	}
	s.out.result("", r)
	s.out.human("wrote %s (AppArmor profile %q for %s):\n", r.ProfileFile, r.ProfileName, r.Binary)
	for _, l := range strings.Split(strings.TrimRight(r.Profile, "\n"), "\n") {
		s.out.human("%s", strings.TrimRight("    "+l, " "))
	}
	for _, w := range r.Warnings {
		s.out.diag("warning: %s", w)
	}
	s.out.human("\nRun once as root to install and load it (%s runs nothing as root itself):", buildinfo.Name)
	for _, c := range r.Commands {
		s.out.human("    %s", c)
	}
	s.out.human("\nTo remove it later:")
	for _, c := range r.RemoveCommands {
		s.out.human("    %s", c)
	}
	s.out.human("\nNext steps:")
	for i, n := range r.NextSteps {
		s.out.human("  %d. %s", i+1, n)
	}
	return nil
}
