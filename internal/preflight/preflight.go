// Package preflight checks whether this host can run davit (SPEC §7):
// hardware virtualization, KVM access, namespaces, storage clone support,
// space, the runtime binary and tailnet readiness. `davit doctor` prints
// the report; the daemon runs the same checks at startup and refuses to
// start on a failure.
//
// Every check gathers facts (syscalls, /proc, /sys) separately from a pure
// classifier, and grades by the configured runtime: missing KVM fails for
// firecracker but only warns for the fake runtime. Checks never change host
// configuration; at most they create and remove a temp file.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"davit/internal/buildinfo"
	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/network"
	"davit/internal/storage"
)

// Status is a check outcome.
type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is one result. Hint is a one-line fix; Detail never contains secrets.
// Sudo, when set, are complete shell command lines that fix the check as
// root, to run in order with `sh -c`; each says `sudo` for its privileged
// part and none needs interaction (`doctor --fixes` prints them).
type Check struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Status Status   `json:"status"`
	Detail string   `json:"detail"`
	Hint   string   `json:"hint,omitempty"`
	Sudo   []string `json:"sudo,omitempty"`
}

// Fix is the check's fix on one line: its root commands joined with
// " && ", else its hint.
func (c Check) Fix() string {
	if len(c.Sudo) > 0 {
		return strings.Join(c.Sudo, " && ")
	}
	return c.Hint
}

// Options are the inputs, normally derived from the daemon config.
type Options struct {
	Runtime           string
	DataDir           string
	StorageMode       string
	FirecrackerBinary string
	// MacHelper is the VZ helper to probe (mac runtime, or one configured).
	MacHelper       string
	TailnetStateDir string
	AuthKeyFile     string
	AuthKeyEnv      string
	TailnetEnabled  bool
	TailnetOwners   int // configured owner logins + tags
	TailnetHTTPS    bool
	TLSCertFile     string
	TLSKeyFile      string
	NetworkMode     string
	Limits          domain.Limits
	// Images are the configured guest images; empty means the daemon looks
	// in <DataDir>/images/manifests.
	Images []config.ImageSource
}

// FromConfig derives Options from a daemon config.
func FromConfig(c config.Daemon) Options {
	var macHelper string
	if c.Runtime == "mac" || c.Mac.Helper != "" {
		if exe, err := os.Executable(); err == nil {
			macHelper = c.MacHelperPath(exe)
		}
	}
	return Options{
		MacHelper: macHelper,
		Runtime:   c.Runtime, DataDir: c.DataDir, StorageMode: c.Storage,
		FirecrackerBinary: c.Firecracker.Binary, TailnetStateDir: c.TailnetStateDir(),
		AuthKeyFile: c.Tailnet.AuthKeyFile, AuthKeyEnv: config.AuthKeyEnv, Limits: c.Limits,
		TailnetEnabled: c.Tailnet.Enabled, TailnetOwners: len(c.Tailnet.OwnerLogins) + len(c.Tailnet.OwnerTags),
		NetworkMode: c.Network.Mode, Images: c.Images,
		TailnetHTTPS: c.Tailnet.HTTPS, TLSCertFile: c.Tailnet.TLSCertFile, TLSKeyFile: c.Tailnet.TLSKeyFile,
	}
}

// needsVM reports whether the runtime boots real VMs (and so needs KVM etc.).
func (o Options) needsVM() bool { return o.Runtime != "fake" }

// Report is the full result.
type Report struct {
	OS     string  `json:"os"`
	Arch   string  `json:"arch"`
	Checks []Check `json:"checks"`
}

// Failed returns the failed checks.
func (r Report) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == Fail {
			out = append(out, c)
		}
	}
	return out
}

// Counts tallies statuses.
func (r Report) Counts() map[Status]int {
	m := map[Status]int{Pass: 0, Warn: 0, Fail: 0, Skip: 0}
	for _, c := range r.Checks {
		m[c.Status]++
	}
	return m
}

// Run executes every check for this OS.
func Run(ctx context.Context, o Options) Report {
	r := Report{OS: goruntime.GOOS, Arch: goruntime.GOARCH}
	r.Checks = append(r.Checks, platformChecks(ctx, o)...)
	r.Checks = append(r.Checks, storageCheck(o), diskSpaceCheck(o), imagesCheck(o))
	r.Checks = append(r.Checks, networkChecks(ctx, o)...)
	r.Checks = append(r.Checks, tailnetStateCheck(o), tailnetAuthCheck(o), tailnetOwnerCheck(o))
	r.Checks = append(r.Checks, GatewayChecks(o, time.Now())...)
	return r
}

// severity is Fail when the runtime needs the feature, else Warn.
func severity(required bool) Status {
	if required {
		return Fail
	}
	return Warn
}

// existingAncestor returns path or its nearest existing ancestor.
func existingAncestor(path string) string {
	for p := path; ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil || p == filepath.Dir(p) {
			return p
		}
	}
}

func storageCheck(o Options) Check {
	c := Check{ID: "storage", Title: "Storage clone support"}
	// Probe the store itself, else the data dir (same filesystem), else the
	// nearest existing ancestor it will be created under.
	dir, note := filepath.Join(o.DataDir, "storage"), ""
	if _, err := os.Stat(dir); err != nil {
		dir = existingAncestor(o.DataDir)
		if dir != o.DataDir {
			note = fmt.Sprintf(" (data dir not created yet; probed %s)", dir)
		}
	}
	p, err := storage.ProbeDir(dir, storage.Mode(o.StorageMode))
	switch {
	case domain.IsCode(err, domain.CodeUnsupportedCapability):
		c.Status, c.Detail = Fail, domain.AsError(err).Message+note
		c.Hint = cloneHint + `, or set "storage": "copy"`
	case errors.Is(err, fs.ErrPermission):
		c.Status, c.Detail, c.Hint = Fail, "data dir is not writable: "+err.Error(), "choose a dataDir you own"
	case err != nil:
		c.Status, c.Detail, c.Hint = Fail, err.Error(), "point dataDir at a local directory you own"
	case p.FastClone:
		c.Status, c.Detail = Pass, p.Detail+note
	default:
		c.Status, c.Detail = Warn, p.Detail+note
		c.Hint = "forks and resumes will copy whole disks; " + cloneHint + " for instant clones"
	}
	return c
}

// cloneHint names the filesystems whose clones the store uses on this OS.
var cloneHint = map[bool]string{
	true:  "put the data dir on an APFS volume (clonefile)",
	false: "use a btrfs or XFS (reflink=1) data dir",
}[goruntime.GOOS == "darwin"]

// ClassifyDiskSpace grades free bytes against the configured disk limit and
// the admission floor (below which the daemon refuses new/fork/resume).
func ClassifyDiskSpace(free, limit, floor int64) (Status, string, string) {
	detail := fmt.Sprintf("%s free", humanBytes(free))
	switch {
	case floor > 0 && free < floor:
		return Fail, detail + fmt.Sprintf(", below the admission floor %s: new, fork and resume are refused", humanBytes(floor)),
			"free space (delete VMs or snapshots) or lower limits.minFreeDiskBytes"
	case limit > 0 && free < limit:
		return Warn, detail + fmt.Sprintf(", less than the configured disk limit %s", humanBytes(limit)),
			"VM disks are sparse but can fill up: free space or lower limits.diskBytes"
	case free < 10*domain.GiB:
		return Warn, detail, "less than 10GiB free: images, snapshots and VM disks need room"
	}
	return Pass, detail, ""
}

func diskSpaceCheck(o Options) Check {
	c := Check{ID: "disk_space", Title: "Free disk space"}
	free, err := FreeBytes(existingAncestor(o.DataDir))
	if err != nil {
		c.Status, c.Detail = Warn, "cannot measure free space: "+err.Error()
		c.Hint = "point dataDir at a mounted local filesystem"
		return c
	}
	c.Status, c.Detail, c.Hint = ClassifyDiskSpace(free, o.Limits.DiskBytes, o.Limits.FreeFloor())
	return c
}

// enrolled reports whether a tsnet state dir already holds a node identity.
func enrolled(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "tailscaled.state"))
	return err == nil && fi.Size() > 0
}

// tailnetOff is the detail of the tailnet checks while the gateway is
// disabled: SSH and the local socket need none of them.
const tailnetOff = "tailnet gateway off (optional)"

func tailnetStateCheck(o Options) Check {
	c := Check{ID: "tailnet_state", Title: "Tailnet state directory"}
	if !o.TailnetEnabled {
		c.Status, c.Detail = Skip, tailnetOff
		return c
	}
	dir := o.TailnetStateDir
	fi, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		anc := existingAncestor(dir)
		if err := writable(anc); err != nil {
			c.Status, c.Detail, c.Hint = Fail, fmt.Sprintf("%s does not exist and %s is not writable", dir, anc), "choose a tailnet.stateDir you own"
			return c
		}
		c.Status, c.Detail = Pass, dir+" will be created (0700)"
		return c
	}
	if err != nil || !fi.IsDir() {
		c.Status, c.Detail, c.Hint = Fail, dir+" is not a directory", "remove it or point tailnet.stateDir elsewhere"
		return c
	}
	if fi.Mode().Perm()&0o077 != 0 {
		c.Status, c.Detail = Fail, fmt.Sprintf("%s is %#o; node keys must be private", dir, fi.Mode().Perm())
		c.Hint = "chmod 700 " + dir
		return c
	}
	if err := writable(dir); err != nil {
		c.Status, c.Detail, c.Hint = Fail, dir+" is not writable", "fix ownership of "+dir
		return c
	}
	c.Status, c.Detail = Pass, dir+" is private and writable"
	if enrolled(dir) {
		c.Detail += "; node already enrolled"
	}
	return c
}

func tailnetAuthCheck(o Options) Check {
	c := Check{ID: "tailnet_auth", Title: "Tailnet enrollment"}
	switch {
	case !o.TailnetEnabled:
		c.Status, c.Detail = Skip, tailnetOff
	case enrolled(o.TailnetStateDir):
		c.Status, c.Detail = Pass, "node already enrolled; no auth key needed"
	case o.AuthKeyEnv != "" && os.Getenv(o.AuthKeyEnv) != "":
		c.Status, c.Detail = Pass, "auth key provided by $"+o.AuthKeyEnv+" (not shown)"
	case o.AuthKeyFile != "":
		fi, err := os.Stat(o.AuthKeyFile)
		switch {
		case err != nil:
			c.Status, c.Detail = Warn, "configured auth key file "+o.AuthKeyFile+" is missing"
			c.Hint = buildinfo.Name + " init --auth-key-stdin < key-file"
		case fi.Mode().Perm()&0o077 != 0:
			c.Status, c.Detail, c.Hint = Fail, fmt.Sprintf("auth key file is %#o; it must be private", fi.Mode().Perm()), "chmod 600 "+o.AuthKeyFile
		case fi.Size() == 0:
			c.Status, c.Detail, c.Hint = Warn, "auth key file is empty", buildinfo.Name+" init --auth-key-stdin < key-file"
		default:
			c.Status, c.Detail = Pass, "auth key file present (contents not shown)"
		}
	default:
		c.Status, c.Detail = Warn, "no auth key configured; the gateway cannot join the tailnet"
		c.Hint = fmt.Sprintf("create a key in the Tailscale admin console, then `%s init --auth-key-stdin` or set $%s", buildinfo.Name, o.AuthKeyEnv)
	}
	return c
}

// tailnetOwnerCheck: an enabled gateway must name its owner, since tailnet
// membership alone grants nothing.
func tailnetOwnerCheck(o Options) Check {
	c := Check{ID: "tailnet_owner", Title: "Tailnet gateway owner"}
	switch {
	case !o.TailnetEnabled:
		c.Status, c.Detail = Skip, tailnetOff
		c.Hint = buildinfo.Name + " init --owner-login you@example.com (with an auth key stored) enables it; or set \"enabled\" and \"ownerLogins\" under \"tailnet\""
	case o.TailnetOwners == 0:
		c.Status, c.Detail = Fail, "tailnet.enabled but no tailnet.ownerLogins or tailnet.ownerTags"
		c.Hint = buildinfo.Name + " init --owner-login you@example.com, or add \"ownerLogins\" under \"tailnet\""
	default:
		c.Status, c.Detail = Pass, fmt.Sprintf("%d owner identities configured; all other tailnet peers are refused", o.TailnetOwners)
	}
	return c
}

// writable checks dir by creating and removing a temp file.
func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".davit-preflight-")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

func humanBytes(n int64) string {
	switch {
	case n >= domain.GiB:
		return fmt.Sprintf("%.1fGiB", float64(n)/float64(domain.GiB))
	case n >= domain.MiB:
		return fmt.Sprintf("%dMiB", n/domain.MiB)
	}
	return fmt.Sprintf("%dB", n)
}

// networkChecks report the guest network mode and whether guests get
// internet egress. Without egress VMs still work: control traffic is vsock.
func networkChecks(ctx context.Context, o Options) []Check {
	if !o.needsVM() {
		return nil
	}
	n := Check{ID: "network", Title: "Guest network"}
	e := Check{ID: "egress", Title: "Guest internet egress"}
	if o.NetworkMode == "none" {
		n.Status, n.Detail = Pass, "network.mode=none: no guest NIC; guests are reached over vsock only"
		e.Status, e.Detail = Skip, "no guest NIC"
		return []Check{n, e}
	}
	r, err := network.NewRootless(network.DefaultConfig())
	if err != nil {
		n.Status, n.Detail, n.Hint = Fail, err.Error(), "fix the host or set network.mode to \"none\""
		return []Check{n}
	}
	caps := r.Probe(ctx)
	for _, m := range caps.Modes {
		if m.Mode != network.ModeRootless {
			continue
		}
		switch {
		case m.Usable:
			n.Status, n.Detail = Pass, "rootless per-VM user+net namespace with tap0 (guest 172.16.0.2)"
		case o.NetworkMode == "rootless":
			n.Status, n.Detail, n.Hint = Fail, m.Reason, "fix the host or set network.mode to \"none\""
		default:
			n.Status, n.Detail, n.Hint = Warn, "rootless namespaces unavailable, VMs get no NIC: "+m.Reason, "fix the host, or set network.mode to \"none\" to silence this"
		}
	}
	if n.Status != Pass {
		e.Status, e.Detail = Skip, "no guest network"
		return []Check{n, e}
	}
	// Mirror network.Rootless.attachEgress: pasta, then slirp4netns (both
	// need nft to filter), else the built-in userspace stack.
	var why []string
	backend := ""
	for _, b := range caps.Egress {
		usable := b.Available && (caps.Filter.Available || b.Name == network.BackendUserspace)
		if usable && backend == "" {
			backend = b.Name
		} else if !b.Available {
			why = append(why, b.Reason)
		}
	}
	switch {
	case backend == network.BackendUserspace && caps.Filter.Available:
		e.Status, e.Detail = Pass, "userspace (built-in gVisor stack), LAN/host/metadata deny list enforced by the stack and by nft"
	case backend == network.BackendUserspace:
		e.Status, e.Detail = Pass, "userspace (built-in gVisor stack), LAN/host/metadata deny list enforced by the stack (no nft: no kernel-level filter)"
	case backend != "":
		e.Status, e.Detail = Pass, backend+", LAN/host/metadata deny list enforced by nft"
	default:
		e.Status, e.Detail = Warn, "off: "+strings.Join(why, "; ")
		e.Hint = "VMs stay reachable over vsock; install pasta (passt) or slirp4netns with nft, or fix the userspace backend"
	}
	return []Check{n, e}
}
