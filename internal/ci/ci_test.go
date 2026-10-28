package ci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/ci/github"
	"davit/internal/ci/githubtest"
	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/guestexec"
	"davit/internal/lifecycle"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
	"davit/internal/state"
	"davit/internal/storage"
)

const owner = domain.PrincipalID("local:test")

// testClock is real time plus an offset the test moves forward: timers
// fire when real time reaches them, or at once when Advance passes them.
type testClock struct {
	mu     sync.Mutex
	skew   time.Duration
	timers []*testTimer
}

type testTimer struct {
	at   time.Time
	f    func()
	once sync.Once
	real *time.Timer
}

func (t *testTimer) fire() { t.once.Do(func() { go t.f() }) }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.skew)
}

func (c *testClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &testTimer{at: time.Now().Add(c.skew + d), f: f}
	t.real = time.AfterFunc(d, t.fire)
	c.timers = append(c.timers, t)
	return func() bool {
		stopped := false
		t.once.Do(func() { stopped = true })
		t.real.Stop()
		return stopped
	}
}

// Advance moves the clock forward by d and fires every timer now due.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.skew += d
	now := time.Now().Add(c.skew)
	var due, rest []*testTimer
	for _, t := range c.timers {
		if t.at.After(now) {
			rest = append(rest, t)
		} else {
			due = append(due, t)
		}
	}
	c.timers = rest
	c.mu.Unlock()
	for _, t := range due {
		t.real.Stop()
		t.fire()
	}
}

// fakeProcs is the guest process API: every process runs until the test
// ends it (exit), it is killed, or its deadline (DeadlineSeconds on the
// clock) passes, unless lostDeadline.
type fakeProcs struct {
	vms          *lifecycle.Service
	clock        *testClock
	lostDeadline bool

	mu     sync.Mutex
	byKey  map[string]*fakeProc
	byID   map[string]*fakeProc
	starts int
}

type fakeProc struct {
	mu   sync.Mutex
	p    api.Process
	req  api.ExecRequest
	done chan struct{}
}

func newFakeProcs() *fakeProcs {
	return &fakeProcs{clock: &testClock{}, byKey: map[string]*fakeProc{}, byID: map[string]*fakeProc{}}
}

func (f *fakeProcs) Exec(ctx context.Context, who domain.PrincipalID, ref, key string, req api.ExecRequest) (api.Process, error) {
	vm, err := f.vms.GetVM(ctx, who, ref)
	if err != nil {
		return api.Process{}, err
	}
	if vm.State != domain.StateReady && vm.State != domain.StateRunning {
		return api.Process{}, domain.Errorf(domain.CodeInvalidState, "vm %s is %s", vm.ID, vm.State)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.byKey[string(vm.ID)+"|"+key]; p != nil {
		return p.snapshot(), nil
	}
	f.starts++
	p := &fakeProc{req: req, done: make(chan struct{}), p: api.Process{ID: "proc_" + strconv.Itoa(f.starts), VMID: vm.ID,
		Status: api.ProcessRunning, Argv: req.Argv, Detached: req.Detached, CreatedAt: f.clock.Now()}}
	f.byKey[string(vm.ID)+"|"+key], f.byID[p.p.ID] = p, p
	if req.DeadlineSeconds > 0 && !f.lostDeadline {
		f.clock.AfterFunc(time.Duration(req.DeadlineSeconds)*time.Second, func() { p.end(api.ProcessKilled, nil, "KILL", true) })
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

// onVM returns the process started in vm (nil when none).
func (f *fakeProcs) onVM(vm domain.VMID) *fakeProc {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.byID {
		if p.p.VMID == vm {
			return p
		}
	}
	return nil
}

func (f *fakeProcs) Get(_ context.Context, _ domain.PrincipalID, _, id string) (api.Process, error) {
	p, err := f.proc(id)
	if err != nil {
		return api.Process{}, err
	}
	return p.snapshot(), nil
}

func (f *fakeProcs) List(_ context.Context, _ domain.PrincipalID, ref string, _ int, _ string) ([]api.Process, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.Process
	for _, p := range f.byID {
		if string(p.p.VMID) == ref {
			out = append(out, p.snapshot())
		}
	}
	return out, "", nil
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

// env is a controller over a real lifecycle (fake runtime) and a fake
// GitHub.
type env struct {
	t     *testing.T
	dir   string
	gh    *githubtest.Server
	cfg   config.GitHubCI
	db    *state.DB
	vms   *lifecycle.Service
	procs *fakeProcs
	svc   *Service
}

const repo = "acme/app"

func newEnv(t *testing.T, mod func(*config.GitHubCI)) *env {
	gh := githubtest.New(t)
	gh.AddRepo(repo, false)
	gh.AddRepo("acme/oss", true)
	gh.AddRepo("acme/other", false)
	e := &env{t: t, dir: t.TempDir(), gh: gh, procs: newFakeProcs()}
	shapes := []config.CIShape{{Label: "small", CPUs: 1, MemoryMiB: 256, DiskGiB: 1}, {Label: "davit-2c-1g", CPUs: 2, MemoryMiB: 1024, DiskGiB: 2}}
	e.cfg = config.GitHubCI{Enabled: true, AppID: gh.AppID, APIURL: gh.URL, PollInterval: -1, Shapes: shapes, DefaultShape: "small",
		RunnerNamePrefix: "davit-test", Installations: []config.GitHubInstallation{{ID: 1, Repos: []config.GitHubRepo{{Name: repo}, {Name: "acme/oss"}}}}}
	if mod != nil {
		mod(&e.cfg)
	}
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
	rt, err := fake.New(fake.Options{Dir: filepath.Join(e.dir, "runtime"), BootDelay: 5 * time.Millisecond,
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
	img.Ref = config.DefaultCIImage
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vms, err := lifecycle.New(lifecycle.Config{DB: db, Runtime: rt, Storage: st, Images: lifecycle.NewStaticCatalog(img),
		Log: log, Prober: lifecycle.RuntimeProber{Poll: time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	if err := vms.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.procs.vms = vms
	svc, err := New(Config{GitHub: e.cfg, App: e.gh.App(), DB: db, VMs: vms, Procs: e.procs, Owner: owner, Hostname: "test",
		Retry: 10 * time.Millisecond, ReconcileEvery: time.Hour, Clock: e.procs.clock, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.db, e.vms, e.svc = db, vms, svc
}

// stop models a daemon exit: jobs stop at a safe point and stay journaled.
func (e *env) stop() {
	if e.svc != nil {
		e.svc.Close()
		e.vms.Close()
		e.db.Close()
		e.svc = nil
	}
}

func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	st, _ := e.svc.Status(context.Background(), 50)
	b, _ := json.MarshalIndent(st, "", "  ")
	e.t.Fatalf("timed out waiting for %s; status:\n%s\nrunners %+v", what, b, e.gh.Runners())
}

// observe hands the intake a job status, as a webhook would.
func (e *env) observe(j github.Job) {
	e.t.Helper()
	if err := e.svc.observe(context.Background(), 1, e.gh.RepoOf(j), nil, j); err != nil {
		e.t.Fatal(err)
	}
}

// queue queues a job on GitHub and tells the controller.
func (e *env) queue(r string, labels ...string) github.Job {
	e.t.Helper()
	j := e.gh.Queue(r, labels, githubtest.QueueOptions{})
	e.observe(j)
	return j
}

// job is the record made for GitHub job id (which may serve another job
// by now; see handoff).
func (e *env) job(id int64) (record, bool) {
	for _, r := range e.records() {
		if r.providerJob() == providerJob(id) {
			return r, true
		}
	}
	return record{}, false
}

// serving lists the records that serve GitHub job id.
func (e *env) serving(id int64) []record {
	var out []record
	for _, r := range e.records() {
		if r.JobID == id {
			out = append(out, r)
		}
	}
	return out
}

func (e *env) records() []record {
	var recs []record
	_ = e.db.Tx(context.Background(), func(tx *state.Tx) (err error) {
		recs, err = loadRecords(tx, nil, false, 0)
		return err
	})
	return recs
}

func (e *env) waitState(id int64, st api.CIJobState) record {
	e.t.Helper()
	var r record
	e.waitFor(fmt.Sprintf("job %d %s", id, st), func() bool {
		var ok bool
		r, ok = e.job(id)
		return ok && r.State == st
	})
	return r
}

// started waits until the runner of GitHub job id runs and returns its
// record, process and the runner name from its just-in-time config.
func (e *env) started(id int64) (record, *fakeProc, string) {
	e.t.Helper()
	r := e.waitState(id, api.CIJobRunning)
	p := e.procs.onVM(r.VMID)
	if p == nil {
		e.t.Fatalf("no runner process in %s", r.VMID)
	}
	jit, err := githubtest.DecodeJIT(p.req.Env[JITEnv])
	if err != nil {
		e.t.Fatalf("runner env holds no JIT config: %v", err)
	}
	return r, p, jit.Runner
}

// work plays the runner: take job id, run it, exit with code.
func (e *env) work(id int64, runner string, p *fakeProc, code int) {
	e.t.Helper()
	j, err := e.gh.Assign(id, runner)
	if err != nil {
		e.t.Fatal(err)
	}
	e.observe(j)
	e.observe(e.gh.Complete(id, map[bool]string{true: "success", false: "failure"}[code == 0]))
	p.end(api.ProcessExited, &code, "", false)
}

func (e *env) vm(id domain.VMID) domain.VM {
	e.t.Helper()
	vm, err := e.vms.GetVM(context.Background(), owner, string(id))
	if err != nil {
		e.t.Fatal(err)
	}
	return *vm
}

func (e *env) events(prefix string) []state.EventRow {
	var evs []state.EventRow
	_ = e.db.Tx(context.Background(), func(tx *state.Tx) (err error) {
		evs, err = tx.ListEvents("", 0, 1000, state.EventFilter{Types: []string{prefix}})
		return err
	})
	return evs
}

func TestMatchLabels(t *testing.T) {
	g := config.GitHubCI{Shapes: []config.CIShape{{Label: "davit-8c-16g", CPUs: 8, MemoryMiB: 16384, DiskGiB: 64}, {Label: "big", CPUs: 16, MemoryMiB: 1, DiskGiB: 1}}}
	set := resolve(g, "h")
	for _, c := range []struct {
		labels []string
		ok     bool
		shape  string
	}{
		{[]string{"self-hosted", "davit"}, true, "davit-default"},
		{[]string{"Self-Hosted", "DAVIT", "Linux", "X64"}, true, "davit-default"},
		{[]string{"self-hosted", "davit", "davit-8c-16g"}, true, "davit-8c-16g"},
		{[]string{"self-hosted", "davit", "DAVIT-8C-16G", "davit-8c-16g"}, true, "davit-8c-16g"},
		{[]string{"self-hosted", "davit", "davit-8c-16g", "big"}, false, ""}, // two shapes
		{[]string{"davit"}, false, ""},                                       // not self-hosted
		{[]string{"self-hosted"}, false, ""},                                 // not for this host
		{[]string{"self-hosted", "davit", "gpu"}, false, ""},                 // another runner's label
		{[]string{"self-hosted", "davit", "arm64"}, false, ""},               // another architecture
		{[]string{"ubuntu-latest"}, false, ""},
		{nil, false, ""},
	} {
		sh, ok := match(c.labels, set, g)
		if ok != c.ok || sh.Label != c.shape {
			t.Errorf("match(%v) = %q, %v; want %q, %v", c.labels, sh.Label, ok, c.shape, c.ok)
		}
	}
	// A configured default shape replaces the built-in one.
	g.DefaultShape = "big"
	if sh, ok := match([]string{"self-hosted", "davit"}, resolve(g, "h"), g); !ok || sh.Label != "big" {
		t.Errorf("default shape: got %q %v", sh.Label, ok)
	}
	// Another host label.
	g.Label = "ci-farm"
	if _, ok := match([]string{"self-hosted", "davit"}, resolve(g, "h"), g); ok {
		t.Error("davit label matched a host labelled ci-farm")
	}
}

func TestPolicyRefusals(t *testing.T) {
	e := newEnv(t, func(g *config.GitHubCI) {
		g.Installations = append(g.Installations, config.GitHubInstallation{ID: 2, Repos: []config.GitHubRepo{{Name: "acme/second"}}})
	})
	e.gh.AddRepo("acme/second", false)
	refused := func(j github.Job, want string) {
		t.Helper()
		r, ok := e.job(j.ID)
		if !ok || r.State != api.CIJobRefused || r.Error == nil || r.Error.Code != domain.CodePermissionDenied || !strings.Contains(r.Error.Message, want) {
			t.Fatalf("job %d: %+v %+v, want refused with %q", j.ID, r.CIJob, r.Error, want)
		}
		if r.VMID != "" || r.EndedAt == nil {
			t.Fatalf("refused job has a box or no end: %+v", r.CIJob)
		}
	}
	// Not on the allowlist.
	refused(e.queue("acme/other", "self-hosted", "davit"), "not allowed")
	// Public without allowPublic.
	refused(e.queue("acme/oss", "self-hosted", "davit"), "is public")
	// A pull request from a fork.
	fork := e.gh.Queue(repo, []string{"self-hosted", "davit"}, githubtest.QueueOptions{Fork: "mallory/app"})
	e.observe(fork)
	refused(fork, "fork mallory/app")
	// Through another installation than the one configured for the repo.
	j := e.gh.Queue("acme/second", []string{"self-hosted", "davit"}, githubtest.QueueOptions{})
	if err := e.svc.observe(context.Background(), 1, "acme/second", nil, j); err != nil {
		t.Fatal(err)
	}
	refused(j, "installation 2, not 1")
	// Labels for another runner: not recorded at all.
	other := e.queue(repo, "self-hosted", "gpu")
	if _, ok := e.job(other.ID); ok {
		t.Fatal("a job for another runner was recorded")
	}
	// Seen again (poll after webhook): still one record, one event.
	e.observe(fork)
	if n := len(e.events(api.EventCIJobRefused)); n != 4 {
		t.Fatalf("%d refusal events, want 4", n)
	}
	if e.procs.count() != 0 || len(e.gh.Runners()) != 0 {
		t.Fatalf("refused jobs started %d runners, registered %v", e.procs.count(), e.gh.Runners())
	}
	// Allowed explicitly, both run.
	e.stop()
	e.cfg.Installations[0].Repos = []config.GitHubRepo{{Name: repo, AllowForks: true}, {Name: "acme/oss", AllowPublic: true}}
	e.start()
	fork2 := e.gh.Queue(repo, []string{"self-hosted", "davit"}, githubtest.QueueOptions{Fork: "mallory/app"})
	e.observe(fork2)
	e.waitState(fork2.ID, api.CIJobRunning)
	pub := e.queue("acme/oss", "self-hosted", "davit")
	e.waitState(pub.ID, api.CIJobRunning)
}

func TestJobLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	j := e.queue(repo, "self-hosted", "davit", "davit-2c-1g")
	r, p, runner := e.started(j.ID)

	// The box: the CI image, the job's shape, ephemeral, labelled, a TTL
	// past the job timeout.
	vm := e.vm(r.VMID)
	if vm.Shape.CPUs != 2 || vm.Shape.MemoryBytes != 1024*domain.MiB || vm.Shape.DiskBytes != 2*domain.GiB {
		t.Errorf("shape %+v", vm.Shape)
	}
	if !vm.Privacy.Ephemeral || vm.Labels[LabelCI] != "github" || vm.Labels[LabelJob] != r.ID || vm.Name != vmName(r.ID) {
		t.Errorf("box %+v labels %v privacy %+v", vm.Name, vm.Labels, vm.Privacy)
	}
	if vm.ExpiresAt == nil || time.Until(*vm.ExpiresAt) < config.DefaultCIJobTimeout {
		t.Errorf("box expires %v, want after the job timeout", vm.ExpiresAt)
	}
	// The runner: run.sh as the unprivileged user, its secret only in the
	// environment, bounded by the job timeout.
	if !slices.Equal(p.req.Argv, config.DefaultCIRunnerCommand) || p.req.User != "runner" || !p.req.Detached ||
		p.req.DeadlineSeconds != int64(config.DefaultCIJobTimeout/time.Second) {
		t.Errorf("exec %+v", p.req)
	}
	regs := e.gh.Runners()
	if len(regs) != 1 || regs[0].Name != runner || runner != "davit-test-"+suffix(r.ID)+"-0" || r.Runner != runner || r.RunnerID != regs[0].ID {
		t.Fatalf("registrations %+v, runner %q, record %+v", regs, runner, r.CIJob)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), p.req.Env[JITEnv]) {
		t.Fatal("the JIT config was stored in the job record")
	}

	e.work(j.ID, runner, p, 0)
	r = e.waitState(j.ID, api.CIJobDone)
	if r.Result != api.CIResultSucceeded || r.Conclusion != "success" || r.ExitCode == nil || *r.ExitCode != 0 || r.EndedAt == nil ||
		r.BoxReadyAt == nil || r.StartedAt == nil || r.CompletedAt == nil {
		t.Fatalf("done record %+v", r.CIJob)
	}
	if st := e.vm(r.VMID).State; st != domain.StateDeleted {
		t.Fatalf("box is %s after the job", st)
	}
	if regs := e.gh.Runners(); len(regs) != 0 {
		t.Fatalf("registrations left: %+v", regs)
	}
	var states []string
	for _, ev := range e.events("ci.job.") {
		var d map[string]any
		_ = json.Unmarshal(ev.Data, &d)
		states = append(states, ev.Type+":"+fmt.Sprint(d["state"]))
		if strings.Contains(string(ev.Data), p.req.Env[JITEnv]) {
			t.Fatal("an event carries the JIT config")
		}
	}
	want := []string{"ci.job.created:queued", "ci.job.state:starting", "ci.job.state:running", "ci.job.state:stopping", "ci.job.state:done"}
	if !slices.Equal(states, want) {
		t.Fatalf("events %v, want %v", states, want)
	}
	st, err := e.svc.Status(context.Background(), 0)
	if err != nil || len(st.Queued)+len(st.Running) != 0 || len(st.Recent) != 1 || st.Recent[0].ID != r.ID || !st.Enabled {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestJobRunnerFails(t *testing.T) {
	e := newEnv(t, nil)
	j := e.queue(repo, "self-hosted", "davit")
	_, p, runner := e.started(j.ID)
	e.work(j.ID, runner, p, 2)
	r := e.waitState(j.ID, api.CIJobDone)
	if r.Result != api.CIResultFailed || r.Conclusion != "failure" || r.Error == nil || !strings.Contains(r.Error.Message, "status 2") {
		t.Fatalf("record %+v %+v", r.CIJob, r.Error)
	}
	if e.vm(r.VMID).State != domain.StateDeleted {
		t.Fatal("box survived a failed runner")
	}
}

func TestJobTimeout(t *testing.T) {
	e := newEnv(t, nil)
	j := e.queue(repo, "self-hosted", "davit")
	_, p, runner := e.started(j.ID)
	if _, err := e.gh.Assign(j.ID, runner); err != nil {
		t.Fatal(err)
	}
	// The guest kills the runner at the timeout (before the host's backstop).
	e.procs.clock.Advance(config.DefaultCIJobTimeout)
	r := e.waitState(j.ID, api.CIJobDone)
	if r.Result != api.CIResultTimedOut || r.Error == nil || r.Error.Code != domain.CodeTimeout || !p.snapshot().TimedOut {
		t.Fatalf("record %+v %+v", r.CIJob, r.Error)
	}
	if e.vm(r.VMID).State != domain.StateDeleted {
		t.Fatal("box survived the timeout")
	}

	// The host's own backstop: a runner the guest does not stop (a lost
	// deadline) has its box deleted after the timeout too.
	e.procs.lostDeadline = true
	j = e.queue(repo, "self-hosted", "davit")
	r, p, _ = e.started(j.ID)
	e.procs.clock.Advance(config.DefaultCIJobTimeout + time.Minute)
	r = e.waitState(j.ID, api.CIJobDone)
	if r.Result != api.CIResultTimedOut || p.snapshot().Signal != "KILL" || e.vm(r.VMID).State != domain.StateDeleted {
		t.Fatalf("backstop: %+v signal %q", r.CIJob, p.snapshot().Signal)
	}
}

func TestCompletedRunnerLingers(t *testing.T) {
	e := newEnv(t, nil)
	j := e.queue(repo, "self-hosted", "davit")
	_, p, runner := e.started(j.ID)
	a, err := e.gh.Assign(j.ID, runner)
	if err != nil {
		t.Fatal(err)
	}
	e.observe(a)
	e.observe(e.gh.Complete(j.ID, "success"))
	// The runner never exits; the box goes after the grace (default 2m).
	e.procs.clock.Advance(time.Minute)
	if r, _ := e.job(j.ID); r.State != api.CIJobRunning {
		t.Fatalf("box went before the grace: %+v", r.CIJob)
	}
	e.procs.clock.Advance(time.Minute)
	r := e.waitState(j.ID, api.CIJobDone)
	if r.Result != api.CIResultSucceeded || r.Conclusion != "success" || p.snapshot().Signal != "KILL" || e.vm(r.VMID).State != domain.StateDeleted {
		t.Fatalf("record %+v, runner %+v", r.CIJob, p.snapshot())
	}
}

func TestDemandGone(t *testing.T) {
	e := newEnv(t, func(g *config.GitHubCI) { g.MaxConcurrentJobs = 1 })
	first := e.queue(repo, "self-hosted", "davit")
	_, p1, runner1 := e.started(first.ID)

	// Queued behind the cap, then cancelled on GitHub: done without a box.
	waiting := e.queue(repo, "self-hosted", "davit")
	e.waitState(waiting.ID, api.CIJobQueued)
	e.observe(e.gh.Complete(waiting.ID, "cancelled"))
	r := e.waitState(waiting.ID, api.CIJobDone)
	if r.Result != api.CIResultCancelled || r.VMID != "" || r.Conclusion != "cancelled" {
		t.Fatalf("cancelled queued job %+v", r.CIJob)
	}

	// The first job goes to another runner (not ours): ours has no job,
	// so its registration and box go.
	other := e.gh.RegisterRunner(repo, "someone-else", false)
	_ = other
	taken := e.gh.Job(first.ID)
	taken.Status, taken.RunnerName = "in_progress", "someone-else"
	e.observe(taken)
	r = e.waitState(first.ID, api.CIJobDone)
	if r.Result != api.CIResultCancelled || e.vm(r.VMID).State != domain.StateDeleted || p1.snapshot().Signal != "KILL" {
		t.Fatalf("unneeded runner: %+v", r.CIJob)
	}
	for _, rn := range e.gh.Runners() {
		if rn.Name == runner1 {
			t.Fatalf("unneeded registration %s left", runner1)
		}
	}

	// A busy runner whose demand goes keeps running: GitHub refuses to
	// remove it, and it ends with its job.
	a := e.queue(repo, "self-hosted", "davit")
	_, pa, runnerA := e.started(a.ID)
	b := e.gh.Queue(repo, []string{"self-hosted", "davit"}, githubtest.QueueOptions{})
	assigned, err := e.gh.Assign(b.ID, runnerA) // GitHub gave our runner the other job
	if err != nil {
		t.Fatal(err)
	}
	gone := e.gh.Complete(a.ID, "cancelled") // and a's own job was cancelled
	e.observe(gone)
	// GitHub refuses to remove the runner (busy): it is left running.
	e.waitFor("runner found busy", func() bool { r, _ := e.job(a.ID); return r.Busy })
	if r, _ := e.job(a.ID); r.State != api.CIJobRunning || !r.Busy {
		t.Fatalf("busy runner was stopped: %+v", r.CIJob)
	}
	e.observe(assigned)
	e.observe(e.gh.Complete(b.ID, "success"))
	code := 0
	pa.end(api.ProcessExited, &code, "", false)
	r = e.waitState(a.ID, api.CIJobDone)
	if r.Result != api.CIResultSucceeded || r.AssignedJobID != b.ID || r.Conclusion != "success" {
		t.Fatalf("busy runner record %+v", r.CIJob)
	}
}

func TestRefusedAssignment(t *testing.T) {
	e := newEnv(t, nil)
	j := e.queue(repo, "self-hosted", "davit")
	_, p, runner := e.started(j.ID)
	// GitHub gives our runner a fork's job (same labels) that the policy
	// refused at intake: the box is deleted at once.
	fork := e.gh.Queue(repo, []string{"self-hosted", "davit"}, githubtest.QueueOptions{Fork: "mallory/app"})
	e.observe(fork)
	a, err := e.gh.Assign(fork.ID, runner)
	if err != nil {
		t.Fatal(err)
	}
	e.observe(a)
	r := e.waitState(j.ID, api.CIJobDone)
	if r.Result != api.CIResultRefused || r.AssignedJobID != fork.ID || r.Error == nil || !strings.Contains(r.Error.Message, "fork") {
		t.Fatalf("record %+v %+v", r.CIJob, r.Error)
	}
	if p.snapshot().Signal != "KILL" || e.vm(r.VMID).State != domain.StateDeleted {
		t.Fatal("the refused job's runner kept running")
	}
	// The job that runner was made for gets a new one.
	e.waitFor("a new runner for the job", func() bool {
		for _, n := range e.serving(j.ID) {
			if n.ID != r.ID && n.State == api.CIJobRunning {
				return true
			}
		}
		return false
	})
}

// TestHandoff: GitHub gives a job to another job's runner. The idle
// runner made for the taken job serves the other one when it can (same
// repository and shape); otherwise a new runner is started for it. No job
// is left without a runner, and no extra box is made when one can serve.
func TestHandoff(t *testing.T) {
	e := newEnv(t, nil)
	a := e.queue(repo, "self-hosted", "davit")
	b := e.queue(repo, "self-hosted", "davit")
	ra, pa, runnerA := e.started(a.ID)
	rb, pb, runnerB := e.started(b.ID)
	taken, err := e.gh.Assign(b.ID, runnerA)
	if err != nil {
		t.Fatal(err)
	}
	e.observe(taken)
	e.waitFor("handoff", func() bool { r, _ := e.job(b.ID); return r.JobID == a.ID })
	if r, _ := e.job(a.ID); r.JobID != b.ID || !r.Busy || r.AssignedJobID != b.ID {
		t.Fatalf("taker %+v", r.CIJob)
	}
	if r, _ := e.job(b.ID); r.DemandGone || r.Busy || r.VMID != rb.VMID {
		t.Fatalf("idle runner did not take over job a: %+v", r)
	}
	if n := len(e.records()); n != 2 {
		t.Fatalf("%d records, want 2: no new runner when one can serve", n)
	}
	// GitHub gives a to the runner that took it over.
	took, err := e.gh.Assign(a.ID, runnerB)
	if err != nil {
		t.Fatal(err)
	}
	e.observe(took)
	e.observe(e.gh.Complete(a.ID, "success"))
	e.observe(e.gh.Complete(b.ID, "failure"))
	code := 0
	pa.end(api.ProcessExited, &code, "", false)
	pb.end(api.ProcessExited, &code, "", false)
	da, db := e.waitState(a.ID, api.CIJobDone), e.waitState(b.ID, api.CIJobDone)
	if da.VMID != ra.VMID || da.JobID != b.ID || da.Conclusion != "failure" || db.JobID != a.ID || db.Conclusion != "success" ||
		da.Result != api.CIResultSucceeded || db.Result != api.CIResultSucceeded {
		t.Fatalf("records %+v / %+v", da.CIJob, db.CIJob)
	}

	// A big runner takes a small job: the small job's runner cannot run
	// the big one, so a new big runner is started and the small one goes.
	small := e.queue(repo, "self-hosted", "davit")
	big := e.queue(repo, "self-hosted", "davit", "davit-2c-1g")
	rs, ps, _ := e.started(small.ID)
	_, pbig, runnerBig := e.started(big.ID)
	got, err := e.gh.Assign(small.ID, runnerBig)
	if err != nil {
		t.Fatal(err)
	}
	e.observe(got)
	if r := e.waitState(small.ID, api.CIJobDone); r.Result != api.CIResultCancelled || ps.snapshot().Signal != "KILL" || e.vm(rs.VMID).State != domain.StateDeleted {
		t.Fatalf("small runner %+v", r.CIJob)
	}
	var again record
	e.waitFor("a new big runner", func() bool {
		for _, r := range e.serving(big.ID) {
			if r.State == api.CIJobRunning && r.Key != providerJob(big.ID) {
				again = r
				return true
			}
		}
		return false
	})
	if again.Shape != "davit-2c-1g" || again.Spec.CPUs != 2 {
		t.Fatalf("new runner %+v", again)
	}
	// A queued webhook for big seen again is not a third record.
	e.observe(e.gh.Job(big.ID))
	if n := len(e.serving(big.ID)); n != 1 {
		t.Fatalf("%d records serve big", n)
	}
	e.observe(e.gh.Complete(small.ID, "success"))
	pbig.end(api.ProcessExited, &code, "", false)
}

func TestConcurrencyCap(t *testing.T) {
	e := newEnv(t, func(g *config.GitHubCI) { g.MaxConcurrentJobs = 2 })
	var jobs []github.Job
	for i := range 4 {
		jobs = append(jobs, e.gh.Queue(repo, []string{"self-hosted", "davit"}, githubtest.QueueOptions{Name: fmt.Sprintf("job-%d", i)}))
		e.observe(jobs[i])
	}
	_, p0, r0 := e.started(jobs[0].ID)
	_, p1, r1 := e.started(jobs[1].ID)
	e.waitFor("two jobs waiting for a slot", func() bool {
		e.svc.mu.Lock()
		defer e.svc.mu.Unlock()
		return len(e.svc.queue) == 2
	})
	st, _ := e.svc.Status(context.Background(), 0)
	if len(st.Running) != 2 || len(st.Queued) != 2 || st.Queued[0].JobID != jobs[2].ID || e.procs.count() != 2 {
		t.Fatalf("over the cap: %d running, %d queued, %d runners", len(st.Running), len(st.Queued), e.procs.count())
	}
	// One ends: the oldest queued job (FIFO) starts, the other still waits.
	e.work(jobs[1].ID, r1, p1, 0)
	_, p2, r2 := e.started(jobs[2].ID)
	if r, _ := e.job(jobs[3].ID); r.State != api.CIJobQueued {
		t.Fatalf("job 3 is %s, want queued behind job 2", r.State)
	}
	e.work(jobs[0].ID, r0, p0, 0)
	_, p3, r3 := e.started(jobs[3].ID)
	e.work(jobs[2].ID, r2, p2, 0)
	e.work(jobs[3].ID, r3, p3, 0)
	for _, j := range jobs {
		if r := e.waitState(j.ID, api.CIJobDone); r.Result != api.CIResultSucceeded || e.vm(r.VMID).State != domain.StateDeleted {
			t.Fatalf("job %d: %+v", j.ID, r.CIJob)
		}
	}
}

func TestRestartReconcile(t *testing.T) {
	e := newEnv(t, nil)
	j := e.queue(repo, "self-hosted", "davit")
	_, p, runner := e.started(j.ID)
	e.stop()

	// While the daemon is down: an orphan box (a record lost), an idle
	// registration no job owns and a busy runner of this host, plus
	// another host's idle runner.
	img := config.DefaultCIImage
	e.start()
	orphan, err := e.vms.Create(context.Background(), lifecycle.Request{Principal: owner, IdempotencyKey: "orphan", Method: "POST", Path: "/vms"},
		api.CreateVMRequest{Name: "ci-orphan", Image: img, Labels: map[string]string{LabelCI: "github", LabelJob: "ci_gone"}})
	if err != nil {
		t.Fatal(err)
	}
	idle := e.gh.RegisterRunner(repo, "davit-test-aaaaaaaaaaaaaaaa-0", false)
	busy := e.gh.RegisterRunner(repo, "davit-test-bbbbbbbbbbbbbbbb-0", true)
	foreign := e.gh.RegisterRunner(repo, "elsewhere-cccc-0", false)
	e.stop()
	e.start()

	// The running job resumes following its runner.
	e.waitFor("orphan box deleted", func() bool {
		v, err := e.vms.GetVM(context.Background(), owner, string(orphan.Operation.VMID))
		return err == nil && (v.State == domain.StateDeleted || v.State == domain.StateDeleting)
	})
	names := map[string]bool{}
	e.waitFor("idle registrations removed", func() bool {
		names = map[string]bool{}
		for _, rn := range e.gh.Runners() {
			names[rn.Name] = true
		}
		return !names[idle.Name]
	})
	if !names[busy.Name] || !names[foreign.Name] || !names[runner] {
		t.Fatalf("reconcile removed too much: %v", names)
	}
	if got, _ := e.job(j.ID); got.State != api.CIJobRunning || e.vm(got.VMID).State == domain.StateDeleted {
		t.Fatalf("the live job's box was touched: %+v", got.CIJob)
	}
	e.work(j.ID, runner, p, 0)
	if r := e.waitState(j.ID, api.CIJobDone); r.Result != api.CIResultSucceeded || e.vm(r.VMID).State != domain.StateDeleted {
		t.Fatalf("after restart: %+v", r.CIJob)
	}
}

func TestRestartBetweenRegisterAndExec(t *testing.T) {
	e := newEnv(t, nil)
	j := e.queue(repo, "self-hosted", "davit")
	r := e.waitState(j.ID, api.CIJobRunning)
	e.stop()
	// Rewind the record to "registered, runner not started": the restarted
	// daemon has lost the runner secret.
	p := e.procs.onVM(r.VMID)
	e.procs.mu.Lock()
	delete(e.procs.byID, p.p.ID)
	for k, v := range e.procs.byKey {
		if v == p {
			delete(e.procs.byKey, k)
		}
	}
	e.procs.mu.Unlock()
	db, err := state.Open(context.Background(), filepath.Join(e.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Tx(context.Background(), func(tx *state.Tx) error {
		r.Phase, r.State, r.ProcessID, r.StartedAt = phaseExec, api.CIJobStarting, "", nil
		return tx.PutCIJob(r.ID, r.providerJob(), "starting", r.QueuedAt.UnixNano(), &r)
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e.start()
	_, p2, runner := e.started(j.ID)
	regs := e.gh.Runners()
	if runner != e.svc.runnerName(r.ID, 1) || len(regs) != 1 || regs[0].Name != runner {
		t.Fatalf("runner %q, registrations %+v", runner, regs)
	}
	e.work(j.ID, runner, p2, 0)
	e.waitState(j.ID, api.CIJobDone)
}

func TestTransientGitHubErrors(t *testing.T) {
	e := newEnv(t, nil)
	var mu sync.Mutex
	failures := 3
	e.gh.FailWith(func(r *http.Request) int {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/generate-jitconfig") && failures > 0 {
			failures--
			return http.StatusBadGateway
		}
		return 0
	})
	j := e.queue(repo, "self-hosted", "davit")
	_, p, runner := e.started(j.ID)
	e.work(j.ID, runner, p, 0)
	e.waitState(j.ID, api.CIJobDone)

	// A permanent refusal (the App may not register runners) fails the job
	// and removes its box.
	e.gh.FailWith(func(r *http.Request) int {
		if strings.HasSuffix(r.URL.Path, "/generate-jitconfig") {
			return http.StatusForbidden
		}
		return 0
	})
	j = e.queue(repo, "self-hosted", "davit")
	r := e.waitState(j.ID, api.CIJobDone)
	if r.Result != api.CIResultFailed || r.Error == nil || r.Error.Code != domain.CodePermissionDenied || e.vm(r.VMID).State != domain.StateDeleted {
		t.Fatalf("record %+v %+v", r.CIJob, r.Error)
	}
}

func TestWebhook(t *testing.T) {
	secret := []byte("s3cret-webhook")
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	if err := os.WriteFile(file, append(secret, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(g *config.GitHubCI) { g.Webhook = config.GitHubWebhook{Listen: "127.0.0.1:0", SecretFile: file} })
	url := "http://" + e.svc.WebhookAddr() + "/"
	j := e.gh.Queue(repo, []string{"self-hosted", "davit"}, githubtest.QueueOptions{})
	body, _ := json.Marshal(map[string]any{"action": "queued", "workflow_job": j, "repository": map[string]any{"full_name": repo, "private": true}, "installation": map[string]any{"id": 1}})

	for _, c := range []struct {
		name   string
		secret []byte
		event  string
		want   int
	}{
		{"unsigned", nil, "workflow_job", http.StatusUnauthorized},
		{"wrong secret", []byte("nope"), "workflow_job", http.StatusUnauthorized},
		{"ping", secret, "ping", http.StatusNoContent},
		{"other event", secret, "push", http.StatusAccepted},
	} {
		if st, err := githubtest.Post(url, c.secret, c.event, body); err != nil || st != c.want {
			t.Errorf("%s: %d %v, want %d", c.name, st, err, c.want)
		}
	}
	if _, ok := e.job(j.ID); ok {
		t.Fatal("a refused delivery queued a job")
	}
	if st, err := e.gh.Deliver(url, secret, "queued", j); err != nil || st != http.StatusAccepted {
		t.Fatalf("delivery: %d %v", st, err)
	}
	_, p, runner := e.started(j.ID)
	a, _ := e.gh.Assign(j.ID, runner)
	if st, _ := e.gh.Deliver(url, secret, "in_progress", a); st != http.StatusAccepted {
		t.Fatal(st)
	}
	e.waitFor("busy", func() bool { r, _ := e.job(j.ID); return r.Busy })
	if st, _ := e.gh.Deliver(url, secret, "completed", e.gh.Complete(j.ID, "success")); st != http.StatusAccepted {
		t.Fatal(st)
	}
	e.waitFor("job done on GitHub", func() bool { r, _ := e.job(j.ID); return r.JobDoneAt != nil })
	code := 0
	p.end(api.ProcessExited, &code, "", false)
	if r := e.waitState(j.ID, api.CIJobDone); r.Conclusion != "success" {
		t.Fatalf("record %+v", r.CIJob)
	}

	// A secret file others can read is refused at start.
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	e.stop()
	svc, err := New(Config{GitHub: e.cfg, App: e.gh.App(), DB: e.db, VMs: e.vms, Procs: e.procs, Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "readable by others") {
		svc.Close()
		t.Fatalf("start with a world-readable secret: %v", err)
	}
}

func TestPolling(t *testing.T) {
	e := newEnv(t, nil)
	j := e.gh.Queue(repo, []string{"self-hosted", "davit"}, githubtest.QueueOptions{})
	e.gh.Queue("acme/other", []string{"self-hosted", "davit"}, githubtest.QueueOptions{}) // not polled: not allowed
	e.svc.pollOnce(context.Background())
	_, p, runner := e.started(j.ID)
	if e.gh.Calls("GET /repos/{owner}/{repo}/actions/runs") == 0 {
		t.Fatal("no runs listed")
	}
	// The runner takes it; the poll sees it in progress.
	if _, err := e.gh.Assign(j.ID, runner); err != nil {
		t.Fatal(err)
	}
	e.svc.pollOnce(context.Background())
	if r, _ := e.job(j.ID); !r.Busy {
		t.Fatalf("poll did not see the runner busy: %+v", r.CIJob)
	}
	// Completed (no longer listed): looked up by ID.
	e.gh.Complete(j.ID, "success")
	e.svc.pollOnce(context.Background())
	if r, _ := e.job(j.ID); r.JobDoneAt == nil || r.Conclusion != "success" {
		t.Fatalf("poll missed the completion: %+v", r.CIJob)
	}
	code := 0
	p.end(api.ProcessExited, &code, "", false)
	e.waitState(j.ID, api.CIJobDone)

	// A queued job cancelled before polling saw it run: its runner goes.
	e.stop()
	e.cfg.MaxConcurrentJobs = 1
	e.start()
	blocker := e.queue(repo, "self-hosted", "davit")
	e.started(blocker.ID)
	q := e.gh.Queue(repo, []string{"self-hosted", "davit"}, githubtest.QueueOptions{})
	e.svc.pollOnce(context.Background())
	e.waitState(q.ID, api.CIJobQueued)
	e.gh.Complete(q.ID, "cancelled")
	e.svc.pollOnce(context.Background())
	if r := e.waitState(q.ID, api.CIJobDone); r.Result != api.CIResultCancelled {
		t.Fatalf("record %+v", r.CIJob)
	}
}
