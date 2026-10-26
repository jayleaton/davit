package runs_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestexec"
	"davit/internal/guestproto"
	"davit/internal/lifecycle"
	"davit/internal/runs"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
	"davit/internal/state"
	"davit/internal/storage"
)

const owner = domain.PrincipalID("local:test")

// fakeProcs is the guest process executor: commands are scripted by argv
// and outlive a daemon restart (one instance serves both "daemon runs"),
// like detached processes in a real guest.
//
//	exit N  exits with N at once
//	hang    runs until killed or its deadline (DeadlineSeconds x tick)
type fakeProcs struct {
	vms  *lifecycle.Service
	tick time.Duration

	mu      sync.Mutex
	byKey   map[string]*fakeProc
	byID    map[string]*fakeProc
	starts  int
	execErr error // the next Exec fails with it
}

type fakeProc struct {
	mu   sync.Mutex
	p    api.Process
	req  api.ExecRequest
	done chan struct{}
}

func newFakeProcs() *fakeProcs {
	return &fakeProcs{tick: 20 * time.Millisecond, byKey: map[string]*fakeProc{}, byID: map[string]*fakeProc{}}
}

func (f *fakeProcs) Exec(ctx context.Context, who domain.PrincipalID, ref, key string, req api.ExecRequest) (api.Process, error) {
	// Guest access needs a ready VM (lifecycle.GuestTarget's rule; the
	// fake runtime here has no guest transport).
	vm, err := f.vms.GetVM(ctx, who, ref)
	if err != nil {
		return api.Process{}, err
	}
	if vm.State != domain.StateReady && vm.State != domain.StateRunning {
		return api.Process{}, domain.Errorf(domain.CodeInvalidState, "vm %s is %s", vm.ID, vm.State)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.execErr; err != nil {
		f.execErr = nil
		return api.Process{}, err
	}
	if p := f.byKey[ref+"|"+key]; p != nil {
		return p.snapshot(), nil
	}
	f.starts++
	p := &fakeProc{req: req, done: make(chan struct{}), p: api.Process{ID: "proc_" + strconv.Itoa(f.starts), VMID: vm.ID,
		Status: api.ProcessRunning, Argv: req.Argv, Detached: req.Detached, CreatedAt: time.Now()}}
	f.byKey[ref+"|"+key], f.byID[p.p.ID] = p, p
	switch req.Argv[0] {
	case "exit":
		n, _ := strconv.Atoi(req.Argv[1])
		p.end(api.ProcessExited, &n, "", false)
	case "hang":
		if req.DeadlineSeconds > 0 {
			time.AfterFunc(time.Duration(req.DeadlineSeconds)*f.tick, func() { p.end(api.ProcessKilled, nil, "KILL", true) })
		}
	}
	return p.snapshot(), nil
}

func (p *fakeProc) end(st api.ProcessStatus, code *int, sig string, timedOut bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.p.ExitedAt != nil {
		return false
	}
	now := time.Now()
	p.p.Status, p.p.ExitCode, p.p.Signal, p.p.TimedOut, p.p.ExitedAt = st, code, sig, timedOut, &now
	close(p.done)
	return true
}

func (p *fakeProc) snapshot() api.Process {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.p
}

func (f *fakeProcs) proc(id string) (*fakeProc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.byID[id]; p != nil {
		return p, nil
	}
	return nil, domain.Errorf(domain.CodeNotFound, "process %s not found", id)
}

func (f *fakeProcs) Get(_ context.Context, _ domain.PrincipalID, _, id string) (api.Process, error) {
	p, err := f.proc(id)
	if err != nil {
		return api.Process{}, err
	}
	return p.snapshot(), nil
}

func (f *fakeProcs) Signal(_ context.Context, _ domain.PrincipalID, _, id, sig string) (api.Process, error) {
	p, err := f.proc(id)
	if err != nil {
		return api.Process{}, err
	}
	if !p.end(api.ProcessKilled, nil, sig, false) {
		return api.Process{}, domain.Errorf(domain.CodeInvalidState, "process %s exited", id)
	}
	return p.snapshot(), nil
}

func (f *fakeProcs) Attach(ctx context.Context, _ domain.PrincipalID, _, id string, _ int64,
	send func(api.ProcessStreamMessage) error, _ func(guestexec.Stream)) error {
	p, err := f.proc(id)
	if err != nil {
		return err
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		return nil
	}
	snap := p.snapshot()
	return send(api.ProcessStreamMessage{Type: api.StreamExit, Process: &snap})
}

func (f *fakeProcs) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

type env struct {
	t     *testing.T
	grace time.Duration // runs.Config.OutputGrace
	dir   string
	db    *state.DB
	vms   *lifecycle.Service
	svc   *runs.Service
	procs *fakeProcs
	// prep, when set, prepares pinned workspaces (lifecycle.Config.Workspace).
	prep lifecycle.WorkspacePreparer
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, grace: -1, dir: t.TempDir(), procs: newFakeProcs()}
	e.start()
	t.Cleanup(e.stop)
	return e
}

// start models a daemon start on the same data dir.
func (e *env) start() {
	t := e.t
	db, err := state.Open(context.Background(), filepath.Join(e.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := fake.New(fake.Options{Dir: filepath.Join(e.dir, "runtime"), BootDelay: 10 * time.Millisecond,
		GuestTransport: func(context.Context, runtime.Handle) (runtime.Dialer, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.Open(filepath.Join(e.dir, "storage"), storage.ModeCopy)
	if err != nil {
		t.Fatal(err)
	}
	img, err := lifecycle.DevImage(filepath.Join(e.dir, "images"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vms, err := lifecycle.New(lifecycle.Config{DB: db, Runtime: rt, Storage: st, Images: lifecycle.NewStaticCatalog(img),
		Log: log, Prober: lifecycle.RuntimeProber{Poll: time.Millisecond}, Workspace: e.prep})
	if err != nil {
		t.Fatal(err)
	}
	if err := vms.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.procs.vms = vms
	svc, err := runs.New(runs.Config{DB: db, VMs: vms, Procs: e.procs, Retry: 10 * time.Millisecond, GiveUp: 2 * time.Second,
		OutputGrace: e.grace, HoldCheck: 20 * time.Millisecond, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.db, e.vms, e.svc = db, vms, svc
}

// stop models a daemon exit: runs stop at a safe point and stay journaled.
func (e *env) stop() {
	if e.svc != nil {
		e.svc.Close()
		e.vms.Close()
		e.db.Close()
		e.svc = nil
	}
}

func (e *env) create(key string, req api.RunRequest) api.Run {
	e.t.Helper()
	r, err := e.svc.CreateRun(context.Background(), owner, key, req)
	if err != nil {
		e.t.Fatalf("createRun: %v", err)
	}
	return r
}

// until polls the run until ok, or fails the test.
func (e *env) until(id string, what string, ok func(api.Run) bool) api.Run {
	e.t.Helper()
	var r api.Run
	var err error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if r, err = e.svc.GetRun(context.Background(), owner, id); err == nil && ok(r) {
			return r
		}
	}
	e.t.Fatalf("run %s never %s: %+v %v", id, what, r, err)
	return r
}

func (e *env) ended(id string) api.Run {
	e.t.Helper()
	return e.until(id, "ended", func(r api.Run) bool { return r.EndedAt != nil })
}

// vmCount counts owner's VMs that are not deleted, and every VM ever made.
func (e *env) vmCount() (live, ever int) {
	e.t.Helper()
	all, _, err := e.vms.ListVMs(context.Background(), owner, nil, true, lifecycle.Page{Limit: 200})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, vm := range all {
		if vm.State != domain.StateDeleted {
			live++
		}
	}
	return live, len(all)
}

func (e *env) noVMs() {
	e.t.Helper()
	if live, _ := e.vmCount(); live != 0 {
		e.t.Fatalf("%d VMs left after the run", live)
	}
}

func cmd(argv ...string) api.RunRequest { return api.RunRequest{Argv: argv} }

func TestRunOutcomesCleanUp(t *testing.T) {
	e := newEnv(t)

	r := e.ended(e.create("ok", cmd("exit", "0")).ID)
	if r.State != api.RunSucceeded || r.ExitCode == nil || *r.ExitCode != 0 || r.Error != nil || r.VMID == "" || r.ProcessID == "" {
		t.Fatalf("success: %+v", r)
	}
	e.noVMs()

	r = e.ended(e.create("exit3", cmd("exit", "3")).ID)
	if r.State != api.RunFailed || r.ExitCode == nil || *r.ExitCode != 3 || r.Error != nil {
		t.Fatalf("non-zero exit: %+v", r)
	}
	e.noVMs()

	req := cmd("hang")
	req.TimeoutSeconds = 1
	r = e.ended(e.create("timeout", req).ID)
	if r.State != api.RunFailed || r.Error == nil || r.Error.Code != domain.CodeTimeout {
		t.Fatalf("timeout: %+v %+v", r, r.Error)
	}
	e.noVMs()

	// Setup failures: the VM cannot be created, or the command cannot start.
	req = cmd("exit", "0")
	req.Image = "no-such-image"
	r = e.ended(e.create("bad-image", req).ID)
	if r.State != api.RunFailed || r.Error == nil || r.Error.Code != domain.CodeNotFound || r.ProcessID != "" {
		t.Fatalf("create failure: %+v %+v", r, r.Error)
	}
	e.noVMs()
	e.procs.mu.Lock()
	e.procs.execErr = domain.Errorf(domain.CodeInvalidArgument, "guest: no such cwd")
	e.procs.mu.Unlock()
	r = e.ended(e.create("bad-exec", cmd("exit", "0")).ID)
	if r.State != api.RunFailed || r.Error == nil || r.Error.Code != domain.CodeInvalidArgument || r.VMID == "" {
		t.Fatalf("exec failure: %+v %+v", r, r.Error)
	}
	e.noVMs()

	// keep leaves the VM ready.
	req = cmd("exit", "3")
	req.Keep = true
	r = e.ended(e.create("keep", req).ID)
	if r.State != api.RunFailed || !r.Keep || *r.ExitCode != 3 {
		t.Fatalf("keep: %+v", r)
	}
	vm, err := e.vms.GetVM(context.Background(), owner, string(r.VMID))
	if err != nil || vm.State != domain.StateReady {
		t.Fatalf("kept VM: %+v %v", vm, err)
	}
}

func TestRunHoldStartAndCancel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	req := cmd("exit", "0")
	req.Hold = true
	r := e.create("held", req)
	r = e.until(r.ID, "waiting", func(r api.Run) bool { return r.State == api.RunWaiting })
	if e.procs.count() != 0 {
		t.Fatal("a held run started its command")
	}
	if r, err := e.svc.StartRun(ctx, owner, r.ID); err != nil || r.State != api.RunRunning {
		t.Fatalf("startRun: %+v %v", r, err)
	}
	if _, err := e.svc.StartRun(ctx, owner, r.ID); !domain.IsCode(err, domain.CodeInvalidState) {
		t.Fatalf("second startRun: %v", err)
	}
	if r = e.ended(r.ID); r.State != api.RunSucceeded {
		t.Fatalf("held run: %+v", r)
	}
	e.noVMs()
	// Cancelling a finished run changes nothing.
	if c, err := e.svc.CancelRun(ctx, owner, r.ID); err != nil || c.State != api.RunSucceeded {
		t.Fatalf("cancel after end: %+v %v", c, err)
	}

	// Cancel while waiting: the command never runs.
	r = e.until(e.create("held-cancel", req).ID, "waiting", func(r api.Run) bool { return r.State == api.RunWaiting })
	for range 2 {
		if _, err := e.svc.CancelRun(ctx, owner, r.ID); err != nil {
			t.Fatalf("cancelRun: %v", err)
		}
	}
	if r = e.ended(r.ID); r.State != api.RunCancelled || e.procs.count() != 1 {
		t.Fatalf("cancelled while waiting: %+v (%d commands)", r, e.procs.count())
	}
	if _, err := e.svc.StartRun(ctx, owner, r.ID); !domain.IsCode(err, domain.CodeInvalidState) {
		t.Fatalf("startRun after cancel: %v", err)
	}
	e.noVMs()

	// Cancel while running kills the command.
	r = e.until(e.create("running-cancel", cmd("hang")).ID, "running", func(r api.Run) bool { return r.ProcessID != "" })
	if _, err := e.svc.CancelRun(ctx, owner, r.ID); err != nil {
		t.Fatal(err)
	}
	if r = e.ended(r.ID); r.State != api.RunCancelled {
		t.Fatalf("cancelled while running: %+v", r)
	}
	p, _ := e.procs.Get(ctx, owner, "", r.ProcessID)
	if p.Status != api.ProcessKilled || p.Signal != "KILL" {
		t.Fatalf("cancelled command: %+v", p)
	}
	e.noVMs()

	if _, err := e.svc.GetRun(ctx, owner, "run_nope"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	if _, err := e.svc.GetRun(ctx, "local:other", r.ID); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("another owner's run: %v", err)
	}
}

func TestCreateRunIdempotencyAndValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	req := cmd("exit", "0")
	req.Keep = true
	r1 := e.create("k1", req)
	r2 := e.create("k1", req)
	if r1.ID != r2.ID {
		t.Fatalf("replay created run %s, first was %s", r2.ID, r1.ID)
	}
	e.ended(r1.ID)
	if r3 := e.create("k1", req); r3.ID != r1.ID {
		t.Fatalf("replay after the end created %s", r3.ID)
	}
	if _, ever := e.vmCount(); ever != 1 || e.procs.count() != 1 {
		t.Fatalf("replays made %d VMs and %d commands", ever, e.procs.count())
	}
	other := cmd("exit", "1")
	if _, err := e.svc.CreateRun(ctx, owner, "k1", other); !domain.IsCode(err, domain.CodeIdempotencyConflict) {
		t.Fatalf("reused key, other body: %v", err)
	}

	env := cmd("true")
	env.Environment = &api.EnvironmentRef{Environment: "web"}
	both := cmd("true")
	both.Image, both.SnapshotID = "x", "snap_y"
	neg := cmd("true")
	neg.TimeoutSeconds = -1
	badEnv := cmd("true")
	badEnv.Env = map[string]string{"": "c"}
	for _, c := range []struct {
		name string
		key  string
		req  api.RunRequest
		code domain.Code
	}{
		{"no key", "", cmd("true"), domain.CodeInvalidArgument},
		{"no argv", "v1", api.RunRequest{}, domain.CodeInvalidArgument},
		{"image and snapshot", "v2", both, domain.CodeInvalidArgument},
		{"negative timeout", "v3", neg, domain.CodeInvalidArgument},
		{"bad env", "v4", badEnv, domain.CodeInvalidArgument},
		{"environment", "v5", env, domain.CodeNotImplemented},
	} {
		if _, err := e.svc.CreateRun(ctx, owner, c.key, c.req); !domain.IsCode(err, c.code) {
			t.Errorf("%s: %v, want %s", c.name, err, c.code)
		}
	}
	if de := domain.AsError(func() error { _, err := e.svc.CreateRun(ctx, owner, "v5", env); return err }()); de.Details["feature"] != domain.FeatureWorkspacePrep {
		t.Errorf("environment refusal names feature %v", de.Details["feature"])
	}
}

// TestRunResumesAfterRestart stops the daemon while a run waits and while
// its command runs: the restarted daemon finishes each run with the same VM
// and the same process.
func TestRunResumesAfterRestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	req := cmd("exit", "0")
	req.Hold = true
	held := e.until(e.create("held", req).ID, "waiting", func(r api.Run) bool { return r.State == api.RunWaiting })
	e.stop()
	e.start()
	if _, err := e.svc.StartRun(ctx, owner, held.ID); err != nil {
		t.Fatalf("startRun after restart: %v", err)
	}
	if r := e.ended(held.ID); r.State != api.RunSucceeded || r.VMID != held.VMID {
		t.Fatalf("resumed held run: %+v", r)
	}

	running := e.until(e.create("running", cmd("hang")).ID, "running", func(r api.Run) bool { return r.ProcessID != "" })
	e.stop()
	e.start()
	// The command finishes while the new daemon follows it.
	time.Sleep(20 * time.Millisecond)
	code := 0
	p, err := e.procs.proc(running.ProcessID)
	if err != nil || !p.end(api.ProcessExited, &code, "", false) {
		t.Fatal("command already ended")
	}
	r := e.ended(running.ID)
	if r.State != api.RunSucceeded || r.ProcessID != running.ProcessID || r.VMID != running.VMID {
		t.Fatalf("resumed running run: %+v", r)
	}
	if _, ever := e.vmCount(); ever != 2 || e.procs.count() != 2 {
		t.Fatalf("restarts made %d VMs and %d commands, want 2 and 2", ever, e.procs.count())
	}
	e.noVMs()
}

// TestOutputGrace: the VM outlives its command until the grace ends or a
// client releases it with cancelRun, which keeps the outcome.
func TestOutputGrace(t *testing.T) {
	e := newEnv(t)
	e.grace = time.Hour
	e.stop()
	e.start()
	ctx := context.Background()
	r := e.until(e.create("graced", cmd("exit", "4")).ID, "decided", func(r api.Run) bool { return r.ExitCode != nil })
	time.Sleep(20 * time.Millisecond)
	if r, _ = e.svc.GetRun(ctx, owner, r.ID); r.EndedAt != nil {
		t.Fatalf("run ended during the output grace: %+v", r)
	}
	if r.Outcome != api.RunFailed || r.DecidedAt == nil || r.State != api.RunRunning {
		t.Fatalf("outcome not visible during the grace: %+v", r)
	}
	if live, _ := e.vmCount(); live != 1 {
		t.Fatalf("%d VMs during the output grace", live)
	}
	if _, err := e.svc.CancelRun(ctx, owner, r.ID); err != nil {
		t.Fatal(err)
	}
	if r = e.ended(r.ID); r.State != api.RunFailed || *r.ExitCode != 4 {
		t.Fatalf("released run: %+v", r)
	}
	e.noVMs()
}

// TestHeldRunFailsWhenItsVMStops: a held run that nobody starts ends once
// its VM stops (auto-stop), and the VM is deleted.
func TestHeldRunFailsWhenItsVMStops(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	req := cmd("exit", "0")
	req.Hold = true
	r := e.until(e.create("held", req).ID, "waiting", func(r api.Run) bool { return r.State == api.RunWaiting })
	_, err := e.vms.Stop(ctx, lifecycle.Request{Principal: owner, IdempotencyKey: "stop", Method: "POST", Path: "/test"}, string(r.VMID), false)
	if err != nil {
		t.Fatal(err)
	}
	if r = e.ended(r.ID); r.State != api.RunFailed || r.Error == nil || r.Error.Code != domain.CodeInvalidState || e.procs.count() != 0 {
		t.Fatalf("held run after its VM stopped: %+v %+v", r, r.Error)
	}
	e.noVMs()
}

// scriptedPreparer answers WorkspacePrepare: ready, or failing at step.
type scriptedPreparer struct{ failStep string }

func (p scriptedPreparer) PrepareWorkspace(_ context.Context, _ domain.PrincipalID, _ string, req guestproto.WorkspacePrepare, _ func(guestproto.WorkspaceProgress)) (guestproto.WorkspaceResult, error) {
	if p.failStep != "" {
		code := 2
		return guestproto.WorkspaceResult{Revision: req.Revision, State: guestproto.WorkspaceFailed, FailedStep: p.failStep, ExitCode: &code, Message: "boom"}, nil
	}
	return guestproto.WorkspaceResult{Revision: req.Revision, State: guestproto.WorkspaceReady}, nil
}

// TestRunPreparesWorkspace: a run with an environment executes only after
// its workspace is ready; a failed preparation fails the run with the step
// and cleans up without running the command.
func TestRunPreparesWorkspace(t *testing.T) {
	for _, c := range []struct {
		name  string
		prep  scriptedPreparer
		state api.RunState
		runs  int
	}{
		{"ready", scriptedPreparer{}, api.RunSucceeded, 1},
		{"failed", scriptedPreparer{failStep: "deps"}, api.RunFailed, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := &env{t: t, grace: -1, dir: t.TempDir(), procs: newFakeProcs(), prep: c.prep}
			e.start()
			t.Cleanup(e.stop)
			m := domain.EnvManifest{Setup: []domain.SetupAction{{Name: "deps", Argv: []string{"make"}}}}
			if _, _, err := e.vms.CreateEnvironment(context.Background(), owner, api.CreateEnvironmentRequest{Name: "web", Manifest: m}); err != nil {
				t.Fatal(err)
			}
			req := cmd("exit", "0")
			req.Environment = &api.EnvironmentRef{Environment: "web"}
			r := e.ended(e.create("k-"+c.name, req).ID)
			if r.State != c.state || e.procs.count() != c.runs {
				t.Fatalf("run %+v, %d commands started", r, e.procs.count())
			}
			if c.state == api.RunFailed && (r.Error == nil || r.Error.Details["step"] != "deps") {
				t.Fatalf("failed preparation error: %+v", r.Error)
			}
			if live, _ := e.vmCount(); live != 0 {
				t.Fatalf("%d VMs left", live)
			}
		})
	}
}

// TestRunContract: a run's own outputGraceSeconds overrides the host's,
// a killed command reports its signal, grace bounds are checked, and
// listRuns pages the caller's runs oldest first.
func TestRunContract(t *testing.T) {
	e := newEnv(t)
	e.grace = time.Hour
	e.stop()
	e.start()
	ctx := context.Background()
	none := int64(0)
	req := cmd("exit", "0")
	req.OutputGraceSeconds = &none
	if r := e.ended(e.create("no-grace", req).ID); r.State != api.RunSucceeded || r.Outcome != api.RunSucceeded || r.DecidedAt == nil {
		t.Fatalf("a run without grace did not end at once: %+v", r)
	}

	hang := cmd("hang")
	hang.TimeoutSeconds = 1
	hang.OutputGraceSeconds = &none
	r := e.ended(e.create("killed", hang).ID)
	if r.State != api.RunFailed || r.Signal != "KILL" || r.Error == nil || r.Error.Code != domain.CodeTimeout {
		t.Fatalf("timed-out run: %+v", r)
	}

	for _, g := range []int64{-1, api.MaxRunOutputGrace + 1} {
		bad := cmd("true")
		bad.OutputGraceSeconds = &g
		if _, err := e.svc.CreateRun(ctx, owner, "bad-grace", bad); !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Fatalf("grace %d: %v", g, err)
		}
	}

	var ids []string
	for cursor := ""; ; {
		l, err := e.svc.ListRuns(ctx, owner, 1, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range l.Items {
			ids = append(ids, r.ID)
		}
		if l.NextCursor == "" {
			break
		}
		cursor = l.NextCursor
	}
	if len(ids) != 2 || ids[1] != r.ID {
		t.Fatalf("listRuns: %v", ids)
	}
	if l, _ := e.svc.ListRuns(ctx, "someone-else", 0, ""); len(l.Items) != 0 {
		t.Fatalf("another owner sees runs: %+v", l.Items)
	}
}

// TestRunEnvRedacted: a run's env values reach its command but are not
// kept in the host database once the command started: they stay only while
// the exec may still have to be (re)issued, e.g. while the run is held.
func TestRunEnvRedacted(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	stored := func(id string) map[string]string {
		t.Helper()
		var doc struct {
			Spec api.RunRequest `json:"spec"`
		}
		if err := e.db.Tx(ctx, func(tx *state.Tx) error { return tx.GetRunDoc(owner, id, &doc) }); err != nil {
			t.Fatal(err)
		}
		return doc.Spec.Env
	}
	req := cmd("exit", "0")
	req.Env = map[string]string{"API_TOKEN": "s3cret"}
	req.Hold = true
	r := e.until(e.create("env", req).ID, "waiting", func(r api.Run) bool { return r.State == api.RunWaiting })
	if v := stored(r.ID)["API_TOKEN"]; v != "s3cret" {
		t.Fatalf("held run lost its env before the exec: %q", v)
	}
	if _, err := e.svc.StartRun(ctx, owner, r.ID); err != nil {
		t.Fatal(err)
	}
	e.ended(r.ID)
	e.procs.mu.Lock()
	var got string
	for _, p := range e.procs.byID {
		got = p.req.Env["API_TOKEN"]
	}
	e.procs.mu.Unlock()
	if got != "s3cret" {
		t.Fatalf("the command got %q", got)
	}
	if env := stored(r.ID); len(env) != 1 || env["API_TOKEN"] != "" {
		t.Fatalf("stored env after the run: %v", env)
	}
	// The event stream's run triggers (G16) copy no env either.
	var evs []state.EventRow
	if err := e.db.Tx(ctx, func(tx *state.Tx) (err error) {
		evs, err = tx.ListEvents(owner, 0, 1000, state.EventFilter{Types: []string{"run."}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var mine int
	for _, ev := range evs {
		if ev.Subject == r.ID {
			mine++
			if bytes.Contains(ev.Data, []byte("s3cret")) || bytes.Contains(ev.Data, []byte("API_TOKEN")) {
				t.Errorf("event %s carries the env: %s", ev.Type, ev.Data)
			}
		}
	}
	if mine < 2 {
		t.Fatalf("%d run events for %s, want the create and its state changes", mine, r.ID)
	}
}
