// Package guestexec runs and tracks guest processes for the API
// (execProcess, getProcess, killProcess, attachProcess, listProcesses). It
// keeps one authenticated guest-agent session per VM instance, a registry
// of the processes started through this daemon run, and the idempotency
// records for execProcess. Lifecycle authorizes every access (the VM must
// belong to the principal and be ready or running); this package never
// decides VM state.
//
// Process records (owner, argv, start time, generation, idempotency key,
// last outcome) are durable in the Store, so status and attach survive a
// daemon restart; live handles (stdin, stream ownership) are per daemon
// run. Non-detached processes die with the daemon's session to the agent.
// execProcess commits a `starting` reservation for its key before the
// agent sees the request, so a crash mid-start leaves a record that a
// retry settles instead of running the command again. With feature jobs
// the reservation's ID is the process's durable handle (Exec.Handle): the
// agent never runs a handle twice, and settling looks the handle up and
// adopts the process it names (or marks the reservation lost when the agent
// never started it). Without jobs the outcome is unknowable and the
// reservation is marked lost.
// An ID the store does not know but the agent does is adopted with argv
// and start time unknown.
//
// Output of running processes is recorded in the Store when it implements
// OutputStore (see output.go).
package guestexec

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
	"davit/internal/lifecycle"
	"davit/internal/runtime"
	"davit/internal/state"
)

// Targets resolves an authorized guest route (lifecycle.Service).
type Targets interface {
	GuestTarget(ctx context.Context, principal domain.PrincipalID, ref string) (lifecycle.GuestTarget, error)
}

// Config configures a Manager.
type Config struct {
	Targets Targets
	// Secrets are the per-generation agent secrets the barrier provisioned.
	Secrets guestclient.SecretStore
	// Port is the agent port; default guestclient.DefaultPort.
	Port uint32
	// AttachGrace is how long a non-detached process waits for its first
	// stream before it is killed; default 60s.
	AttachGrace time.Duration
	// Store keeps process records. Required.
	Store Store
	// Retain bounds records kept per VM (and live handles in memory);
	// default 1024.
	Retain int
	// RetainOutput bounds the output kept per process when Store
	// implements OutputStore (the newest; the oldest is dropped); default
	// 8 MiB.
	RetainOutput int
	// MaxLivePerVM and MaxLive bound the processes started and not yet
	// exited, per VM and across the host (each holds an output collector
	// and an agent session); default 256 and 2048. A new exec beyond either
	// is refused resource_exhausted (retryable); replays are not.
	MaxLivePerVM int
	MaxLive      int
	// Activity, if set, is called on every exec, process stream attach and
	// detach and tunnel attach and close (lifecycle.Service.Touch: idle
	// stops count from it).
	Activity func(vm domain.VMID)
	Now      func() time.Time
	Log      *slog.Logger
}

// Manager is safe for concurrent use.
type Manager struct {
	cfg Config

	mu         sync.Mutex
	sessions   map[domain.VMID]*vmSession
	tunnels    map[string]*tunnel // opened, not yet attached or closed
	tunnelKeys map[tunnelKey]keyedTunnel
	procs      map[string]*proc    // live and recently used processes
	order      []string            // procs in insertion order, for trimming
	starting   map[idemKey]bool    // execProcess keys being started now
	collecting map[string]bool     // processes whose output is being recorded
	inUse      map[domain.VMID]int // attached process streams and tunnels (Busy)

	out    OutputStore // nil when Store does not retain output
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // collectors

	// crash, when set by tests, aborts Exec at stage ("reserved": the
	// durable reservation is committed; "started": the guest process
	// runs) as a daemon crash would.
	crash func(stage string) bool
}

type vmSession struct {
	key runtime.Key
	t   lifecycle.GuestTarget
	c   *guestclient.Client
}

type idemKey struct {
	principal domain.PrincipalID
	vm        domain.VMID
	key       string
}

type proc struct {
	owner    domain.PrincipalID
	idemKey  string
	bodyHash string

	mu       sync.Mutex
	p        api.Process
	stdin    *guestclient.Stdin
	claimed  bool // stdin handed to a stream
	attached bool // a stream has attached
	// collected: output recording ended for good (bound reached, or the
	// guest evicted output it never delivered); see output.go.
	collected bool
	// handle is the durable job handle (Exec.Handle), "" without jobs.
	handle string
}

// New returns a manager.
func New(cfg Config) *Manager {
	if cfg.Port == 0 {
		cfg.Port = guestclient.DefaultPort
	}
	if cfg.AttachGrace == 0 {
		cfg.AttachGrace = time.Minute
	}
	if cfg.Retain == 0 {
		cfg.Retain = 1024
	}
	if cfg.RetainOutput == 0 {
		cfg.RetainOutput = 8 << 20
	}
	if cfg.MaxLivePerVM == 0 {
		cfg.MaxLivePerVM = 256
	}
	if cfg.MaxLive == 0 {
		cfg.MaxLive = 2048
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m := &Manager{cfg: cfg, sessions: map[domain.VMID]*vmSession{}, tunnels: map[string]*tunnel{}, tunnelKeys: map[tunnelKey]keyedTunnel{},
		procs: map[string]*proc{}, starting: map[idemKey]bool{}, collecting: map[string]bool{}, inUse: map[domain.VMID]int{}}
	m.out, _ = cfg.Store.(OutputStore)
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m
}

// Close ends every guest session (non-detached processes die with them)
// and waits for output collectors.
func (m *Manager) Close() {
	m.cancel()
	defer m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.c.Close()
		delete(m.sessions, id)
	}
	for id, t := range m.tunnels {
		t.stop()
		delete(m.tunnels, id)
	}
}

// session returns a live session for t's instance, replacing a dead one or
// one bound to an older generation.
func (m *Manager) session(ctx context.Context, t lifecycle.GuestTarget) (*guestclient.Client, error) {
	// A transport reset (e.g. the VMM snapshotting the VM for a live fork)
	// is only noticed on use, so a reused session is pinged first, outside
	// the manager lock. Only a transport failure drops it: a slow guest
	// keeps its session (and the processes bound to it).
	m.mu.Lock()
	cached := m.sessions[t.Key.VMID]
	m.mu.Unlock()
	if cached != nil && cached.key == t.Key && cached.c.Err() == nil {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := cached.c.Ping(pctx)
		cancel()
		if err == nil || (errors.Is(err, context.DeadlineExceeded) && cached.c.Err() == nil) {
			m.mu.Lock()
			cached.t = t // the latest resolution (its privacy policy is current)
			m.mu.Unlock()
			return cached.c, nil
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[t.Key.VMID]; s != nil {
		if s != cached && s.key == t.Key && s.c.Err() == nil {
			s.t = t
			return s.c, nil // another caller reconnected meanwhile
		}
		s.c.Close()
		delete(m.sessions, t.Key.VMID)
	}
	c, err := m.connect(ctx, t)
	if err != nil {
		return nil, err
	}
	m.sessions[t.Key.VMID] = &vmSession{key: t.Key, t: t, c: c}
	return c, nil
}

// route returns how to reach t's agent and authenticate a session to it.
func (m *Manager) route(ctx context.Context, t lifecycle.GuestTarget) (guestclient.DialFunc, guestclient.Session, error) {
	secrets, err := m.cfg.Secrets.Load(ctx, t.Key)
	if err != nil {
		return nil, guestclient.Session{}, err
	}
	if len(secrets) == 0 {
		return nil, guestclient.Session{}, domain.Errorf(domain.CodeUnavailable, "vm %s: no guest credential for generation %d", t.Key.VMID, t.Key.Generation)
	}
	port := m.cfg.Port
	return func(ctx context.Context) (net.Conn, error) { return t.Dialer.DialGuest(ctx, port) },
		guestclient.Session{VMID: string(t.Key.VMID), Generation: uint64(t.Key.Generation), Secrets: secrets}, nil
}

// connect opens a new session to t's agent (not the shared one).
func (m *Manager) connect(ctx context.Context, t lifecycle.GuestTarget) (*guestclient.Client, error) {
	dial, sess, err := m.route(ctx, t)
	if err != nil {
		return nil, err
	}
	c, err := guestclient.Connect(ctx, dial, sess)
	if err != nil {
		return nil, unavailable("connect to guest agent", err)
	}
	return c, nil
}

// Exec starts a process. key is the request's Idempotency-Key.
func (m *Manager) Exec(ctx context.Context, principal domain.PrincipalID, ref, key string, req api.ExecRequest) (api.Process, error) {
	if key == "" {
		return api.Process{}, domain.Errorf(domain.CodeInvalidArgument, "Idempotency-Key is required").WithDetail("field", api.HeaderIdempotencyKey)
	}
	if req.TTY {
		return api.Process{}, domain.Errorf(domain.CodeUnsupportedCapability, "tty exec is not supported; use ssh").WithDetail("capability", "tty")
	}
	if req.DeadlineSeconds < 0 {
		return api.Process{}, domain.Errorf(domain.CodeInvalidArgument, "deadlineSeconds must be >= 1").WithDetail("field", "deadlineSeconds")
	}
	g := guestproto.Exec{Argv: req.Argv, Cwd: req.Cwd, User: req.User, DeadlineSeconds: req.DeadlineSeconds,
		Detached: req.Detached, Stdin: req.Stdin}
	for _, k := range slices.Sorted(maps.Keys(req.Env)) {
		g.Env = append(g.Env, k+"="+req.Env[k])
	}
	if err := g.Validate(); err != nil {
		return api.Process{}, protoErr(err)
	}

	t, err := m.cfg.Targets.GuestTarget(ctx, principal, ref)
	if err != nil {
		return api.Process{}, err
	}
	body, _ := json.Marshal(req)
	ik := idemKey{principal, t.Key.VMID, key}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	m.mu.Lock()
	busy := m.starting[ik]
	m.starting[ik] = true // reserved while starting
	m.mu.Unlock()
	if busy {
		e := domain.Errorf(domain.CodeOperationInProgress, "a request with this Idempotency-Key is still starting")
		e.Retryable = true
		return api.Process{}, e
	}
	release := func() {
		m.mu.Lock()
		delete(m.starting, ik)
		m.mu.Unlock()
	}
	prev, err := m.cfg.Store.ProcessByKey(ctx, principal, t.Key.VMID, key)
	if err != nil {
		release()
		return api.Process{}, err
	}
	if prev != nil {
		release()
		if prev.BodyHash != hash {
			return api.Process{}, domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key was used with a different request")
		}
		return m.Get(ctx, principal, ref, prev.ID)
	}
	defer release()

	c, err := m.session(ctx, t)
	if err != nil {
		return api.Process{}, err
	}
	if err := m.admit(ctx, t.Key, c); err != nil {
		return api.Process{}, err
	}
	// Reserve the key durably before the guest sees the request (SPEC §4):
	// the agent assigns process IDs, so the reservation carries a host ID
	// and becomes the started process's record once the agent answers. A
	// reservation an earlier daemon run left behind is settled lost.
	p := &proc{owner: principal, idemKey: key, bodyHash: hash, p: api.Process{
		ID: domain.NewID(pendingPrefix), VMID: t.Key.VMID, Generation: t.Key.Generation, Status: api.ProcessStarting,
		Argv: slices.Clone(req.Argv), Detached: req.Detached, CreatedAt: m.cfg.Now().UTC(),
	}}
	pendingID := p.p.ID
	if c.Has(guestproto.FeatureJobs) {
		p.handle = pendingID // recorded with the reservation
	}
	if err := m.cfg.Store.PutProcess(ctx, p.record()); err != nil {
		return api.Process{}, err
	}
	if m.crashed("reserved") {
		return api.Process{}, errCrashed
	}
	if c.Has(guestproto.FeatureJobs) {
		g.Handle = pendingID
	}
	started, stdin, err := c.StartProcess(ctx, g)
	if err != nil {
		var pe *guestproto.Error
		if errors.As(err, &pe) {
			// The agent refused it: nothing ran, so the key is free again.
			if derr := m.cfg.Store.DeleteProcess(context.WithoutCancel(ctx), pendingID); derr != nil {
				m.cfg.Log.Error("drop process reservation", "process", pendingID, "err", derr)
			}
		} else if g.Handle == "" && p.markLost(m.cfg.Now()) {
			// It may or may not have started; never run it again. (With a
			// handle the reservation stays starting: settle asks the agent.)
			m.persist(ctx, p)
		}
		return api.Process{}, protoErr(err)
	}
	if m.crashed("started") {
		return api.Process{}, errCrashed
	}
	p.stdin = stdin
	p.p.ID, p.p.Status = started.ProcessID, api.ProcessRunning
	m.cache(p)
	if err := m.cfg.Store.ReplaceProcess(context.WithoutCancel(ctx), pendingID, p.record()); err != nil {
		// The process runs, but its reservation will read as lost.
		m.cfg.Log.Error("persist process", "process", p.p.ID, "reservation", pendingID, "err", err)
	}
	if err := m.cfg.Store.PruneProcesses(ctx, t.Key.VMID, m.cfg.Retain); err != nil {
		m.cfg.Log.Warn("prune processes", "vm", t.Key.VMID, "err", err)
	}
	m.collect(p, c)
	m.active(t.Key.VMID)
	m.cfg.Log.Info("guest process started", "vm", t.Key.VMID, "generation", t.Key.Generation, "process", p.p.ID, "detached", req.Detached)

	if !req.Detached {
		time.AfterFunc(m.cfg.AttachGrace, func() {
			p.mu.Lock()
			orphan := !p.attached && p.p.ExitedAt == nil
			p.mu.Unlock()
			if orphan {
				m.cfg.Log.Warn("killing unattached process", "process", p.p.ID)
				_ = c.Signal(context.Background(), p.p.ID, "KILL")
			}
		})
	}
	return p.snapshot(), nil
}

// Busy reports whether vm has an attached process stream or tunnel now, or
// a process of generation still running (attached or detached) or starting.
func (m *Manager) Busy(vm domain.VMID, generation int64) bool {
	m.mu.Lock()
	if m.inUse[vm] > 0 {
		m.mu.Unlock()
		return true
	}
	for ik := range m.starting {
		if ik.vm == vm {
			m.mu.Unlock()
			return true
		}
	}
	procs := make([]*proc, 0, len(m.procs))
	for _, p := range m.procs {
		procs = append(procs, p)
	}
	m.mu.Unlock()
	for _, p := range procs {
		p.mu.Lock()
		running := p.p.VMID == vm && p.p.Generation == generation && p.p.ExitedAt == nil
		p.mu.Unlock()
		if running {
			return true
		}
	}
	return false
}

// active records activity on vm.
func (m *Manager) active(vm domain.VMID) {
	if m.cfg.Activity != nil {
		m.cfg.Activity(vm)
	}
}

// busy holds vm busy until the returned func is called (once).
func (m *Manager) busy(vm domain.VMID) (done func()) {
	m.mu.Lock()
	m.inUse[vm]++
	m.mu.Unlock()
	m.active(vm)
	return func() {
		m.mu.Lock()
		if m.inUse[vm]--; m.inUse[vm] <= 0 {
			delete(m.inUse, vm)
		}
		m.mu.Unlock()
		m.active(vm) // idle counts from the end of the session
	}
}

// admit refuses a new process beyond MaxLivePerVM or MaxLive. The caller's
// own start is already counted in starting. An exit nobody observed (no
// collector under noOutputRetention, no stream attached) still counts as
// live, so before refusing it asks the agent about the VM's live
// processes of this generation.
func (m *Manager) admit(ctx context.Context, key runtime.Key, c *guestclient.Client) error {
	vmLive, all, mine := m.live(key)
	if vmLive <= m.cfg.MaxLivePerVM && all <= m.cfg.MaxLive {
		return nil
	}
	for _, p := range mine {
		_ = m.refresh(ctx, p, c)
	}
	vmLive, all, _ = m.live(key)
	var e *domain.Error
	switch {
	case vmLive > m.cfg.MaxLivePerVM:
		e = domain.Errorf(domain.CodeResourceExhausted, "vm %s already runs %d processes (limit %d)", key.VMID, vmLive-1, m.cfg.MaxLivePerVM)
	case all > m.cfg.MaxLive:
		e = domain.Errorf(domain.CodeResourceExhausted, "the host already runs %d guest processes (limit %d)", all-1, m.cfg.MaxLive)
	default:
		return nil
	}
	e.Retryable = true
	return e
}

// live counts processes started and not seen to exit, plus starts in
// flight: on key's VM and on the host, and returns the VM's live processes
// of key's generation.
func (m *Manager) live(key runtime.Key) (vm, all int, mine []*proc) {
	m.mu.Lock()
	procs := make([]*proc, 0, len(m.procs))
	for _, p := range m.procs {
		procs = append(procs, p)
	}
	for ik := range m.starting {
		all++
		if ik.vm == key.VMID {
			vm++
		}
	}
	m.mu.Unlock()
	for _, p := range procs {
		p.mu.Lock()
		running, pvm, gen := p.p.ExitedAt == nil, p.p.VMID, p.p.Generation
		p.mu.Unlock()
		if !running {
			continue
		}
		all++
		if pvm == key.VMID {
			vm++
			if gen == key.Generation {
				mine = append(mine, p)
			}
		}
	}
	return vm, all, mine
}

// pendingPrefix marks host-assigned IDs of execProcess reservations; the
// agent never knows them.
const pendingPrefix = "pstart"

var errCrashed = errors.New("guestexec: simulated crash")

func (m *Manager) crashed(stage string) bool { return m.crash != nil && m.crash(stage) }

func isPending(id string) bool { return strings.HasPrefix(id, pendingPrefix+"_") }

// settle reconciles a reservation that is not being started by this
// daemon run: an earlier run crashed between reserving the key and
// recording the agent's answer. With feature jobs the reservation ID is the
// handle the Exec carried, so the agent says whether the process started:
// it is adopted (the reservation record becomes the process's) or, when no
// process carries the handle, marked lost; nothing ever runs twice. Without
// jobs, or for an older generation, the outcome is unknowable and it is
// marked lost. A guest that cannot be asked leaves it starting. It returns
// the settled process and, when adopted, the session to it.
func (m *Manager) settle(ctx context.Context, p *proc, t lifecycle.GuestTarget) (*proc, *guestclient.Client, error) {
	m.mu.Lock()
	inFlight := m.starting[idemKey{p.owner, p.p.VMID, p.idemKey}]
	m.mu.Unlock()
	if inFlight || p.exited() {
		return p, nil, nil
	}
	if p.p.Generation == t.Key.Generation {
		c, err := m.session(ctx, t)
		if err != nil {
			return p, nil, err
		}
		if c.Has(guestproto.FeatureJobs) {
			st, err := c.Lookup(ctx, p.p.ID)
			switch {
			case err == nil:
				return m.adopt(ctx, p, st, c), c, nil
			case !errors.Is(err, guestproto.Errorf(guestproto.CodeNotFound, "")):
				return p, nil, protoErr(err)
			}
		}
	}
	if p.markLost(m.cfg.Now()) {
		m.cfg.Log.Warn("process start outcome unknown after daemon restart; marking lost", "vm", p.p.VMID, "reservation", p.p.ID)
		m.persist(ctx, p)
	}
	return p, nil, nil
}

// adopt turns reservation p into the process the agent started for it.
func (m *Manager) adopt(ctx context.Context, p *proc, st guestproto.ProcessStatus, c *guestclient.Client) *proc {
	q := &proc{owner: p.owner, idemKey: p.idemKey, bodyHash: p.bodyHash, attached: true, p: p.snapshot()}
	q.p.ID, q.p.Status = st.ProcessID, api.ProcessRunning
	q.observe(m.cfg.Now(), st.Status, st.ExitCode, st.Signal, st.TimedOut, st.OutputSeq)
	if err := m.cfg.Store.ReplaceProcess(context.WithoutCancel(ctx), p.p.ID, q.record()); err != nil {
		m.cfg.Log.Error("adopt process", "process", q.p.ID, "reservation", p.p.ID, "err", err)
		return p
	}
	m.cfg.Log.Info("reattached guest process after daemon restart", "vm", q.p.VMID, "process", q.p.ID, "reservation", p.p.ID)
	q = m.cache(q)
	m.collect(q, c)
	return q
}

// cache remembers p (or returns the one already cached under its ID) and
// forgets the oldest exited processes beyond Retain; records stay durable.
func (m *Manager) cache(p *proc) *proc {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur := m.procs[p.p.ID]; cur != nil {
		return cur
	}
	m.procs[p.p.ID] = p
	m.order = append(m.order, p.p.ID)
	for i := 0; len(m.order) > m.cfg.Retain && i < len(m.order); {
		if q := m.procs[m.order[i]]; q == nil || q.exited() {
			delete(m.procs, m.order[i])
			m.order = slices.Delete(m.order, i, i+1)
			continue
		}
		i++
	}
	return p
}

// persist saves p's record; failures are logged (the agent remains the
// source of truth for a live process).
func (m *Manager) persist(ctx context.Context, p *proc) {
	if err := m.cfg.Store.PutProcess(context.WithoutCancel(ctx), p.record()); err != nil {
		m.cfg.Log.Warn("persist process", "process", p.p.ID, "err", err)
	}
}

func (p *proc) record() *state.ProcessRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return &state.ProcessRecord{
		ID: p.p.ID, VMID: p.p.VMID, Owner: p.owner, Generation: p.p.Generation, Argv: slices.Clone(p.p.Argv),
		Detached: p.p.Detached, CreatedAt: p.p.CreatedAt, IdempotencyKey: p.idemKey, BodyHash: p.bodyHash,
		Status: string(p.p.Status), ExitCode: p.p.ExitCode, Signal: p.p.Signal, TimedOut: p.p.TimedOut,
		OutputCursor: p.p.OutputCursor, ExitedAt: p.p.ExitedAt, Handle: p.handle,
	}
}

// fromRecord rebuilds a process without live handles (a previous daemon
// run started it, so no stream may claim stdin or own its lifetime).
func fromRecord(r *state.ProcessRecord) *proc {
	argv := r.Argv
	if argv == nil {
		argv = []string{}
	}
	return &proc{owner: r.Owner, idemKey: r.IdempotencyKey, bodyHash: r.BodyHash, handle: r.Handle, attached: true, p: api.Process{
		ID: r.ID, VMID: r.VMID, Generation: r.Generation, Status: api.ProcessStatus(r.Status), Argv: argv,
		Detached: r.Detached, ExitCode: r.ExitCode, Signal: r.Signal, TimedOut: r.TimedOut,
		OutputCursor: r.OutputCursor, CreatedAt: r.CreatedAt, ExitedAt: r.ExitedAt,
	}}
}

func (p *proc) snapshot() api.Process {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.p
	out.Argv = slices.Clone(p.p.Argv)
	return out
}

func (p *proc) exited() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.p.ExitedAt != nil
}

// observe records an agent status or exit report.
func (p *proc) observe(now time.Time, status guestproto.ProcessState, code *int, signal string, timedOut bool, seq uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if int64(seq) > p.p.OutputCursor {
		p.p.OutputCursor = int64(seq)
	}
	if p.p.ExitedAt != nil {
		return
	}
	p.p.Status = api.ProcessStatus(status)
	if status != guestproto.StatusRunning {
		t := now.UTC()
		p.p.ExitedAt, p.p.ExitCode, p.p.Signal, p.p.TimedOut = &t, code, signal, timedOut
	}
}

// markLost records an unknowable outcome; it reports whether p changed.
func (p *proc) markLost(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.p.ExitedAt != nil {
		return false
	}
	t := now.UTC()
	p.p.Status, p.p.ExitedAt = api.ProcessLost, &t
	return true
}

// resolve finds process id on ref's current instance and a session to it.
// A process of an older generation is reported lost.
func (m *Manager) resolve(ctx context.Context, principal domain.PrincipalID, ref, id string) (*proc, *guestclient.Client, error) {
	t, err := m.cfg.Targets.GuestTarget(ctx, principal, ref)
	if err != nil {
		return nil, nil, err
	}
	m.mu.Lock()
	p := m.procs[id]
	m.mu.Unlock()
	if p == nil {
		rec, err := m.cfg.Store.GetProcess(ctx, id)
		if err != nil && !domain.IsCode(err, domain.CodeNotFound) {
			return nil, nil, err
		}
		if rec != nil {
			if p = fromRecord(rec); !isPending(id) { // reservations are never cached
				p = m.cache(p)
			}
		}
	}
	if p != nil && (p.owner != principal || p.p.VMID != t.Key.VMID) {
		return nil, nil, domain.Errorf(domain.CodeNotFound, "process %s not found", id)
	}
	if p != nil && isPending(id) {
		return m.settle(ctx, p, t)
	}
	if p != nil && p.p.Generation != t.Key.Generation {
		if p.markLost(m.cfg.Now()) {
			m.persist(ctx, p)
		}
		return p, nil, nil
	}
	c, err := m.session(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		// Not started in this daemon run: adopt it if the agent knows it.
		st, err := c.Status(ctx, id)
		if err != nil {
			if errors.Is(err, guestproto.Errorf(guestproto.CodeNotFound, "")) || errors.Is(err, guestproto.ErrInvalid) {
				return nil, nil, domain.Errorf(domain.CodeNotFound, "process %s not found", id)
			}
			return nil, nil, protoErr(err)
		}
		p = &proc{owner: principal, p: api.Process{ID: id, VMID: t.Key.VMID, Generation: t.Key.Generation,
			Status: api.ProcessUnknown, Argv: []string{}, Detached: true, CreatedAt: m.cfg.Now().UTC()}, attached: true}
		p.observe(m.cfg.Now(), st.Status, st.ExitCode, st.Signal, st.TimedOut, st.OutputSeq)
		p = m.cache(p)
		m.persist(ctx, p)
	}
	return p, c, nil
}

// refresh updates a running process from the agent.
func (m *Manager) refresh(ctx context.Context, p *proc, c *guestclient.Client) error {
	if c == nil || p.exited() {
		return nil
	}
	st, err := c.Status(ctx, p.p.ID)
	if err != nil {
		if errors.Is(err, guestproto.Errorf(guestproto.CodeNotFound, "")) {
			if p.markLost(m.cfg.Now()) {
				m.persist(ctx, p)
			}
			return nil
		}
		return protoErr(err)
	}
	p.observe(m.cfg.Now(), st.Status, st.ExitCode, st.Signal, st.TimedOut, st.OutputSeq)
	m.persist(ctx, p)
	m.collect(p, c)
	return nil
}

// Get returns a process with fresh status.
func (m *Manager) Get(ctx context.Context, principal domain.PrincipalID, ref, id string) (api.Process, error) {
	p, c, err := m.resolve(ctx, principal, ref, id)
	if err != nil {
		return api.Process{}, err
	}
	if err := m.refresh(ctx, p, c); err != nil {
		return api.Process{}, err
	}
	return p.snapshot(), nil
}

// List returns one page of ref's processes in (createdAt, id) order with
// fresh status, and the cursor of the next page ("" after the last).
func (m *Manager) List(ctx context.Context, principal domain.PrincipalID, ref string, limit int, cursor string) ([]api.Process, string, error) {
	t, err := m.cfg.Targets.GuestTarget(ctx, principal, ref)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > m.cfg.Retain {
		limit = m.cfg.Retain
	}
	recs, next, err := m.cfg.Store.ListProcesses(ctx, principal, t.Key.VMID,
		state.Page{Limit: limit, Cursor: cursor, Filter: string(principal) + "|" + string(t.Key.VMID)})
	if err != nil {
		return nil, "", err
	}
	out := make([]api.Process, 0, len(recs))
	var c *guestclient.Client
	for _, r := range recs {
		m.mu.Lock()
		p := m.procs[r.ID]
		m.mu.Unlock()
		if p == nil {
			p = fromRecord(r)
		}
		switch {
		case isPending(p.p.ID):
			if q, _, err := m.settle(ctx, p, t); err == nil {
				p = q
			}
		case p.p.Generation != t.Key.Generation:
			if p.markLost(m.cfg.Now()) {
				m.persist(ctx, p)
			}
		case !p.exited():
			if c == nil {
				if c, err = m.session(ctx, t); err != nil {
					return nil, "", err
				}
			}
			if err := m.refresh(ctx, p, c); err != nil {
				return nil, "", err
			}
		}
		out = append(out, p.snapshot())
	}
	return out, next, nil
}

// Signal delivers signal ("TERM" or "SIGTERM") to the process group.
func (m *Manager) Signal(ctx context.Context, principal domain.PrincipalID, ref, id, signal string) (api.Process, error) {
	sig := strings.TrimPrefix(strings.ToUpper(signal), "SIG")
	if err := (guestproto.ProcessSignal{ProcessID: id, Signal: sig}).Validate(); err != nil {
		return api.Process{}, domain.Errorf(domain.CodeInvalidArgument, "unsupported signal %q", signal).WithDetail("field", "signal")
	}
	p, c, err := m.resolve(ctx, principal, ref, id)
	if err != nil {
		return api.Process{}, err
	}
	if c == nil || p.exited() {
		return api.Process{}, domain.Errorf(domain.CodeInvalidState, "process %s is %s", id, p.snapshot().Status)
	}
	if err := c.Signal(ctx, id, sig); err != nil {
		return api.Process{}, protoErr(err)
	}
	if err := m.refresh(ctx, p, c); err != nil {
		return api.Process{}, err
	}
	return p.snapshot(), nil
}

// Stream is one attached client: Send delivers server messages in order.
type Stream struct {
	Send func(api.ProcessStreamMessage) error
	// Stdin is the process's stdin when this stream claimed it, else nil.
	Stdin io.WriteCloser
	// Signal signals the process group from the stream.
	Signal func(ctx context.Context, signal string) error
}

// Attach follows process id from cursor after, calling run with the stream
// handle (for stdin/signal) concurrently with output delivery. It returns
// after the exit (or error) message was sent, or when ctx ends; a
// non-detached process whose first stream ends early is killed.
func (m *Manager) Attach(ctx context.Context, principal domain.PrincipalID, ref, id string, after int64,
	send func(api.ProcessStreamMessage) error, run func(Stream)) error {
	if after < 0 {
		return domain.Errorf(domain.CodeInvalidArgument, "outputCursor must be >= 0").WithDetail("field", "outputCursor")
	}
	p, c, err := m.resolve(ctx, principal, ref, id)
	if err != nil {
		return err
	}
	if c == nil {
		snap := p.snapshot()
		return send(api.ProcessStreamMessage{Type: api.StreamExit, Seq: snap.OutputCursor, Process: &snap})
	}
	defer m.busy(p.snapshot().VMID)() // a following client keeps the VM busy
	p.mu.Lock()
	first := !p.attached
	p.attached = true
	var stdin io.WriteCloser
	if p.stdin != nil && !p.claimed {
		p.claimed, stdin = true, p.stdin
	}
	p.mu.Unlock()

	if run != nil {
		go run(Stream{Send: send, Stdin: stdin, Signal: func(ctx context.Context, sig string) error {
			_, err := m.Signal(ctx, principal, ref, id, sig)
			return err
		}})
	}

	ex, err := c.AttachFunc(ctx, id, uint64(after), func(seq uint64, isErr bool, data []byte) error {
		typ := api.StreamStdout
		if isErr {
			typ = api.StreamStderr
		}
		p.observe(m.cfg.Now(), guestproto.StatusRunning, nil, "", false, seq)
		return send(api.ProcessStreamMessage{Type: typ, Seq: int64(seq), Data: base64.StdEncoding.EncodeToString(data)})
	})
	if err != nil {
		if ctx.Err() != nil {
			if first && !p.snapshot().Detached {
				m.cfg.Log.Info("stream closed before exit; killing non-detached process", "process", id)
				_ = c.Signal(context.WithoutCancel(ctx), id, "KILL")
			}
			return nil
		}
		de := domain.AsError(protoErr(err))
		return send(api.ProcessStreamMessage{Type: api.StreamError, Error: &api.Error{Code: de.Code, Message: de.Message, Retryable: de.Retryable, Details: de.Details}})
	}
	_ = m.refresh(ctx, p, c) // picks up OutputSeq
	p.observe(m.cfg.Now(), ex.Status, ex.ExitCode, ex.Signal, ex.TimedOut, 0)
	m.persist(ctx, p)
	snap := p.snapshot()
	return send(api.ProcessStreamMessage{Type: api.StreamExit, Seq: snap.OutputCursor, Process: &snap})
}

// protoErr maps agent errors to domain errors. Agent messages are written
// for the host and carry no host secrets.
func protoErr(err error) error {
	var pe *guestproto.Error
	if !errors.As(err, &pe) {
		var de *domain.Error
		if errors.As(err, &de) {
			return de
		}
		return unavailable("guest agent", err)
	}
	if pe.Code == guestproto.CodeInternal && domain.NoSpaceMessage(pe.Message) {
		return domain.BoxDiskFull("guest: " + pe.Message)
	}
	code := domain.CodeInternal
	switch pe.Code {
	case guestproto.CodeInvalidArgument:
		code = domain.CodeInvalidArgument
	case guestproto.CodeNotFound:
		code = domain.CodeNotFound
	case guestproto.CodeUnsupported:
		code = domain.CodeUnsupportedCapability
	case guestproto.CodePermissionDenied:
		code = domain.CodePermissionDenied
	case guestproto.CodeUnexpectedType, guestproto.CodeUnauthenticated:
		code = domain.CodeUnavailable
	}
	return domain.Errorf(code, "guest: %s", pe.Message)
}

func unavailable(what string, err error) error {
	return domain.Errorf(domain.CodeUnavailable, "%s: %v", what, err)
}
