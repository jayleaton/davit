// Package ci runs GitHub Actions jobs in ephemeral davit boxes
// (docs/ci-runners.md): for each queued workflow_job whose runs-on labels
// ask for this host, it registers a single-use (just-in-time) runner, forks
// a fresh box from the runner image, runs the runner in it as an
// unprivileged user and deletes the box when the runner exits, the job ends
// elsewhere or it times out. Owner: CI.
//
// Like internal/runs it is not a second VM state machine: every effect is
// a lifecycle operation (create, delete) or a guest process call, keyed by
// the job's ID. A job record journals its phase before and after each
// step, so a restarted daemon resumes it; boxes and runner registrations
// no record accounts for are removed (reconcile.go).
//
// Jobs arrive by webhook (webhook.go) or polling (poll.go); both feed the
// same intake (intake.go), which decides from labels and policy whether a
// job is this host's, and records it once per GitHub job. Jobs wait in a
// FIFO queue for one of MaxConcurrent slots, then in lifecycle's own
// capacity queue for memory and disk.
package ci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/ci/github"
	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/guestexec"
	"davit/internal/httpx"
	"davit/internal/lifecycle"
	"davit/internal/state"
)

// VMs is the lifecycle API the controller uses (lifecycle.Service).
type VMs interface {
	Create(ctx context.Context, req lifecycle.Request, body api.CreateVMRequest) (*lifecycle.Accepted, error)
	Delete(ctx context.Context, req lifecycle.Request, ref string) (*lifecycle.Accepted, error)
	Events(ctx context.Context, principal domain.PrincipalID, id domain.OperationID, after int64, wait time.Duration) (*domain.Operation, []domain.OperationEvent, error)
	GetVM(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.VM, error)
	ListVMs(ctx context.Context, principal domain.PrincipalID, states []domain.VMState, all bool, p lifecycle.Page) ([]domain.VM, string, error)
}

// Processes is the guest process API the controller uses
// (guestexec.Manager).
type Processes interface {
	Exec(ctx context.Context, principal domain.PrincipalID, ref, key string, req api.ExecRequest) (api.Process, error)
	Get(ctx context.Context, principal domain.PrincipalID, ref, id string) (api.Process, error)
	List(ctx context.Context, principal domain.PrincipalID, ref string, limit int, cursor string) ([]api.Process, string, error)
	Signal(ctx context.Context, principal domain.PrincipalID, ref, id, signal string) (api.Process, error)
	Attach(ctx context.Context, principal domain.PrincipalID, ref, id string, after int64,
		send func(api.ProcessStreamMessage) error, run func(guestexec.Stream)) error
}

// Config configures a Service.
type Config struct {
	GitHub config.GitHubCI
	// App is the GitHub API client (the App's key loaded by the caller).
	App   *github.App
	DB    *state.DB
	VMs   VMs
	Procs Processes
	// Owner owns the runner boxes (the host owner).
	Owner domain.PrincipalID
	// Hostname makes the default runner name prefix.
	Hostname string
	// Retry is the first backoff after a transient step failure (default
	// 1s); it doubles up to 30s. GiveUp is how long one step may keep
	// failing before the job fails (default 5m; waiting for capacity never
	// gives up).
	Retry, GiveUp time.Duration
	// CompletedGrace is how long a runner whose job GitHub reports
	// completed may take to exit before its box is deleted anyway
	// (default 2m).
	CompletedGrace time.Duration
	// ReconcileEvery is how often idle runner registrations and orphan
	// boxes are looked for after the start (default 10m).
	ReconcileEvery time.Duration
	// Retention keeps finished job records (default 30 days).
	Retention time.Duration
	// Clock is the controller's time (default the real one): record
	// timestamps and the runner deadlines (job timeout backstop, completed
	// grace). Retry and poll pacing use real timers.
	Clock Clock
	Log   *slog.Logger
}

// Clock tells the time and runs a function once a duration has passed.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f in its own goroutine after d; stop cancels that
	// unless it already started.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }

// settings is the resolved configuration.
type settings struct {
	label       string
	archLabel   string
	poll        time.Duration
	image       string
	maxJobs     int
	timeout     time.Duration
	prefix      string
	command     []string
	user        string
	scopeOrg    bool
	group       int64
	repos       map[string]repoPolicy // by lower-case owner/name
	webhook     config.GitHubWebhook
	installs    []int64
	defaultName string
}

// repoPolicy is one allowed repository.
type repoPolicy struct {
	config.GitHubRepo
	installation int64
}

func resolve(g config.GitHubCI, hostname string) settings {
	s := settings{label: g.Label, image: g.Image, maxJobs: g.MaxConcurrentJobs, timeout: time.Duration(g.JobTimeout),
		prefix: g.RunnerNamePrefix, command: g.RunnerCommand, user: g.RunnerUser, scopeOrg: g.RunnerScope == "org",
		group: g.RunnerGroupID, repos: map[string]repoPolicy{}, webhook: g.Webhook, poll: time.Duration(g.PollInterval)}
	if s.label == "" {
		s.label = config.DefaultCILabel
	}
	s.archLabel = "x64"
	if goruntime.GOARCH == "arm64" {
		s.archLabel = "arm64"
	}
	if s.image == "" {
		s.image = config.DefaultCIImage
	}
	if s.maxJobs == 0 {
		s.maxJobs = config.DefaultCIConcurrency
	}
	if s.timeout == 0 {
		s.timeout = config.DefaultCIJobTimeout
	}
	if s.poll == 0 {
		s.poll = config.DefaultCIPoll
	}
	if s.prefix == "" {
		s.prefix = "davit-" + sanitize(hostname)
	}
	if len(s.command) == 0 {
		s.command = config.DefaultCIRunnerCommand
	}
	if s.user == "" {
		s.user = config.DefaultCIRunnerUser
	}
	if s.scopeOrg && s.group == 0 {
		s.group = 1
	}
	for _, in := range g.Installations {
		s.installs = append(s.installs, in.ID)
		for _, r := range in.Repos {
			s.repos[strings.ToLower(r.Name)] = repoPolicy{r, in.ID}
		}
	}
	sh, _ := g.ShapeFor("")
	s.defaultName = sh.Label
	return s
}

// sanitize keeps the runner-name characters of a host name.
func sanitize(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "host"
	}
	return b.String()
}

// Service is the controller. It is safe for concurrent use.
type Service struct {
	cfg    Config
	set    settings
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	srv    *http.Server

	mu      sync.Mutex
	drivers map[string]*driver
	queue   []*driver // drivers waiting for a slot, by QueuedAt
	active  map[string]bool
	slotted chan struct{} // closed and replaced when a slot frees

	cache   cache
	hooks   chan hook
	webAddr string
}

// New returns a service. Call Start before use.
func New(cfg Config) (*Service, error) {
	if cfg.DB == nil || cfg.VMs == nil || cfg.Procs == nil || cfg.App == nil || cfg.Owner == "" {
		return nil, errors.New("ci: DB, VMs, Procs, App and Owner are required")
	}
	if cfg.Retry == 0 {
		cfg.Retry = time.Second
	}
	if cfg.GiveUp == 0 {
		cfg.GiveUp = 5 * time.Minute
	}
	if cfg.CompletedGrace == 0 {
		cfg.CompletedGrace = 2 * time.Minute
	}
	if cfg.ReconcileEvery == 0 {
		cfg.ReconcileEvery = 10 * time.Minute
	}
	if cfg.Retention == 0 {
		cfg.Retention = 30 * 24 * time.Hour
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Service{cfg: cfg, set: resolve(cfg.GitHub, cfg.Hostname), drivers: map[string]*driver{}, active: map[string]bool{},
		slotted: make(chan struct{}), hooks: make(chan hook, 1024)}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

func (s *Service) now() time.Time { return s.cfg.Clock.Now().UTC() }

// Start resumes unfinished jobs, starts the webhook receiver (if
// configured), the poller and the reconciler.
func (s *Service) Start(ctx context.Context) error {
	var ln net.Listener
	var secret []byte
	if w := s.set.webhook; w.Listen != "" {
		var err error
		if secret, err = readSecret(w.SecretFile); err != nil {
			return fmt.Errorf("ci: webhook secret: %w", err)
		}
		if ln, err = net.Listen("tcp", w.Listen); err != nil {
			return fmt.Errorf("ci: webhook listener: %w", err)
		}
	}
	var unfinished []record
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		unfinished, err = loadRecords(tx, state.CIJobUnfinished, false, 0)
		return err
	}); err != nil {
		if ln != nil {
			ln.Close()
		}
		return fmt.Errorf("ci: %w", err)
	}
	// Jobs holding a box keep their slot; queued ones queue again in
	// their old order.
	for _, r := range unfinished {
		if r.Phase != phaseQueued {
			s.active[r.ID] = true
		}
	}
	for _, r := range unfinished {
		s.cfg.Log.Info("resuming CI job after daemon restart", "job", r.ID, "repo", r.Repo, "phase", r.Phase)
		s.spawn(&r)
	}
	if ln != nil {
		s.webAddr = ln.Addr().String()
		s.srv = httpx.Harden(&http.Server{Handler: s.webhookHandler(secret)})
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.cfg.Log.Error("ci webhook listener", "err", err)
			}
		}()
		s.cfg.Log.Info("ci: receiving GitHub webhooks", "addr", s.webAddr)
	}
	s.wg.Add(3)
	go func() { defer s.wg.Done(); s.handleHooks() }()
	go func() { defer s.wg.Done(); s.pollLoop() }()
	go func() { defer s.wg.Done(); s.reconcileLoop() }()
	s.cfg.Log.Info("ci: GitHub runners enabled", "repos", len(s.set.repos), "label", s.set.label, "image", s.set.image,
		"maxConcurrentJobs", s.set.maxJobs, "poll", s.set.poll.String(), "runnerPrefix", s.set.prefix)
	return nil
}

// WebhookAddr is the webhook receiver's address ("" without one).
func (s *Service) WebhookAddr() string { return s.webAddr }

// Close stops at a safe point; unfinished jobs stay journaled and resume
// on the next Start.
func (s *Service) Close() {
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.srv.Shutdown(ctx)
		cancel()
	}
	s.cancel()
	s.wg.Wait()
}

// readSecret reads an owner-only secret file, trimmed.
func readSecret(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %04o); chmod 600 it", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return b, nil
}

// Job phases, journaled in the record. Each names the next step.
const (
	phaseQueued   = "queued"   // wait for a slot
	phaseCreate   = "create"   // create the box (waits for host capacity)
	phaseBoot     = "boot"     // wait for the create operation
	phaseRegister = "register" // register a just-in-time runner
	phaseExec     = "exec"     // start the runner
	phaseFollow   = "follow"   // wait for the runner to exit
	phaseCleanup  = "cleanup"  // delete the box and the registration
	phaseDone     = "done"
)

// record is a job's stored document: the api.CIJob fields plus what the
// controller needs to resume it.
type record struct {
	api.CIJob
	Phase        string             `json:"phase"`
	Installation int64              `json:"installation"`
	Spec         domain.Shape       `json:"spec"`
	CreateOp     domain.OperationID `json:"createOperationId,omitempty"`
	// Attempt numbers runner registrations: a restart between registering
	// and starting the runner registers a new one.
	Attempt int `json:"attempt,omitempty"`
	// DemandGone: the job that asked for this runner ended or went to
	// another runner. A runner that has no job yet is then not needed.
	DemandGone bool `json:"demandGone,omitempty"`
	// Busy: the runner was given a job.
	Busy bool `json:"busy,omitempty"`
	// JobDoneAt: GitHub reports the runner's job completed.
	JobDoneAt *time.Time `json:"jobDoneAt,omitempty"`
	// Kill ends the job now with this result (Error says why).
	Kill api.CIJobResult `json:"kill,omitempty"`
	// Key is the record's unique provider key, fixed when it is created:
	// JobID follows the job the runner serves (see handoff).
	Key string `json:"key,omitempty"`
}

func (r *record) providerJob() string {
	if r.Key != "" {
		return r.Key
	}
	return providerJob(r.JobID)
}

// stateOf is the public state of a phase.
func stateOf(r *record) api.CIJobState {
	switch r.Phase {
	case phaseQueued:
		return api.CIJobQueued
	case phaseCreate, phaseBoot, phaseRegister, phaseExec:
		return api.CIJobStarting
	case phaseFollow:
		return api.CIJobRunning
	case phaseCleanup:
		return api.CIJobStopping
	}
	if r.State == api.CIJobRefused {
		return api.CIJobRefused
	}
	return api.CIJobDone
}

func loadRecords(tx *state.Tx, states []string, newest bool, limit int) ([]record, error) {
	docs, err := tx.CIJobDocs(states, newest, limit)
	if err != nil {
		return nil, err
	}
	out := make([]record, 0, len(docs))
	for _, d := range docs {
		var r record
		if err := json.Unmarshal(d, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// put stores r, deriving its state from its phase, and appends a
// ci.job.state event when the state changed (ci.job.created for a new
// record).
func (s *Service) put(tx *state.Tx, r *record, prev api.CIJobState, fresh bool) error {
	r.State = stateOf(r)
	if err := tx.PutCIJob(r.ID, r.providerJob(), string(r.State), r.QueuedAt.UnixNano(), r); err != nil {
		return err
	}
	switch {
	case fresh && r.State == api.CIJobRefused:
		return tx.AppendHostEvent(s.cfg.Owner, api.EventCIJobRefused, string(r.VMID), r.ID, eventData(r, ""))
	case fresh:
		return tx.AppendHostEvent(s.cfg.Owner, api.EventCIJobCreated, string(r.VMID), r.ID, eventData(r, ""))
	case prev != r.State:
		return tx.AppendHostEvent(s.cfg.Owner, api.EventCIJobState, string(r.VMID), r.ID, eventData(r, prev))
	}
	return nil
}

// eventData is the allowlisted event summary: never a runner secret.
func eventData(r *record, prev api.CIJobState) map[string]any {
	d := map[string]any{"state": r.State, "repo": r.Repo, "jobId": r.JobID, "job": r.Job}
	if prev != "" {
		d["previousState"] = prev
	}
	if r.Runner != "" {
		d["runner"] = r.Runner
	}
	if r.Result != "" {
		d["result"] = r.Result
	}
	if r.Conclusion != "" {
		d["conclusion"] = r.Conclusion
	}
	if r.Error != nil {
		d["error"] = r.Error.Code
	}
	return d
}

// update applies fn to job id's stored record in one transaction; fn
// reports whether it changed anything.
func (s *Service) update(ctx context.Context, id string, fn func(*record) (bool, error)) (*record, error) {
	var rec record
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec = record{}
		if err := tx.GetCIJob(id, &rec); err != nil {
			return err
		}
		prev := stateOf(&rec)
		changed, err := fn(&rec)
		if err != nil || !changed {
			return err
		}
		return s.put(tx, &rec, prev, false)
	})
	return &rec, err
}

// Status reports queued, running and recent jobs (at most recent of them).
func (s *Service) Status(ctx context.Context, recent int) (api.CIStatus, error) {
	if recent <= 0 || recent > 200 {
		recent = 20
	}
	out := api.CIStatus{Enabled: true, MaxConcurrentJobs: s.set.maxJobs, Queued: []api.CIJob{}, Running: []api.CIJob{}, Recent: []api.CIJob{}}
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		live, err := loadRecords(tx, state.CIJobUnfinished, false, 0)
		if err != nil {
			return err
		}
		for _, r := range live {
			if r.State == api.CIJobQueued {
				out.Queued = append(out.Queued, r.CIJob)
			} else {
				out.Running = append(out.Running, r.CIJob)
			}
		}
		done, err := loadRecords(tx, []string{string(api.CIJobDone), string(api.CIJobRefused)}, true, recent)
		for _, r := range done {
			out.Recent = append(out.Recent, r.CIJob)
		}
		return err
	})
	return out, err
}

// spawn starts job id's driver unless it runs.
func (s *Service) spawn(r *record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drivers[r.ID] != nil || s.ctx.Err() != nil {
		return
	}
	d := &driver{id: r.ID, wake: make(chan struct{}, 1), queuedAt: r.QueuedAt}
	s.drivers[r.ID] = d
	if r.Phase == phaseQueued && !s.active[r.ID] {
		// Its place in the slot queue is taken now, in intake order, not
		// when its goroutine gets to acquire.
		s.enqueue(d)
	}
	s.wg.Add(1)
	go s.drive(d)
}

// poke tells job id's driver that its record changed.
func (s *Service) poke(id string) {
	s.mu.Lock()
	d := s.drivers[id]
	s.mu.Unlock()
	if d == nil {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
	d.mu.Lock()
	if d.stop != nil {
		d.stop()
	}
	d.mu.Unlock()
}

// acquire waits until d is first in the queue and a slot is free, then
// takes the slot. The queue is in the order the jobs were queued (rec's
// QueuedAt), not the order their drivers got here. It returns early
// (errInterrupted) when d's record changes, leaving d's place in the queue.
func (s *Service) acquire(d *driver, rec *record) error {
	s.mu.Lock()
	if s.active[d.id] {
		s.mu.Unlock()
		return nil
	}
	if !slices.Contains(s.queue, d) {
		d.queuedAt = rec.QueuedAt
		s.enqueue(d)
	}
	s.mu.Unlock()
	return d.interruptible(s.ctx, func(ctx context.Context) error {
		for {
			s.mu.Lock()
			if len(s.queue) > 0 && s.queue[0] == d && len(s.active) < s.set.maxJobs {
				s.queue = s.queue[1:]
				s.active[d.id] = true
				s.mu.Unlock()
				return nil
			}
			wait := s.slotted
			s.mu.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
}

// enqueue puts d in the slot queue by its queuedAt (then ID). s.mu is held.
func (s *Service) enqueue(d *driver) {
	i, _ := slices.BinarySearchFunc(s.queue, d, func(a, b *driver) int {
		if c := a.queuedAt.Compare(b.queuedAt); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	s.queue = slices.Insert(s.queue, i, d)
}

// release gives d's slot (or place in the queue) back.
func (s *Service) release(d *driver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, d.id)
	if i := slices.Index(s.queue, d); i >= 0 {
		s.queue = slices.Delete(s.queue, i, i+1)
	}
	close(s.slotted)
	s.slotted = make(chan struct{})
}
