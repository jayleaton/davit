package network

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"davit/internal/domain"
	"davit/internal/network/dnsallow"
	"davit/internal/network/netnsdns"
	"davit/internal/network/netnslisten"
	"davit/internal/network/userspace"
)

// holderArgv creates a user namespace (caller mapped to root) and a network
// namespace, then sleeps forever. The sleeping process is the namespace
// holder: the namespaces live exactly as long as it does.
var holderArgv = []string{"unshare", "--user", "--map-root-user", "--net", "--", "sleep", "infinity"}

const (
	readyTimeout = 5 * time.Second
	stopGrace    = 2 * time.Second
)

// Rootless provisions each VM network in an unprivileged user+net namespace
// held by a long-lived holder process (no root, no named netns). It needs
// unprivileged user namespaces, /dev/net/tun access, and unshare, nsenter and
// ip in PATH; nft enables egress filtering and pasta or slirp4netns enable
// egress. Holders are started in their own session so they outlive the
// daemon; Handles carry their identity so Destroy works after a restart.
type Rootless struct {
	cfg      Config
	lookPath func(string) (string, error)
	// upstreams overrides the resolver's upstream servers (tests); nil
	// means the host's resolv.conf.
	upstreams []netip.AddrPort
}

// NewRootless returns a rootless provider for cfg.
func NewRootless(cfg Config) (*Rootless, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Rootless{cfg: cfg, lookPath: exec.LookPath}, nil
}

// Probe reports whether rootless namespaces work here (by actually creating
// a throwaway one) and which egress backends and filter are available.
func (r *Rootless) Probe(ctx context.Context) Capabilities {
	caps := Capabilities{Modes: []ModeStatus{
		r.probeRootless(ctx),
		{Mode: ModePrivileged, Reason: "privileged helper is render-only in this build (see internal/network/privileged)"},
		{Mode: ModeNone, Usable: true},
	}}
	for _, b := range []string{BackendPasta, BackendSlirp4netns} {
		caps.Egress = append(caps.Egress, r.tool(b))
	}
	us := BackendStatus{Name: BackendUserspace, Available: true}
	if _, err := selfExe(); err != nil {
		us = BackendStatus{Name: BackendUserspace, Reason: "cannot locate the davit binary: " + err.Error()}
	}
	caps.Egress = append(caps.Egress, us)
	caps.Filter = r.tool("nft")
	if !caps.Filter.Available {
		caps.Filter.Reason += "; only the userspace backend (which enforces the policy itself) is used"
	}
	return caps
}

func (r *Rootless) tool(name string) BackendStatus {
	if _, err := r.lookPath(name); err != nil {
		return BackendStatus{Name: name, Reason: name + " not found in PATH"}
	}
	return BackendStatus{Name: name, Available: true}
}

func (r *Rootless) probeRootless(ctx context.Context) ModeStatus {
	st := ModeStatus{Mode: ModeRootless}
	for _, t := range []string{"unshare", "nsenter", "ip"} {
		if _, err := r.lookPath(t); err != nil {
			st.Reason = t + " not found in PATH"
			return st
		}
	}
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		st.Reason = "cannot open /dev/net/tun: " + err.Error()
		return st
	}
	f.Close()
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "unshare", "--user", "--map-root-user", "--net", "--", "true").CombinedOutput()
	if err != nil {
		st.Reason = fmt.Sprintf("unprivileged user+net namespace failed: %v: %s", err, strings.TrimSpace(string(out)))
		if b, _ := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); strings.TrimSpace(string(b)) == "1" {
			st.Reason += " (kernel.apparmor_restrict_unprivileged_userns=1: install an AppArmor profile allowing userns for the daemon, or use privileged mode)"
		}
		return st
	}
	st.Usable = true
	return st
}

// Create starts the namespace holder, creates the TAP with the fixed host
// side address, installs the guest egress ruleset when nft is available, and
// attaches an egress backend. On error everything started is torn down.
func (r *Rootless) Create(ctx context.Context, vmID domain.VMID, generation int64, policy domain.Egress) (h Handle, err error) {
	if err := validateKey(vmID, generation); err != nil {
		return Handle{}, err
	}
	dir := ""
	if r.cfg.StateDir != "" {
		if err := os.MkdirAll(r.cfg.StateDir, 0o700); err != nil {
			return Handle{}, fmt.Errorf("network: state dir: %w", err)
		}
		if dir, err = os.MkdirTemp(r.cfg.StateDir, "h-"+ShortID(vmID, generation)+"-"); err != nil {
			return Handle{}, fmt.Errorf("network: state dir: %w", err)
		}
	}
	holder, err := startHolder(dir)
	if err != nil {
		if dir != "" {
			_ = os.Remove(dir)
		}
		return Handle{}, err
	}
	pid := strconv.Itoa(holder.PID)
	h = Handle{
		VMID: vmID, Generation: generation, Mode: ModeRootless,
		NetNSPath: "/proc/" + pid + "/ns/net", UserNSPath: "/proc/" + pid + "/ns/user",
		Holder: holder, StateDir: dir,
		TapName: r.cfg.TapName, GuestIP: r.cfg.GuestIP, TapIP: r.cfg.TapIP,
		Prefix: r.cfg.Prefix, GuestMAC: r.cfg.GuestMAC, MTU: r.cfg.MTU,
		Policy: policy,
	}
	defer func() {
		if err != nil {
			_ = r.Destroy(context.WithoutCancel(ctx), h)
			h = Handle{}
		}
	}()
	tapAddr := netip.PrefixFrom(r.cfg.TapIP, r.cfg.Prefix).String()
	setup := [][]string{
		{"ip", "link", "set", "dev", "lo", "up"},
		{"ip", "tuntap", "add", "dev", r.cfg.TapName, "mode", "tap"},
		{"ip", "addr", "add", tapAddr, "dev", r.cfg.TapName},
		{"ip", "link", "set", "dev", r.cfg.TapName, "mtu", strconv.Itoa(r.cfg.MTU), "up"},
	}
	// Return routes for subnets the guest routes without masquerading.
	for _, sn := range r.cfg.GuestSubnets {
		setup = append(setup, []string{"ip", "route", "add", sn.String(), "via", r.cfg.GuestIP.String(), "dev", r.cfg.TapName})
	}
	for _, argv := range setup {
		if err := r.inNS(ctx, h, argv, ""); err != nil {
			return h, err
		}
	}
	h.Egress = r.attachEgress(ctx, &h)
	return h, nil
}

// attachEgress picks pasta, then slirp4netns, then the built-in userspace
// stack, installs the guest ruleset for h.Policy and, for pasta/slirp4netns
// in allowlist mode, starts the in-netns resolver. It fails closed: pasta and
// slirp4netns need the nft ruleset to enforce the policy, so without nft only
// the userspace stack (which enforces the policy itself, in addition to the
// ruleset when present) is used. Backend failures disable egress with a
// reason rather than failing Create; the VM stays usable over vsock. Egress
// is attached in every mode (off included), so SetEgress can relax it later.
func (r *Rootless) attachEgress(ctx context.Context, h *Handle) Egress {
	const noEgress = "; guest has no egress (control plane over vsock unaffected)"
	_, nftErr := r.lookPath("nft")
	backend := BackendUserspace
	if nftErr == nil {
		for _, b := range []string{BackendPasta, BackendSlirp4netns} {
			if _, err := r.lookPath(b); err == nil {
				backend = b
				break
			}
		}
	}
	stack := backend == BackendUserspace
	eg := Egress{}
	if nftErr == nil {
		if err := r.inNS(ctx, *h, []string{"nft", "-f", "-"}, RenderGuestRuleset(r.cfg, h.Policy, stack)); err != nil {
			return Egress{Reason: "installing guest ruleset: " + err.Error() + noEgress}
		}
		eg.Filtered = true
	}
	if stack {
		if _, err := selfExe(); err != nil {
			eg.Reason = "no egress backend (no pasta/slirp4netns, and the davit binary for userspace egress: " + err.Error() + ")" + noEgress
			return eg
		}
		eg.Filtered = true // the stack enforces the policy on every flow
	}
	if err := r.inNS(ctx, *h, []string{"sysctl", "-q", "-w", "net.ipv4.ip_forward=1"}, ""); err != nil {
		eg.Reason = "enabling forwarding: " + err.Error() + noEgress
		return eg
	}
	var proc *Process
	var err error
	switch backend {
	case BackendPasta:
		proc, err = startPasta(ctx, r.cfg, *h)
	case BackendSlirp4netns:
		proc, err = startSlirp(r.cfg, *h)
	default:
		proc, err = r.startUserspace(ctx, h)
	}
	if err != nil {
		eg.Reason = backend + ": " + err.Error() + noEgress
		return eg
	}
	h.EgressProc = proc
	eg.Enabled, eg.Backend = true, backend
	if r.needsResolver(eg, h.Policy) {
		if h.DNSProc, err = r.startResolver(ctx, *h, h.Policy.Allow); err != nil {
			eg.Reason = "allowlist resolver: " + err.Error() + "; guest DNS fails, only allowlisted CIDRs are reachable"
		}
	}
	return eg
}

// needsResolver reports whether policy on egress eg needs the in-netns
// resolver: allowlist mode on an attached nft-only backend.
func (r *Rootless) needsResolver(eg Egress, policy domain.Egress) bool {
	return eg.Enabled && eg.Backend != BackendUserspace && policy.Mode == domain.EgressAllowlist
}

// Enter returns the nsenter argv prefix for h after checking that the
// holder is still the process the handle recorded.
func (r *Rootless) Enter(h Handle) ([]string, error) {
	if h.Mode != ModeRootless {
		return nil, fmt.Errorf("%w: handle mode %q is not rootless", ErrInvalid, h.Mode)
	}
	if !alive(h.Holder) {
		return nil, fmt.Errorf("network: namespace holder for %s/%d is gone", h.VMID, h.Generation)
	}
	return EnterArgv(h), nil
}

// ListenTCP implements TapListener: it runs `davit netns-listenfd
// TapIP:port` inside h's namespaces and returns the TCP listener it bound
// there. The socket lives in the VM netns, reachable only from that VM's
// guest (the guest table admits Config.TapServices ports), while the caller
// accepts on it from the host netns. port must be in Config.TapServices.
// Close the listener before or when the network is destroyed: it keeps the
// namespace (and its TAP) alive while open.
func (r *Rootless) ListenTCP(ctx context.Context, h Handle, port uint16) (net.Listener, error) {
	if h.Mode != ModeRootless {
		return nil, fmt.Errorf("%w: handle mode %q is not rootless", ErrInvalid, h.Mode)
	}
	if !slices.Contains(r.cfg.TapServices, port) {
		return nil, fmt.Errorf("%w: port %d is not in Config.TapServices", ErrInvalid, port)
	}
	if !alive(h.Holder) {
		return nil, fmt.Errorf("network: namespace holder for %s/%d is gone", h.VMID, h.Generation)
	}
	exe, err := selfExe()
	if err != nil {
		return nil, fmt.Errorf("network: locate davit binary: %w", err)
	}
	addr := netip.AddrPortFrom(r.cfg.TapIP, port)
	f, err := fetchFD(ctx, h, exe, netnslisten.Cmd, addr.String())
	if err != nil {
		return nil, fmt.Errorf("network: listen on %s in the VM netns: %w", addr, err)
	}
	defer f.Close()
	l, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("network: listen on %s in the VM netns: %w", addr, err)
	}
	return l, nil
}

// Destroy stops the resolver, the egress process and the holder; the kernel then frees
// the namespaces, TAP and ruleset. Processes that are gone (or whose PID now
// belongs to someone else) are skipped, so Destroy is idempotent. Once they
// are gone the handle's state directory is removed, but only if it lies
// directly under this provider's Config.StateDir.
func (r *Rootless) Destroy(_ context.Context, h Handle) error {
	if h.Mode != ModeRootless {
		return fmt.Errorf("%w: handle mode %q is not rootless", ErrInvalid, h.Mode)
	}
	if err := errors.Join(stopProcess(h.DNSProc, stopGrace), stopProcess(h.EgressProc, stopGrace), stopProcess(h.Holder, stopGrace)); err != nil {
		return err
	}
	if name, ok := r.handleDir(h.StateDir); ok {
		if err := os.RemoveAll(filepath.Join(r.cfg.StateDir, name)); err != nil {
			return fmt.Errorf("network: remove state dir: %w", err)
		}
	}
	return nil
}

func (r *Rootless) inNS(ctx context.Context, h Handle, argv []string, stdin string) error {
	_, err := r.nsOutput(ctx, h, argv, stdin)
	return err
}

// nsOutput runs argv inside h's namespaces and returns its stdout.
func (r *Rootless) nsOutput(ctx context.Context, h Handle, argv []string, stdin string) ([]byte, error) {
	full := append(EnterArgv(h), argv...)
	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// startHolder starts the namespace holder in its own session (so daemon
// signals and context cancellation do not reach it) with cwd dir (inherited
// when empty) and waits until unshare has exec'd sleep, i.e. the namespaces
// and ID maps are in place.
func startHolder(dir string) (*Process, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(holderArgv[0], holderArgv[1:]...)
	cmd.Dir = dir
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("network: start holder: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }() // reap; holder outlives Create
	comm := "/proc/" + strconv.Itoa(cmd.Process.Pid) + "/comm"
	deadline := time.After(readyTimeout)
	for {
		if b, err := os.ReadFile(comm); err == nil && strings.TrimSpace(string(b)) == "sleep" {
			break
		}
		select {
		case <-exited:
			return nil, fmt.Errorf("network: holder exited: %s", strings.TrimSpace(stderr.String()))
		case <-deadline:
			_ = cmd.Process.Kill()
			return nil, errors.New("network: holder did not become ready")
		case <-time.After(2 * time.Millisecond):
		}
	}
	p, err := identify(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("network: identify holder: %w", err)
	}
	return p, nil
}

// startPasta runs pasta, which daemonizes after configuring the netns and
// writes its PID file first.
func startPasta(ctx context.Context, cfg Config, h Handle) (*Process, error) {
	dir, err := os.MkdirTemp("", "davit-pasta-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	pidFile := filepath.Join(dir, "pid")
	argv := PastaArgv(cfg, h.UserNSPath, h.NetNSPath, pidFile)
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = h.StateDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return nil, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("bad pid file: %w", err)
	}
	return identify(pid)
}

// startSlirp runs slirp4netns in its own session and waits for it to write
// to the ready pipe (passed as fd 3).
func startSlirp(cfg Config, h Handle) (*Process, error) {
	rd, wr, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	argv := Slirp4netnsArgv(h.UserNSPath, h.NetNSPath, cfg.Uplink, 3)
	var stderr bytes.Buffer
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = h.StateDir
	cmd.ExtraFiles = []*os.File{wr}
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	wr.Close()
	if err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	_ = rd.SetReadDeadline(time.Now().Add(readyTimeout))
	buf := make([]byte, 1)
	if _, err := io.ReadFull(rd, buf); err != nil || buf[0] != '1' {
		_ = cmd.Process.Kill()
		<-exited // stderr is complete only after Wait returns
		return nil, fmt.Errorf("not ready: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return identify(cmd.Process.Pid)
}

// setStackPolicy switches the userspace stack's policy (tests replace it).
var setStackPolicy = userspace.SetPolicy

// SetEgress applies policy to h's live network (see Provider). The guest
// table is replaced in one nft transaction; the userspace stack is switched
// through its control socket; on the nft-only backends the allowlist
// resolver is stopped before the table changes and (re)started with the new
// list after it, and pins whose names the new list still allows are carried
// over. On error the previous table is restored and, best effort, the
// previous resolver restarted (if that fails guest DNS fails, which is
// stricter than either policy). Re-applying the policy in force is a no-op
// for a running resolver.
func (r *Rootless) SetEgress(ctx context.Context, h Handle, policy domain.Egress) (Handle, error) {
	if h.Mode != ModeRootless {
		return h, fmt.Errorf("%w: handle mode %q is not rootless", ErrInvalid, h.Mode)
	}
	if !alive(h.Holder) {
		return h, fmt.Errorf("network: namespace holder for %s/%d is gone", h.VMID, h.Generation)
	}
	stack := h.Egress.Backend == BackendUserspace
	_, nftErr := r.lookPath("nft")
	haveTable := nftErr == nil
	if !haveTable && h.Egress.Enabled && !stack {
		return h, fmt.Errorf("network: nft not found; cannot enforce egress on backend %s", h.Egress.Backend)
	}
	oldRes, newRes := r.needsResolver(h.Egress, h.Policy), r.needsResolver(h.Egress, policy)
	if newRes && oldRes && alive(h.DNSProc) && slices.Equal(h.Policy.Allow, policy.Allow) {
		h.Policy = policy
		return h, nil
	}
	nh := h
	nh.Policy = policy
	nh.DNSProc = nil
	if err := stopProcess(h.DNSProc, stopGrace); err != nil {
		return h, fmt.Errorf("network: stop resolver: %w", err)
	}
	var oldPins []pinned
	if oldRes && haveTable {
		oldPins, _ = r.listPins(ctx, h) // unreadable pins are dropped (fail closed)
	}
	restore := func(cause error) (Handle, error) {
		ctx := context.WithoutCancel(ctx)
		var errs []error
		if haveTable {
			errs = append(errs, r.inNS(ctx, h, []string{"nft", "-f", "-"}, RenderGuestReplace(r.cfg, h.Policy, stack)+pinBatch(oldPins)))
		}
		old := h
		old.DNSProc = nil
		if oldRes {
			p, err := r.startResolver(ctx, h, h.Policy.Allow)
			old.DNSProc, errs = p, append(errs, err)
		}
		if err := errors.Join(errs...); err != nil {
			cause = fmt.Errorf("%w (restoring the previous policy: %v)", cause, err)
		}
		return old, cause
	}
	if haveTable {
		var keep []pinned
		if oldRes && newRes {
			l, _ := dnsallow.Parse(policy.Allow)
			for _, p := range oldPins {
				if l.AllowsName(p.name) {
					keep = append(keep, p)
				}
			}
		}
		if err := r.inNS(ctx, h, []string{"nft", "-f", "-"}, RenderGuestReplace(r.cfg, policy, stack)+pinBatch(keep)); err != nil {
			return restore(fmt.Errorf("network: replace guest ruleset: %w", err))
		}
	}
	if stack {
		if err := setStackPolicy(ctx, h.EgressControl, string(policy.Mode), policy.Allow); err != nil {
			return restore(fmt.Errorf("network: userspace egress policy: %w", err))
		}
	}
	if newRes {
		p, err := r.startResolver(ctx, nh, policy.Allow)
		if err != nil {
			return restore(fmt.Errorf("network: allowlist resolver: %w", err))
		}
		nh.DNSProc = p
	}
	return nh, nil
}

// pinned is one element of the guest table's pin set.
type pinned struct {
	addr netip.Addr
	secs int
	name string
}

// listPins reads the pin set (address, remaining seconds, comment name).
func (r *Rootless) listPins(ctx context.Context, h Handle) ([]pinned, error) {
	out, err := r.nsOutput(ctx, h, []string{"nft", "-j", "list", "set", "inet", GuestTable, PinnedSet}, "")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Nftables []struct {
			Set *struct {
				Elem []json.RawMessage `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	var ps []pinned
	for _, o := range doc.Nftables {
		if o.Set == nil {
			continue
		}
		for _, raw := range o.Set.Elem {
			var e struct {
				Elem struct {
					Val     string `json:"val"`
					Expires int    `json:"expires"`
					Comment string `json:"comment"`
				} `json:"elem"`
			}
			if json.Unmarshal(raw, &e) != nil {
				continue // an element without timeout/comment is not ours
			}
			a, err := netip.ParseAddr(e.Elem.Val)
			if err != nil || !a.Is4() || e.Elem.Expires <= 0 || e.Elem.Comment == "" {
				continue
			}
			ps = append(ps, pinned{addr: a, secs: e.Elem.Expires, name: e.Elem.Comment})
		}
	}
	return ps, nil
}

// pinBatch renders nft commands that add ps to the pin set.
func pinBatch(ps []pinned) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(netnsdns.PinBatch(GuestTable, PinnedSet, p.addr, p.secs, p.name))
	}
	return b.String()
}

// resolvConf is read for the resolver's upstream servers.
var resolvConf = "/etc/resolv.conf"

// startResolver starts `davit netns-dns` inside h's namespaces in its own
// session (it outlives the daemon and exits with the holder) and waits for
// its ready byte. Strays from an earlier attempt in the same netns (e.g. a
// daemon that died before persisting the handle) are stopped first, since
// they would hold port 53.
func (r *Rootless) startResolver(ctx context.Context, h Handle, allow []string) (*Process, error) {
	exe, err := selfExe()
	if err != nil {
		return nil, fmt.Errorf("locate davit binary: %w", err)
	}
	if err := stopResolvers(h); err != nil {
		return nil, err
	}
	ups := r.upstreams
	if ups == nil {
		b, _ := os.ReadFile(resolvConf)
		ups = netnsdns.Upstreams(b)
	}
	cfg, _ := json.Marshal(netnsdns.Config{
		Listen: netip.AddrPortFrom(r.cfg.TapIP, 53), Upstreams: ups, Allow: allow,
		Table: GuestTable, Set: PinnedSet,
		Holder: netnsdns.Holder{PID: h.Holder.PID, StartTime: h.Holder.StartTime},
	})
	rd, wr, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	argv := append(EnterArgv(h), exe, netnsdns.Cmd, string(cfg))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.ExtraFiles = []*os.File{wr} // fd 3
	// stdout/stderr stay /dev/null (the process outlives the daemon);
	// startup errors arrive on the ready pipe instead of "1".
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	wr.Close()
	if err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	_ = rd.SetReadDeadline(time.Now().Add(readyTimeout))
	msg, err := io.ReadAll(io.LimitReader(rd, 4096))
	if err != nil || string(msg) != "1" {
		_ = cmd.Process.Kill()
		<-exited
		if err == nil {
			err = errors.New(cmp.Or(strings.TrimSpace(string(msg)), "exited before becoming ready"))
		}
		return nil, fmt.Errorf("resolver not ready: %w", err)
	}
	return identify(cmd.Process.Pid)
}

// stopResolvers stops every `netns-dns` process in h's network namespace.
func stopResolvers(h Handle) error {
	ns, err := os.Readlink(h.NetNSPath)
	if err != nil {
		return err
	}
	ents, _ := os.ReadDir("/proc")
	var errs []error
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		// Identify first: the checks below then vouch for this process, and
		// stopProcess signals it only if the PID still names it.
		p, err := identify(pid)
		if err != nil {
			continue
		}
		dir := "/proc/" + e.Name()
		if l, err := os.Readlink(dir + "/ns/net"); err != nil || l != ns {
			continue
		}
		b, _ := os.ReadFile(dir + "/cmdline")
		if args := strings.Split(string(b), "\x00"); len(args) < 2 || args[1] != netnsdns.Cmd {
			continue
		}
		if alive(p) { // still the process the checks just read
			errs = append(errs, stopProcess(p, stopGrace))
		}
	}
	return errors.Join(errs...)
}
