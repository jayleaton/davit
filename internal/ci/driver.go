package ci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/ci/github"
	"davit/internal/domain"
	"davit/internal/lifecycle"
	"davit/internal/state"
)

// Box labels: every runner box carries both, so a restarted daemon finds
// the boxes no job record accounts for.
const (
	LabelCI  = "davit.ci"     // "github"
	LabelJob = "davit.ci.job" // the job record's ID
)

// JITEnv carries the runner's just-in-time configuration into the box:
// the runner reads ACTIONS_RUNNER_INPUT_<ARG> as its --<arg> and removes it
// from the environment its jobs see. The host never stores it.
const JITEnv = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// driver advances one job, one journaled step at a time.
type driver struct {
	id   string
	wake chan struct{} // the record changed

	mu   sync.Mutex
	stop context.CancelFunc // ends the current interruptible wait

	deletes  int       // delete attempts (each gets a fresh key)
	queuedAt time.Time // its place in the slot queue
	// jit is the runner secret between register and exec; a restart in
	// between loses it on purpose (exec registers a new runner).
	jit string
}

var (
	errInterrupted = errors.New("ci: interrupted")
	errDeadline    = errors.New("ci: deadline")
)

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
	case <-d.wake:
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
		s.release(d)
		s.mu.Lock()
		if s.drivers[d.id] == d {
			delete(s.drivers, d.id)
		}
		s.mu.Unlock()
	}()
	backoff := s.cfg.Retry
	var failing string
	var since time.Time
	for s.ctx.Err() == nil {
		var rec record
		err := s.cfg.DB.Tx(s.ctx, func(tx *state.Tx) error { return tx.GetCIJob(d.id, &rec) })
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
		if failing != rec.Phase || waitingForRoom(err) {
			failing, since = rec.Phase, s.cfg.Clock.Now()
		}
		if rec.Phase != "" && s.cfg.Clock.Now().Sub(since) >= s.cfg.GiveUp {
			s.cfg.Log.Warn("ci: job step keeps failing; giving up", "job", d.id, "phase", rec.Phase, "err", err)
			if err := s.giveUp(d, &rec, err); err != nil && s.ctx.Err() == nil {
				s.cfg.Log.Error("ci: record give-up", "job", d.id, "err", err)
			}
			failing = ""
			continue
		}
		s.cfg.Log.Warn("ci: job step failed; retrying", "job", d.id, "phase", rec.Phase, "err", err)
		var ra *github.Error
		wait := backoff
		if errors.As(err, &ra) && ra.RetryAfter > wait {
			wait = ra.RetryAfter
		}
		select {
		case <-time.After(wait):
		case <-d.wake:
		case <-s.ctx.Done():
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (s *Service) step(d *driver, rec *record) error {
	if rec.Phase != phaseCleanup {
		if rec.Kill != "" {
			return s.decide(d, rec.Kill, nil, nil)
		}
		if rec.DemandGone && !rec.Busy {
			if done, err := s.unneeded(d, rec); done || err != nil {
				return err
			}
		}
	}
	switch rec.Phase {
	case phaseQueued:
		if err := s.acquire(d, rec); err != nil {
			return err
		}
		_, err := s.update(s.ctx, d.id, func(r *record) (bool, error) {
			if r.Phase != phaseQueued {
				return false, nil
			}
			r.Phase = phaseCreate
			return true, nil
		})
		return err
	case phaseCreate:
		return s.create(d, rec)
	case phaseBoot:
		return s.boot(d, rec)
	case phaseRegister:
		return s.register(d, rec)
	case phaseExec:
		return s.exec(d, rec)
	case phaseFollow:
		return s.follow(d, rec)
	case phaseCleanup:
		return s.cleanup(d, rec)
	}
	return s.decide(d, api.CIResultFailed, domain.Errorf(domain.CodeInternal, "CI job record has unknown phase %q", rec.Phase), nil)
}

// unneeded ends a job whose demand went before its runner got a job. A
// registered runner is removed first; GitHub refuses that once the runner
// is busy, and then the runner runs to the end. done reports the job was
// decided.
func (s *Service) unneeded(d *driver, rec *record) (done bool, err error) {
	if rec.RunnerID != 0 {
		err := s.cfg.App.Installation(rec.Installation).DeleteRunner(s.ctx, s.scope(rec.Repo), rec.RunnerID)
		if github.IsStatus(err, http.StatusUnprocessableEntity) {
			_, err = s.update(s.ctx, d.id, func(r *record) (bool, error) { r.Busy = true; return true, nil })
			return false, err
		}
		if err != nil {
			return false, fmt.Errorf("remove the unneeded runner %s: %w", rec.Runner, err)
		}
		_, err = s.update(s.ctx, d.id, func(r *record) (bool, error) { r.RunnerID = 0; return true, nil })
		if err != nil {
			return false, err
		}
	}
	s.cfg.Log.Info("ci: job ended or went to another runner before this one got it", "job", d.id, "githubJob", rec.JobID)
	return true, s.decide(d, api.CIResultCancelled, nil, nil)
}

// req is the lifecycle request of one step, keyed by the job.
func (d *driver) req(method, step string, wait time.Duration) lifecycle.Request {
	return lifecycle.Request{IdempotencyKey: "ci:" + d.id + ":" + step, Method: method, Path: "internal/ci/" + d.id + "/" + step, CapacityWait: wait}
}

func (s *Service) request(d *driver, method, step string, wait time.Duration) lifecycle.Request {
	r := d.req(method, step, wait)
	r.Principal = s.cfg.Owner
	return r
}

// suffix is the random part of a job ID.
func suffix(id string) string { return strings.TrimPrefix(id, "ci_") }

// vmName is the job's box name: unique per job, so a create replayed after
// its idempotency record expired cannot make a second box.
func vmName(id string) string { return "ci-" + suffix(id) }

// runnerName is the name of a job's runner registration attempt.
func (s *Service) runnerName(id string, attempt int) string {
	return fmt.Sprintf("%s-%s-%d", s.set.prefix, suffix(id), attempt)
}

// parseRunner splits one of this host's runner names into its job ID and
// attempt.
func (s *Service) parseRunner(name string) (id string, attempt int, ok bool) {
	rest, ok := strings.CutPrefix(name, s.set.prefix+"-")
	if !ok {
		return "", 0, false
	}
	sfx, n, ok := strings.Cut(rest, "-")
	if !ok {
		return "", 0, false
	}
	a, err := strconv.Atoi(n)
	if err != nil || sfx == "" {
		return "", 0, false
	}
	return "ci_" + sfx, a, true
}

// scope is where the runner for a job of repo registers.
func (s *Service) scope(repo string) github.Scope {
	if s.set.scopeOrg {
		owner, _, _ := strings.Cut(repo, "/")
		return github.Scope{Org: owner}
	}
	return github.Scope{Repo: repo}
}

// boxTTL bounds the box's life should the controller never come back: the
// job timeout plus room to boot and clean up. The box is ephemeral, so
// the TTL stop deletes it.
func (s *Service) boxTTL() int64 { return int64((s.set.timeout + 30*time.Minute) / time.Second) }

func (s *Service) create(d *driver, rec *record) error {
	ttl := s.boxTTL()
	body := api.CreateVMRequest{Name: vmName(rec.ID), Image: s.set.image,
		Shape:      &api.ShapeOverride{CPUs: rec.Spec.CPUs, MemoryBytes: rec.Spec.MemoryBytes, DiskBytes: rec.Spec.DiskBytes},
		TTLSeconds: &ttl, Privacy: &api.PrivacyPolicy{Ephemeral: true},
		Labels: map[string]string{LabelCI: "github", LabelJob: rec.ID}}
	var a *lifecycle.Accepted
	err := d.interruptible(s.ctx, func(ctx context.Context) (err error) {
		a, err = s.cfg.VMs.Create(ctx, s.request(d, http.MethodPost, "create", domain.MaxCapacityWaitSeconds*time.Second), body)
		return err
	})
	if err != nil {
		if errors.Is(err, errInterrupted) || transient(err) {
			return err
		}
		if vm, gerr := s.cfg.VMs.GetVM(s.ctx, s.cfg.Owner, vmName(rec.ID)); gerr == nil {
			_, _ = s.update(s.ctx, d.id, func(r *record) (bool, error) { r.VMID = vm.ID; return true, nil })
		}
		return s.decide(d, api.CIResultFailed, prefixed("create the runner box", err), nil)
	}
	_, err = s.update(s.ctx, d.id, func(r *record) (bool, error) {
		r.VMID, r.CreateOp = a.Operation.VMID, a.Operation.ID
		if r.Phase == phaseCreate {
			r.Phase = phaseBoot
		}
		return true, nil
	})
	if err == nil {
		s.cfg.Log.Info("ci: runner box accepted", "job", d.id, "vm", a.Operation.VMID)
	}
	return err
}

func (s *Service) boot(d *driver, rec *record) error {
	op, err := s.waitOp(d, rec.CreateOp)
	if err != nil {
		if errors.Is(err, errInterrupted) || transient(err) {
			return err
		}
		return s.decide(d, api.CIResultFailed, prefixed("create the runner box", err), nil)
	}
	if op.State != domain.OpSucceeded {
		cause := domain.Errorf(domain.CodeInternal, "create operation %s %s", op.ID, op.State)
		if op.Error != nil {
			cause = op.Error
		}
		return s.decide(d, api.CIResultFailed, prefixed("create the runner box", cause), nil)
	}
	_, err = s.update(s.ctx, d.id, func(r *record) (bool, error) {
		if r.Phase != phaseBoot {
			return false, nil
		}
		now := s.now()
		r.BoxReadyAt, r.Phase = &now, phaseRegister
		return true, nil
	})
	return err
}

// register registers a single-use runner for the job and keeps its secret
// in memory for exec.
func (s *Service) register(d *driver, rec *record) error {
	name := s.runnerName(rec.ID, rec.Attempt)
	jit, err := s.cfg.App.Installation(rec.Installation).GenerateJITConfig(s.ctx, s.scope(rec.Repo), github.JITRequest{
		Name: name, RunnerGroupID: max(s.set.group, 1), Labels: runnerLabels(s.set, rec.Shape), WorkFolder: "_work"})
	if github.IsStatus(err, http.StatusConflict) {
		// A registration of this attempt exists (a restart right after
		// registering): take the next name; reconcile removes the old one.
		_, err = s.update(s.ctx, d.id, func(r *record) (bool, error) { r.Attempt++; return true, nil })
		return err
	}
	if err != nil {
		if transient(err) {
			return err
		}
		return s.decide(d, api.CIResultFailed, prefixed("register the runner", err), nil)
	}
	_, err = s.update(s.ctx, d.id, func(r *record) (bool, error) {
		r.Runner, r.RunnerID = name, jit.RunnerID
		if r.Phase == phaseRegister {
			r.Phase = phaseExec
		}
		return true, nil
	})
	if err == nil {
		d.jit = jit.Encoded
		s.cfg.Log.Info("ci: runner registered", "job", d.id, "runner", name, "runnerId", jit.RunnerID)
	}
	return err
}

func (s *Service) exec(d *driver, rec *record) error {
	if d.jit == "" {
		return s.reregister(d, rec)
	}
	key := fmt.Sprintf("ci:%s:exec:%d", d.id, rec.Attempt)
	p, err := s.cfg.Procs.Exec(s.ctx, s.cfg.Owner, string(rec.VMID), key, api.ExecRequest{Argv: s.set.command, User: s.set.user,
		Env: map[string]string{JITEnv: d.jit}, DeadlineSeconds: int64(s.set.timeout / time.Second), Detached: true})
	if err != nil {
		if transient(err) {
			return err
		}
		return s.decide(d, api.CIResultFailed, prefixed("start the runner", err), nil)
	}
	_, err = s.update(s.ctx, d.id, func(r *record) (bool, error) {
		now := s.now()
		r.ProcessID, r.StartedAt = p.ID, &now
		if r.Phase == phaseExec {
			r.Phase = phaseFollow
		}
		return true, nil
	})
	if err == nil {
		d.jit = ""
		s.cfg.Log.Info("ci: runner started", "job", d.id, "vm", rec.VMID, "process", p.ID)
	}
	return err
}

// reregister handles exec after a restart lost the runner secret: a runner
// that already started is followed; otherwise its registration is removed
// and a new one made.
func (s *Service) reregister(d *driver, rec *record) error {
	ps, _, err := s.cfg.Procs.List(s.ctx, s.cfg.Owner, string(rec.VMID), 50, "")
	if err != nil {
		if transient(err) {
			return err
		}
		return s.decide(d, api.CIResultFailed, prefixed("find the runner", err), nil)
	}
	for _, p := range ps {
		if slices.Equal(p.Argv, s.set.command) {
			_, err := s.update(s.ctx, d.id, func(r *record) (bool, error) {
				started := p.CreatedAt
				r.ProcessID, r.StartedAt, r.Phase = p.ID, &started, phaseFollow
				return true, nil
			})
			return err
		}
	}
	if rec.RunnerID != 0 {
		if err := s.cfg.App.Installation(rec.Installation).DeleteRunner(s.ctx, s.scope(rec.Repo), rec.RunnerID); err != nil {
			s.cfg.Log.Warn("ci: remove a runner registration that never started", "job", d.id, "runner", rec.Runner, "err", err)
		}
	}
	_, err = s.update(s.ctx, d.id, func(r *record) (bool, error) {
		r.Attempt++
		r.Runner, r.RunnerID, r.Phase = "", 0, phaseRegister
		return true, nil
	})
	return err
}

// deadline is when a running runner's box goes regardless: a little after
// the job timeout counted from the runner's start (the guest kills the
// runner at the timeout itself; this is the host's backstop), or
// CompletedGrace after GitHub reported its job completed.
func (s *Service) deadline(rec *record) time.Time {
	dl := rec.StartedAt.Add(s.set.timeout + min(time.Minute, s.set.timeout/10))
	if rec.JobDoneAt != nil && rec.JobDoneAt.Add(s.cfg.CompletedGrace).Before(dl) {
		dl = rec.JobDoneAt.Add(s.cfg.CompletedGrace)
	}
	return dl
}

func (s *Service) follow(d *driver, rec *record) error {
	if rec.StartedAt == nil {
		now := s.now()
		rec.StartedAt = &now
	}
	vm, pid := string(rec.VMID), rec.ProcessID
	dl := s.deadline(rec)
	var final *api.Process
	var cursor int64
	err := d.interruptible(s.ctx, func(ctx context.Context) error {
		actx, cancel := context.WithCancelCause(ctx)
		defer cancel(nil)
		stop := s.cfg.Clock.AfterFunc(dl.Sub(s.cfg.Clock.Now()), func() { cancel(errDeadline) })
		defer stop()
		err := s.cfg.Procs.Attach(actx, s.cfg.Owner, vm, pid, cursor, func(m api.ProcessStreamMessage) error {
			if m.Type == api.StreamExit && m.Process != nil {
				p := *m.Process
				final = &p
			} else if m.Seq > cursor {
				cursor = m.Seq
			}
			return nil
		}, nil)
		if final == nil && ctx.Err() == nil && errors.Is(context.Cause(actx), errDeadline) {
			return errDeadline
		}
		return err
	})
	if errors.Is(err, errInterrupted) {
		return err
	}
	if errors.Is(err, errDeadline) {
		if rec.JobDoneAt != nil {
			s.cfg.Log.Warn("ci: the runner's job completed but the runner did not exit; deleting its box", "job", d.id)
			return s.decide(d, api.CIResultSucceeded, nil, nil)
		}
		return s.decide(d, api.CIResultTimedOut, domain.Errorf(domain.CodeTimeout, "the runner ran longer than ci.github.jobTimeout (%s)", s.set.timeout), nil)
	}
	if final == nil || final.ExitedAt == nil {
		p, gerr := s.cfg.Procs.Get(s.ctx, s.cfg.Owner, vm, pid)
		switch {
		case gerr == nil && p.ExitedAt != nil:
			final = &p
		case gerr != nil && !transient(gerr):
			return s.decide(d, api.CIResultFailed, prefixed("follow the runner", gerr), nil)
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

// decideProcess maps the runner's exit to the job's result.
func (s *Service) decideProcess(d *driver, p api.Process) error {
	switch {
	case p.TimedOut:
		return s.decide(d, api.CIResultTimedOut, domain.Errorf(domain.CodeTimeout, "the runner ran longer than ci.github.jobTimeout (%s)", s.set.timeout), p.ExitCode)
	case p.Status == api.ProcessExited && p.ExitCode != nil && *p.ExitCode == 0:
		return s.decide(d, api.CIResultSucceeded, nil, p.ExitCode)
	case p.Status == api.ProcessExited && p.ExitCode != nil:
		return s.decide(d, api.CIResultFailed, domain.Errorf(domain.CodeInternal, "the runner exited with status %d (its output: `davit history %s %s`)", *p.ExitCode, p.VMID, p.ID), p.ExitCode)
	case p.Signal != "":
		return s.decide(d, api.CIResultFailed, domain.Errorf(domain.CodeInternal, "the runner was killed by %s", p.Signal), p.ExitCode)
	}
	return s.decide(d, api.CIResultFailed, domain.Errorf(domain.CodeUnavailable, "the runner's outcome is unknown (process %s is %s)", p.ID, p.Status), p.ExitCode)
}

// decide records the job's result (unless one was decided first) and moves
// it to cleanup.
func (s *Service) decide(d *driver, result api.CIJobResult, cause error, exit *int) error {
	_, err := s.update(s.ctx, d.id, func(r *record) (bool, error) {
		if exit != nil && r.ExitCode == nil {
			code := *exit
			r.ExitCode = &code
		}
		if r.Result == "" {
			now := s.now()
			r.Result, r.Phase, r.CompletedAt = result, phaseCleanup, &now
			if cause != nil && r.Error == nil {
				r.Error = apiError(cause)
			}
		}
		r.Phase = phaseCleanup
		return true, nil
	})
	if err == nil {
		s.cfg.Log.Info("ci: job result", "job", d.id, "result", result, "err", cause)
	}
	return err
}

func (s *Service) cleanup(d *driver, rec *record) error {
	if rec.ProcessID != "" && rec.VMID != "" && rec.ExitCode == nil {
		// Best effort: deleting the box ends the runner anyway.
		_, _ = s.cfg.Procs.Signal(s.ctx, s.cfg.Owner, string(rec.VMID), rec.ProcessID, "KILL")
	}
	vm := rec.VMID
	if vm == "" {
		// A create that went through but was never recorded.
		if v, err := s.cfg.VMs.GetVM(s.ctx, s.cfg.Owner, vmName(rec.ID)); err == nil && v.State != domain.StateDeleted {
			vm = v.ID
		}
	}
	if vm != "" {
		if err := s.deleteVM(d, vm); err != nil {
			return err
		}
	}
	if rec.RunnerID != 0 {
		// A just-in-time runner is removed by GitHub once its job is done;
		// one that never got a job (or whose box failed) is removed here.
		err := s.cfg.App.Installation(rec.Installation).DeleteRunner(s.ctx, s.scope(rec.Repo), rec.RunnerID)
		if err != nil && transient(err) {
			return err
		}
		if err != nil && !github.IsStatus(err, http.StatusUnprocessableEntity) {
			s.cfg.Log.Warn("ci: remove the runner registration", "job", d.id, "runner", rec.Runner, "err", err)
		}
	}
	_, err := s.update(s.ctx, d.id, func(r *record) (bool, error) {
		if r.Phase == phaseDone {
			return false, nil
		}
		now := s.now()
		r.Phase, r.EndedAt = phaseDone, &now
		if r.VMID == "" {
			r.VMID = vm
		}
		if r.Result == "" {
			r.Result = api.CIResultFailed
		}
		return true, nil
	})
	if err == nil {
		s.cfg.Log.Info("ci: job done", "job", d.id, "result", rec.Result, "vm", vm)
	}
	return err
}

func (s *Service) deleteVM(d *driver, vm domain.VMID) error {
	a, err := s.cfg.VMs.Delete(s.ctx, s.request(d, http.MethodDelete, fmt.Sprintf("delete-%d", d.deletes), 0), string(vm))
	if domain.IsCode(err, domain.CodeNotFound) {
		return nil
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
		return prefixed("delete the runner box", cause)
	}
	return nil
}

// waitOp follows a lifecycle operation until it is terminal.
func (s *Service) waitOp(d *driver, id domain.OperationID) (*domain.Operation, error) {
	var after int64
	for {
		var op *domain.Operation
		var evs []domain.OperationEvent
		err := d.interruptible(s.ctx, func(ctx context.Context) (err error) {
			op, evs, err = s.cfg.VMs.Events(ctx, s.cfg.Owner, id, after, 25*time.Second)
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

// giveUp ends a step that kept failing transiently.
func (s *Service) giveUp(d *driver, rec *record, cause error) error {
	if rec.Phase == phaseCleanup {
		_, err := s.update(s.ctx, d.id, func(r *record) (bool, error) {
			now := s.now()
			r.Phase, r.EndedAt = phaseDone, &now
			if r.Error == nil {
				r.Error = apiError(prefixed("clean up", cause))
			}
			if r.Result == "" {
				r.Result = api.CIResultFailed
			}
			return true, nil
		})
		return err
	}
	return s.decide(d, api.CIResultFailed, cause, nil)
}

// waitingForRoom reports a refusal for host capacity that the job keeps
// waiting through (it never gives up while GitHub keeps the job queued).
func waitingForRoom(err error) bool { return domain.IsRoomError(err) }

// transient reports an error worth retrying: the guest, daemon or GitHub
// is busy or unreachable, or an error that is neither a typed domain error
// nor a GitHub answer.
func transient(err error) bool {
	var ge *github.Error
	if errors.As(err, &ge) {
		return ge.Status >= 500 || ge.Status == http.StatusTooManyRequests || ge.RetryAfter > 0
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		return true
	}
	switch de.Code {
	case domain.CodeUnavailable, domain.CodeOperationInProgress, domain.CodeTimeout:
		return true
	case domain.CodeResourceExhausted:
		return de.Retryable || domain.IsRoomError(de)
	}
	return false
}

func prefixed(what string, err error) error {
	var ge *github.Error
	if errors.As(err, &ge) {
		code := domain.CodeUnavailable
		switch ge.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			code = domain.CodePermissionDenied
		case http.StatusNotFound:
			code = domain.CodeNotFound
		case http.StatusUnprocessableEntity:
			code = domain.CodeInvalidArgument
		}
		return domain.Errorf(code, "%s: %s", what, ge.Error())
	}
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
