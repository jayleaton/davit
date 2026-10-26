//go:build linux

package guestagent_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/internal/guestagent"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
)

var errNotFound = &guestproto.Error{Code: guestproto.CodeNotFound}

// countLines counts the lines of a file a command appends to once per run.
func countLines(path string) int {
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), "\n")
}

// TestJobHandlesDedupAndSurviveAgentRestart: an Exec handle runs once per
// boot, however often (or concurrently) it is retried, from any session;
// ProcessLookup finds it; and a restarted agent still refuses to run it
// again and reports it lost.
func TestJobHandlesDedupAndSurviveAgentRestart(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, s := e.ready(ctx, "vm_j", 1)
	defer c.Close()
	runs := filepath.Join(t.TempDir(), "runs")
	req := guestproto.Exec{Argv: []string{"sh", "-c", `echo x >> "$0"; echo out`, runs}, Handle: "pstart_job1"}

	// Concurrent starts of one handle, on two sessions.
	c2, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_j", Generation: 1, Secrets: [][]byte{s}})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	var wg sync.WaitGroup
	got := make([]guestproto.ProcessStarted, 4)
	errs := make([]error, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl := c
			if i%2 == 1 {
				cl = c2
			}
			got[i], errs[i] = cl.Start(ctx, req)
		}()
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i].ProcessID != got[0].ProcessID || got[i].Handle != req.Handle || got[i].PID != got[0].PID {
			t.Fatalf("start %d: %+v %v (first %+v)", i, got[i], errs[i], got[0])
		}
	}
	// A following Exec of the handle replays the output and exit.
	var out strings.Builder
	res, err := c2.Exec(ctx, req, guestclient.ExecIO{Stdout: &out})
	if err != nil || res.ProcessID != got[0].ProcessID || res.ExitCode != 0 || out.String() != "out\n" {
		t.Fatalf("exec again: %+v %q %v", res, out.String(), err)
	}
	if n := countLines(runs); n != 1 {
		t.Fatalf("handle ran %d times", n)
	}
	st, err := c.Lookup(ctx, req.Handle)
	if err != nil || st.ProcessID != got[0].ProcessID || st.Handle != req.Handle || st.Status != guestproto.StatusExited {
		t.Fatalf("lookup: %+v %v", st, err)
	}
	if st, err := c.Status(ctx, got[0].ProcessID); err != nil || st.Handle != req.Handle {
		t.Fatalf("status carries handle: %+v %v", st, err)
	}
	if _, err := c.Lookup(ctx, "pstart_unknown"); !errors.Is(err, errNotFound) {
		t.Fatalf("unknown handle: %v", err)
	}
	// A start that fails releases its handle: nothing ran.
	bad := guestproto.Exec{Argv: []string{"/nonexistent/cmd"}, Handle: "pstart_bad"}
	if _, err := c.Start(ctx, bad); !errors.Is(err, errNotFound) {
		t.Fatalf("bad start: %v", err)
	}
	if _, err := c.Lookup(ctx, bad.Handle); !errors.Is(err, errNotFound) {
		t.Fatalf("failed start keeps its handle: %v", err)
	}

	// The agent process restarts (same boot, same run dir).
	c.Close()
	c2.Close()
	e.stop()
	e.start()
	c3, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_j", Generation: 1, Secrets: [][]byte{s}})
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	st, err = c3.Lookup(ctx, req.Handle)
	if err != nil || st.ProcessID != got[0].ProcessID || st.Status != guestproto.StatusLost {
		t.Fatalf("lookup after agent restart: %+v %v", st, err)
	}
	again, err := c3.Start(ctx, req)
	if err != nil || again.ProcessID != got[0].ProcessID {
		t.Fatalf("start after agent restart: %+v %v", again, err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := countLines(runs); n != 1 {
		t.Fatalf("handle ran %d times across an agent restart", n)
	}
}

// TestProcessListSample reads a fake /proc: fields, user, start time, and
// the size bound that keeps a sample inside one frame.
func TestProcessListSample(t *testing.T) {
	procRoot := t.TempDir()
	writeFile(t, filepath.Join(procRoot, "stat"), "cpu  1 2 3\nbtime 1700000000\n")
	uid := strconv.Itoa(os.Getuid())
	addProc := func(pid, ppid int, comm, cmdline string) {
		dir := filepath.Join(procRoot, strconv.Itoa(pid))
		// utime 150, stime 50 ticks; starttime 250 ticks; rss 3 pages.
		writeFile(t, filepath.Join(dir, "stat"), fmt.Sprintf("%d (%s) S %d 1 1 0 -1 0 0 0 0 0 150 50 0 0 20 0 1 0 250 1000 3 0", pid, comm, ppid))
		writeFile(t, filepath.Join(dir, "cmdline"), cmdline)
		writeFile(t, filepath.Join(dir, "status"), "Name:\tx\nUid:\t"+uid+"\t"+uid+"\t"+uid+"\t"+uid+"\n")
	}
	addProc(1, 0, "init", "/sbin/init\x00splash\x00")
	addProc(7, 1, "kworker) (odd", "") // kernel thread with a hostile comm
	big := strings.Repeat("a", 1000) + "\x00"
	for pid := 100; pid < 1100; pid++ {
		addProc(pid, 1, "big", big)
	}
	e := newEnvWith(t, func(c *guestagent.Config) { c.ProcRoot = procRoot })
	ctx := ctxT(t)
	c, _ := e.ready(ctx, "vm_ps", 1)
	defer c.Close()
	l, err := c.ProcessList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Truncated || len(l.Processes) < 3 || len(l.Processes) >= 1002 {
		t.Fatalf("size bound: truncated %v with %d entries", l.Truncated, len(l.Processes))
	}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	p1, p7 := l.Processes[0], l.Processes[1]
	page := int64(os.Getpagesize())
	want := time.Unix(1700000000, 0).Add(2500 * time.Millisecond).UnixNano()
	if p1.PID != 1 || p1.PPID != 0 || !slices.Equal(p1.Args, []string{"/sbin/init", "splash"}) || p1.State != "S" ||
		p1.CPUTimeMillis != 2000 || p1.RSSBytes != 3*page || p1.StartedAtUnixNano != want || p1.User != e.user {
		t.Fatalf("init sample: %+v", p1)
	}
	if p7.PID != 7 || !slices.Equal(p7.Args, []string{"[kworker) (odd]"}) || p7.PPID != 1 {
		t.Fatalf("kernel thread sample: %+v", p7)
	}
}

// TestProcessListMarksManagedJobs samples the real /proc: a running managed
// job carries its handle.
func TestProcessListMarksManagedJobs(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, _ := e.ready(ctx, "vm_ps2", 1)
	defer c.Close()
	p, err := c.Start(ctx, guestproto.Exec{Argv: []string{"sleep", "30"}, Handle: "pstart_sleeper"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Signal(context.Background(), p.ProcessID, "KILL") }()
	l, err := c.ProcessList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(l.Processes, func(g guestproto.GuestProcess) bool { return g.PID == p.PID })
	if i < 0 || l.Processes[i].Handle != "pstart_sleeper" || !slices.Equal(l.Processes[i].Args, []string{"sleep", "30"}) {
		t.Fatalf("managed job in sample: %d %+v", i, l.Processes)
	}
	if j := slices.IndexFunc(l.Processes, func(g guestproto.GuestProcess) bool { return g.PID == os.Getpid() }); j < 0 || l.Processes[j].Handle != "" {
		t.Fatalf("unmanaged process in sample: %d", j)
	}
}

// guestDial connects to a guest loopback port as a guest process would.
func guestDial(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	return c
}

// waitClosed asserts the agent closes c (EOF or reset) within a few
// seconds; a timeout means it is still open.
func waitClosed(t *testing.T, c net.Conn, what string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("%s still open: %v", what, err)
	}
}

// acceptT is Accept bounded by a deadline.
func acceptT(t *testing.T, l net.Listener) net.Conn {
	t.Helper()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := l.Accept(); ch <- res{c, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("accept: %v", r.err)
		}
		t.Cleanup(func() { r.c.Close() })
		_ = r.c.SetDeadline(time.Now().Add(10 * time.Second))
		return r.c
	case <-time.After(5 * time.Second):
		t.Fatal("accept timed out")
		return nil
	}
}

// TestListenReverseConnect: a guest connection to the listener reaches the
// host as a half-close-aware stream; announcements are bounded (backpressure)
// and unclaimed connections expire; Close ends everything.
func TestListenReverseConnect(t *testing.T) {
	e := newEnvWith(t, func(c *guestagent.Config) {
		c.ListenPending = 1
		c.ListenClaimWindow = 400 * time.Millisecond
	})
	ctx := ctxT(t)
	c, s := e.ready(ctx, "vm_l", 1)
	defer c.Close()
	sess := guestclient.Session{VMID: "vm_l", Generation: 1, Secrets: [][]byte{s}}
	l, err := guestclient.Listen(ctx, e.dial, sess, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Port() == 0 || l.ID() == "" {
		t.Fatalf("listener %s port %d", l.ID(), l.Port())
	}

	// Round trip with half-close in both directions.
	g := guestDial(t, l.Port())
	h := acceptT(t, l)
	if _, err := g.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = g.(*net.TCPConn).CloseWrite()
	if b, err := io.ReadAll(h); err != nil || string(b) != "ping" {
		t.Fatalf("host read %q %v", b, err)
	}
	if _, err := h.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	_ = h.(interface{ CloseWrite() error }).CloseWrite()
	if b, err := io.ReadAll(g); err != nil || string(b) != "pong" {
		t.Fatalf("guest read %q %v", b, err)
	}

	// Backpressure: with one pending slot, three guest connections are all
	// delivered, each announced only after the previous one was claimed.
	for i := range 3 {
		if _, err := guestDial(t, l.Port()).Write([]byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	for range 3 {
		hc := acceptT(t, l)
		b := make([]byte, 1)
		if _, err := io.ReadFull(hc, b); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, string(b))
	}
	slices.Sort(seen)
	if !slices.Equal(seen, []string{"a", "b", "c"}) {
		t.Fatalf("delivered %v", seen)
	}

	// An unclaimed connection is closed after the claim window.
	waitClosed(t, guestDial(t, l.Port()), "unclaimed connection")

	// An unknown (or already claimed, or expired) connection is not found.
	if _, err := guestclient.ReverseConnect(ctx, e.dial, sess, l.ID(), "conn_nope"); !errors.Is(err, errNotFound) {
		t.Fatalf("unknown conn: %v", err)
	}

	// Close: the guest port stops listening and Accept reports closed.
	l.Close()
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("accept after close: %v", err)
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Port())), time.Second); err == nil {
		c.Close()
		t.Fatal("guest port still listening after Close")
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("control session after listener close: %v", err)
	}
}

// TestListenEndsWithGeneration: a new generation's bootstrap ends the old
// generation's sessions, and with them its listeners and unclaimed
// connections.
func TestListenEndsWithGeneration(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, s := e.ready(ctx, "vm_lg", 1)
	defer c.Close()
	l, err := guestclient.Listen(ctx, e.dial, guestclient.Session{VMID: "vm_lg", Generation: 1, Secrets: [][]byte{s}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	pending := guestDial(t, l.Port())
	// Let the agent accept it (closing a listener resets connections still
	// in its backlog, which would not exercise the pending set).
	time.Sleep(200 * time.Millisecond)
	c2, _ := e.ready(ctx, "vm_lg", 2)
	defer c2.Close()
	if _, err := l.Accept(); err == nil {
		t.Fatal("accept on a listener of an ended generation")
	}
	waitClosed(t, pending, "unclaimed connection of an ended generation")
	// Listen is refused before the barrier of the new generation's session.
	s3 := secret()
	c3, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_lg", Generation: 3, Bootstrap: s3})
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	if _, err := guestclient.Listen(ctx, e.dial, guestclient.Session{VMID: "vm_lg", Generation: 3, Secrets: [][]byte{s3}}, 0); !errors.Is(err, guestproto.ErrUnexpectedType) {
		t.Fatalf("listen before barrier: %v", err)
	}
}

// closeSignal is a listener that reports its Close.
type closeSignal struct {
	net.Listener
	once   sync.Once
	closed chan struct{}
}

func (c *closeSignal) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Listener.Close()
}

// TestListenSurvivesHostHangup: the host drops its session before the agent
// writes ListenStarted. The failed write must close the session and the
// listener, not wedge the session on the listener's own lock.
func TestListenSurvivesHostHangup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var ln *closeSignal
	e := newEnvWith(t, func(c *guestagent.Config) {
		c.Listen = func(ctx context.Context, port int) (net.Listener, error) {
			close(entered)
			<-release
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			ln = &closeSignal{Listener: l, closed: make(chan struct{})}
			return ln, nil
		}
	})
	ctx := ctxT(t)
	c, s := e.ready(ctx, "vm_lh", 1)
	defer c.Close()
	lctx, cancel := context.WithCancel(ctx)
	go func() { <-entered; cancel() }() // the host gives up and hangs up
	if _, err := guestclient.Listen(lctx, e.dial, guestclient.Session{VMID: "vm_lh", Generation: 1, Secrets: [][]byte{s}}, 0); err == nil {
		t.Fatal("listen survived its cancellation")
	}
	close(release)
	for end := time.Now().Add(5 * time.Second); ln == nil && time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
	}
	if ln == nil {
		t.Fatal("agent never opened the listener")
	}
	select {
	case <-ln.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("listener of a hung-up session was never closed (session wedged)")
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

// gitRepo makes a local repository with a tagged first commit and a second
// commit on the default branch.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	writeFile(t, filepath.Join(dir, "README"), "v1\n")
	git("add", "README")
	git("commit", "-q", "-m", "one")
	git("tag", "v1")
	writeFile(t, filepath.Join(dir, "README"), "v2\n")
	git("commit", "-q", "-am", "two")
	return dir
}

// prepare runs req and returns its progress reports and result.
func prepare(t *testing.T, ctx context.Context, c *guestclient.Client, req guestproto.WorkspacePrepare) ([]guestproto.WorkspaceProgress, guestproto.WorkspaceResult) {
	t.Helper()
	var steps []guestproto.WorkspaceProgress
	res, err := c.PrepareWorkspace(ctx, req, func(p guestproto.WorkspaceProgress) { steps = append(steps, p) })
	if err != nil {
		t.Fatal(err)
	}
	return steps, res
}

func TestWorkspacePrepare(t *testing.T) {
	repo := gitRepo(t)
	e := newEnv(t)
	ctx := ctxT(t)
	c, _ := e.ready(ctx, "vm_w", 1)
	defer c.Close()
	ws := t.TempDir()
	const secretData = "s3cr3t-token-value\nline-two-secret\n"
	req := guestproto.WorkspacePrepare{
		Revision: "env_x@3",
		Repos: []guestproto.RepoCheckout{
			{URL: "file://" + repo, Ref: "v1", Path: ws + "/src/tagged"},
			{URL: "file://" + repo, Path: ws + "/src/head"},
		},
		Files: []guestproto.WorkspaceFile{{Path: ws + "/conf/secret", Mode: 0o640, Data: []byte(secretData)}},
		Env:   []string{"TOKEN=hunter2-env-value"},
		Setup: []guestproto.SetupStep{{Name: "check", Argv: []string{"sh", "-c", `cat src/tagged/README src/head/README > out; test "$TOKEN" = hunter2-env-value`}, Cwd: ws}},
	}
	steps, res := prepare(t, ctx, c, req)
	if res.State != guestproto.WorkspaceReady || res.Revision != "env_x@3" || res.FailedStep != "" {
		t.Fatalf("result %+v", res)
	}
	var trace []string
	for _, p := range steps {
		trace = append(trace, string(p.Phase)+":"+filepath.Base(p.Step)+":"+string(p.State))
	}
	want := []string{"clone:tagged:running", "clone:tagged:done", "clone:head:running", "clone:head:done",
		"file:secret:running", "file:secret:done", "setup:check:running", "setup:check:done"}
	if !slices.Equal(trace, want) {
		t.Fatalf("progress %v", trace)
	}
	if got := e.readAbs(ws + "/out"); got != "v1\nv2\n" {
		t.Fatalf("checkouts: %q", got)
	}
	if fi, err := os.Stat(ws + "/conf/secret"); err != nil || fi.Mode().Perm() != 0o640 || e.readAbs(ws+"/conf/secret") != secretData {
		t.Fatalf("secret file: %v %v", fi, err)
	}

	// A failing setup step: typed failure with its exit code, and its output
	// quoted with every secret redacted.
	req.Repos = nil
	req.Setup = []guestproto.SetupStep{
		{Name: "leak", Argv: []string{"sh", "-c", `echo "token=$TOKEN"; cat conf/secret; exit 3`}, Cwd: ws},
		{Name: "never", Argv: []string{"sh", "-c", "touch never"}, Cwd: ws},
	}
	steps, res = prepare(t, ctx, c, req)
	if res.State != guestproto.WorkspaceFailed || res.FailedStep != "leak" || res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("failed setup: %+v", res)
	}
	for _, m := range append([]string{res.Message}, steps[len(steps)-1].Message) {
		if strings.Contains(m, "hunter2") || strings.Contains(m, "s3cr3t") || strings.Contains(m, "line-two") || !strings.Contains(m, "[redacted]") {
			t.Fatalf("message leaks or lacks redaction: %q", m)
		}
	}
	if last := steps[len(steps)-1]; last.Step != "leak" || last.State != guestproto.WorkspaceStepFailed {
		t.Fatalf("last progress %+v", last)
	}
	if _, err := os.Stat(ws + "/never"); err == nil {
		t.Fatal("a step after the failure ran")
	}

	// A secret cut by the output tail (its prefix dropped as older output)
	// must not leak its suffix. The 40-byte token is printed, then filler,
	// then the token again: the kept tail (2048+40 bytes) starts 12 bytes
	// into the first one, and redacting the second shortens the text, so a
	// plain final cut would keep most of the 28-byte suffix.
	const token = "SECRETVALUE-abcdefghijklmnopqrstuvwxyz01"
	_, res = prepare(t, ctx, c, guestproto.WorkspacePrepare{Revision: "env_x@7", Env: []string{"TOKEN=" + token},
		Setup: []guestproto.SetupStep{{Name: "cut", Argv: []string{"sh", "-c", `printf %s "$TOKEN"; yes B | head -c 2020; printf %s "$TOKEN"; exit 1`}}}})
	if res.State != guestproto.WorkspaceFailed || strings.Contains(res.Message, "xyz01") || !strings.Contains(res.Message, "B\nB\n[redacted]") {
		if len(res.Message) > 200 {
			res.Message = res.Message[:200]
		}
		t.Fatalf("cut secret: %q", res.Message)
	}

	// A clone failure names the repo path and has no exit code; a timeout
	// is reported as such.
	_, res = prepare(t, ctx, c, guestproto.WorkspacePrepare{Revision: "env_x@4",
		Repos: []guestproto.RepoCheckout{{URL: "file://" + ws + "/missing", Path: ws + "/src/missing"}}})
	if res.State != guestproto.WorkspaceFailed || res.FailedStep != ws+"/src/missing" || res.ExitCode != nil || !strings.Contains(res.Message, "git clone") {
		t.Fatalf("clone failure: %+v", res)
	}
	_, res = prepare(t, ctx, c, guestproto.WorkspacePrepare{Revision: "env_x@5",
		Repos: []guestproto.RepoCheckout{{URL: "file://" + repo, Ref: "no-such-ref", Path: ws + "/src/badref"}}})
	if res.State != guestproto.WorkspaceFailed || !strings.Contains(res.Message, "checkout no-such-ref") {
		t.Fatalf("bad ref: %+v", res)
	}
	start := time.Now()
	_, res = prepare(t, ctx, c, guestproto.WorkspacePrepare{Revision: "env_x@6",
		Setup: []guestproto.SetupStep{{Name: "slow", Argv: []string{"sleep", "30"}, TimeoutSeconds: 1}}})
	if res.State != guestproto.WorkspaceFailed || res.FailedStep != "slow" || !strings.Contains(res.Message, "timed out") || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %+v after %v", res, time.Since(start))
	}
}

func (e *env) readAbs(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}
