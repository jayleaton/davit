package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/push"
)

// run and push (G6): a one-command run in a fresh VM, and git-aware upload
// of a local tree (tracked plus untracked-not-ignored files, submodules
// included, never .git).

// DefaultPushPath is where push and run --push put a tree when no
// GUEST_PATH is given and the image's default user is unknown: a work
// directory in the default user's home (guestWorkdir).
const DefaultPushPath = "/home/davit/work"

type runOpts struct {
	keep bool
	push string // DIR[:GUEST_PATH]
}

func runFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.image, "image", "", "image ref or digest")
	fs.StringVar(&o.from, "from", "", "snapshot ID to start from")
	shapeFlags(fs, o, true)
	e := &o.exec
	fs.StringVar(&e.cwd, "cwd", "", "working directory in the guest (default with --push: the push directory)")
	fs.Var(&e.env, "env", "KEY=VALUE for the command (repeatable)")
	fs.Var(&e.env, "e", "KEY=VALUE for the command (repeatable)")
	fs.Var(&e.timeout, "timeout", "command deadline: seconds or a duration; exit 124 when it fires")
	fs.BoolVar(&o.run.keep, "keep", false, "keep the VM after the command (default: delete it)")
	fs.StringVar(&o.run.push, "push", "", "DIR[:GUEST_PATH]: push DIR's git selection before the command (default GUEST_PATH "+DefaultPushPath+")")
	fs.DurationVar(&o.waitTimeout, "wait-timeout", 0, "how long to wait for the VM, and for cleanup after the command")
	fs.StringVar(&e.key, "idempotency-key", "", "deduplicate separate invocations")
}

// envMap parses repeated KEY=VALUE flags.
func envMap(kvs multiFlag) (map[string]string, error) {
	var m map[string]string
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, usageErr("--env wants KEY=VALUE, got %q", kv)
		}
		if m == nil {
			m = map[string]string{}
		}
		m[k] = v
	}
	return m, nil
}

// pushSpec is DIR[:GUEST_PATH].
type pushSpec struct{ dir, guest string }

func parsePushSpec(spec string) (pushSpec, error) {
	ps := pushSpec{dir: spec} // no GUEST_PATH: the caller fills in the guest's work dir
	if i := strings.LastIndex(spec, ":/"); i > 0 {
		ps.dir, ps.guest = spec[:i], spec[i+1:]
	}
	if ps.dir == "" {
		return ps, usageErr("push needs a local directory: DIR[:GUEST_PATH]")
	}
	if ps.guest != "" && path.Clean(ps.guest) != ps.guest {
		return ps, usageErr("GUEST_PATH must be an absolute, clean path, got %q", ps.guest)
	}
	fi, err := os.Stat(ps.dir)
	if err != nil {
		return ps, clientErr(domain.CodeNotFound, "%v", err)
	}
	if !fi.IsDir() {
		return ps, usageErr("%s is not a directory", ps.dir)
	}
	return ps, nil
}

// selectTree is the git-aware selection of a push.
func selectTree(ctx context.Context, ps pushSpec) ([]push.Entry, error) {
	es, err := push.Git{}.Select(ctx, ps.dir)
	if err != nil {
		var de *domain.Error
		if errors.As(err, &de) {
			return nil, clientErr(de.Code, "%s", de.Message)
		}
		return nil, clientErr(domain.CodeInvalidArgument, "selecting files in %s: %v", ps.dir, err)
	}
	return es, nil
}

// pushTree tars the selection and applies it under ps.guest (merge).
func (s *session) pushTree(ctx context.Context, vm string, ps pushSpec, es []push.Entry) (api.PushResult, error) {
	pr, pw := io.Pipe()
	go func() {
		_, err := push.WriteTar(pw, ps.dir, es)
		pw.CloseWithError(err)
	}()
	defer pr.Close()
	p := &progress{s: s, start: time.Now()}
	stop := p.run()
	q := url.Values{"path": {ps.guest}, "mode": {api.PushMerge}}
	resp, err := s.cl.stream(ctx, http.MethodPut, vmPath(vm)+"/tree", q, p.reader(pr), api.ContentTypeTar)
	stop()
	if err != nil {
		return api.PushResult{}, err
	}
	defer resp.Body.Close()
	var res api.PushResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res); err != nil {
		return res, clientErr(domain.CodeInternal, "decoding response: %v", err)
	}
	return res, nil
}

func runPush(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) == 0 || len(args) > 2 {
		return usageErr("usage: %s push [ID] DIR[:GUEST_PATH]", buildinfo.Name)
	}
	id, err := s.currentID(args[:len(args)-1])
	if err != nil {
		return err
	}
	ps, err := parsePushSpec(args[len(args)-1])
	if err != nil {
		return err
	}
	if ps.guest == "" {
		ps.guest = s.vmWorkdir(ctx, id)
	}
	es, err := selectTree(ctx, ps)
	if err != nil {
		return err
	}
	res, err := s.pushTree(ctx, id, ps, es)
	if err != nil {
		return err
	}
	s.out.result("", res)
	s.out.human("pushed %d files (%s) to %s:%s", res.Files, humanBytes(res.Bytes), id, res.Path)
	return nil
}

func runRun(ctx context.Context, s *session, o *opts, args []string) error {
	e := &o.exec
	if len(args) > 0 || len(e.argv) == 0 {
		return usageErr("run needs a command after --: %s run [flags] -- COMMAND...", buildinfo.Name)
	}
	body := api.RunRequest{Image: o.image, SnapshotID: domain.SnapshotID(o.from), Type: o.typ, Shape: o.shape(),
		Argv: e.argv, Cwd: e.cwd, TimeoutSeconds: e.timeout.v, Keep: o.run.keep}
	env, err := envMap(e.env)
	if err != nil {
		return err
	}
	body.Env = env
	// The selection is made before the VM exists: a bad DIR costs nothing.
	var ps pushSpec
	var sel []push.Entry
	if o.run.push != "" {
		if ps, err = parsePushSpec(o.run.push); err != nil {
			return err
		}
		if ps.guest == "" {
			ps.guest = s.imageWorkdir(ctx, o.image)
		}
		if sel, err = selectTree(ctx, ps); err != nil {
			return err
		}
		body.Hold = true
		if body.Cwd == "" {
			body.Cwd = ps.guest
		}
	}
	key := e.key
	if key == "" {
		key = newIdempotencyKey()
	}

	// Ctrl-C cancels the run (the server kills the command and deletes the
	// VM, unless --keep); a second one stops waiting for that.
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigc)
	var run api.Run
	if err := s.cl.do(ctx, http.MethodPost, "/runs", nil, body, map[string]string{api.HeaderIdempotencyKey: key}, &run); err != nil {
		return err
	}
	s.out.emit("run", "", 0, run, nil)
	s.out.diag("run %s: creating a VM", run.ID)
	ctx, abort := context.WithCancel(ctx)
	defer abort()
	interrupted := make(chan struct{})
	go func() {
		n := 0
		for range sigc {
			if n == 0 {
				close(interrupted)
				s.out.diag("cancelling run %s (Ctrl-C again to stop waiting)", run.ID)
				go s.cancelRun(run.ID)
			} else {
				abort()
			}
			n++
		}
	}()
	budget := s.waitBudget(o)

	// Until the VM is ready: waiting (held for the push) or running.
	run, err = s.waitRun(ctx, run.ID, budget, func(r api.Run) bool {
		return r.EndedAt != nil || r.State == api.RunWaiting || r.ProcessID != ""
	})
	if err != nil {
		return s.runStopped(ctx, run, err)
	}
	if run.State == api.RunWaiting && run.EndedAt == nil {
		res, err := s.pushTree(ctx, string(run.VMID), ps, sel)
		if err != nil {
			// Nothing ran: give the VM back before reporting.
			s.cancelRun(run.ID)
			if r, werr := s.waitRun(ctx, run.ID, budget, func(r api.Run) bool { return r.EndedAt != nil }); werr == nil {
				s.out.emit("run", "", 0, r, nil)
			}
			return err
		}
		s.out.emit("push", "", 0, res, nil)
		s.out.diag("pushed %d files (%s) to %s", res.Files, humanBytes(res.Bytes), res.Path)
		err = s.cl.do(ctx, http.MethodPost, "/runs/"+url.PathEscape(run.ID)+"/start", nil, nil, nil, &run)
		if err != nil && !isCode(err, domain.CodeInvalidState) { // invalid_state: cancelled meanwhile
			return err
		}
		if run, err = s.waitRun(ctx, run.ID, budget, func(r api.Run) bool { return r.EndedAt != nil || r.ProcessID != "" }); err != nil {
			return s.runStopped(ctx, run, err)
		}
	}
	if run.EndedAt != nil && run.ProcessID == "" {
		// The command never started.
		s.out.emit("run", "", 0, run, nil)
		return s.runResult(run, nil, interrupted)
	}

	// The command is detached, so its whole output replays from the start.
	var final *api.Process
	exit, err := s.streamProcess(ctx, string(run.VMID), run.ProcessID, 0, false, nil, nil)
	switch {
	case err == nil:
		final = exit.Process
	case ctx.Err() != nil:
		return s.runStopped(ctx, run, ctx.Err())
	default:
		// The stream can end with the VM (cleanup); the run says how.
		s.out.diag("process stream: %v", err)
	}
	// The host keeps the VM a few seconds after the command so late
	// readers get all the output. Once the outcome is recorded, release it
	// (a cancel before that would replace the outcome).
	decided := func(r api.Run) bool { return r.EndedAt != nil || r.DecidedAt != nil }
	if run, err = s.waitRun(ctx, run.ID, budget, decided); err != nil {
		return s.runStopped(ctx, run, err)
	}
	if run.EndedAt == nil {
		s.cancelRun(run.ID)
	}
	if run, err = s.waitRun(ctx, run.ID, budget, func(r api.Run) bool { return r.EndedAt != nil }); err != nil {
		return s.runStopped(ctx, run, err)
	}
	return s.runResult(run, final, interrupted)
}

// runResult prints the finished run (the one terminal line) and returns
// the CLI exit status: the command's, 124 for its timeout, 130 when
// cancelled; a run that failed before its command ran is an error.
func (s *session) runResult(run api.Run, final *api.Process, interrupted chan struct{}) error {
	cancelled := run.State == api.RunCancelled
	select {
	case <-interrupted:
		cancelled = true
	default:
	}
	if run.ProcessID == "" && !cancelled && run.Error != nil {
		return &cliError{ErrorBody{Code: run.Error.Code, Message: run.Error.Message, Retryable: run.Error.Retryable,
			Details: run.Error.Details, Origin: "server", OperationID: run.Error.OperationID}}
	}
	s.out.result("", run)
	where := "deleted"
	if run.Keep && run.VMID != "" {
		where = "kept: " + string(run.VMID)
	}
	st := &exitStatus{}
	switch {
	case cancelled:
		st.code = ExitInterrupt
	case run.Error != nil && run.Error.Code == domain.CodeTimeout:
		st.code, st.msg = ExitDeadline, run.Error.Message
	case run.ExitCode != nil:
		st.code = *run.ExitCode & 0xff
	case final != nil:
		st.code = processExit(*final)
	case run.Signal != "":
		st.code = processExit(api.Process{Signal: run.Signal})
	default:
		st.code = ExitLost
	}
	if st.msg == "" && run.Error != nil {
		st.msg = run.Error.Message
	}
	s.out.diag("run %s %s (exit %d); VM %s", run.ID, run.State, st.code, where)
	if st.code == 0 {
		return nil
	}
	return st
}

// runStopped reports a wait that ended before the run did: Ctrl-C twice
// (exit 130) or the wait budget. The run itself continues on the host.
func (s *session) runStopped(ctx context.Context, run api.Run, err error) error {
	if ctx.Err() != nil {
		return &exitStatus{code: ExitInterrupt, msg: "stopped waiting; run " + run.ID + " continues on the host"}
	}
	return err
}

// waitRun polls run id until ok or the budget ends.
func (s *session) waitRun(ctx context.Context, id string, budget time.Duration, ok func(api.Run) bool) (api.Run, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	poll := 50 * time.Millisecond
	var r api.Run
	for {
		err := s.cl.get(ctx, "/runs/"+url.PathEscape(id), nil, &r)
		if err == nil && ok(r) {
			return r, nil
		}
		if err != nil && ctx.Err() == nil {
			return r, err
		}
		select {
		case <-time.After(poll):
			poll = min(poll*2, 200*time.Millisecond)
		case <-ctx.Done():
			if r.ID == "" {
				r.ID = id
			}
			e := clientErr(domain.CodeTimeout, "stopped waiting after %s; run %s continues on the host", budget, id)
			return r, e
		}
	}
}

// cancelRun asks the host to cancel run id (best effort: errors are
// reported, the wait that follows tells the outcome).
func (s *session) cancelRun(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.cl.do(ctx, http.MethodDelete, "/runs/"+url.PathEscape(id), nil, nil, nil, nil); err != nil {
		s.out.diag("cancel run %s: %v", id, err)
	}
}

func isCode(err error, code domain.Code) bool {
	var ce *cliError
	return errors.As(err, &ce) && ce.Code == code
}
