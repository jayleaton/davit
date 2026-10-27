// Package config holds the daemon configuration file format and the default
// host paths shared by the daemon and the CLI.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/network"
	"davit/internal/network/netguard"
	"davit/internal/network/reverse"
)

// Duration is a time.Duration that reads and writes as "1h30m".
// Runs configures one-command runs.
type Runs struct {
	// OutputGrace is how long a run's VM outlives its command so a client
	// can still read the output (default 5s; negative: none). A run's
	// outputGraceSeconds overrides it.
	OutputGrace Duration `json:"outputGrace,omitempty"`
}

type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

// Daemon is the daemon configuration file (JSON).
type Daemon struct {
	// DataDir holds state.db, storage/, runtime/ and images/.
	DataDir string `json:"dataDir"`
	// Socket is the owner-only local API socket.
	Socket string `json:"socket"`
	// Runtime selects the engine adapter: one of KnownRuntimes that this
	// build supports (daemon.Runtimes).
	Runtime string `json:"runtime"`
	// Images lists verified image manifests; the first is the default. Empty
	// means every manifest in <dataDir>/images/manifests with blobs in
	// <dataDir>/images/blobs, or (fake runtime only) a non-bootable
	// placeholder when there are none.
	Images []ImageSource `json:"images"`
	// Storage is "" (probe: reflink if it works, else copy), "reflink" or "copy".
	Storage    string        `json:"storage"`
	Limits     domain.Limits `json:"limits"`
	DefaultTTL Duration      `json:"defaultTTL"`
	StopGrace  Duration      `json:"stopGrace"`
	// StopAttempts is how many times a graceful stop asks the guest to shut
	// down, stopGrace each (0: 3; at most 10). After the last one the VMM is
	// killed and the snapshot is crash-consistent, unless
	// StopForceAfterRetries is false (then the stop fails, as before).
	StopAttempts          int   `json:"stopAttempts,omitempty"`
	StopForceAfterRetries *bool `json:"stopForceAfterRetries,omitempty"`
	// TTLWarning is how long before an auto-stop the host warns (VM
	// autoStopWarningAt, a guest broadcast); default 10m, negative disables.
	TTLWarning Duration `json:"ttlWarning,omitempty"`
	// AutoDelete configures the low-disk cleanup's deletion of unused boxes
	// (docs/vm-host.md#disk-space). The floor it frees to is
	// limits.minFreeDiskBytes / limits.minFreeDiskPercent.
	AutoDelete AutoDelete `json:"autoDelete,omitempty"`
	// ColdStorage is the optional cold storage tier: stopped VMs unused
	// for a while move there from the data dir (docs/storage-tiers.md).
	ColdStorage ColdStorage `json:"coldStorage,omitempty"`
	// Runs configures one-command runs (createRun).
	Runs Runs `json:"runs,omitempty"`
	// Profiles are administrator-defined VM shapes for `new/fork --type`.
	Profiles    []domain.Profile `json:"profiles,omitempty"`
	Fake        FakeRuntime      `json:"fake"`
	Firecracker Firecracker      `json:"firecracker"`
	Mac         Mac              `json:"mac"`
	Network     Network          `json:"network"`
	Tailnet     Tailnet          `json:"tailnet"`
	Harness     Harness          `json:"harness"`
	Telemetry   Telemetry        `json:"telemetry"`
	// LogLevel is debug, info, warn or error.
	LogLevel string `json:"logLevel"`
	// ReleaseBaseURL is a release directory `davit version --check` reads
	// (its release.json) instead of the published GitHub releases, like
	// install.sh's --from; DAVIT_RELEASE_URL overrides it. https:// or
	// file://. Nothing is ever installed automatically.
	ReleaseBaseURL string `json:"releaseBaseUrl,omitempty"`
}

// AutoDelete configures the low-disk cleanup's deletion of unused boxes.
type AutoDelete struct {
	// Enabled turns auto-delete on (default true). Off, the cleanup still
	// collects garbage and stops idle boxes.
	Enabled *bool `json:"enabled,omitempty"`
	// InstanceMinAge and ImageMinAge are how long a stopped box instance,
	// or a project image without instances, must have been unused before
	// it may be deleted (default 72h and 24h).
	InstanceMinAge Duration `json:"instanceMinAge,omitempty"`
	ImageMinAge    Duration `json:"imageMinAge,omitempty"`
	// MarginBytes is how far above the floor the cleanup frees before it
	// stops (default 2 GiB).
	MarginBytes int64 `json:"marginBytes,omitempty"`
	// BlobMinAge is how long an unreferenced image blob must have been
	// unchanged before the cleanup or `box image prune` removes it
	// (default 1h).
	BlobMinAge Duration `json:"blobMinAge,omitempty"`
}

// ColdStorage configures the cold storage tier.
type ColdStorage struct {
	// Dir is a directory on another, larger filesystem (XFS with
	// reflink=1, like the data dir); empty: no cold tier.
	Dir string `json:"dir,omitempty"`
	// After is how long a stopped VM must be unused before it moves
	// (default 6h).
	After Duration `json:"after,omitempty"`
	// BytesPerSecond caps a move's disk reads (default 100 MiB/s;
	// negative: unlimited).
	BytesPerSecond int64 `json:"bytesPerSecond,omitempty"`
	// MinBytes is the least data of its own a VM's disk must hold to move
	// (default 512 MiB; negative: any).
	MinBytes int64 `json:"minBytes,omitempty"`
	// MinFreeBytes and MinFreePercent are the cold filesystem's floor, the
	// larger of the two (default 5 GiB and 10%; minFreeBytes negative: no
	// floor). Moves never take it below; under it, unused cold boxes are
	// auto-deleted.
	MinFreeBytes   int64 `json:"minFreeBytes,omitempty"`
	MinFreePercent int   `json:"minFreePercent,omitempty"`
	// IOBusyPercent is the host I/O pressure (Linux PSI "some avg10",
	// percent of time some task waited for I/O) above which moves wait
	// (default 20; negative: never wait).
	IOBusyPercent int `json:"ioBusyPercent,omitempty"`
	// Drain moves nothing more to cold storage and brings every cold box
	// back to the data dir, one at a time, to empty the tier.
	Drain bool `json:"drain,omitempty"`
}

// Mac configures the macOS runtime (Virtualization.framework through a
// signed per-VM helper; docs/research/MAC-BACKEND.md).
type Mac struct {
	// Helper is the signed VZ helper executable. Empty means
	// davit-vz-helper next to the davit executable.
	Helper string `json:"helper,omitempty"`
	// RuntimeDir holds per-VM helper directories and their sockets (default
	// <dataDir>/runtime/mac). macOS limits socket paths to 103 bytes; the
	// daemon refuses a directory that is too long.
	RuntimeDir string `json:"runtimeDir,omitempty"`
}

// MacHelperName is the helper's file name next to the davit executable.
const MacHelperName = "davit-vz-helper"

// MacHelperPath resolves the configured helper (empty: next to exe).
func (c Daemon) MacHelperPath(exe string) string {
	if c.Mac.Helper != "" {
		return c.Mac.Helper
	}
	return filepath.Join(filepath.Dir(exe), MacHelperName)
}

// MacRuntimeDir is the mac runtime's directory.
func (c Daemon) MacRuntimeDir() string {
	if c.Mac.RuntimeDir != "" {
		return c.Mac.RuntimeDir
	}
	return filepath.Join(c.DataDir, "runtime", "mac")
}

// Firecracker configures the Firecracker runtime.
type Firecracker struct {
	// Binary is an explicit firecracker path. Empty means the pinned,
	// checksum-verified release under <dataDir>/bin, fetched on first use.
	Binary string `json:"binary,omitempty"`
	// CheckpointDir holds live-fork RAM checkpoints (default
	// <dataDir>/runtime/firecracker/checkpoints). A tmpfs makes the source's
	// pause short and predictable at the cost of pinning each live-forked
	// child's RAM image in host memory.
	CheckpointDir string `json:"checkpointDir,omitempty"`
	// GoldenMinFreeBytes is the free space the daemon keeps on the data
	// dir's filesystem by evicting least-recently-used goldens (keeping the
	// newest of each image); a capture that would go below it is refused
	// (the create cold boots). Zero means 10 GiB; negative disables it.
	GoldenMinFreeBytes int64 `json:"goldenMinFreeBytes,omitempty"`
}

// Network selects how VMs get a guest NIC (internal/network).
type Network struct {
	// Mode is "rootless" (per-VM user+net namespace with tap0; the default on
	// Linux when the host allows it) or "none" (no guest NIC, vsock only).
	// Guest internet egress uses pasta or slirp4netns when installed (with
	// nft for the deny list), else the built-in userspace stack; doctor
	// reports which.
	Mode string `json:"mode,omitempty"`
	// Egress is the default egress policy of new VMs (forks inherit their
	// source's); empty means filtered. Per VM it is set at create/fork and
	// changed with `davit egress` (PATCH /vms/{id}).
	Egress *domain.Egress `json:"egress,omitempty"`
	// LANServices are LAN services every VM may reach although the LAN is
	// denied: "tcp/192.168.1.10:5432" or "udp/ADDRESS:PORT", one address
	// and port each (network.ParseService). An allowlist-mode VM also needs
	// the address on its own allowlist. The host's own addresses, metadata
	// and the tailnet can never be listed.
	LANServices []string `json:"lanServices,omitempty"`
	// GuestSubnets are private IPv4 subnets guests route without
	// masquerading (e.g. a Docker bridge with masquerading off); their
	// traffic gets the same egress policy. Docker's default bridge
	// masquerades and needs nothing here.
	GuestSubnets []string `json:"guestSubnets,omitempty"`
	// ReverseHostServices are host services ("127.0.0.1:5432", ip:port) the
	// owner may make reachable from inside a VM with a reverse forward
	// (`davit forward --reverse --host-service`). Nothing else on the host
	// can be a reverse forward target; it does not depend on egress.
	ReverseHostServices []string `json:"reverseHostServices,omitempty"`
	// AllowPrivateUpstreams lets credential-proxy upstreams and webhook
	// URLs reach loopback, private (RFC 1918), tailnet (CGNAT, ULA)
	// addresses and plain-http credential upstreams, e.g. a model server or
	// a webhook receiver on the LAN. Off by default: the daemon then refuses
	// them at connect time (after DNS). Link-local and cloud metadata
	// addresses are refused either way.
	AllowPrivateUpstreams bool `json:"allowPrivateUpstreams,omitempty"`
}

// ReversePolicy is the parsed ReverseHostServices.
func (n Network) ReversePolicy() (reverse.Policy, error) {
	var p reverse.Policy
	for _, s := range n.ReverseHostServices {
		ap, err := netip.ParseAddrPort(s)
		if err != nil {
			return p, fmt.Errorf("config: network.reverseHostServices: %q: want ip:port: %v", s, err)
		}
		if ap.Addr().IsUnspecified() || ap.Port() == 0 {
			return p, fmt.Errorf("config: network.reverseHostServices: %q: needs a specific address and port", s)
		}
		if err := (netguard.Policy{AllowPrivate: true}).Check(ap.Addr()); err != nil {
			return p, fmt.Errorf("config: network.reverseHostServices: %q: link-local, metadata, multicast and reserved addresses are refused", s)
		}
		p.HostServices = append(p.HostServices, ap)
	}
	return p, nil
}

// Apply adds the LAN services and guest subnets to a network config.
func (n Network) Apply(c *network.Config) error {
	svcs, err := network.ParseServices(n.LANServices)
	if err != nil {
		return fmt.Errorf("config: network.lanServices: %w", err)
	}
	c.Egress.Services = svcs
	c.GuestSubnets = nil
	for _, s := range n.GuestSubnets {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("config: network.guestSubnets: %q: %v", s, err)
		}
		c.GuestSubnets = append(c.GuestSubnets, p)
	}
	if err := c.Validate(); err != nil {
		return fmt.Errorf("config: network: %w", err)
	}
	return nil
}

// DefaultEgress is the normalized host default egress policy.
func (n Network) DefaultEgress() domain.Egress {
	if n.Egress == nil {
		return domain.DefaultEgress()
	}
	e, err := n.Egress.Normalize()
	if err != nil {
		return domain.DefaultEgress() // Validate rejects this config first
	}
	return e
}

// Tailnet configures the persistent tsnet gateway node (internal/gateway).
type Tailnet struct {
	// Enabled starts the gateway beside the Unix socket.
	Enabled bool `json:"enabled,omitempty"`
	// StateDir holds the node identity; default <dataDir>/tailnet, 0700.
	StateDir string `json:"stateDir,omitempty"`
	// Hostname is the requested tailnet hostname (the registered one may differ).
	Hostname string `json:"hostname,omitempty"`
	// ControlURL selects the coordination server; empty means Tailscale's.
	ControlURL string `json:"controlURL,omitempty"`
	// AuthKeyFile holds a one-time enrollment key (0600). The AuthKeyEnv
	// variable is the alternative. Neither is needed once StateDir is enrolled.
	AuthKeyFile string `json:"authKeyFile,omitempty"`
	// OwnerLogins and OwnerTags name the tailnet identities that act as the
	// host owner; everyone else on the tailnet is refused. Enabled requires
	// at least one.
	OwnerLogins []string `json:"ownerLogins,omitempty"`
	OwnerTags   []string `json:"ownerTags,omitempty"`
	// HTTPS serves the API on :443 with a tailnet certificate (the tailnet
	// must have HTTPS enabled); otherwise :80, encrypted by WireGuard.
	HTTPS bool `json:"https,omitempty"`
	// TLSCertFile and TLSKeyFile serve the API on :443 with an operator
	// certificate instead (a private CA, Headscale, or a tailnet without
	// HTTPS certificates); clients trust its CA via DAVIT_CA_FILE. Both or
	// neither; reloaded when the files change.
	TLSCertFile string `json:"tlsCertFile,omitempty"`
	TLSKeyFile  string `json:"tlsKeyFile,omitempty"`
	// AppPorts is the gateway port range app endpoints (`davit expose`)
	// get their stable URLs from, "FIRST-LAST"; default 10000-10999.
	AppPorts string `json:"appPorts,omitempty"`
	// DefaultVisibility is who sees a new VM: "private" (the default, only
	// the owner until it shares it) or "members" (every member, at each
	// member's own role). VMs that existed before members were enabled
	// stay private.
	DefaultVisibility string `json:"defaultVisibility,omitempty"`
}

// AppPortRange parses AppPorts; zero when unset.
func (t Tailnet) AppPortRange() ([2]int, error) {
	if t.AppPorts == "" {
		return [2]int{}, nil
	}
	a, b, ok := strings.Cut(t.AppPorts, "-")
	lo, err1 := strconv.Atoi(strings.TrimSpace(a))
	hi, err2 := strconv.Atoi(strings.TrimSpace(b))
	if !ok || err1 != nil || err2 != nil || lo < 1 || hi > 65535 || lo > hi {
		return [2]int{}, fmt.Errorf("config: tailnet.appPorts must be FIRST-LAST within 1-65535, got %q", t.AppPorts)
	}
	for _, reserved := range []int{22, 80, 443} {
		if lo <= reserved && reserved <= hi {
			return [2]int{}, fmt.Errorf("config: tailnet.appPorts %q overlaps the gateway's port %d", t.AppPorts, reserved)
		}
	}
	return [2]int{lo, hi}, nil
}

// AuthKeyEnv is the environment variable that may carry a tailnet auth key.
var AuthKeyEnv = buildinfo.Env("TS_AUTHKEY")

// TailnetStateDir returns the effective tsnet state directory.
func (c Daemon) TailnetStateDir() string {
	if c.Tailnet.StateDir != "" {
		return c.Tailnet.StateDir
	}
	return filepath.Join(c.DataDir, "tailnet")
}

// KnownRuntimes are the runtime names a config may select. Whether this
// build can run one is the daemon's decision (daemon.Runtimes).
var KnownRuntimes = []string{"fake", "firecracker", "mac"}

// Telemetry configures optional OTLP export of the host event stream and a
// few gauges (G16). Off unless otlpEndpoint is set.
type Telemetry struct {
	// OTLPEndpoint is an OTLP/HTTP collector base URL (http://host:4318);
	// events go to /v1/logs, gauges to /v1/metrics, JSON-encoded.
	OTLPEndpoint string `json:"otlpEndpoint,omitempty"`
	// OTLPHeaders are sent with every export (e.g. collector auth).
	OTLPHeaders map[string]string `json:"otlpHeaders,omitempty"`
	// IntervalSeconds batches and samples (default 10).
	IntervalSeconds int `json:"intervalSeconds,omitempty"`
}

// Harness configures `prompt` (coding-agent harnesses run in VMs).
type Harness struct {
	// Commands overrides a harness executable inside the guest, by harness
	// name ("claude", "codex", "opencode", "pi"); default the bare name via a login shell.
	Commands map[string]string `json:"commands,omitempty"`
	// SecretsFile is a JSON object {"NAME": "value"} (owner-only, 0600)
	// that prompt --secret NAME references. Values are passed to the
	// harness process in the guest as environment variables: they enter the
	// guest. Read on every use, so edits apply without a restart.
	SecretsFile string `json:"secretsFile,omitempty"`
	// DefaultTurnTimeout bounds a prompt turn that sets no
	// turnTimeoutSeconds (default 1h, at most MaxTurnTimeout). The guest
	// kills the harness at the deadline; until then the turn holds the VM's
	// auto-stop.
	DefaultTurnTimeout Duration `json:"defaultTurnTimeout,omitempty"`
}

// MaxTurnTimeout caps harness.defaultTurnTimeout and a prompt's
// turnTimeoutSeconds.
const MaxTurnTimeout = 24 * time.Hour

// FakeRuntime tunes the fake adapter (tests and demos).
type FakeRuntime struct {
	BootDelay Duration `json:"bootDelay"`
	StopDelay Duration `json:"stopDelay"`
	// GuestAgent runs an in-process guest agent per instance so exec works
	// against the fake. Its "guest" is the host: processes run on the host
	// as the daemon user. Development and tests only.
	GuestAgent bool `json:"guestAgent"`
}

// ImageSource is one image manifest (images/manifests/*.json) and the
// directory holding its blobs as sha256/<hex>.
type ImageSource struct {
	Manifest string `json:"manifest"`
	BlobDir  string `json:"blobDir"`
}

// Defaults returns the configuration used when no file is given.
func Defaults() Daemon {
	return Daemon{
		DataDir:    DefaultDataDir(),
		Socket:     DefaultSocket(),
		Runtime:    "fake",
		DefaultTTL: Duration(time.Hour),
		StopGrace:  Duration(30 * time.Second),
		LogLevel:   "info",
	}
}

// Load reads path over Defaults. Unknown fields are errors.
func Load(path string) (Daemon, error) {
	c := Defaults()
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	return c, c.Validate()
}

// Validate checks c.
func (c Daemon) Validate() error {
	if u := c.ReleaseBaseURL; u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "file://") {
		return fmt.Errorf("config: releaseBaseUrl must start with https:// or file://")
	}
	seen := map[string]bool{}
	for _, p := range c.Profiles {
		if !domain.ValidName(p.Name) || seen[p.Name] {
			return fmt.Errorf("config: profile name %q must be unique, 1-63 lowercase letters, digits and '-'", p.Name)
		}
		seen[p.Name] = true
		if p.Shape.CPUs < 0 || p.Shape.MemoryBytes < 0 || p.Shape.DiskBytes < 0 {
			return fmt.Errorf("config: profile %q has a negative shape value", p.Name)
		}
	}
	switch {
	case c.DataDir == "" || !filepath.IsAbs(c.DataDir):
		return fmt.Errorf("config: dataDir must be an absolute path")
	case c.Socket == "" || !filepath.IsAbs(c.Socket):
		return fmt.Errorf("config: socket must be an absolute path")
	case !slices.Contains(KnownRuntimes, c.Runtime):
		return fmt.Errorf("config: unknown runtime %q (known: %v)", c.Runtime, KnownRuntimes)
	case c.Network.Egress != nil && func() bool { _, err := c.Network.Egress.Normalize(); return err != nil }():
		_, err := c.Network.Egress.Normalize()
		return fmt.Errorf("config: network.egress: %v", err)
	case func() bool { nc := network.DefaultConfig(); return c.Network.Apply(&nc) != nil }():
		nc := network.DefaultConfig()
		return c.Network.Apply(&nc)
	case func() bool { _, err := c.Network.ReversePolicy(); return err != nil }():
		_, err := c.Network.ReversePolicy()
		return err
	case c.Harness.SecretsFile != "" && !filepath.IsAbs(c.Harness.SecretsFile):
		return fmt.Errorf("config: harness.secretsFile must be an absolute path")
	case c.Harness.DefaultTurnTimeout != 0 && (c.Harness.DefaultTurnTimeout < Duration(time.Second) || c.Harness.DefaultTurnTimeout > Duration(MaxTurnTimeout)):
		return fmt.Errorf("config: harness.defaultTurnTimeout must be 1s..%s", MaxTurnTimeout)
	case c.Network.Mode != "" && c.Network.Mode != "rootless" && c.Network.Mode != "none":
		return fmt.Errorf("config: network.mode must be \"rootless\" or \"none\" (privileged mode is not wired yet)")
	case c.Storage != "" && c.Storage != "reflink" && c.Storage != "copy":
		return fmt.Errorf("config: storage must be \"\", \"reflink\" or \"copy\"")
	case c.StopAttempts < 0 || c.StopAttempts > 10:
		return fmt.Errorf("config: stopAttempts must be 1..10 (0: the default, 3)")
	case c.Limits.MinFreeDiskPercent > 90:
		return fmt.Errorf("config: limits.minFreeDiskPercent must be at most 90 (0: the default, %d; negative: bytes only)", domain.DefaultMinFreeDiskPercent)
	case c.Limits.DiskHeadroomPercent > 100:
		return fmt.Errorf("config: limits.diskHeadroomPercent must be at most 100 (0: the default, %d; negative: none)", domain.DefaultDiskHeadroomPercent)
	case c.AutoDelete.InstanceMinAge < 0 || c.AutoDelete.ImageMinAge < 0 || c.AutoDelete.MarginBytes < 0 || c.AutoDelete.BlobMinAge < 0:
		return fmt.Errorf("config: autoDelete.instanceMinAge, imageMinAge, marginBytes and blobMinAge must not be negative (0: the defaults)")
	case c.ColdStorage.Dir != "" && !filepath.IsAbs(c.ColdStorage.Dir):
		return fmt.Errorf("config: coldStorage.dir must be an absolute path")
	case c.ColdStorage.Dir != "" && (within(c.ColdStorage.Dir, c.DataDir) || within(c.DataDir, c.ColdStorage.Dir)):
		return fmt.Errorf("config: coldStorage.dir and dataDir must not contain each other (cold storage belongs on another filesystem)")
	case c.ColdStorage.After < 0:
		return fmt.Errorf("config: coldStorage.after must not be negative (0: the default, 6h)")
	case c.ColdStorage.MinFreePercent > 90:
		return fmt.Errorf("config: coldStorage.minFreePercent must be at most 90 (0: the default, %d; negative: bytes only)", domain.DefaultMinFreeDiskPercent)
	case c.ColdStorage.Dir == "" && (c.ColdStorage.After != 0 || c.ColdStorage.Drain || c.ColdStorage.BytesPerSecond != 0 ||
		c.ColdStorage.MinBytes != 0 || c.ColdStorage.MinFreeBytes != 0 || c.ColdStorage.MinFreePercent != 0 || c.ColdStorage.IOBusyPercent != 0):
		return fmt.Errorf("config: coldStorage settings need coldStorage.dir")
	case c.Limits.MemoryOvercommit != 0 && !(c.Limits.MemoryOvercommit >= 1 && c.Limits.MemoryOvercommit <= 16):
		return fmt.Errorf("config: limits.memoryOvercommit must be 1..16 (0: the default, %g)", domain.DefaultMemoryOvercommit)
	}
	for name, p := range map[string]string{"firecracker.binary": c.Firecracker.Binary, "firecracker.checkpointDir": c.Firecracker.CheckpointDir, "tailnet.stateDir": c.Tailnet.StateDir, "tailnet.authKeyFile": c.Tailnet.AuthKeyFile,
		"tailnet.tlsCertFile": c.Tailnet.TLSCertFile, "tailnet.tlsKeyFile": c.Tailnet.TLSKeyFile,
		"mac.helper": c.Mac.Helper, "mac.runtimeDir": c.Mac.RuntimeDir} {
		if p != "" && !filepath.IsAbs(p) {
			return fmt.Errorf("config: %s must be an absolute path", name)
		}
	}
	if e := c.Telemetry.OTLPEndpoint; e != "" {
		if u, err := url.Parse(e); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: telemetry.otlpEndpoint must be an http(s) URL")
		}
	}
	if c.Telemetry.IntervalSeconds < 0 {
		return fmt.Errorf("config: telemetry.intervalSeconds must not be negative")
	}
	if (c.Tailnet.TLSCertFile == "") != (c.Tailnet.TLSKeyFile == "") {
		return fmt.Errorf("config: tailnet.tlsCertFile and tailnet.tlsKeyFile go together")
	}
	if _, err := c.Tailnet.AppPortRange(); err != nil {
		return err
	}
	if v := c.Tailnet.DefaultVisibility; v != "" && v != "private" && v != "members" {
		return fmt.Errorf("config: tailnet.defaultVisibility must be \"private\" or \"members\", got %q", v)
	}
	if c.Tailnet.Enabled && len(c.Tailnet.OwnerLogins) == 0 && len(c.Tailnet.OwnerTags) == 0 {
		return fmt.Errorf("config: tailnet.enabled needs tailnet.ownerLogins or tailnet.ownerTags (tailnet membership alone grants nothing)")
	}
	for _, img := range c.Images {
		if !filepath.IsAbs(img.Manifest) || !filepath.IsAbs(img.BlobDir) {
			return fmt.Errorf("config: images entries need absolute manifest and blobDir paths")
		}
	}
	return nil
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir()
	}
	return h
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// DefaultDataDir is $XDG_DATA_HOME/davit or ~/.local/share/davit on
// Linux, ~/Library/Application Support/davit on macOS.
func DefaultDataDir() string {
	if goruntime.GOOS == "darwin" {
		return filepath.Join(home(), "Library", "Application Support", buildinfo.Name)
	}
	if x := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, buildinfo.Name)
	}
	return filepath.Join(home(), ".local", "share", buildinfo.Name)
}

// DefaultConfigDir is $XDG_CONFIG_HOME/davit or ~/.config/davit on
// Linux, the data dir on macOS.
func DefaultConfigDir() string {
	if goruntime.GOOS == "darwin" {
		return DefaultDataDir()
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, buildinfo.Name)
	}
	return filepath.Join(home(), ".config", buildinfo.Name)
}

// DefaultConfigPath is where `init` writes and the daemon looks without
// --config.
func DefaultConfigPath() string { return filepath.Join(DefaultConfigDir(), "config.json") }

// LoadDefault loads path, or the default config file if path is empty and
// it exists, else built-in defaults. It returns the path actually used ("" for
// built-in defaults).
func LoadDefault(path string) (Daemon, string, error) {
	if path == "" {
		if _, err := os.Stat(DefaultConfigPath()); err == nil {
			path = DefaultConfigPath()
		}
	}
	c, err := Load(path)
	return c, path, err
}

// DefaultSocket is $XDG_RUNTIME_DIR/davit/davit.sock, else under the
// data dir.
func DefaultSocket() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(x) {
		return filepath.Join(x, buildinfo.Name, buildinfo.Name+".sock")
	}
	return filepath.Join(DefaultDataDir(), buildinfo.Name+".sock")
}
