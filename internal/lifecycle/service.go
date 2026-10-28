// Package lifecycle is the one VM state machine (SPEC §2, §5): it plans
// requests against capabilities and limits, journals every operation in
// SQLite before any external effect, executes it against runtime + storage,
// and replays unfinished operations after a daemon restart.
//
// Execution pattern for every step: journal intent (phase) -> call runtime or
// storage outside any transaction -> commit the observation. Each step is
// idempotent on IDs chosen at planning time, so recovery simply re-runs an
// unfinished operation from the start.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/network"
	"davit/internal/runtime"
	"davit/internal/state"
	"davit/internal/storage"
)

// ReadinessProber decides when a booted instance is ready. The guest-agent
// workstream supplies one that runs the authenticated restore barrier.
type ReadinessProber interface {
	Probe(ctx context.Context, rt runtime.Runtime, h runtime.Handle) (domain.Readiness, error)
}

// GuestIdentity reports what the ReadinessProber verified about one boot.
type GuestIdentity interface {
	SSHHostKeyFingerprints(key runtime.Key) []string
}

// RuntimeProber reports ready once the runtime says the instance is running.
// It is the M0 prober for runtimes without a guest transport; it never
// claims agent or SSH readiness it did not verify.
type RuntimeProber struct{ Poll time.Duration }

func (p RuntimeProber) Probe(ctx context.Context, rt runtime.Runtime, h runtime.Handle) (domain.Readiness, error) {
	poll := p.Poll
	if poll == 0 {
		poll = 20 * time.Millisecond
	}
	for {
		st, err := rt.Inspect(ctx, h)
		if err != nil {
			return domain.Readiness{}, err
		}
		switch st.Phase {
		case runtime.PhaseRunning:
			return domain.Readiness{Services: map[string]bool{}}, nil
		case runtime.PhaseExited, runtime.PhaseGone:
			return domain.Readiness{}, domain.Errorf(domain.CodeUnavailable, "instance exited during boot (%s)", st.ExitReason)
		}
		select {
		case <-time.After(poll):
		case <-ctx.Done():
			return domain.Readiness{}, ctx.Err()
		}
	}
}

// Config wires the service.
type Config struct {
	DB      *state.DB
	Runtime runtime.Runtime
	Storage storage.Store
	Images  ImageCatalog
	Prober  ReadinessProber
	// Network provisions each instance generation's network; nil means
	// network.None (no guest NIC, vsock only).
	Network network.Provider
	// OnDeleted runs after a VM's deletion committed (host-side per-VM
	// records owned outside lifecycle, e.g. guest SSH host keys).
	OnDeleted func(ctx context.Context, vm domain.VMID)
	// Credentials, when set, is the host credential proxy (G4): it serves
	// each live VM network and applies domain.GrantAfter to lifecycle events.
	Credentials CredentialHooks
	// Identity, when set, supplies the guest identity the prober verified for
	// a boot (SSH host-key fingerprints); it is published in VM.Endpoint with
	// readiness.
	Identity GuestIdentity
	Limits   domain.Limits
	// DiskSpace measures the data dir's filesystem. With it, admission
	// reserves each new, fork or resume's growth headroom against
	// Limits.DiskFloor (the floor `doctor` checks), the save reserve is
	// kept and the low-disk cleanup runs (diskclean.go). Nil disables them.
	DiskSpace func() (domain.DiskUsage, error)
	// SaveReserve is the space set aside for stops (diskreserve.go); nil
	// keeps none.
	SaveReserve SaveReserve
	// SyncDisk flushes the data dir's filesystem so that DiskSpace sees the
	// space a delete freed (XFS releases unlinked blocks in the background);
	// nil skips it.
	SyncDisk func() error
	// AutoDelete is the low-disk cleanup's policy for unused boxes.
	AutoDelete AutoDeletePolicy
	// Cold is the cold storage tier (coldtier.go); nil: none. Storage
	// must then be a storage.Tiered with its cold tier attached.
	Cold *ColdPolicy
	// ColdUnavailable says why the configured (or once used) cold tier is
	// not attached; cold VMs cannot resume until it is back.
	ColdUnavailable string
	// ImageBlobs is the image blob store pruneImages and the low-disk
	// cleanup prune (blobprune.go); nil: none.
	ImageBlobs *BlobStore
	// FreeMemory reports the host's available memory; with it, admission
	// keeps Limits.MemoryFloor() available (capacity.go). Nil, or
	// errors.ErrUnsupported, disables the check.
	FreeMemory func() (int64, error)
	// TotalMemory reports the host's memory for the capacity report; nil
	// or errors.ErrUnsupported leaves it out.
	TotalMemory func() (int64, error)

	DefaultTTL           time.Duration // default 1h
	StopGrace            time.Duration // default 30s
	ReadyTimeout         time.Duration // default 2m
	IdempotencyRetention time.Duration // default 24h, after terminal state
	// FailureRetry is the first backoff (doubling, five tries) when
	// recording an operation's failure meets a journal error; default
	// 100ms.
	FailureRetry       time.Duration
	TombstoneRetention time.Duration // default 24h
	SweepInterval      time.Duration // default 15s; TTL stops and purges
	// StopAttempts is how many times a graceful stop asks the guest to shut
	// down, StopGrace each (default 3, at most 10); NoStopForce fails the
	// stop after the last attempt instead of killing the VMM (stopretry.go).
	StopAttempts int
	NoStopForce  bool
	// TTLWarning is how long before an auto-stop the host warns (default
	// 10m; negative disables). OnTTLWarning, if set, is called once per
	// expiry when the window opens, e.g. to broadcast to the guest.
	TTLWarning   time.Duration
	OnTTLWarning func(ctx context.Context, vm domain.VM)
	// Profiles are administrator-defined shapes for `type`.
	Profiles []domain.Profile
	// Harnesses are the prompt harnesses capabilities report.
	Harnesses []domain.HarnessCapabilities
	// Workspace prepares pinned environments in guests (G3); nil leaves
	// environment pins not implemented.
	Workspace WorkspacePreparer
	// WorkspaceSecret resolves a manifest's secret file reference to its
	// value on the host; nil means no secrets are configured.
	WorkspaceSecret func(ctx context.Context, name string) ([]byte, error)
	// TurnLease, if set, reports when the active prompt turns on a VM stop
	// holding its auto-stop (zero: none active); Sweep does not auto-stop
	// the VM before then (feature promptTurnLease).
	TurnLease func(ctx context.Context, vm domain.VMID) (time.Time, error)
	// Busy, if set, reports that vm has live activity right now (an open
	// tunnel such as an ssh session, a process stream, or a process of
	// generation still running); Sweep never idle-stops it then, and
	// admission never stops it to make room (reclaim.go).
	Busy func(vm domain.VMID, generation int64) bool
	// Features overrides the status of parity contract features by name
	// (domain.ParityFeatures; default: all not_implemented).
	Features map[string]domain.FeatureStatus
	// Listeners and Services describe what the daemon serves, for
	// capabilities (default: the unix socket, no guest services).
	Listeners []string
	Services  []string

	// DefaultEgress is the egress policy of VMs created without one
	// (default domain.DefaultEgress); it must be normalized.
	DefaultEgress domain.Egress

	Now func() time.Time
	Log *slog.Logger

	// HostOwner owns host-level events (golden.evicted of an image's golden).
	HostOwner domain.PrincipalID
}

// Service is the lifecycle service.
type Service struct {
	cfg   Config
	caps  domain.RuntimeCapabilities
	probe storage.ProbeResult

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	boots  chan struct{} // boot slots (Limits.BootSlots); nil = unlimited

	mu      sync.Mutex
	changed chan struct{} // closed and replaced on every journal write
	leases  map[domain.VMID]storage.Lease
	warned  map[domain.VMID]time.Time // expiry already warned about
	// activity is the last exec, stream or tunnel use of each VM this run
	// saw (Touch); idle stops count from it (idle.go).
	activity map[domain.VMID]time.Time
	// preparing marks VMs whose workspace preparation is running.
	preparing map[domain.VMID]bool
	// queue are the requests waiting for host capacity, FIFO (reclaim.go).
	queue []*waiter
	// reclaimMu serializes making room (reclaim.go).
	reclaimMu sync.Mutex
	// cleanMu serializes the low-disk cleanup (diskclean.go), and guards
	// lastGarbage, when it last collected garbage.
	cleanMu     sync.Mutex
	lastGarbage time.Time
	// fruitless is the journal's change channel when a cleanup (for
	// fruitlessOwner's boxes, or "" all) last freed nothing, at
	// fruitlessAt; the next waits for it to close. stuckLogAt is when a
	// stuck cleanup last logged what blocks it (cleanMu).
	fruitless      <-chan struct{}
	fruitlessAt    time.Time
	fruitlessOwner domain.PrincipalID
	stuckLogAt     time.Time
	// saving counts stops in flight, which may be using the released save
	// reserve (diskreserve.go).
	saving int
	// demoteMu runs one move to cold storage at a time and coldMu one cold
	// sweep (coldtier.go); demoting cancels the move of a VM in flight and
	// coldSkip holds the revision at which a VM measured too small to move;
	// coldFullAt is when the sweep last warned that cold storage is full
	// (coldMu).
	demoteMu, coldMu sync.Mutex
	demoting         map[domain.VMID]context.CancelFunc
	coldSkip         map[domain.VMID]int64
	coldFullAt       time.Time
}

// New validates cfg and returns a service. Call Start before serving.
func New(cfg Config) (*Service, error) {
	if cfg.DB == nil || cfg.Runtime == nil || cfg.Storage == nil || cfg.Images == nil {
		return nil, errors.New("lifecycle: DB, Runtime, Storage and Images are required")
	}
	if cfg.Prober == nil {
		cfg.Prober = RuntimeProber{}
	}
	if cfg.Network == nil {
		cfg.Network = network.None{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	if cfg.DefaultEgress.Mode == "" {
		cfg.DefaultEgress = domain.DefaultEgress()
	}
	if _, err := cfg.DefaultEgress.Normalize(); err != nil {
		return nil, fmt.Errorf("lifecycle: default egress: %w", err)
	}
	def(&cfg.DefaultTTL, time.Hour)
	def(&cfg.StopGrace, 30*time.Second)
	if cfg.StopAttempts == 0 {
		cfg.StopAttempts = DefaultStopAttempts
	}
	if cfg.StopAttempts < 1 || cfg.StopAttempts > MaxStopAttempts {
		return nil, fmt.Errorf("lifecycle: stop attempts %d outside 1..%d", cfg.StopAttempts, MaxStopAttempts)
	}
	def(&cfg.ReadyTimeout, 2*time.Minute)
	def(&cfg.IdempotencyRetention, 24*time.Hour)
	def(&cfg.FailureRetry, 100*time.Millisecond)
	def(&cfg.TombstoneRetention, 24*time.Hour)
	def(&cfg.SweepInterval, 15*time.Second)
	def(&cfg.TTLWarning, 10*time.Minute)
	if cfg.TTLWarning < 0 {
		cfg.TTLWarning = 0
	}
	if cfg.Cold != nil {
		if st, ok := cfg.Storage.(storage.Tiered); !ok {
			return nil, errors.New("lifecycle: a cold storage tier needs a tiered store")
		} else if _, attached := st.ColdProbe(); !attached {
			return nil, errors.New("lifecycle: the store has no cold tier attached")
		}
	}
	s := &Service{cfg: cfg, caps: cfg.Runtime.Capabilities(), changed: make(chan struct{}), leases: map[domain.VMID]storage.Lease{}, warned: map[domain.VMID]time.Time{}, activity: map[domain.VMID]time.Time{}, preparing: map[domain.VMID]bool{},
		demoting: map[domain.VMID]context.CancelFunc{}, coldSkip: map[domain.VMID]int64{}}
	if n := cfg.Limits.BootSlots(); n > 0 {
		s.boots = make(chan struct{}, n)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// Start probes storage, reconciles runtime instances against the journal,
// replays unfinished operations and starts the TTL sweeper.
func (s *Service) Start(ctx context.Context) error {
	p, err := s.cfg.Storage.Probe(ctx)
	if err != nil {
		return fmt.Errorf("lifecycle: storage probe: %w", err)
	}
	s.probe = p
	if err := s.recover(ctx); err != nil {
		return fmt.Errorf("lifecycle: recovery: %w", err)
	}
	s.watchGoldens(ctx)
	// Writing the save reserve may take a while where it cannot be
	// preallocated; the sweeper retries it.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.rearmSaveReserve()
	}()
	s.wg.Add(1)
	go s.sweeper()
	s.kickWorkspaces(ctx) // re-drive preparation a restart interrupted
	return nil
}

// Close stops accepting work and waits for running operations to reach a
// safe point. Interrupted operations stay journaled and resume on restart.
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func (s *Service) notify() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) watch() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// Capabilities reports what this host actually supports.
func (s *Service) Capabilities() domain.HostCapabilities {
	rc := s.caps
	// Lists are always arrays on the wire, whatever the adapter left nil.
	rc.Arch = nonNil(rc.Arch)
	rc.RAMCheckpointFormats = nonNil(rc.RAMCheckpointFormats)
	rc.FingerprintRules = nonNil(rc.FingerprintRules)
	return domain.HostCapabilities{
		Version:   buildinfo.Version,
		Runtime:   rc,
		BootModes: s.caps.BootModes(),
		Storage:   s.probe.Capabilities(),
		Network:   domain.NetworkCapabilities{Listeners: nonNil(s.listeners()), EndpointMode: []string{}},
		States:    domain.AllStates,
		Services:  nonNil(s.cfg.Services),
		Profiles:  nonNil(s.cfg.Profiles),

		TTLWarningSeconds: int64(s.cfg.TTLWarning / time.Second),
		Features:          s.features(),
		Harnesses:         nonNil(s.cfg.Harnesses),
		Capacity:          s.capacity(context.Background()),
	}
}

// features is the parity contract with the configured overrides applied.
func (s *Service) features() []domain.Feature {
	fs := domain.ParityFeatures()
	for i, f := range fs {
		if st, ok := s.cfg.Features[f.Name]; ok {
			fs[i].Status = st
		}
	}
	return fs
}

func (s *Service) listeners() []string {
	if len(s.cfg.Listeners) == 0 {
		return []string{"unix"}
	}
	return s.cfg.Listeners
}

// decorate fills read-time fields of vm.
func (s *Service) decorate(vm *domain.VM) {
	vm.LastActivityAt = nil
	if vm.IdleTimeoutSeconds > 0 && (vm.State == domain.StateReady || vm.State == domain.StateRunning) {
		at := s.lastActivity(*vm)
		vm.LastActivityAt = &at
	}
	vm.AutoStopWarningAt = nil
	if vm.ExpiresAt != nil && s.cfg.TTLWarning > 0 && (vm.State == domain.StateReady || vm.State == domain.StateRunning) {
		at := vm.ExpiresAt.Add(-s.cfg.TTLWarning)
		vm.AutoStopWarningAt = &at
	}
}

// recover reconciles the runtime with the journal (SPEC §5): orphan
// instances are destroyed, live VMs whose instance died go to error, and
// unfinished operations are replayed.
func (s *Service) recover(ctx context.Context) error {
	var vms []*state.VMRecord
	var ops []*domain.Operation
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		if vms, err = tx.AllVMs(); err != nil {
			return err
		}
		ops, err = tx.UnfinishedOperations()
		return err
	}); err != nil {
		return err
	}
	byID := map[domain.VMID]*state.VMRecord{}
	for _, r := range vms {
		byID[r.VM.ID] = r
	}
	pending := map[domain.VMID]bool{}
	for _, op := range ops {
		pending[op.VMID] = true
	}

	// Network namespace holders whose handle never reached a VM record
	// (a crash between create and persist, or a golden capture network)
	// are found by their state directory and reaped before anything new is
	// created.
	if reaper, ok := s.cfg.Network.(network.Reaper); ok {
		var keep []network.Handle
		for _, r := range vms {
			if h := decodeNetwork(r.Network); h != nil {
				keep = append(keep, *h)
			}
		}
		if n, err := reaper.Reap(ctx, keep); err != nil {
			s.cfg.Log.Warn("reap network holders", "err", err)
		} else if n > 0 {
			s.cfg.Log.Warn("reaped orphaned network namespace holders", "count", n)
		}
	}

	handles, err := s.cfg.Runtime.Discover(ctx)
	if err != nil {
		return err
	}
	for _, h := range handles {
		r := byID[h.VMID]
		if r != nil && r.VM.State.Live() && r.VM.InstanceGeneration == h.Generation {
			continue
		}
		s.cfg.Log.Warn("destroying orphan runtime instance", "vm", h.VMID, "generation", h.Generation)
		if err := s.cfg.Runtime.Destroy(ctx, h); err != nil {
			return fmt.Errorf("destroy orphan %v: %w", h.Key, err)
		}
	}
	// Networks of VMs that cannot be running (stopped, error, deleted,
	// unfinished operations aside) are released: their VMM is gone.
	for _, r := range vms {
		if !pending[r.VM.ID] && !r.VM.State.Live() && len(r.Network) > 0 {
			s.releaseNetwork(ctx, r.VM.ID)
		}
	}

	for _, r := range vms {
		if pending[r.VM.ID] || (r.VM.State != domain.StateReady && r.VM.State != domain.StateRunning) {
			continue
		}
		h, err := decodeHandle(r.Handle)
		var st runtime.Status
		if err == nil && h != nil {
			st, err = s.cfg.Runtime.Inspect(ctx, *h)
		}
		if err == nil && h != nil && st.Phase == runtime.PhasePaused {
			// Only a live fork pauses a VM, and never across a restart: a
			// daemon that died mid-checkpoint left it paused.
			s.cfg.Log.Warn("unpausing vm left paused by an interrupted operation", "vm", r.VM.ID)
			if uerr := s.cfg.Runtime.Unpause(ctx, *h); uerr == nil {
				st.Phase = runtime.PhaseRunning
			}
		}
		if err == nil && h != nil && (st.Phase == runtime.PhaseRunning || st.Phase == runtime.PhasePaused) {
			s.relock(ctx, r)
			if nh := decodeNetwork(r.Network); nh != nil && s.cfg.Credentials != nil {
				s.cfg.Credentials.AttachNetwork(ctx, r.VM.ID, *nh)
			}
			continue
		}
		reason := st.ExitReason
		if err != nil {
			reason = err.Error()
		}
		s.cfg.Log.Warn("vm instance lost while daemon was down", "vm", r.VM.ID, "reason", reason)
		if err := s.updateVM(ctx, r.VM.ID, func(rec *state.VMRecord, now time.Time) error {
			rec.VM.LastError = domain.Errorf(domain.CodeUnavailable, "runtime instance exited unexpectedly: %s", reason)
			rec.VM.BootMode, rec.VM.Readiness = domain.BootModeNone, domain.Readiness{}
			return rec.VM.Transition(domain.StateError, now)
		}); err != nil {
			return err
		}
		if h != nil {
			s.cfg.Runtime.Destroy(ctx, *h)
		}
		s.releaseNetwork(ctx, r.VM.ID)
	}

	// Disks a crash left unreferenced (e.g. an ephemeral or noSnapshot
	// stop that recorded its discard but died before collecting) go now.
	if err := s.collect(ctx); err != nil {
		s.cfg.Log.Warn("collect unreferenced storage at start", "err", err)
	}
	for _, op := range ops {
		s.event(ctx, op, "recovered", "resuming after daemon restart")
		s.dispatch(op.ID)
	}
	return nil
}

// relock re-establishes the in-memory storage lease of a running VM.
func (s *Service) relock(ctx context.Context, r *state.VMRecord) {
	l, err := s.cfg.Storage.OpenForRuntime(ctx, storage.DiskID(r.DiskID))
	if err != nil {
		s.cfg.Log.Warn("cannot re-lease disk of running vm", "vm", r.VM.ID, "err", err)
		return
	}
	s.setLease(r.VM.ID, l)
}

func (s *Service) setLease(vm domain.VMID, l storage.Lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases[vm] = l
}

func (s *Service) releaseLease(ctx context.Context, vm domain.VMID) {
	s.mu.Lock()
	l, ok := s.leases[vm]
	delete(s.leases, vm)
	s.mu.Unlock()
	if ok {
		s.cfg.Storage.Release(ctx, l)
	}
}

func (s *Service) sweeper() {
	defer s.wg.Done()
	t := time.NewTicker(s.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			if err := s.Sweep(s.ctx); err != nil && s.ctx.Err() == nil {
				s.cfg.Log.Error("sweep", "err", err)
			}
			s.enforceGoldenFloor(s.ctx)
		}
	}
}

// leased reports whether an active prompt turn holds vm's auto-stop. A
// lease that cannot be read does not hold it.
func (s *Service) leased(ctx context.Context, vm domain.VMID, now time.Time) bool {
	if s.cfg.TurnLease == nil {
		return false
	}
	until, err := s.cfg.TurnLease(ctx, vm)
	if err != nil {
		s.cfg.Log.Warn("read turn lease", "vm", vm, "err", err)
		return false
	}
	if now.Before(until) {
		s.cfg.Log.Debug("auto-stop held by an active prompt turn", "vm", vm, "until", until.Format(time.RFC3339))
		return true
	}
	return false
}

// Sweep stops VMs whose TTL expired and purges expired idempotency keys and
// tombstones. The daemon calls it periodically; tests call it directly.
func (s *Service) Sweep(ctx context.Context) error {
	s.rearmReserve()
	s.rearmSaveReserve()
	if s.lowDisk() && s.ctx.Err() == nil {
		// It waits for deletes: never hold up the TTL and idle stops below.
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.freeDisk(s.ctx, 0, "")
		}()
	}
	now := s.now()
	var due, warn, idle []*state.VMRecord
	// Housekeeping has its own transaction: a failure there must never stop
	// the TTL and idle reclaim below. It is returned once they ran.
	purgeErr := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		if _, err := tx.PurgeIdempotency(now); err != nil {
			return err
		}
		_, err := tx.PurgeTombstones(now.Add(-s.cfg.TombstoneRetention))
		return err
	})
	if purgeErr != nil {
		purgeErr = fmt.Errorf("purge: %w", purgeErr)
	}
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		vms, err := tx.AllVMs()
		for _, r := range vms {
			v := r.VM
			live := (v.State == domain.StateReady || v.State == domain.StateRunning) && v.AutoStop && v.ExpiresAt != nil
			switch {
			case live && !now.Before(*v.ExpiresAt) && v.ActiveOperationID == "":
				due = append(due, r)
			case s.idleDue(v, now):
				idle = append(idle, r)
			case live && s.cfg.TTLWarning > 0 && !now.Before(v.ExpiresAt.Add(-s.cfg.TTLWarning)):
				warn = append(warn, r)
			}
		}
		return err
	})
	if err != nil {
		return err
	}
	for _, r := range warn {
		s.mu.Lock()
		already := s.warned[r.VM.ID].Equal(*r.VM.ExpiresAt)
		s.warned[r.VM.ID] = *r.VM.ExpiresAt
		s.mu.Unlock()
		if already {
			continue
		}
		s.cfg.Log.Info("auto-stop soon", "vm", r.VM.ID, "name", r.VM.Name, "at", r.VM.ExpiresAt.Format(time.RFC3339))
		if s.cfg.OnTTLWarning != nil {
			vm := r.VM
			s.decorate(&vm)
			s.cfg.OnTTLWarning(ctx, vm)
		}
	}
	s.stopIdle(ctx, idle, now)
	if !s.lowDisk() {
		s.coldSweep() // the low-disk cleanup moves VMs itself
	}
	for _, r := range due {
		if s.leased(ctx, r.VM.ID, now) {
			continue
		}
		req := Request{
			Principal:      r.VM.Owner,
			IdempotencyKey: fmt.Sprintf("ttl:%s:%d", r.VM.ID, r.VM.InstanceGeneration),
			Method:         "POST", Path: "internal/ttl-stop",
		}
		if _, err := s.stop(ctx, req, string(r.VM.ID), false, "ttl"); err != nil && !domain.IsCode(err, domain.CodeOperationInProgress) {
			s.cfg.Log.Warn("ttl stop", "vm", r.VM.ID, "err", err)
		} else {
			s.cfg.Log.Info("ttl expired; stopping", "vm", r.VM.ID)
		}
	}
	return purgeErr
}
