// Package runs implements createRun (G6, parity v1 contract): create a VM,
// run one command in it, keep the exit code and delete the VM unless keep.
// It is not a second state machine: every effect is an existing lifecycle
// operation (create, delete) or guest process call (exec, attach, signal),
// made with an idempotency key derived from the run ID. A run record
// journals its phase before and after each step, so a restarted daemon
// replays the unfinished step and the keys return the VM or process that
// already exists instead of creating a second one. Owner: CLI/API.
//
// A run reaches a terminal state only after cleanup: its VM is deleted
// (unless keep) on success, non-zero exit, timeout, setup failure and
// cancel alike.
package runs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestexec"
	"davit/internal/guestproto"
	"davit/internal/lifecycle"
	"davit/internal/state"
)

// VMs is the lifecycle API a run uses (lifecycle.Service).
type VMs interface {
	Create(ctx context.Context, req lifecycle.Request, body api.CreateVMRequest) (*lifecycle.Accepted, error)
	Delete(ctx context.Context, req lifecycle.Request, ref string) (*lifecycle.Accepted, error)
	Events(ctx context.Context, principal domain.PrincipalID, id domain.OperationID, after int64, wait time.Duration) (*domain.Operation, []domain.OperationEvent, error)
	GetVM(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.VM, error)
}

// Processes is the guest process API a run uses (guestexec.Manager).
type Processes interface {
	Exec(ctx context.Context, principal domain.PrincipalID, ref, key string, req api.ExecRequest) (api.Process, error)
	Get(ctx context.Context, principal domain.PrincipalID, ref, id string) (api.Process, error)
	Signal(ctx context.Context, principal domain.PrincipalID, ref, id, signal string) (api.Process, error)
	Attach(ctx context.Context, principal domain.PrincipalID, ref, id string, after int64,
		send func(api.ProcessStreamMessage) error, run func(guestexec.Stream)) error
}

// Config configures a Service.
type Config struct {
	DB    *state.DB
	VMs   VMs
	Procs Processes
	// Retry is the first backoff after a transient step failure (default
	// 500ms); it doubles up to 10s.
	Retry time.Duration
	// GiveUp is how long one step may keep failing transiently before the
	// run fails with that error (default 2m).
	GiveUp time.Duration
	// OutputGrace is how long a VM outlives its command (unless keep or
	// cancelled) so a client following the process stream can still read
	// output a fast command produced before the client attached; cancelRun
	// ends it early. Default 5s; negative disables it.
	OutputGrace time.Duration
	// HoldCheck is how often a held run checks that its VM is still
	// ready (default 30s): one that auto-stopped before startRun fails and
	// is cleaned up instead of waiting forever.
	HoldCheck time.Duration
	Now       func() time.Time
	Log       *slog.Logger
}

// Run phases, journaled in the run record. Each names the next step.
const (
	phaseCreate  = "create"  // create the VM
	phaseBoot    = "boot"    // wait for the create operation
	phaseHold    = "hold"    // waiting for startRun
	phaseExec    = "exec"    // start the command
	phaseFollow  = "follow"  // wait for the command to exit
	phaseCleanup = "cleanup" // kill (cancel), delete the VM unless keep, then finish
	phaseDone    = "done"
)

// record is a run's stored document: the api.Run fields (what getRun
// returns) plus what the orchestrator needs to resume it.
type record struct {
	api.Run
	Spec     api.RunRequest     `json:"spec"`
	Phase    string             `json:"phase"`
	CreateOp domain.OperationID `json:"createOperationId,omitempty"`
	// Run.Outcome is the decided terminal state, applied once cleanup is
	// done (Run.DecidedAt: when).
	Cancelled bool `json:"cancelRequested,omitempty"`
	// Released: cancelRun after the outcome ends the output grace.
	Released bool `json:"released,omitempty"`
}

// Service runs and tracks runs. It is safe for concurrent use.
type Service struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	drivers map[string]*driver // runs with a live driver goroutine
}

// driver advances one run, one journaled step at a time.
type driver struct {
	owner domain.PrincipalID
	id    string
	wake  chan struct{} // the record changed (startRun, cancelRun)

	mu   sync.Mutex
	stop context.CancelFunc // ends the current interruptible wait

	deletes int // delete attempts of this driver (each gets a fresh key)
}

// New returns a service. Call Start before serving.
func New(cfg Config) (*Service, error) {
	if cfg.DB == nil || cfg.VMs == nil || cfg.Procs == nil {
		return nil, errors.New("runs: DB, VMs and Procs are required")
	}
	if cfg.Retry == 0 {
		cfg.Retry = 500 * time.Millisecond
	}
	if cfg.GiveUp == 0 {
		cfg.GiveUp = 2 * time.Minute
	}
	if cfg.OutputGrace == 0 {
		cfg.OutputGrace = 5 * time.Second
	}
	if cfg.HoldCheck == 0 {
		cfg.HoldCheck = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Service{cfg: cfg, drivers: map[string]*driver{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

func (s *Service) now() time.Time { return s.cfg.Now().UTC() }

// Start resumes every unfinished run (after a daemon restart) from its
// journaled phase.
func (s *Service) Start(ctx context.Context) error {
	var refs []state.RunRef
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		refs, err = tx.UnfinishedRuns()
		return err
	}); err != nil {
		return fmt.Errorf("runs: %w", err)
	}
	for _, r := range refs {
		s.cfg.Log.Info("resuming run after daemon restart", "run", r.ID)
		s.spawn(r.Owner, r.ID)
	}
	return nil
}

// Close stops the drivers at a safe point; unfinished runs stay journaled
// and resume on the next Start.
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

// CreateRun accepts a run. key (the Idempotency-Key) is required: the same
// key and body return the same run, another body is a conflict.
func (s *Service) CreateRun(ctx context.Context, owner domain.PrincipalID, key string, req api.RunRequest) (api.Run, error) {
	if owner == "" {
		return api.Run{}, domain.Errorf(domain.CodeUnauthenticated, "no principal")
	}
	if key == "" {
		return api.Run{}, domain.Errorf(domain.CodeInvalidArgument, "Idempotency-Key is required").WithDetail("field", api.HeaderIdempotencyKey)
	}
	if len(key) > 255 {
		return api.Run{}, domain.Errorf(domain.CodeInvalidArgument, "Idempotency-Key longer than 255").WithDetail("field", api.HeaderIdempotencyKey)
	}
	if err := validate(req); err != nil {
		return api.Run{}, err
	}
	if req.Environment != nil {
		if ws, ok := s.cfg.VMs.(interface{ PreparesWorkspaces() bool }); !ok || !ws.PreparesWorkspaces() {
			return api.Run{}, domain.NotImplemented(domain.FeatureWorkspacePrep, "createRun.environment")
		}
	}
	if s.ctx.Err() != nil {
		return api.Run{}, domain.Errorf(domain.CodeUnavailable, "daemon is shutting down")
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return api.Run{}, err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	var out api.Run
	fresh := false
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		id, prev, err := tx.RunByKey(owner, key)
		if err != nil {
			return err
		}
		if id != "" {
			if prev != hash {
				return domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key was used with a different run request").WithDetail("runId", id)
			}
			var rec record
			if err := tx.GetRunDoc(owner, id, &rec); err != nil {
				return err
			}
			out = rec.Run
			return nil
		}
		rec := &record{Run: api.Run{ID: domain.NewID("run"), State: api.RunCreating, Keep: req.Keep, CreatedAt: s.now()},
			Spec: req, Phase: phaseCreate}
		out, fresh = rec.Run, true
		return tx.PutRunDoc(owner, key, hash, &rec.Run, rec)
	})
	if err != nil {
		return api.Run{}, err
	}
	if fresh {
		s.cfg.Log.Info("run accepted", "run", out.ID, "hold", req.Hold, "keep", req.Keep)
		s.spawn(owner, out.ID)
	}
	return out, nil
}

// validate refuses what can never run, before any record exists.
func validate(req api.RunRequest) error {
	if len(req.Argv) == 0 {
		return domain.Errorf(domain.CodeInvalidArgument, "argv must name a command").WithDetail("field", "argv")
	}
	if req.Image != "" && req.SnapshotID != "" {
		return domain.Errorf(domain.CodeInvalidArgument, "image and snapshotId are mutually exclusive")
	}
	if g := req.OutputGraceSeconds; g != nil && (*g < 0 || *g > api.MaxRunOutputGrace) {
		return domain.Errorf(domain.CodeInvalidArgument, "outputGraceSeconds must be 0..%d", api.MaxRunOutputGrace).WithDetail("field", "outputGraceSeconds")
	}
	if req.TimeoutSeconds < 0 {
		return domain.Errorf(domain.CodeInvalidArgument, "timeoutSeconds must be >= 0").WithDetail("field", "timeoutSeconds")
	}
	if err := protoExec(execRequest(req)).Validate(); err != nil {
		msg := err.Error()
		var pe *guestproto.Error
		if errors.As(err, &pe) {
			msg = pe.Message
		}
		return domain.Errorf(domain.CodeInvalidArgument, "%s", msg)
	}
	return nil
}

// execRequest is the run's command as a detached process: it outlives the
// daemon's guest session, so a restarted daemon follows the same process.
func execRequest(req api.RunRequest) api.ExecRequest {
	return api.ExecRequest{Argv: req.Argv, Cwd: req.Cwd, Env: req.Env, DeadlineSeconds: req.TimeoutSeconds, Detached: true}
}

// protoExec is what the guest agent will be asked (validated up front, as
// guestexec does).
func protoExec(e api.ExecRequest) guestproto.Exec {
	g := guestproto.Exec{Argv: e.Argv, Cwd: e.Cwd, DeadlineSeconds: e.DeadlineSeconds, Detached: e.Detached}
	for _, k := range slices.Sorted(maps.Keys(e.Env)) {
		g.Env = append(g.Env, k+"="+e.Env[k])
	}
	return g
}

// GetRun returns owner's run.
func (s *Service) GetRun(ctx context.Context, owner domain.PrincipalID, id string) (api.Run, error) {
	var r *api.Run
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		r, err = tx.GetRun(owner, id)
		return err
	})
	if err != nil {
		return api.Run{}, err
	}
	return *r, nil
}

// ListRuns returns one page of owner's runs, oldest first.
func (s *Service) ListRuns(ctx context.Context, owner domain.PrincipalID, limit int, cursor string) (api.RunList, error) {
	if limit <= 0 || limit > api.MaxListLimit {
		limit = api.DefaultListLimit
	}
	out := api.RunList{Items: []api.Run{}}
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		runs, next, err := tx.ListRuns(owner, state.Page{Limit: limit, Cursor: cursor, Filter: "runs|" + string(owner)})
		for _, r := range runs {
			out.Items = append(out.Items, *r)
		}
		out.NextCursor = next
		return err
	})
	return out, err
}

// StartRun starts the command of a run created with hold. Only a waiting
// run starts; any other state is invalid_state.
func (s *Service) StartRun(ctx context.Context, owner domain.PrincipalID, id string) (api.Run, error) {
	rec, err := s.update(ctx, owner, id, func(r *record) (bool, error) {
		if r.State != api.RunWaiting || r.Phase != phaseHold {
			st := string(r.State)
			if r.Outcome != "" && !terminal(r.State) {
				st = "ending (" + string(r.Outcome) + ")"
			}
			return false, domain.Errorf(domain.CodeInvalidState, "run %s is %s; only a waiting run (created with hold) can start", id, st).
				WithDetail("state", string(r.State))
		}
		r.State, r.Phase = api.RunRunning, phaseExec
		return true, nil
	})
	if err != nil {
		return api.Run{}, err
	}
	s.poke(id)
	return rec.Run, nil
}

// CancelRun cancels a run: the command is killed and the VM deleted
// (unless keep), then the run is cancelled. A run whose command already
// ended keeps its outcome: cancelling it only ends the output grace, so
// cleanup starts at once. Cancelling an ended run changes nothing.
func (s *Service) CancelRun(ctx context.Context, owner domain.PrincipalID, id string) (api.Run, error) {
	rec, err := s.update(ctx, owner, id, func(r *record) (bool, error) {
		if terminal(r.State) || r.Released {
			return false, nil
		}
		if r.Outcome != "" {
			r.Released = true
			return true, nil
		}
		now := s.now()
		r.Outcome, r.DecidedAt, r.Cancelled, r.Phase = api.RunCancelled, &now, true, phaseCleanup
		return true, nil
	})
	if err != nil {
		return api.Run{}, err
	}
	s.poke(id)
	return rec.Run, nil
}

func terminal(st api.RunState) bool {
	return st == api.RunSucceeded || st == api.RunFailed || st == api.RunCancelled
}

// update applies fn to the stored record in one transaction; fn reports
// whether it changed anything.
func (s *Service) update(ctx context.Context, owner domain.PrincipalID, id string, fn func(*record) (bool, error)) (*record, error) {
	var rec record
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec = record{}
		if err := tx.GetRunDoc(owner, id, &rec); err != nil {
			return err
		}
		changed, err := fn(&rec)
		if err != nil || !changed {
			return err
		}
		return tx.PutRunDoc(owner, "", "", &rec.Run, &rec)
	})
	return &rec, err
}

func (s *Service) spawn(owner domain.PrincipalID, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drivers[id] != nil || s.ctx.Err() != nil {
		return
	}
	d := &driver{owner: owner, id: id, wake: make(chan struct{}, 1)}
	s.drivers[id] = d
	s.wg.Add(1)
	go s.drive(d)
}

// poke tells run id's driver that its record changed: a pending wait ends
// and the driver reloads the record.
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

// errInterrupted ends a step whose wait a poke cut short; the driver
// reloads the record and continues from its (possibly new) phase.
var errInterrupted = errors.New("runs: interrupted")

// interruptible runs the wait f, which ends early (errInterrupted) when the
// record changes.
func (d *driver) interruptible(parent context.Context, f func(ctx context.Context) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	d.mu.Lock()
	d.stop = cancel
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.stop = nil
		d.mu.Unlock()
	}()
	select {
	case <-d.wake: // changed before the wait began
		return errInterrupted
	default:
	}
	err := f(ctx)
	if ctx.Err() != nil && parent.Err() == nil {
		return errInterrupted
	}
	return err
}

func (s *Service) drive(d *driver) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		if s.drivers[d.id] == d {
			delete(s.drivers, d.id)
		}
		s.mu.Unlock()
	}()
	backoff := s.cfg.Retry
	var failing string // the phase that keeps failing
	var since time.Time
	for s.ctx.Err() == nil {
		var rec record
		err := s.cfg.DB.Tx(s.ctx, func(tx *state.Tx) error { return tx.GetRunDoc(d.owner, d.id, &rec) })
		if err == nil {
			if rec.Phase == phaseDone {
				return
			}
			err = s.step(d, &rec)
		}
		if err == nil || errors.Is(err, errInterrupted) {
			backoff, failing = s.cfg.Retry, ""
			continue
		}
		if s.ctx.Err() != nil {
			return
		}
		if failing != rec.Phase {
			failing, since = rec.Phase, s.cfg.Now()
		}
		if rec.Phase != "" && s.cfg.Now().Sub(since) >= s.cfg.GiveUp {
			s.cfg.Log.Warn("run step keeps failing; giving up", "run", d.id, "phase", rec.Phase, "err", err)
			if err := s.giveUp(d, &rec, err); err != nil && s.ctx.Err() == nil {
				s.cfg.Log.Error("run: record give-up", "run", d.id, "err", err)
			}
			failing = ""
			continue
		}
		s.cfg.Log.Warn("run step failed; retrying", "run", d.id, "phase", rec.Phase, "err", err)
		select {
		case <-time.After(backoff):
		case <-d.wake:
		case <-s.ctx.Done():
			return
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (s *Service) step(d *driver, rec *record) error {
	switch rec.Phase {
	case phaseCreate:
		return s.create(d, rec)
	case phaseBoot:
		return s.boot(d, rec)
	case phaseHold:
		select {
		case <-d.wake:
			return nil
		case <-time.After(s.cfg.HoldCheck):
			return s.checkHeld(d, rec)
		case <-s.ctx.Done():
			return nil
		}
	case phaseExec:
		return s.exec(d, rec)
	case phaseFollow:
		return s.follow(d, rec)
	case phaseCleanup:
		return s.cleanup(d, rec)
	}
	return s.decide(d, api.RunFailed, domain.Errorf(domain.CodeInternal, "run record has unknown phase %q", rec.Phase), nil)
}

// req is the lifecycle request of one run step. The key is derived from
// the run, so a replayed step returns the operation it already started.
func (d *driver) req(method, step string) lifecycle.Request {
	return lifecycle.Request{Principal: d.owner, IdempotencyKey: "run:" + d.id + ":" + step, Method: method, Path: "internal/runs/" + d.id + "/" + step}
}

// vmName is the run's VM name: unique per run, so even a create replayed
// after its idempotency record expired cannot make a second VM.
func vmName(run string) string { return "run-" + strings.TrimPrefix(run, "run_") }

// ttlFor keeps the host's default TTL unless the command may run longer.
func ttlFor(sp api.RunRequest) *int64 {
	if sp.TimeoutSeconds <= 0 {
		return nil
	}
	ttl := sp.TimeoutSeconds + int64((15 * time.Minute).Seconds())
	if ttl <= int64(time.Hour.Seconds()) {
		return nil
	}
	return &ttl
}

func (s *Service) create(d *driver, rec *record) error {
	sp := rec.Spec
	body := api.CreateVMRequest{Name: vmName(rec.ID), Type: sp.Type, Image: sp.Image, SnapshotID: sp.SnapshotID,
		Shape: sp.Shape, TTLSeconds: ttlFor(sp), Environment: sp.Environment}
	a, err := s.cfg.VMs.Create(s.ctx, d.req("POST", "create"), body)
	if err != nil {
		if transient(err) {
			return err
		}
		// Should a VM of this run exist anyway, cleanup must find it.
		if vm, gerr := s.cfg.VMs.GetVM(s.ctx, d.owner, vmName(rec.ID)); gerr == nil {
			_, _ = s.update(s.ctx, d.owner, d.id, func(r *record) (bool, error) { r.VMID = vm.ID; return true, nil })
		}
		return s.decide(d, api.RunFailed, prefixed("create VM", err), nil)
	}
	_, err = s.update(s.ctx, d.owner, d.id, func(r *record) (bool, error) {
		r.VMID, r.CreateOp = a.Operation.VMID, a.Operation.ID
		if r.Phase == phaseCreate {
			r.Phase = phaseBoot
		}
		return true, nil
	})
	if err == nil {
		s.cfg.Log.Info("run vm accepted", "run", d.id, "vm", a.Operation.VMID, "operation", a.Operation.ID)
	}
	return err
}

func (s *Service) boot(d *driver, rec *record) error {
	op, err := s.waitOp(d, rec.CreateOp)
	if err != nil {
		if transient(err) {
			return err
		}
		return s.decide(d, api.RunFailed, prefixed("create VM", err), nil)
	}
	if op.State != domain.OpSucceeded {
		cause := domain.Errorf(domain.CodeInternal, "create operation %s %s", op.ID, op.State)
		if op.Error != nil {
			cause = op.Error
		}
		return s.decide(d, api.RunFailed, prefixed("create VM", cause), nil)
	}
	if rec.Spec.Environment != nil {
		if ready, err := s.waitWorkspace(d, rec); err != nil || !ready {
			return err
		}
	}
	_, err = s.update(s.ctx, d.owner, d.id, func(r *record) (bool, error) {
		if r.Phase != phaseBoot {
			return false, nil
		}
		if r.Spec.Hold {
			r.State, r.Phase = api.RunWaiting, phaseHold
		} else {
			r.State, r.Phase = api.RunRunning, phaseExec
		}
		return true, nil
	})
	return err
}

// waitWorkspace waits until the run VM's pinned workspace is prepared. A
// failed preparation decides the run (failed, with the step in the error)
// and reports not ready.
func (s *Service) waitWorkspace(d *driver, rec *record) (bool, error) {
	for {
		vm, err := s.cfg.VMs.GetVM(s.ctx, d.owner, string(rec.VMID))
		if err != nil {
			if transient(err) {
				return false, err
			}
			return false, s.decide(d, api.RunFailed, prefixed("prepare workspace", err), nil)
		}
		switch ws := vm.Workspace; {
		case ws != nil && ws.State == domain.WorkspaceReady:
			return true, nil
		case ws == nil || ws.State == domain.WorkspaceFailed:
			cause := domain.Errorf(domain.CodeInvalidState, "workspace preparation failed")
			if ws != nil && ws.Error != nil {
				cause = domain.Errorf(domain.CodeInvalidState, "workspace preparation failed at %s %s: %s", ws.Error.Phase, ws.Error.Step, ws.Error.Message).
					WithDetail("step", ws.Error.Step).WithDetail("phase", ws.Error.Phase)
			}
			return false, s.decide(d, api.RunFailed, cause, nil)
		}
		if err := d.interruptible(s.ctx, func(ctx context.Context) error {
			select {
			case <-time.After(s.cfg.Retry):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}); err != nil {
			return false, err
		}
	}
}

// waitOp follows a lifecycle operation until it is terminal.
func (s *Service) waitOp(d *driver, id domain.OperationID) (*domain.Operation, error) {
	var after int64
	for {
		var op *domain.Operation
		var evs []domain.OperationEvent
		err := d.interruptible(s.ctx, func(ctx context.Context) (err error) {
			op, evs, err = s.cfg.VMs.Events(ctx, d.owner, id, after, 25*time.Second)
			return err
		})
		if err != nil {
			return nil, err
		}
		if n := len(evs); n > 0 {
			after = evs[n-1].Seq
		}
		if op.State.Terminal() && len(evs) == 0 {
			return op, nil
		}
		if s.ctx.Err() != nil {
			return nil, s.ctx.Err()
		}
	}
}

// checkHeld fails a held run whose VM is no longer ready (auto-stopped or
// gone): nobody can start it any more.
func (s *Service) checkHeld(d *driver, rec *record) error {
	vm, err := s.cfg.VMs.GetVM(s.ctx, d.owner, string(rec.VMID))
	switch {
	case err != nil && transient(err):
		return err
	case err == nil && (vm.State == domain.StateReady || vm.State == domain.StateRunning):
		return nil
	case err == nil:
		err = domain.Errorf(domain.CodeInvalidState, "vm %s is %s; the run was not started while it was ready", vm.ID, vm.State).
			WithDetail("state", string(vm.State))
	}
	return s.decide(d, api.RunFailed, prefixed("waiting for startRun", err), nil)
}

func (s *Service) exec(d *driver, rec *record) error {
	p, err := s.cfg.Procs.Exec(s.ctx, d.owner, string(rec.VMID), "run:"+d.id+":exec", execRequest(rec.Spec))
	if err != nil {
		if transient(err) {
			return err
		}
		return s.decide(d, api.RunFailed, prefixed("start command", err), nil)
	}
	_, err = s.update(s.ctx, d.owner, d.id, func(r *record) (bool, error) {
		r.ProcessID = p.ID
		redactEnv(r)
		if r.Phase == phaseExec {
			r.Phase = phaseFollow
		}
		return true, nil
	})
	if err == nil {
		s.cfg.Log.Info("run command started", "run", d.id, "vm", rec.VMID, "process", p.ID)
	}
	return err
}

// redactEnv drops the values of the run's environment variables (the names
// stay, for inspection). They may be secrets, and the host keeps them only
// while it may still have to (re)issue the keyed exec: once the process
// exists, or the run ended without one, nothing needs them again.
func redactEnv(r *record) {
	for k := range r.Spec.Env {
		r.Spec.Env[k] = ""
	}
}

func (s *Service) follow(d *driver, rec *record) error {
	vm, pid := string(rec.VMID), rec.ProcessID
	var cursor int64
	var final *api.Process
	err := d.interruptible(s.ctx, func(ctx context.Context) error {
		return s.cfg.Procs.Attach(ctx, d.owner, vm, pid, cursor, func(m api.ProcessStreamMessage) error {
			if m.Type == api.StreamExit && m.Process != nil {
				p := *m.Process
				final = &p
			} else if m.Seq > cursor {
				cursor = m.Seq
			}
			return nil
		}, nil)
	})
	if errors.Is(err, errInterrupted) {
		return err
	}
	if final == nil || final.ExitedAt == nil {
		// The stream ended without an exit: ask for the process itself.
		p, gerr := s.cfg.Procs.Get(s.ctx, d.owner, vm, pid)
		switch {
		case gerr == nil && p.ExitedAt != nil:
			final = &p
		case gerr != nil && !transient(gerr):
			return s.decide(d, api.RunFailed, prefixed("follow command", gerr), nil)
		case gerr != nil:
			return gerr
		case err != nil:
			return err
		default:
			return domain.Errorf(domain.CodeUnavailable, "process %s: stream ended before it exited", pid)
		}
	}
	return s.decideProcess(d, *final)
}

// decideProcess maps the exited command to the run's outcome.
func (s *Service) decideProcess(d *driver, p api.Process) error {
	if p.Signal != "" {
		if _, err := s.update(s.ctx, d.owner, d.id, func(r *record) (bool, error) {
			r.Signal = p.Signal
			return true, nil
		}); err != nil {
			return err
		}
	}
	switch {
	case p.TimedOut:
		return s.decide(d, api.RunFailed, domain.Errorf(domain.CodeTimeout, "the command exceeded its timeout"), p.ExitCode)
	case p.Status == api.ProcessExited && p.ExitCode != nil && *p.ExitCode == 0:
		return s.decide(d, api.RunSucceeded, nil, p.ExitCode)
	case p.Status == api.ProcessExited && p.ExitCode != nil:
		return s.decide(d, api.RunFailed, nil, p.ExitCode)
	case p.Signal != "":
		return s.decide(d, api.RunFailed, nil, p.ExitCode) // Run.signal says how
	}
	return s.decide(d, api.RunFailed, domain.Errorf(domain.CodeUnavailable, "the command's outcome is unknown (process %s is %s)", p.ID, p.Status), p.ExitCode)
}

// decide records the run's outcome (unless a cancel decided it first) and
// moves it to cleanup.
func (s *Service) decide(d *driver, outcome api.RunState, cause error, exit *int) error {
	_, err := s.update(s.ctx, d.owner, d.id, func(r *record) (bool, error) {
		if exit != nil && r.ExitCode == nil {
			code := *exit
			r.ExitCode = &code
		}
		if r.Outcome == "" {
			now := s.now()
			r.Outcome, r.Phase, r.DecidedAt = outcome, phaseCleanup, &now
			r.Error = apiError(cause)
		}
		return true, nil
	})
	if err == nil {
		s.cfg.Log.Info("run outcome", "run", d.id, "outcome", outcome, "err", cause)
	}
	return err
}

func (s *Service) cleanup(d *driver, rec *record) error {
	if rec.Cancelled && rec.ProcessID != "" && rec.VMID != "" {
		// Best effort: deleting the VM ends the command anyway, and a
		// process that already exited refuses the signal.
		_, _ = s.cfg.Procs.Signal(s.ctx, d.owner, string(rec.VMID), rec.ProcessID, "KILL")
	}
	if !rec.Keep && rec.VMID != "" {
		if err := s.outputGrace(d, rec); err != nil {
			return err
		}
		if err := s.deleteVM(d, rec); err != nil {
			return err
		}
	}
	return s.finish(d, nil)
}

// outputGrace keeps the VM of a command that ran for up to OutputGrace
// after its outcome, so a client that attaches late still reads all the
// output; cancelRun (Released) or a cancelled run skips it.
func (s *Service) outputGrace(d *driver, rec *record) error {
	grace := s.cfg.OutputGrace
	if g := rec.Spec.OutputGraceSeconds; g != nil {
		grace = time.Duration(*g) * time.Second
	}
	if rec.ProcessID == "" || rec.Cancelled || rec.Released || rec.DecidedAt == nil || grace <= 0 {
		return nil
	}
	wait := rec.DecidedAt.Add(grace).Sub(s.now())
	if wait <= 0 {
		return nil
	}
	return d.interruptible(s.ctx, func(ctx context.Context) error {
		select {
		case <-time.After(wait):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

// finish makes the decided outcome the run's terminal state. cause, when
// set, overrides it as a failure (cleanup gave up).
func (s *Service) finish(d *driver, cause error) error {
	_, err := s.update(s.ctx, d.owner, d.id, func(r *record) (bool, error) {
		if r.Phase == phaseDone {
			return false, nil
		}
		now := s.now()
		r.State, r.EndedAt, r.Phase = r.Outcome, &now, phaseDone
		redactEnv(r) // a run that never started its command
		if r.State == "" {
			r.State = api.RunFailed
		}
		if cause != nil {
			r.State, r.Error = api.RunFailed, apiError(cause)
		}
		return true, nil
	})
	if err == nil {
		s.cfg.Log.Info("run finished", "run", d.id)
	}
	return err
}

func (s *Service) deleteVM(d *driver, rec *record) error {
	// A failed delete operation stays failed under its key: each attempt
	// of this driver gets a fresh one.
	a, err := s.cfg.VMs.Delete(s.ctx, d.req("DELETE", fmt.Sprintf("delete-%d", d.deletes)), string(rec.VMID))
	if domain.IsCode(err, domain.CodeNotFound) {
		return nil // already gone
	}
	if err != nil {
		return err
	}
	op, err := s.waitOp(d, a.Operation.ID)
	if err != nil {
		return err
	}
	if op.State != domain.OpSucceeded {
		d.deletes++
		cause := domain.Errorf(domain.CodeInternal, "delete operation %s %s", op.ID, op.State)
		if op.Error != nil {
			cause = op.Error
		}
		return prefixed("delete VM", cause)
	}
	return nil
}

// giveUp ends a step that kept failing transiently: a run still setting up
// or running fails with the error; a run whose cleanup fails ends failed.
func (s *Service) giveUp(d *driver, rec *record, cause error) error {
	if rec.Phase == phaseCleanup {
		return s.finish(d, prefixed("cleanup", cause))
	}
	return s.decide(d, api.RunFailed, cause, nil)
}

// transient reports an error worth retrying: the guest or daemon is busy
// or unreachable, or a host error that is not a typed domain error.
func transient(err error) bool {
	var de *domain.Error
	if !errors.As(err, &de) {
		return true
	}
	switch de.Code {
	case domain.CodeUnavailable, domain.CodeOperationInProgress, domain.CodeTimeout:
		return true
	}
	return false
}

func prefixed(what string, err error) error {
	de := domain.AsError(err)
	out := *de
	out.Message = what + ": " + de.Message
	return &out
}

func apiError(err error) *api.Error {
	if err == nil {
		return nil
	}
	de := domain.AsError(err)
	return &api.Error{Code: de.Code, Message: de.Message, Retryable: de.Retryable, Details: de.Details, OperationID: de.OperationID}
}
