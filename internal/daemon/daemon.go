// Package daemon wires state, storage, runtime, lifecycle and the HTTP API
// into the `davit daemon` process.
package daemon

import (
	"context"
	"davit/api"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"davit/internal/appendpoint"
	"davit/internal/buildinfo"
	"davit/internal/config"
	"davit/internal/credproxy"
	"davit/internal/domain"
	"davit/internal/gateway"
	"davit/internal/gateway/desktopui"
	"davit/internal/guestclient"
	"davit/internal/guestexec"
	"davit/internal/guestproto"
	"davit/internal/guestssh"
	"davit/internal/harness"
	"davit/internal/httpapi"
	"davit/internal/httpx"
	"davit/internal/jobs"
	"davit/internal/lifecycle"
	"davit/internal/members"
	"davit/internal/network"
	"davit/internal/preflight"
	"davit/internal/reversefwd"
	"davit/internal/runs"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
	"davit/internal/runtime/firecracker"
	"davit/internal/runtime/mac"
	"davit/internal/snapfiles"
	"davit/internal/state"
	"davit/internal/storage"
	"davit/internal/tokens"
)

// Options injects host seams. Zero values use the daemon's defaults when
// the runtime has a guest transport: the guest agent's TCP dial over vsock
// (GuestDialer) and the daemon's guest SSH CA (GuestSSH). Without a guest
// transport they stay nil: tailnet SSH/forwards fail cleanly while the
// tailnet API still works.
type Options struct {
	GuestDialer gateway.GuestDialer
	GuestSSH    gateway.GuestSSH
}

// Run is RunWith with no injected seams.
func Run(ctx context.Context, cfg config.Daemon, log *slog.Logger) error {
	return RunWith(ctx, cfg, log, Options{})
}

// RunWith serves until ctx is done, then shuts down gracefully: in-flight
// operations stop at a safe point and resume on the next start.
func RunWith(ctx context.Context, cfg config.Daemon, log *slog.Logger, opts Options) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := checkHost(ctx, cfg, log); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	db, err := state.Open(ctx, filepath.Join(cfg.DataDir, "state.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	store, err := storage.Open(filepath.Join(cfg.DataDir, "storage"), storage.Mode(cfg.Storage))
	if err != nil {
		return err
	}
	cold, coldErr := coldPolicy(ctx, cfg, db, store, log)
	// Guest agent: with a guest transport, readiness runs the restore
	// barrier and the process API is enabled. Secrets persist owner-only
	// in the data dir so a restarted daemon can still reach running guests.
	secrets := guestclient.DirSecrets{Dir: filepath.Join(cfg.DataDir, "guest-secrets")}
	netProv, nic, ncfg, err := newNetwork(ctx, cfg, log)
	if err != nil {
		return err
	}
	rt, err := newRuntime(ctx, cfg, log, netProv, nic, agentShutdown(secrets))
	if err != nil {
		return err
	}
	images, err := newCatalog(ctx, cfg)
	if err != nil {
		return err
	}
	if mc, ok := images.(*lifecycle.ManifestCatalog); ok {
		for _, w := range mc.Warnings() {
			log.Warn("image", "warning", w)
		}
	}
	// Credential proxy (G4): secrets in owner-only files, grants in the state
	// database, one listener per running VM inside its network namespace.
	credHost := &credproxy.Host{DB: db, Secrets: credproxy.FileSecrets{Dir: filepath.Join(cfg.DataDir, "credentials")},
		Log: log.With("component", "credproxy"), AllowPrivateUpstreams: cfg.Network.AllowPrivateUpstreams}
	// G13: a VM shared with exec or admin without credentials pauses its
	// credential grants (set once the members service exists).
	var team atomic.Pointer[members.Service]
	credHost.Usable = func(vm domain.VMID) error {
		if m := team.Load(); m != nil {
			return m.CredentialsUsable(vm)
		}
		return nil
	}
	proxyStatus := domain.FeatureUnsupported
	if tl, ok := netProv.(network.TapListener); ok {
		credHost.Listen = func(ctx context.Context, h network.Handle) (net.Listener, error) {
			return tl.ListenTCP(ctx, h, credproxy.GuestPort)
		}
		proxyStatus = domain.FeatureAvailable
	}
	// Prompts inject the VM's grants only when the proxy can listen, at
	// the guest-facing address of the network the VMs actually get.
	proxyURL := ""
	if proxyStatus == domain.FeatureAvailable && ncfg.TapIP.IsValid() {
		proxyURL = credproxy.GuestBaseURL(ncfg)
	}
	defer credHost.Close()
	credControl := &credproxy.Control{DB: db, Secrets: credHost.Secrets, Proxy: credHost}
	var svc *lifecycle.Service
	var procs *guestexec.Manager                    // set below; the TTL warning reaches guests through it
	var busyProcs atomic.Pointer[guestexec.Manager] // procs, for the idle sweep (another goroutine)
	var leases atomic.Pointer[harness.Service]      // set below; active turns hold auto-stop through it
	var prober lifecycle.ReadinessProber
	var sshCA *guestssh.Authority
	var onDeleted func(context.Context, domain.VMID)
	var identity lifecycle.GuestIdentity // VM.Endpoint host-key fingerprints
	guest := rt.Capabilities().GuestTransport != "none"
	fakeRuntime = rt.Capabilities().Engine == fake.Engine
	if guest {
		// Guest sshd trusts this CA for short-lived user certificates; the
		// barrier records each instance's host keys for the gateway.
		if sshCA, err = guestssh.Open(filepath.Join(cfg.DataDir, "guest-ssh")); err != nil {
			return err
		}
		prober = &guestclient.Prober{Secrets: secrets, OnIdentity: sshCA.RecordIdentity,
			Plan: func(ctx context.Context, key runtime.Key) (guestclient.Plan, error) {
				p, err := bootPlan(ctx, svc, rt, key)
				if err != nil {
					return p, err
				}
				// This generation's CA only: certificates for other VMs or
				// generations do not log in here.
				p.SSHUserCAKeys, err = sshCA.CAPublicKeys(key)
				return p, err
			}}
		identity = sshCA
		onDeleted = func(_ context.Context, vm domain.VMID) {
			if err := sshCA.Forget(vm); err != nil {
				log.Warn("forget guest ssh records", "vm", vm, "err", err)
			}
		}
	}
	// Workspace preparation (G3) runs through the process manager, which is
	// built after the lifecycle service it targets: hand lifecycle a
	// forwarder now and the manager once it exists.
	var workspace lifecycle.WorkspacePreparer
	late := &lateGuest{ready: make(chan struct{})}
	if guest {
		workspace = late
	}
	svc, err = lifecycle.New(lifecycle.Config{
		HostOwner: httpapi.LocalPrincipal(os.Getuid()),
		DB:        db, Runtime: rt, Storage: store, Images: images, Prober: prober, Network: netProv, OnDeleted: onDeleted,
		Identity: identity,
		Limits:   cfg.Limits, DiskSpace: func() (domain.DiskUsage, error) { return preflight.DiskSpace(cfg.DataDir) },
		SaveReserve: saveReserve(cfg), SyncDisk: func() error { return preflight.SyncFS(cfg.DataDir) },
		AutoDelete: autoDeletePolicy(cfg.AutoDelete), ImageBlobs: imageBlobs(cfg), Cold: cold, ColdUnavailable: coldErr,
		FreeMemory: preflight.FreeMemory, DefaultTTL: time.Duration(cfg.DefaultTTL), StopGrace: time.Duration(cfg.StopGrace),
		StopAttempts: cfg.StopAttempts, NoStopForce: cfg.StopForceAfterRetries != nil && !*cfg.StopForceAfterRetries,
		TTLWarning: time.Duration(cfg.TTLWarning), Profiles: cfg.Profiles,
		Listeners: listenerNames(cfg), Services: serviceNames(guest),
		Harnesses: harnessCaps(guest),
		Features:  shippedFeatures(guest, proxyStatus, appEndpointStatus(cfg)),
		TurnLease: func(ctx context.Context, vm domain.VMID) (time.Time, error) {
			if h := leases.Load(); h != nil {
				return h.TurnLease(ctx, vm)
			}
			return time.Time{}, nil
		},
		OnTTLWarning: func(ctx context.Context, vm domain.VM) { warnGuest(ctx, procs, vm, log) },
		Busy: func(vm domain.VMID, generation int64) bool { // the sweeper starts before procs is set
			p := busyProcs.Load()
			return p != nil && p.Busy(vm, generation)
		},
		TotalMemory:     preflight.TotalMemory,
		DefaultEgress:   cfg.Network.DefaultEgress(),
		Credentials:     credHost,
		Workspace:       workspace,
		WorkspaceSecret: workspaceSecrets(secretsFile(cfg.Harness.SecretsFile)),
		Log:             log,
	})
	if err != nil {
		return err
	}
	if err := svc.Start(ctx); err != nil {
		return err
	}
	defer svc.Close()
	g16, err := startEvents(ctx, cfg, db, svc, log)
	if err != nil {
		return err
	}
	if guest {
		principal := httpapi.LocalPrincipal(os.Getuid())
		sshCA.SetDefaultUser(func(vm domain.VMID) string { return svc.DefaultUser(context.Background(), vm) })
		if opts.GuestDialer == nil {
			opts.GuestDialer = agentDialer{svc: svc, secrets: secrets, principal: principal}
		}
		if opts.GuestSSH == nil {
			opts.GuestSSH = sshCA
		}
	}
	probe, _ := store.Probe(ctx)
	caps := rt.Capabilities()
	log.Info("daemon starting", "version", buildinfo.Version, "runtime", caps.Engine, "engineVersion", caps.EngineVersion,
		"storage", probe.Mode, "fastClone", probe.FastClone, "storageDetail", probe.Detail)
	for _, c := range caps.HostChecks {
		log.Info("host check", "name", c.Name, "ok", c.OK, "detail", c.Detail)
	}
	if rt.Capabilities().Engine == fake.Engine {
		log.Warn("runtime=fake: no real VMs are started; for development and tests only")
	}

	// The process manager is shared by both listeners and outlives them.
	if guest {
		procs = guestexec.New(guestexec.Config{Targets: svc, Secrets: secrets, Store: guestexec.DBStore{DB: db}, Log: log, Activity: svc.Touch})
		busyProcs.Store(procs)
		defer procs.Close()
		late.set(procs)
	}
	// Job history and retained output (G7) read the state store; live
	// process samples need the guest.
	jcfg := jobs.Config{DB: db, VMs: svc}
	if procs != nil {
		jcfg.Sampler = procs
	}
	jobSvc := jobs.New(jcfg)

	// Prompts (harness conversations) run on the process API.
	var prompts *harness.Service
	if procs != nil {
		prompts = harness.New(harness.Config{DB: db, Procs: procs, VMs: svc, Commands: cfg.Harness.Commands,
			Secrets: secretsFile(cfg.Harness.SecretsFile), CredentialProxyURL: proxyURL,
			DefaultTurnTimeout: time.Duration(cfg.Harness.DefaultTurnTimeout), MaxTurnTimeout: config.MaxTurnTimeout,
			Log: log.With("component", "harness")})
		if err := prompts.Start(ctx); err != nil {
			return err
		}
		leases.Store(prompts)
		defer prompts.Close()
	}

	// Runs (createRun): VM create/delete through lifecycle, the command
	// through the process API. Unfinished runs resume here after a restart.
	var rs *runs.Service
	if procs != nil {
		if rs, err = runs.New(runs.Config{DB: db, VMs: svc, Procs: procs, OutputGrace: time.Duration(cfg.Runs.OutputGrace),
			Log: log.With("component", "runs")}); err != nil {
			return err
		}
		if err := rs.Start(ctx); err != nil {
			return err
		}
		defer rs.Close()
	}

	// issueSSHCredential: certificates from the guest SSH CA for this VM
	// generation, host keys pinned from the barrier's registry.
	var creds httpapi.SSHCredentialIssuer
	if procs != nil && sshCA != nil {
		creds = &guestssh.Issuer{CA: sshCA, Guest: procs}
	}

	// Listeners: the owner-restricted Unix socket, plus the tailnet gateway
	// when enabled (same handler, WhoIs-based owner Authenticator).
	// Service tokens: managed by the owner, accepted as bearers on the
	// tailnet listener (the local socket is owner-only and ignores them).
	toks := &tokens.Service{DB: db, VMs: svc}
	// Token use counts are written in batches; the last batch at shutdown.
	tokCtx, stopToks := context.WithCancel(ctx)
	tokDone := make(chan struct{})
	go func() { toks.Run(tokCtx); close(tokDone) }()
	defer func() { stopToks(); <-tokDone }()

	files := snapfiles.New(svc, store)
	// Reverse forwards: grants on guest loopback ports, revoked with their
	// VM's stop, delete or next generation.
	rev, err := newReverse(cfg, svc, procs, httpapi.LocalPrincipal(os.Getuid()), log)
	if err != nil {
		return err
	}
	defer rev.Close()

	// App endpoints: stable tailnet URLs for guest apps, served by the
	// gateway (unsupported without one).
	apps, err := newAppEndpoints(cfg, svc, log)
	if err != nil {
		return err
	}
	defer apps.Close()

	// Members (G13): other tailnet users with rights on what is shared;
	// their live sessions are tracked so unsharing cuts them.
	sessions := &gateway.Sessions{}
	mem, err := newMembers(cfg, svc, sessions, credControl, credHost, log)
	if err != nil {
		return err
	}
	if mem != nil {
		team.Store(mem)
		defer mem.Close()
	}

	stopTailnet, err := startTailnet(ctx, cfg, svc, procs, prompts, jobSvc, rs, creds, toks, credControl, files, rev, apps, g16, mem, sessions, opts, log)
	if err != nil {
		return err
	}
	defer stopTailnet()

	ln, err := httpapi.ListenUnix(cfg.Socket)
	if err != nil {
		return err
	}
	defer os.Remove(cfg.Socket)
	api := httpapi.New(svc, httpapi.PeerAuthenticator{OwnerUID: os.Getuid()}, log)
	api.SetTokens(toks)
	api.SetCredentials(credControl)
	api.SetSnapshotFiles(files)
	api.SetReverse(rev)
	api.SetJobs(jobSvc) // host records: works without a guest transport
	if cfg.Tailnet.Enabled {
		api.SetAppEndpoints(apps)
		api.SetMembers(mem, sessions)
	}
	g16.apply(api)
	if procs != nil {
		api.SetProcesses(procs)
		api.SetPrompts(prompts)
		api.SetRuns(rs)
		api.SetSSHCredentials(creds)
	}
	srv := httpx.Harden(&http.Server{
		Handler:     api.Handler(),
		ConnContext: httpapi.PeerConnContext,
		BaseContext: func(net.Listener) context.Context { return ctx },
	})
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", "socket", cfg.Socket)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Runtimes lists the runtimes this build can run; keep it in sync with
// newRuntime. `init` picks firecracker only when it is listed here.
func Runtimes() []string {
	switch {
	case goruntime.GOOS == "linux" && goruntime.GOARCH == "amd64":
		return []string{"fake", "firecracker"}
	case goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64":
		return []string{"fake", "mac"}
	}
	return []string{"fake"}
}

// checkHost runs the same preflight checks as `davit doctor` and refuses to
// start on a failure; warnings are logged with their fix hints.
func checkHost(ctx context.Context, cfg config.Daemon, log *slog.Logger) error {
	if !slices.Contains(Runtimes(), cfg.Runtime) {
		return fmt.Errorf("runtime %q is not available in this build (available: %v)", cfg.Runtime, Runtimes())
	}
	rep := preflight.Run(ctx, preflight.FromConfig(cfg))
	var failed []string
	for _, c := range rep.Checks {
		switch c.Status {
		case preflight.Warn:
			log.Warn("preflight: "+c.Detail, "check", c.ID, "hint", c.Hint)
		case preflight.Fail:
			if c.ID == "disk_space" {
				// Low space must not stop the daemon: it is needed to stop and
				// delete VMs and reclaim space. Admission enforces the floor.
				log.Warn("preflight: "+c.Detail, "check", c.ID, "hint", c.Hint)
				continue
			}
			log.Error("preflight: "+c.Detail, "check", c.ID, "hint", c.Hint)
			failed = append(failed, c.ID)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("host preflight failed (%s); run `%s doctor` for details", strings.Join(failed, ", "), buildinfo.Name)
	}
	return nil
}

// bootPlan is the restore-barrier plan for key: hostname from the VM name
// (else its ID); new VMs and forks rotate machine-id and SSH host keys,
// resume of the same disk keeps them. SSH readiness is required except on
// the fake runtime, which has no sshd.
func bootPlan(ctx context.Context, svc *lifecycle.Service, rt runtime.Runtime, key runtime.Key) (guestclient.Plan, error) {
	info, err := svc.BootInfo(ctx, key)
	if err != nil {
		return guestclient.Plan{}, err
	}
	name := info.Name
	if name == "" {
		name = string(key.VMID)
	}
	return guestclient.Plan{
		Hostname:       guestclient.HostnameFor(name),
		RotateIdentity: info.Kind == domain.OpCreate || info.Kind == domain.OpFork,
		WaitSSH:        rt.Capabilities().Engine != fake.Engine,
	}, nil
}

func newRuntime(ctx context.Context, cfg config.Daemon, log *slog.Logger, netProv network.Provider, nic firecracker.NIC,
	shutdown func(context.Context, runtime.Key, runtime.Dialer) error) (runtime.Runtime, error) {
	switch cfg.Runtime {
	case "fake":
		opts := fake.Options{
			Dir:       filepath.Join(cfg.DataDir, "runtime"),
			BootDelay: time.Duration(cfg.Fake.BootDelay),
			StopDelay: time.Duration(cfg.Fake.StopDelay),
		}
		if cfg.Fake.GuestAgent {
			if opts.GuestTransport = newFakeGuests(filepath.Join(cfg.DataDir, "fake-guests"), log); opts.GuestTransport == nil {
				return nil, fmt.Errorf("config: fake.guestAgent needs Linux (the guest agent is Linux-only)")
			}
			log.Warn("fake.guestAgent: exec runs commands on this host as the daemon user")
		}
		return fake.New(opts)
	case "firecracker":
		var bin firecracker.Binary
		var err error
		if cfg.Firecracker.Binary != "" {
			bin, err = firecracker.VerifyBinary(cfg.Firecracker.Binary, "")
		} else {
			bin, err = firecracker.EnsureBinary(ctx, filepath.Join(cfg.DataDir, "bin"), firecracker.FetchOptions{})
		}
		if err != nil {
			return nil, err
		}
		o := firecracker.Options{
			Dir: filepath.Join(cfg.DataDir, "runtime", "firecracker"), Binary: bin.Path, NIC: nic,
			CheckpointDir: cfg.Firecracker.CheckpointDir, GoldenMinFree: cfg.Firecracker.GoldenMinFreeBytes,
			GuestShutdown: shutdown, Log: log, GuestReady: guestclient.SettledGate(guestclient.DefaultPort, 0),
		}
		if nic.Tap != "" {
			// Golden captures run in a network of their own with the same
			// device layout as instances (a golden restores only into it).
			o.CaptureNetwork = func(ctx context.Context) (runtime.NetworkHandle, func(), error) {
				// No user workload runs in a golden capture: no egress.
				h, err := netProv.Create(ctx, "golden", time.Now().UnixNano(), domain.Egress{Mode: domain.EgressOff})
				if err != nil {
					return runtime.NetworkHandle{}, nil, err
				}
				release := func() { netProv.Destroy(context.Background(), h) }
				enter, err := netProv.Enter(h)
				if err != nil {
					release()
					return runtime.NetworkHandle{}, nil, err
				}
				return lifecycle.RuntimeNetwork(h, enter), release, nil
			}
		}
		rt, err := firecracker.New(o)
		if err != nil {
			return nil, err
		}
		return rt, nil
	case "mac":
		if !slices.Contains(Runtimes(), "mac") {
			break
		}
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		return mac.New(mac.Options{
			Dir: cfg.MacRuntimeDir(), Helper: cfg.MacHelperPath(exe), Log: log,
			Client: buildinfo.Name + " " + buildinfo.Version,
		})
	}
	return nil, fmt.Errorf("runtime %q not available on %s/%s (available: %v)", cfg.Runtime, goruntime.GOOS, goruntime.GOARCH, Runtimes())
}

// newNetwork picks the guest network provider and the NIC layout it gives
// guests. Only real VMs get one; mode "" (auto) uses rootless namespaces
// when the host allows them and otherwise runs without a guest NIC.
func newNetwork(ctx context.Context, cfg config.Daemon, log *slog.Logger) (network.Provider, firecracker.NIC, network.Config, error) {
	if cfg.Runtime != "firecracker" || cfg.Network.Mode == "none" {
		return network.None{}, firecracker.NIC{}, network.Config{}, nil
	}
	ncfg := network.DefaultConfig(hostAddrs()...)
	if err := cfg.Network.Apply(&ncfg); err != nil {
		return nil, firecracker.NIC{}, network.Config{}, err
	}
	for _, s := range ncfg.Egress.Services {
		for _, h := range ncfg.Egress.HostAddrs {
			if h.Contains(s.Addr) {
				log.Warn("network.lanServices entry is a host address and stays denied; use a reverse connection", "service", s.String())
			}
		}
	}
	// Holders live under the data dir so a restarted daemon can reap ones it
	// never recorded.
	ncfg.StateDir = filepath.Join(cfg.DataDir, "network")
	// The guest reaches the credential proxy on its own TAP.
	ncfg.TapServices = []uint16{credproxy.GuestPort}
	r, err := network.NewRootless(ncfg)
	if err != nil {
		return nil, firecracker.NIC{}, network.Config{}, err
	}
	caps := r.Probe(ctx)
	for _, m := range caps.Modes {
		if m.Mode != network.ModeRootless {
			continue
		}
		if m.Usable {
			return r, firecracker.NIC{Tap: ncfg.TapName, MAC: ncfg.GuestMAC}, ncfg, nil
		}
		if cfg.Network.Mode == "rootless" {
			return nil, firecracker.NIC{}, network.Config{}, fmt.Errorf("network.mode=rootless unavailable: %s", m.Reason)
		}
		log.Warn("rootless guest network unavailable; VMs get no NIC (vsock only)", "reason", m.Reason)
	}
	return network.None{}, firecracker.NIC{}, network.Config{}, nil
}

// hostAddrs are the host's own addresses, which guest egress may never
// reach (network.EgressPolicy.HostAddrs).
func hostAddrs() []netip.Prefix {
	var out []netip.Prefix
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				ip = ip.Unmap()
				out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
			}
		}
	}
	return out
}

// agentShutdown asks the guest agent of key's generation to shut the guest
// down cleanly (services stopped, filesystems unmounted), now: the runtime
// enforces the stop grace.
func agentShutdown(secrets guestclient.SecretStore) func(ctx context.Context, key runtime.Key, d runtime.Dialer) error {
	return func(ctx context.Context, key runtime.Key, d runtime.Dialer) error {
		s, err := secrets.Load(ctx, key)
		if err != nil {
			return err
		}
		if len(s) == 0 {
			return fmt.Errorf("no guest agent session secret for %v", key)
		}
		c, err := guestclient.Connect(ctx, func(ctx context.Context) (net.Conn, error) { return d.DialGuest(ctx, guestclient.DefaultPort) },
			guestclient.Session{VMID: string(key.VMID), Generation: uint64(key.Generation), Secrets: s})
		if err != nil {
			return err
		}
		defer c.Close()
		return c.Shutdown(ctx, 0)
	}
}

// agentDialer is the gateway's GuestDialer: a TCP port on the guest's
// loopback through the guest agent (guestclient.DialTCP) over the current
// instance's vsock, refusing a target whose generation was replaced.
type agentDialer struct {
	svc       *lifecycle.Service
	secrets   guestclient.SecretStore
	principal domain.PrincipalID
}

func (d agentDialer) DialGuest(ctx context.Context, t gateway.Target, port uint16) (net.Conn, error) {
	tgt, err := d.svc.GuestTarget(ctx, d.principal, string(t.VMID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", gateway.ErrNotRunning, err)
	}
	if tgt.Key.Generation != t.Generation {
		return nil, fmt.Errorf("%w: generation %d was replaced by %d", gateway.ErrNotRunning, t.Generation, tgt.Key.Generation)
	}
	s, err := d.secrets.Load(ctx, tgt.Key)
	if err != nil {
		return nil, err
	}
	dial := func(ctx context.Context) (net.Conn, error) { return tgt.Dialer.DialGuest(ctx, guestclient.DefaultPort) }
	return guestclient.DialTCP(ctx, dial, guestclient.Session{VMID: string(tgt.Key.VMID), Generation: uint64(tgt.Key.Generation), Secrets: s}, port)
}

// saveReserve is the space set aside for stops: <dataDir>/save-reserve
// (nil when limits.saveReserveBytes disables it).
func saveReserve(cfg config.Daemon) lifecycle.SaveReserve {
	n := cfg.Limits.SaveReserve()
	if n == 0 {
		return nil
	}
	return &lifecycle.FileReserve{Path: filepath.Join(cfg.DataDir, "save-reserve"), Bytes: n}
}

// coldPolicy attaches the configured cold storage tier to store (nil
// without one). A tier that cannot be attached (its disk is not mounted)
// does not stop the daemon: everything hot works, and the reason is
// returned for the VMs on cold storage, which cannot resume until it is
// back (docs/storage-tiers.md#when-the-cold-disk-is-missing).
func coldPolicy(ctx context.Context, cfg config.Daemon, db *state.DB, store *storage.FS, log *slog.Logger) (*lifecycle.ColdPolicy, string) {
	c := cfg.ColdStorage
	root := filepath.Join(cfg.DataDir, "storage")
	if c.Dir == "" {
		was, ok := storage.ColdRecorded(root)
		if !ok {
			return nil, ""
		}
		n, err := lifecycle.ColdVMs(ctx, db)
		if err == nil && n == 0 {
			if err := storage.ForgetCold(root); err != nil {
				log.Warn("forget the removed cold storage tier", "err", err)
			}
			return nil, ""
		}
		why := fmt.Sprintf("coldStorage.dir is not configured, but %d VMs are on the cold tier at %s", n, was)
		log.Error("cold storage unavailable", "reason", why)
		return nil, why
	}
	if err := store.AttachCold(c.Dir); err != nil {
		log.Error("cold storage unavailable: VMs on it cannot resume until it is back", "dir", c.Dir, "err", err)
		return nil, err.Error()
	}
	same := preflight.SameFilesystem(cfg.DataDir, c.Dir)
	p, _ := store.ColdProbe()
	if same {
		log.Warn("coldStorage.dir is on the data dir's filesystem: moving VMs there frees nothing", "dir", c.Dir)
	}
	pol := &lifecycle.ColdPolicy{
		After: time.Duration(c.After), BytesPerSecond: c.BytesPerSecond, MinBytes: c.MinBytes,
		DiskSpace:      func() (domain.DiskUsage, error) { return preflight.DiskSpace(c.Dir) },
		SyncDisk:       func() error { return preflight.SyncFS(c.Dir) },
		SameFilesystem: same, HostBusy: ioBusy(c.IOBusyPercent),
		MinFreeBytes: c.MinFreeBytes, MinFreePercent: c.MinFreePercent, Drain: c.Drain,
	}
	log.Info("cold storage tier", "dir", c.Dir, "probe", p.Detail, "after", pol.AfterOrDefault().String(), "drain", c.Drain)
	return pol, ""
}

// ioBusy reports the host busy above pct percent I/O pressure (0: the
// default; negative: never).
func ioBusy(pct int) func() bool {
	if pct < 0 {
		return nil
	}
	if pct == 0 {
		pct = preflight.DefaultIOBusyPercent
	}
	return func() bool {
		v, ok := preflight.IOPressure()
		return ok && v > float64(pct)
	}
}

func autoDeletePolicy(a config.AutoDelete) lifecycle.AutoDeletePolicy {
	return lifecycle.AutoDeletePolicy{
		Disabled:       a.Enabled != nil && !*a.Enabled,
		InstanceMinAge: time.Duration(a.InstanceMinAge), ImageMinAge: time.Duration(a.ImageMinAge),
		MarginBytes: a.MarginBytes,
	}
}

// imageBlobs is the daemon's own blob store, <dataDir>/images/blobs. Its
// blobs stay while any manifest in <dataDir>/images/manifests, any retired
// manifest (lifecycle.RetiredDir: older boxes still boot its kernel) or any
// configured manifest names them (configured images may share it).
func imageBlobs(cfg config.Daemon) *lifecycle.BlobStore {
	dir := filepath.Join(cfg.DataDir, "images")
	return &lifecycle.BlobStore{Dir: filepath.Join(dir, "blobs"), Manifests: func() ([]string, error) {
		found, err := filepath.Glob(filepath.Join(dir, "manifests", "*.json"))
		if err != nil {
			return nil, err
		}
		retired, err := filepath.Glob(filepath.Join(lifecycle.RetiredDir(dir), "*.json"))
		if err != nil {
			return nil, err
		}
		found = append(found, retired...)
		for _, i := range cfg.Images {
			found = append(found, i.Manifest)
		}
		return found, nil
	}, MinAge: time.Duration(cfg.AutoDelete.BlobMinAge)}
}

func newCatalog(ctx context.Context, cfg config.Daemon) (lifecycle.ImageCatalog, error) {
	var src []lifecycle.ManifestSource
	for _, i := range cfg.Images {
		src = append(src, lifecycle.ManifestSource{Manifest: i.Manifest, BlobDir: i.BlobDir})
	}
	if len(src) == 0 {
		dir := filepath.Join(cfg.DataDir, "images")
		found, err := filepath.Glob(filepath.Join(dir, "manifests", "*.json"))
		if err != nil {
			return nil, err
		}
		slices.Sort(found)
		for _, m := range found {
			src = append(src, lifecycle.ManifestSource{Manifest: m, BlobDir: filepath.Join(dir, "blobs")})
		}
	}
	if len(src) > 0 {
		cache := filepath.Join(cfg.DataDir, "images", "cache")
		cat, err := lifecycle.LoadManifestCatalog(ctx, cache, src...)
		if err != nil {
			return nil, err
		}
		// The images upgrades replaced, for the VMs and snapshots made from them.
		dir := filepath.Join(cfg.DataDir, "images")
		retired, err := filepath.Glob(filepath.Join(lifecycle.RetiredDir(dir), "*.json"))
		if err != nil {
			return nil, err
		}
		slices.Sort(retired)
		var rs []lifecycle.ManifestSource
		for _, m := range retired {
			rs = append(rs, lifecycle.ManifestSource{Manifest: m, BlobDir: filepath.Join(dir, "blobs")})
		}
		if err := cat.LoadRetired(ctx, cache, rs...); err != nil {
			return nil, err
		}
		return cat, nil
	}
	if cfg.Runtime != "fake" {
		return nil, fmt.Errorf("no images: put a manifest in %s (blobs in %s) or set images in the config",
			filepath.Join(cfg.DataDir, "images", "manifests"), filepath.Join(cfg.DataDir, "images", "blobs"))
	}
	img, err := lifecycle.DevImage(filepath.Join(cfg.DataDir, "images"))
	if err != nil {
		return nil, err
	}
	return lifecycle.NewStaticCatalog(img), nil
}

// startTailnet configures the gateway synchronously (bad config fails the
// daemon) and joins the tailnet in the background, so the local socket is
// usable while enrollment waits for a login or the network. The returned
// stop function waits for that and closes the node.
func startTailnet(ctx context.Context, cfg config.Daemon, svc *lifecycle.Service, procs *guestexec.Manager, prompts *harness.Service, jobSvc *jobs.Service,
	rs *runs.Service, creds httpapi.SSHCredentialIssuer, toks *tokens.Service, credControl *credproxy.Control, files *snapfiles.Service, rev *reversefwd.Service, apps *appendpoint.Service, g16 *g16,
	mem *members.Service, sessions *gateway.Sessions, opts Options, log *slog.Logger) (func(), error) {
	t := cfg.Tailnet
	if !t.Enabled {
		return func() {}, nil
	}
	owner := gateway.OwnerPolicy{Logins: t.OwnerLogins, Tags: t.OwnerTags}
	if err := owner.Validate(); err != nil {
		return nil, err
	}
	// The tailnet owner is the same principal as the local owner, so both
	// transports see the same VMs (v0: single owner).
	principal := httpapi.LocalPrincipal(os.Getuid())
	stateDir := cfg.TailnetStateDir()
	hostname := t.Hostname
	if hostname == "" {
		hostname = defaultTailnetHostname()
	}
	glog := log.With("component", "tailnet")
	// Configured logins bootstrap the owner; each is pinned to the stable
	// tailnet user ID it first matched (owners.json in the state dir).
	owners, err := gateway.OpenOwners(filepath.Join(stateDir, "owners.json"), owner,
		func(f string, a ...any) { glog.Info(fmt.Sprintf(f, a...)) })
	if err != nil {
		return nil, err
	}
	api := httpapi.New(svc, gateway.Authenticator{Owners: owners, Principal: principal, Tokens: toks, Members: mem}, log)
	api.SetTokens(toks)
	api.SetCredentials(credControl)
	api.SetSnapshotFiles(files)
	api.SetReverse(rev)
	api.SetJobs(jobSvc) // host records: works without a guest transport
	api.SetAppEndpoints(apps)
	g16.apply(api)
	api.SetMembers(mem, sessions)
	if procs != nil {
		api.SetProcesses(procs)
		api.SetPrompts(prompts)
		api.SetRuns(rs)
		api.SetSSHCredentials(creds)
	}
	gw, err := gateway.New(gateway.Config{
		StateDir:    stateDir,
		Hostname:    hostname,
		ControlURL:  t.ControlURL,
		AuthKeyFile: t.AuthKeyFile,
		AuthKeyEnv:  config.AuthKeyEnv,
		Logf:        func(f string, a ...any) { glog.Debug(fmt.Sprintf(f, a...)) },
		UserLogf:    func(f string, a ...any) { glog.Info(fmt.Sprintf(f, a...)) },
	}, gateway.Options{
		Auth:        gateway.OwnerAuthorizer{Owners: owners, Principal: principal, VMs: svc, Members: mem},
		Apps:        gateway.OwnerAppAuthorizer{Owners: owners, Principal: principal, Tokens: toks, VMs: svc, Members: mem},
		Sessions:    sessions,
		DesktopPage: desktopui.Page,
		Dialer:      opts.GuestDialer,
		Guest:       opts.GuestSSH,
		API:         api.Handler(),
		APITLS:      t.HTTPS,
		TLSCertFile: t.TLSCertFile,
		TLSKeyFile:  t.TLSKeyFile,
		// The enrollment report `doctor` and `status` read: why remote
		// access does not work yet, and the URL clients use once it does.
		OnStatus: func(s gateway.Status) {
			if err := gateway.WriteStatus(filepath.Join(stateDir, gateway.StatusFile), s); err != nil {
				glog.Warn("write gateway status", "err", err)
			}
			switch s.State {
			case gateway.EnrollNeedsLogin:
				glog.Warn("tailnet: sign in to enroll this host", "url", s.LoginURL)
			case gateway.EnrollNeedsApproval:
				glog.Warn("tailnet: waiting for a tailnet admin to approve this device")
			}
		},
		Logf: func(f string, a ...any) { glog.Info(fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		return nil, err
	}
	if opts.GuestDialer == nil {
		glog.Warn("no guest transport for this runtime: tailnet SSH and forwards are unavailable")
	}
	apps.SetGateway(gw)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := gw.Start(ctx); err != nil {
			if ctx.Err() == nil {
				glog.Error("tailnet gateway failed", "err", err)
			}
			return
		}
		glog.Info("tailnet gateway up", "fqdn", gw.Node().FQDN(), "addrs", fmt.Sprint(gw.Node().Addrs()),
			"ssh", opts.GuestSSH != nil, "tls", gw.TLSMode())
	}()
	return func() {
		<-done
		if err := gw.Close(); err != nil {
			glog.Warn("tailnet gateway close", "err", err)
		}
	}, nil
}

// defaultTailnetHostname is "davit-<os hostname>" reduced to a DNS label.
func defaultTailnetHostname() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	label := strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, h), "-")
	name := buildinfo.Name
	if label != "" {
		name += "-" + label
	}
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

// shippedFeatures marks the parity v1 contract features this build serves
// (CONTRACT-v1 "Shipping a feature"); the rest stay not_implemented. Some
// need a guest transport.
func shippedFeatures(guest bool, proxy, apps domain.FeatureStatus) map[string]domain.FeatureStatus {
	f := map[string]domain.FeatureStatus{
		// Members, like app endpoints, need the tailnet gateway.
		domain.FeatureMembers: apps,
		// App endpoints need the tailnet gateway.
		domain.FeatureAppEndpoints: apps,
		// So does the desktop viewer, an app endpoint of kind desktop.
		domain.FeatureDesktop:     apps,
		domain.FeatureCredentials: domain.FeatureAvailable, domain.FeatureCredentialProxy: proxy,
		// Snapshot files read the store directly: no runtime involved.
		domain.FeatureSnapshotFiles: domain.FeatureAvailable,
		// Reverse forwards need guest listeners.
		domain.FeatureReverseForward: reverseStatus(guest),
		domain.FeatureEnvironments:   domain.FeatureAvailable,
		domain.FeatureProcessHistory: domain.FeatureAvailable,
		// G9: lifecycle features on every runtime.
		domain.FeatureHostCapacity:    domain.FeatureAvailable,
		domain.FeatureBatchCreate:     domain.FeatureAvailable,
		domain.FeatureCapacityReclaim: domain.FeatureAvailable,
		domain.FeatureResize:          domain.FeatureAvailable,
		domain.FeatureRecover:         domain.FeatureAvailable,
		// G18 conveniences.
		domain.FeatureWhoami:      domain.FeatureAvailable,
		domain.FeatureTokenRotate: domain.FeatureAvailable,
		domain.FeatureTokenUsage:  domain.FeatureAvailable,
		// G16: the event stream and webhooks run beside lifecycle.
		domain.FeatureEvents:   domain.FeatureAvailable,
		domain.FeatureWebhooks: domain.FeatureAvailable,
		// G17: enforced by lifecycle and the state store on every runtime.
		domain.FeaturePrivacy: domain.FeatureAvailable,
	}
	if guest {
		// The process manager prepares workspaces and samples guests.
		f[domain.FeatureWorkspacePrep] = domain.FeatureAvailable
		f[domain.FeatureGuestPS] = domain.FeatureAvailable
		// Runs exec through the process API; pushTree writes through the
		// guest agent's archive extraction.
		f[domain.FeatureRuns] = domain.FeatureAvailable
		f[domain.FeaturePush] = domain.FeatureAvailable
	}
	// The G5 prompt loop runs on the process API.
	prompt := domain.FeatureUnsupported
	if guest {
		prompt = domain.FeatureAvailable
	}
	for _, name := range []string{domain.FeaturePromptFeed, domain.FeaturePromptInterrupt, domain.FeaturePromptAttachments,
		domain.FeaturePromptOptions, domain.FeaturePromptTurnLease, domain.FeaturePromptSteer} {
		f[name] = prompt
	}
	return f
}

// lateGuest forwards workspace preparation to the process manager once it
// exists; a preparation started earlier (lifecycle re-drives them at
// start) waits for it.
type lateGuest struct {
	ready chan struct{}
	m     *guestexec.Manager
}

func (l *lateGuest) set(m *guestexec.Manager) { l.m = m; close(l.ready) }

func (l *lateGuest) PrepareWorkspace(ctx context.Context, principal domain.PrincipalID, ref string, req guestproto.WorkspacePrepare,
	progress func(guestproto.WorkspaceProgress)) (guestproto.WorkspaceResult, error) {
	select {
	case <-l.ready:
	case <-ctx.Done():
		return guestproto.WorkspaceResult{}, ctx.Err()
	}
	return l.m.PrepareWorkspace(ctx, principal, ref, req, progress)
}

// workspaceSecrets resolves manifest secret files from the daemon's named
// secrets (harness.secretsFile); nil when none are configured.
func workspaceSecrets(src harness.Secrets) func(ctx context.Context, name string) ([]byte, error) {
	if src == nil {
		return nil
	}
	return func(_ context.Context, name string) ([]byte, error) {
		v, ok, err := src(name)
		switch {
		case err != nil:
			return nil, err
		case !ok:
			return nil, domain.Errorf(domain.CodeNotFound, "no secret %q in the daemon's secrets file", name)
		}
		return []byte(v), nil
	}
}

// harnessCaps reports the prompt harnesses (none without a guest
// transport). No harness takes attachments, reasoning options or steering
// yet: the parity v1 contract marks those not implemented.
func harnessCaps(guest bool) []domain.HarnessCapabilities {
	if !guest {
		return nil
	}
	return harness.Capabilities()
}

// fakeRuntime is set when the daemon runs the fake runtime.
var fakeRuntime bool

// listenerNames reports the API listeners for capabilities.
func listenerNames(cfg config.Daemon) []string {
	l := []string{"unix"}
	if cfg.Tailnet.Enabled {
		l = append(l, "tailnet")
	}
	return l
}

// serviceNames reports the guest services for capabilities.
func serviceNames(guest bool) []string {
	if !guest {
		return []string{}
	}
	return []string{"exec", "files", "prompts", "ssh", "tunnels"}
}

// warnGuest broadcasts the coming auto-stop to the guest's terminals
// (best effort: `wall` may be missing or the agent unreachable). Never on
// the fake runtime, whose "guest" commands run on this host.
func warnGuest(ctx context.Context, procs *guestexec.Manager, vm domain.VM, log *slog.Logger) {
	if procs == nil || vm.ExpiresAt == nil || fakeRuntime {
		return
	}
	left := time.Until(*vm.ExpiresAt).Round(time.Minute)
	msg := fmt.Sprintf("%s: this VM (%s) auto-stops in %s at %s. Extend it from the host: %s extend %s [DURATION]",
		buildinfo.Name, vm.Name, left, vm.ExpiresAt.Local().Format("15:04"), buildinfo.Name, vm.Name)
	key := fmt.Sprintf("ttl-warning:%s:%d", vm.ID, vm.ExpiresAt.Unix())
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := procs.Exec(ctx, vm.Owner, string(vm.ID), key, api.ExecRequest{Argv: []string{"wall", msg}, DeadlineSeconds: 10}); err != nil {
		log.Info("auto-stop warning not delivered to the guest", "vm", vm.ID, "err", err)
	}
}
