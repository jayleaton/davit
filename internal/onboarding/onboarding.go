// Package onboarding implements `davit init` (SPEC §7): create the config
// and data directories, write a default config whose runtime matches what
// this host and build can actually run, store a tailnet auth key, optionally
// install a systemd --user unit, and say what to do next. AppArmor renders
// the userns profile for Ubuntu hosts (`init --apparmor`).
//
// Init is idempotent: re-running keeps an existing config (and any edited
// unit) unless Force is set, in which case the old config is backed up.
// It never echoes or logs the auth key.
package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"time"

	"davit/internal/buildinfo"
	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/gateway"
	"davit/internal/lifecycle"
	"davit/internal/preflight"
	"davit/packaging"
)

// Options configure Init. Zero values pick the platform defaults.
type Options struct {
	ConfigPath string
	DataDir    string
	Runtime    string // explicit runtime; empty chooses from host + build
	Storage    string
	Hostname   string
	ControlURL string
	// AuthKey, when non-nil, is a tailnet auth key to store (0600).
	AuthKey []byte
	// OwnerLogins and OwnerTags are added to tailnet.ownerLogins/ownerTags,
	// also in an existing config. When this run supplies an owner or a key
	// and the config then has both, tailnet.enabled is set.
	OwnerLogins []string
	OwnerTags   []string
	// Images are guest image manifests to add to the config (verified
	// against their blobs first). An empty BlobDir is found next to the
	// manifest (see ImageBlobDir).
	Images []config.ImageSource
	Force  bool
	// Service installs and starts a systemd --user unit (Linux).
	Service bool

	// SupportedRuntimes is what this build can run (daemon.Runtimes).
	SupportedRuntimes []string
	// KVM reports host KVM usability; default preflight.KVM.
	KVM func() preflight.Check
	// Binary is the executable the unit runs; default os.Executable.
	Binary string
	// UnitDir defaults to $XDG_CONFIG_HOME/systemd/user.
	UnitDir string
	// Systemctl runs `systemctl --user ARGS`; default execs systemctl.
	Systemctl func(ctx context.Context, args ...string) error
	// User is this user's login name; default from os/user.
	User string
	// Linger reports whether systemd keeps User's services running without
	// a session; default Lingering.
	Linger func(ctx context.Context, user string) bool
}

// Result reports what Init did. It never contains the auth key.
type Result struct {
	ConfigPath       string   `json:"configPath"`
	ConfigWritten    bool     `json:"configWritten"`
	ConfigBackup     string   `json:"configBackup,omitempty"`
	DataDir          string   `json:"dataDir"`
	Socket           string   `json:"socket"`
	Runtime          string   `json:"runtime"`
	RuntimeReason    string   `json:"runtimeReason"`
	AuthKeyFile      string   `json:"authKeyFile,omitempty"`
	AuthKeyWritten   bool     `json:"authKeyWritten"`
	TailnetEnabled   bool     `json:"tailnetEnabled"`
	OwnerLogins      []string `json:"ownerLogins,omitempty"`
	OwnerTags        []string `json:"ownerTags,omitempty"`
	ServiceUnit      string   `json:"serviceUnit,omitempty"`
	ServiceInstalled bool     `json:"serviceInstalled"`
	ServiceStarted   bool     `json:"serviceStarted"`
	// Images names the images verified and configured by this run.
	Images    []string `json:"images,omitempty"`
	Warnings  []string `json:"warnings"`
	NextSteps []string `json:"nextSteps"`
}

func (o *Options) defaults() error {
	if o.ConfigPath == "" {
		o.ConfigPath = config.DefaultConfigPath()
	}
	if o.DataDir == "" {
		o.DataDir = config.DefaultDataDir()
	}
	if o.KVM == nil {
		o.KVM = preflight.KVM
	}
	if o.Systemctl == nil {
		o.Systemctl = func(ctx context.Context, args ...string) error {
			out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
			}
			return nil
		}
	}
	if o.UnitDir == "" {
		o.UnitDir = DefaultUnitDir()
	}
	if o.User == "" {
		if u, err := user.Current(); err == nil {
			o.User = u.Username
		}
	}
	if o.Linger == nil {
		o.Linger = Lingering
	}
	for name, p := range map[string]string{"config path": o.ConfigPath, "data dir": o.DataDir} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("init: %s %q must be absolute", name, p)
		}
	}
	return nil
}

// DefaultUnitDir is where Init installs the systemd --user unit:
// $XDG_CONFIG_HOME/systemd/user, else ~/.config/systemd/user.
func DefaultUnitDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		h, _ := os.UserHomeDir()
		base = filepath.Join(h, ".config")
	}
	return filepath.Join(base, "systemd", "user")
}

// Lingering reports whether systemd keeps name's user services running with
// no session open (and starts them at boot): its linger file exists, or
// loginctl says so.
func Lingering(ctx context.Context, name string) bool {
	if name == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join("/var/lib/systemd/linger", name)); err == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "loginctl", "show-user", name, "--property=Linger", "--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

// FakeRuntimeWarning labels a config that runs the fake runtime.
const FakeRuntimeWarning = "runtime fake (development only): no real VMs"

// devOnlyFake is how init offers the fake runtime: only on request.
const devOnlyFake = "development only: --runtime fake starts no real VMs"

// chooseRuntime picks firecracker when KVM works and the build has it. On
// Linux anything else is an error naming the fix: the fake runtime is only
// chosen with an explicit --runtime fake. Other hosts (no KVM by design)
// still get the fake runtime with the reason.
func chooseRuntime(o Options) (string, string, error) {
	if o.Runtime != "" {
		if !slices.Contains(o.SupportedRuntimes, o.Runtime) {
			return "", "", fmt.Errorf("init: runtime %q is not available in this build (available: %v)", o.Runtime, o.SupportedRuntimes)
		}
		return o.Runtime, "requested with --runtime", nil
	}
	kvm := o.KVM()
	switch {
	case kvm.Status != preflight.Pass && goruntime.GOOS != "linux":
		return "fake", "no usable KVM (" + kvm.Detail + "); the fake runtime starts no real VMs", nil
	case kvm.Status != preflight.Pass:
		return "", "", fmt.Errorf("init: KVM is not usable: %s; fix: %s (%s)", kvm.Detail, kvm.Fix(), devOnlyFake)
	case !slices.Contains(o.SupportedRuntimes, "firecracker"):
		return "", "", fmt.Errorf("init: KVM works but this build has no firecracker runtime for %s/%s (%s)", goruntime.GOOS, goruntime.GOARCH, devOnlyFake)
	}
	return "firecracker", "KVM is usable", nil
}

// Init performs onboarding.
func Init(ctx context.Context, o Options) (*Result, error) {
	if err := o.defaults(); err != nil {
		return nil, err
	}
	r := &Result{ConfigPath: o.ConfigPath, DataDir: o.DataDir, Warnings: []string{}, NextSteps: []string{}}
	configDir := filepath.Dir(o.ConfigPath)
	for _, d := range []string{configDir, o.DataDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("init: %w", err)
		}
	}

	cfg, err := loadExisting(o.ConfigPath)
	switch {
	case err == nil && !o.Force:
		// Keep the user's config; report what it says.
		r.Runtime, r.RuntimeReason = cfg.Runtime, "from existing config (use --force to regenerate)"
		if o.Runtime != "" || o.Storage != "" || o.Hostname != "" || o.ControlURL != "" {
			r.Warnings = append(r.Warnings, "existing config kept; --runtime/--storage/--hostname/--control-url need --force (the old file is backed up)")
		}
	case err != nil && !errors.Is(err, os.ErrNotExist) && !o.Force:
		return nil, fmt.Errorf("init: existing config is invalid (fix it or re-run with --force to back it up and regenerate): %w", err)
	default:
		if cfg, err = newConfig(o, r); err != nil {
			return nil, err
		}
		if _, statErr := os.Stat(o.ConfigPath); statErr == nil {
			r.ConfigBackup = fmt.Sprintf("%s.bak.%d", o.ConfigPath, time.Now().Unix())
			if err := os.Rename(o.ConfigPath, r.ConfigBackup); err != nil {
				return nil, fmt.Errorf("init: back up config: %w", err)
			}
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		if err := writeFileAtomic(o.ConfigPath, append(b, '\n'), 0o600); err != nil {
			return nil, err
		}
		r.ConfigWritten = true
	}
	r.DataDir, r.Socket = cfg.DataDir, cfg.Socket
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	if err := os.MkdirAll(cfg.TailnetStateDir(), 0o700); err != nil {
		return nil, fmt.Errorf("init: %w", err)
	}
	if r.Runtime == "fake" {
		r.Warnings = append(r.Warnings, FakeRuntimeWarning+": "+r.RuntimeReason)
	}

	if err := storeAuthKey(o, cfg, configDir, r); err != nil {
		return nil, err
	}
	changed, err := applyTailnetOwner(o, &cfg, r)
	if err != nil {
		return nil, err
	}
	added, err := applyImages(ctx, o, &cfg, r)
	if err != nil {
		return nil, err
	}
	if changed || added {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		if err := writeFileAtomic(o.ConfigPath, append(b, '\n'), 0o600); err != nil {
			return nil, err
		}
		r.ConfigWritten = true
	}
	if o.Service {
		if err := installService(ctx, o, r); err != nil {
			return nil, err
		}
	}
	needAppArmor := false
	for _, c := range preflight.Run(ctx, preflight.FromConfig(cfg)).Checks {
		if c.Status == preflight.Fail {
			r.Warnings = append(r.Warnings, fmt.Sprintf("preflight %s failed: %s", c.ID, c.Detail))
		}
		if c.ID == "apparmor" && (c.Status == preflight.Fail || c.Status == preflight.Warn) {
			needAppArmor = true
		}
	}
	if needAppArmor {
		r.NextSteps = append(r.NextSteps, fmt.Sprintf("AppArmor blocks user namespaces here: %s init --apparmor prints a profile for this binary and the one-time root commands to load it", buildinfo.Name))
	}
	nextSteps(ctx, o, cfg, r)
	return r, nil
}

func loadExisting(path string) (config.Daemon, error) {
	if _, err := os.Stat(path); err != nil {
		return config.Daemon{}, err
	}
	return config.Load(path)
}

func newConfig(o Options, r *Result) (config.Daemon, error) {
	rt, why, err := chooseRuntime(o)
	if err != nil {
		return config.Daemon{}, err
	}
	r.Runtime, r.RuntimeReason = rt, why
	c := config.Defaults()
	c.DataDir, c.Runtime, c.Storage = o.DataDir, rt, o.Storage
	if goruntime.GOOS == "darwin" || os.Getenv("XDG_RUNTIME_DIR") == "" {
		c.Socket = filepath.Join(o.DataDir, buildinfo.Name+".sock")
	}
	c.Tailnet = config.Tailnet{
		StateDir:    filepath.Join(o.DataDir, "tailnet"),
		Hostname:    o.Hostname,
		ControlURL:  o.ControlURL,
		AuthKeyFile: filepath.Join(filepath.Dir(o.ConfigPath), "tailnet-authkey"),
	}
	if rt != "fake" {
		if total, err := preflight.TotalMemory(); err == nil {
			c.Limits = memoryReservation(total)
		}
	}
	return c, c.Validate()
}

// ReservedHeadroom is the host memory a new VM-host config keeps out of the
// VMs' reservation budget, for the host itself and the daemon.
const ReservedHeadroom = 2 * domain.GiB

// memoryReservation is the reservation of a new VM-host config on a host
// with total bytes of memory: total less ReservedHeadroom, overcommitted by
// the default factor (the live VMs' declared memory may sum to 1.5× that,
// since golden-restored VMs share pages). A host too small for headroom
// gets no reservation (the free-memory floor still applies).
func memoryReservation(total int64) domain.Limits {
	if total < 2*ReservedHeadroom {
		return domain.Limits{}
	}
	return domain.Limits{MemoryBytes: total - ReservedHeadroom, MemoryOvercommit: domain.DefaultMemoryOvercommit}
}

// ImageBlobDir finds the blob directory (holding sha256/<hex>) for a
// manifest when none is given: next to it (release layout), or the images
// pipeline's out/blobs (images/manifests/*.json + images/out/blobs).
func ImageBlobDir(manifest string) (string, error) {
	dir := filepath.Dir(manifest)
	for _, c := range []string{filepath.Join(dir, "blobs"), filepath.Join(dir, "..", "blobs"), filepath.Join(dir, "..", "out", "blobs")} {
		if fi, err := os.Stat(filepath.Join(c, "sha256")); err == nil && fi.IsDir() {
			return filepath.Clean(c), nil
		}
	}
	return "", fmt.Errorf("no blob directory (with sha256/) next to %s; pass --image-blobs", manifest)
}

// applyImages verifies each requested image against its blobs (with the
// daemon's verification cache, so the daemon does not hash them again) and
// adds the new ones to cfg.Images.
func applyImages(ctx context.Context, o Options, cfg *config.Daemon, r *Result) (bool, error) {
	added := false
	for _, img := range o.Images {
		man, err := filepath.Abs(img.Manifest)
		if err != nil {
			return false, err
		}
		blobs := img.BlobDir
		if blobs == "" {
			if blobs, err = ImageBlobDir(man); err != nil {
				return false, fmt.Errorf("init: %w", err)
			}
		}
		if blobs, err = filepath.Abs(blobs); err != nil {
			return false, err
		}
		cat, err := lifecycle.LoadManifestCatalog(ctx, filepath.Join(cfg.DataDir, "images", "cache"),
			lifecycle.ManifestSource{Manifest: man, BlobDir: blobs})
		if err != nil {
			return false, fmt.Errorf("init: image %s: %w", man, err)
		}
		def, err := cat.Resolve(ctx, "")
		if err != nil {
			return false, err
		}
		r.Images = append(r.Images, def.Ref)
		if slices.ContainsFunc(cfg.Images, func(x config.ImageSource) bool { return x.Manifest == man }) {
			continue
		}
		cfg.Images = append(cfg.Images, config.ImageSource{Manifest: man, BlobDir: blobs})
		added = true
	}
	return added, nil
}

// applyTailnetOwner merges the requested owners into cfg and enables the
// gateway once it has an owner and an auth key. A run that supplies neither
// owners nor a key never changes tailnet.enabled, so plain re-runs stay
// no-ops and a gateway the user disabled stays disabled.
func applyTailnetOwner(o Options, cfg *config.Daemon, r *Result) (bool, error) {
	if len(o.OwnerLogins)+len(o.OwnerTags) > 0 {
		if err := (gateway.OwnerPolicy{Logins: o.OwnerLogins, Tags: o.OwnerTags}).Validate(); err != nil {
			return false, fmt.Errorf("init: %w", err)
		}
	}
	t, changed := &cfg.Tailnet, false
	for _, l := range o.OwnerLogins {
		if !slices.ContainsFunc(t.OwnerLogins, func(x string) bool { return strings.EqualFold(x, l) }) {
			t.OwnerLogins, changed = append(t.OwnerLogins, l), true
		}
	}
	for _, tag := range o.OwnerTags {
		if !slices.Contains(t.OwnerTags, tag) {
			t.OwnerTags, changed = append(t.OwnerTags, tag), true
		}
	}
	supplied := len(o.OwnerLogins)+len(o.OwnerTags) > 0 || o.AuthKey != nil
	if !t.Enabled && supplied && len(t.OwnerLogins)+len(t.OwnerTags) > 0 && authKeyReady(*cfg, r) {
		t.Enabled, changed = true, true
	}
	r.TailnetEnabled, r.OwnerLogins, r.OwnerTags = t.Enabled, t.OwnerLogins, t.OwnerTags
	return changed, nil
}

// authKeyReady reports whether the gateway can enroll: a stored or
// environment key, or a node that is already enrolled.
func authKeyReady(cfg config.Daemon, r *Result) bool {
	if r.AuthKeyWritten || os.Getenv(config.AuthKeyEnv) != "" {
		return true
	}
	for _, p := range []string{cfg.Tailnet.AuthKeyFile, filepath.Join(cfg.TailnetStateDir(), "tailscaled.state")} {
		if fi, err := os.Stat(p); p != "" && err == nil && fi.Size() > 0 {
			return true
		}
	}
	return false
}

// storeAuthKey writes a provided key 0600 to the configured key file.
func storeAuthKey(o Options, cfg config.Daemon, configDir string, r *Result) error {
	r.AuthKeyFile = cfg.Tailnet.AuthKeyFile
	if o.AuthKey == nil {
		return nil
	}
	key := bytes.TrimSpace(o.AuthKey)
	if len(key) == 0 || bytes.ContainsAny(key, "\r\n\x00 ") {
		return errors.New("init: the auth key on stdin must be a single non-empty token")
	}
	path := cfg.Tailnet.AuthKeyFile
	if path == "" {
		path = filepath.Join(configDir, "tailnet-authkey")
		r.AuthKeyFile = path
		r.Warnings = append(r.Warnings, fmt.Sprintf("existing config has no tailnet.authKeyFile; add \"authKeyFile\": %q under \"tailnet\"", path))
	}
	if err := writeFileAtomic(path, append(key, '\n'), 0o600); err != nil {
		return err
	}
	r.AuthKeyWritten = true
	return nil
}

func installService(ctx context.Context, o Options, r *Result) error {
	if goruntime.GOOS != "linux" {
		return fmt.Errorf("init --service: only systemd (Linux) is supported; launchd support comes with the Mac backend")
	}
	bin := o.Binary
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if bin, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
	}
	unit, err := packaging.SystemdUserUnit(packaging.UnitParams{
		Name: buildinfo.Name, Binary: bin, Config: o.ConfigPath, AuthKeyEnv: config.AuthKeyEnv,
		EnvFile: filepath.Join(filepath.Dir(o.ConfigPath), "daemon.env"),
	})
	if err != nil {
		return err
	}
	path := filepath.Join(o.UnitDir, buildinfo.Name+".service")
	r.ServiceUnit = path
	cur, err := os.ReadFile(path)
	switch {
	case err == nil && string(cur) == unit:
	case err == nil && !o.Force:
		r.Warnings = append(r.Warnings, path+" differs from the generated unit and was kept; use --force to replace it")
	default:
		if err := os.MkdirAll(o.UnitDir, 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(path, []byte(unit), 0o644); err != nil {
			return err
		}
	}
	r.ServiceInstalled = true
	name := buildinfo.Name + ".service"
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", name}} {
		if err := o.Systemctl(ctx, args...); err != nil {
			r.Warnings = append(r.Warnings, "could not start the service: "+err.Error())
			r.NextSteps = append(r.NextSteps, fmt.Sprintf("systemctl --user daemon-reload && systemctl --user enable --now %s", name))
			return nil
		}
	}
	r.ServiceStarted = true
	return nil
}

func nextSteps(ctx context.Context, o Options, cfg config.Daemon, r *Result) {
	n := buildinfo.Name
	step := func(s string) { r.NextSteps = append(r.NextSteps, s) }
	if cfg.Runtime != "fake" && len(cfg.Images) == 0 {
		if found, _ := filepath.Glob(filepath.Join(cfg.DataDir, "images", "manifests", "*.json")); len(found) == 0 {
			step(fmt.Sprintf("Add a guest image (built with images/build.sh, or a release image): %s init --image <manifest.json> [--image-blobs <dir>]", n))
		}
	}
	switch {
	case !r.ServiceStarted && goruntime.GOOS == "linux":
		step(fmt.Sprintf("Start the daemon: %s daemon   (or install it as a user service: %s init --service)", n, n))
	case !r.ServiceStarted:
		step(fmt.Sprintf("Start the daemon: %s daemon", n))
	case o.User != "" && !o.Linger(ctx, o.User):
		step(fmt.Sprintf("Keep the daemon running after you log out, and start it at boot (systemd stops user services with your last session): sudo loginctl enable-linger %s", packaging.ShellQuote(o.User)))
	}
	step(fmt.Sprintf("Check the host: %s doctor", n))
	step(fmt.Sprintf("Create your first VM: %s new", n))

	// The tailnet gateway is optional (SSH and the local socket need none
	// of it): at most one closing line about it.
	keyReady := authKeyReady(cfg, r)
	owners := len(cfg.Tailnet.OwnerLogins)+len(cfg.Tailnet.OwnerTags) > 0
	switch {
	case cfg.Tailnet.Enabled:
		step(fmt.Sprintf("Tailnet gateway enabled for %s: once the daemon (re)starts it serves the API and `ssh <vm>@<host>` on your tailnet; everyone else is refused",
			strings.Join(append(slices.Clone(cfg.Tailnet.OwnerLogins), cfg.Tailnet.OwnerTags...), ", ")))
	case owners && keyReady:
		step(`Optional: tailnet owners and key are set but the gateway is disabled; set "enabled": true under "tailnet" to reach this host over Tailscale`)
	case owners:
		step(fmt.Sprintf("Optional: finish the Tailscale gateway by storing an auth key: %s init --auth-key-stdin < key", n))
	case keyReady:
		step(fmt.Sprintf("Optional: reach this host over Tailscale: %s init --owner-login you@example.com", n))
	default:
		step(fmt.Sprintf("Optional: reach this host over Tailscale: %s init --owner-login you@example.com --auth-key-stdin < key", n))
	}
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = tmp.Chmod(mode)
	if err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
