package main_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"davit/api"
	"davit/internal/cli"
	"davit/internal/domain"
)

// bin is the davit binary built once for these tests.
var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "davit-e2e-bin-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "davit")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build davit: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type host struct {
	t      *testing.T
	dir    string
	sock   string
	daemon *exec.Cmd
	log    bytes.Buffer
	// daemonDone closes when the daemon exits (daemonErr says how);
	// killed marks an exit the test caused.
	daemonDone chan struct{}
	daemonErr  error
	killed     bool
	// guestAgent runs the fake runtime with in-process guest agents.
	guestAgent bool
	// extraConfig is appended to the daemon config object, e.g.
	// `"harness":{...}`.
	extraConfig string
	stdin       string // stdin for the next cli call
	// env overrides the CLI's environment (later entries win).
	env []string
}

func newHost(t *testing.T) *host {
	dir := t.TempDir()
	h := &host{t: t, dir: dir, sock: filepath.Join(shortSockDir(t), "davit.sock")}
	t.Cleanup(func() {
		died := h.diedUnexpectedly()
		if h.daemon != nil {
			h.daemon.Process.Kill()
			<-h.daemonDone
		}
		if t.Failed() {
			t.Logf("daemon log:\n%s", h.log.String())
			if died != "" {
				t.Log(died)
			}
		}
	})
	return h
}

// start runs the daemon with the given fake-runtime delays and waits until it
// serves.
func (h *host) start(bootDelay, stopDelay time.Duration) {
	h.t.Helper()
	extra := ""
	if h.extraConfig != "" {
		extra = "," + h.extraConfig
	}
	cfg := fmt.Sprintf(`{"dataDir":%q,"socket":%q,"logLevel":"debug","fake":{"bootDelay":%q,"stopDelay":%q,"guestAgent":%v}%s}`,
		filepath.Join(h.dir, "data"), h.sock, bootDelay.String(), stopDelay.String(), h.guestAgent, extra)
	path := filepath.Join(h.dir, "config.json")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.launch(path, 30*time.Second)
}

// launch starts `davit daemon --config path` and waits until its socket
// answers. Every daemon a test starts goes through here, so daemonDone is
// always set for the cleanup (which waits on it) and diedUnexpectedly.
func (h *host) launch(path string, timeout time.Duration) {
	h.t.Helper()
	h.daemon = exec.Command(bin, "daemon", "--config", path)
	h.daemon.Stdout, h.daemon.Stderr = &h.log, &h.log
	if err := h.daemon.Start(); err != nil {
		h.t.Fatal(err)
	}
	done, d := make(chan struct{}), h.daemon
	h.daemonDone, h.killed = done, false
	go func() { h.daemonErr = d.Wait(); close(done) }()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if c, err := net.Dial("unix", h.sock); err == nil {
			c.Close()
			return
		}
	}
	h.t.Fatalf("daemon did not start:\n%s", h.log.String())
}

// kill SIGKILLs the daemon: no graceful shutdown, journal left mid-operation.
func (h *host) kill() {
	h.killed = true
	h.daemon.Process.Signal(syscall.SIGKILL)
	<-h.daemonDone
}

// diedUnexpectedly describes a daemon exit the test did not cause, such as
// the OOM killer under memory pressure ("" while it runs or after kill).
// gate.sh reads "signal: killed" in a failure as an OS kill.
func (h *host) diedUnexpectedly() string {
	if h.daemon == nil || h.killed {
		return ""
	}
	select {
	case <-h.daemonDone:
	default:
		return ""
	}
	if ee, ok := h.daemonErr.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signal() == syscall.SIGKILL {
			return fmt.Sprintf("the daemon died by %v, not from the test: the OS killed it (memory pressure?), which explains the failure above", ee)
		}
	}
	return fmt.Sprintf("the daemon exited on its own: %v", h.daemonErr)
}

// fatalIfSignaled fails the test when a davit CLI process died by a
// signal. Tests never SIGKILL the CLI, so a SIGKILL came from the OS
// (typically the OOM killer); any other signal means davit did not handle
// it.
func fatalIfSignaled(t *testing.T, args []string, err error) {
	t.Helper()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != -1 {
		return
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signal() == syscall.SIGKILL {
		t.Fatalf("davit %v: %v: killed by the OS (memory pressure?), not a davit failure", args, ee)
	}
	t.Fatalf("davit %v died by %v instead of handling it", args, ee)
}

type run struct {
	code   int
	lines  []cli.Envelope
	raw    []json.RawMessage
	stderr string
}

// cli runs `davit --json args...` and parses every stdout line.
func (h *host) cli(args ...string) run {
	h.t.Helper()
	cmd := exec.Command(bin, append([]string{"--json"}, args...)...)
	cmd.Env = append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=",
		"XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"))
	cmd.Env = append(cmd.Env, h.env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.Stdin, h.stdin = strings.NewReader(h.stdin), ""
	err := cmd.Run()
	fatalIfSignaled(h.t, args, err)
	r := run{stderr: stderr.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatal(err)
	}
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var env cli.Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			h.t.Fatalf("davit %v: stdout line is not JSON: %q", args, sc.Text())
		}
		if env.SchemaVersion != 1 || env.RequestID == "" || env.Time.IsZero() {
			h.t.Fatalf("davit %v: bad envelope %s", args, sc.Text())
		}
		var raw struct {
			Data json.RawMessage `json:"data"`
		}
		json.Unmarshal(sc.Bytes(), &raw)
		r.lines = append(r.lines, env)
		r.raw = append(r.raw, raw.Data)
	}
	if len(r.lines) == 0 {
		h.t.Fatalf("davit %v: no JSON output (exit %d, stderr %q)", args, r.code, r.stderr)
	}
	reqID := r.lines[0].RequestID
	for _, l := range r.lines {
		if l.RequestID != reqID {
			h.t.Fatalf("davit %v: requestId changed within one invocation", args)
		}
	}
	// Exactly one terminal line, and it is last (process.exited ends an
	// exec stream).
	for i, l := range r.lines {
		terminal := l.Event == "result" || l.Event == "error" || l.Event == "process.exited"
		if terminal != (i == len(r.lines)-1) {
			h.t.Fatalf("davit %v: line %d event %q breaks the one-terminal-line rule", args, i, l.Event)
		}
	}
	return r
}

func (r run) last() cli.Envelope { return r.lines[len(r.lines)-1] }

// waited asserts a successful lifecycle wait: operation, ordered progress,
// result. It returns the final VM.
func (h *host) waited(r run, kind domain.OperationKind) api.VM {
	h.t.Helper()
	if r.code != 0 {
		h.t.Fatalf("%s: exit %d: %+v", kind, r.code, r.last().Error)
	}
	if r.lines[0].Event != "operation" {
		h.t.Fatalf("%s: first event %q, want operation", kind, r.lines[0].Event)
	}
	var seq int64
	for _, l := range r.lines[1 : len(r.lines)-1] {
		if l.Event != "progress" || l.Seq <= seq || l.OperationID != r.lines[0].OperationID {
			h.t.Fatalf("%s: bad progress line %+v after seq %d", kind, l, seq)
		}
		seq = l.Seq
	}
	if seq == 0 {
		h.t.Fatalf("%s: no progress events", kind)
	}
	var res struct {
		Operation api.Operation `json:"operation"`
		VM        api.VM        `json:"vm"`
	}
	if err := json.Unmarshal(r.raw[len(r.raw)-1], &res); err != nil {
		h.t.Fatal(err)
	}
	if res.Operation.Kind != kind || res.Operation.State != domain.OpSucceeded || res.Operation.Result == nil {
		h.t.Fatalf("%s: result operation %+v", kind, res.Operation)
	}
	return res.VM
}

func (h *host) info(ref string) api.VM {
	h.t.Helper()
	r := h.cli("info", ref)
	if r.code != 0 || r.last().Event != "result" {
		h.t.Fatalf("info %s: exit %d %+v", ref, r.code, r.last().Error)
	}
	var vm api.VM
	if err := json.Unmarshal(r.raw[0], &vm); err != nil {
		h.t.Fatal(err)
	}
	return vm
}

func (h *host) expectError(r run, exit int, code domain.Code) {
	h.t.Helper()
	l := r.last()
	if r.code != exit || l.Event != "error" || l.Error == nil || l.Error.Code != code {
		h.t.Fatalf("want exit %d %s, got exit %d %+v", exit, code, r.code, l.Error)
	}
}

func TestEndToEndLifecycle(t *testing.T) {
	h := newHost(t)
	h.start(50*time.Millisecond, 0)

	web := h.waited(h.cli("new", "web", "--cpus", "1", "--memory", "512MiB", "-e", "TOKEN=s3cret"), domain.OpCreate)
	if web.State != domain.StateReady || web.BootMode != domain.BootModeColdBoot || web.Shape.CPUs != 1 || web.Shape.MemoryBytes != 512*domain.MiB {
		t.Fatalf("new: %+v", web)
	}

	// Fork needs a committed disk snapshot; a never-stopped VM has none.
	h.expectError(h.cli("fork", "web", "too-early"), cli.ExitConflict, domain.CodeStopRequired)

	stopped := h.waited(h.cli("stop", "web"), domain.OpStop)
	if stopped.State != domain.StateStopped || stopped.LastSnapshotID == "" {
		t.Fatalf("stop: %+v", stopped)
	}
	if got := h.info("web"); got.StateRevision != stopped.StateRevision || got.LastSnapshotID != stopped.LastSnapshotID {
		t.Fatalf("info disagrees with stop result: %+v", got)
	}

	fork := h.waited(h.cli("fork", "web", "web-fork"), domain.OpFork)
	if fork.State != domain.StateReady || fork.ID == web.ID || fork.SourceSnapshotID != stopped.LastSnapshotID {
		t.Fatalf("fork: %+v", fork)
	}

	resumed := h.waited(h.cli("resume", "web"), domain.OpResume)
	if resumed.State != domain.StateReady || resumed.InstanceGeneration != 2 {
		t.Fatalf("resume: %+v", resumed)
	}

	list := h.cli("list")
	if list.code != 0 || len(list.lines) != 3 || list.lines[0].Event != "item" || list.lines[1].Event != "item" {
		t.Fatalf("list: %+v", list.lines)
	}

	h.expectError(h.cli("delete", "web"), cli.ExitUsage, domain.CodeInvalidArgument) // --yes required
	deleted := h.waited(h.cli("delete", "web", "--yes"), domain.OpDelete)
	if deleted.State != domain.StateDeleted || deleted.DeletedAt == nil {
		t.Fatalf("delete: %+v", deleted)
	}
	h.waited(h.cli("delete", "web-fork", "--yes"), domain.OpDelete)

	h.expectError(h.cli("info", "web"), cli.ExitNotFound, domain.CodeNotFound)
	h.expectError(h.cli("new", "--bogus"), cli.ExitUsage, domain.CodeInvalidArgument)
	// Without a guest transport exec is an explicit capability error.
	h.expectError(h.cli("exec", "x", "--", "true"), cli.ExitConflict, domain.CodeUnsupportedCapability)
	if empty := h.cli("list"); len(empty.lines) != 1 || empty.last().Event != "result" {
		t.Fatalf("list after delete: %+v", empty.lines)
	}
	if all := h.cli("list", "--all"); len(all.lines) != 3 {
		t.Fatalf("list --all should show 2 tombstones: %+v", all.lines)
	}
}

func TestDaemonKillMidOperationRecovers(t *testing.T) {
	h := newHost(t)
	h.start(3*time.Second, 3*time.Second)

	// Kill the daemon while the VMM is booting.
	acc := h.cli("new", "slow", "--no-wait")
	if acc.code != 0 || acc.lines[0].Event != "operation" || acc.last().Event != "result" {
		t.Fatalf("new --no-wait: %+v", acc.lines)
	}
	createOp := acc.lines[0].OperationID
	h.waitPhase(createOp, "runtime.boot")
	h.kill()

	h.start(0, 0)
	watch := h.cli("operations", string(createOp), "--watch")
	vm := h.waited(watch, domain.OpCreate)
	if vm.State != domain.StateReady || vm.InstanceGeneration != 1 {
		t.Fatalf("after recovery: %+v", vm)
	}
	if n := h.instances(vm.ID); n != 1 {
		t.Fatalf("recovery spawned %d runtime instances for %s, want 1", n, vm.ID)
	}
	var recovered bool
	for i, l := range watch.lines {
		var ev domain.OperationEvent
		json.Unmarshal(watch.raw[i], &ev)
		recovered = recovered || (l.Event == "progress" && ev.Phase == "recovered")
	}
	if !recovered {
		t.Fatal("operation journal shows no recovery after daemon kill")
	}

	// Kill the daemon during a clean guest shutdown.
	h.kill()
	h.start(0, 3*time.Second)
	acc = h.cli("stop", "slow", "--no-wait")
	stopOp := acc.lines[0].OperationID
	h.waitPhase(stopOp, "guest.shutdown")
	h.kill()

	h.start(0, 0)
	stopped := h.waited(h.cli("operations", string(stopOp), "--watch"), domain.OpStop)
	if stopped.State != domain.StateStopped || stopped.LastSnapshotID == "" {
		t.Fatalf("stop after recovery: %+v", stopped)
	}
	if n := h.instances(vm.ID); n != 0 {
		t.Fatalf("stopped VM still has %d runtime instances", n)
	}
	// Idempotent replay across restarts: the same key returns the same operation.
	r1 := h.cli("resume", "slow", "--idempotency-key", "resume-once", "--no-wait")
	r2 := h.cli("resume", "slow", "--idempotency-key", "resume-once", "--no-wait")
	if r1.lines[0].OperationID != r2.lines[0].OperationID {
		t.Fatalf("idempotency key not honoured: %s vs %s", r1.lines[0].OperationID, r2.lines[0].OperationID)
	}
	if vm := h.waited(h.cli("operations", string(r1.lines[0].OperationID), "--watch"), domain.OpResume); vm.State != domain.StateReady {
		t.Fatalf("resume: %+v", vm)
	}
}

// waitPhase polls an operation until it reports phase.
func (h *host) waitPhase(op domain.OperationID, phase string) {
	h.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		r := h.cli("operations", string(op))
		var o api.Operation
		json.Unmarshal(r.raw[0], &o)
		if o.Phase == phase {
			return
		}
	}
	h.t.Fatalf("operation %s never reached %s", op, phase)
}

// instances counts the fake runtime's live instance records for vm: the
// stand-in for "VMM processes on the host".
func (h *host) instances(vm domain.VMID) int {
	m, _ := filepath.Glob(filepath.Join(h.dir, "data", "runtime", string(vm)+".g*.json"))
	n := 0
	for _, p := range m {
		if !strings.HasSuffix(p, ".tmp") {
			n++
		}
	}
	return n
}

func TestEndToEndSnapshotsAndContext(t *testing.T) {
	h := newHost(t)
	h.start(0, 0)

	h.waited(h.cli("new", "web"), domain.OpCreate)
	h.expectError(h.cli("snapshot", "create", "web", "base"), cli.ExitConflict, domain.CodeStopRequired)
	h.waited(h.cli("stop", "web"), domain.OpStop)

	r := h.cli("snapshot", "create", "web", "base")
	h.waited(r, domain.OpSnapshot)
	var res struct {
		Snapshot api.Snapshot `json:"snapshot"`
	}
	json.Unmarshal(r.raw[len(r.raw)-1], &res)
	if res.Snapshot.Name != "base" || res.Snapshot.SourceVMID == "" || res.Snapshot.Kind != domain.SnapshotKindDisk {
		t.Fatalf("snapshot create result: %+v", res.Snapshot)
	}
	h.expectError(h.cli("snapshot", "create", "web", "base"), cli.ExitUsage, domain.CodeInvalidArgument) // name taken
	h.expectError(h.cli("snapshot", "web", "base"), cli.ExitUsage, domain.CodeInvalidArgument)           // old form is not a subcommand

	list := h.cli("snapshots", "web")
	names := map[string]bool{}
	for i, l := range list.lines {
		if l.Event == "item" {
			var sn api.Snapshot
			json.Unmarshal(list.raw[i], &sn)
			names[sn.Name] = true
		}
	}
	if !names["base"] || !names[""] {
		t.Fatalf("snapshots web: want the named and the automatic snapshot, got %v", names)
	}

	restored := h.waited(h.cli("new", "restored", "--from", "base"), domain.OpCreate)
	if restored.SourceSnapshotID != res.Snapshot.ID {
		t.Fatalf("new --from by name: %+v", restored)
	}
	h.expectError(h.cli("snapshot", "delete", "base"), cli.ExitUsage, domain.CodeInvalidArgument) // --yes required
	h.expectError(h.cli("snapshot", "delete", "base", "--yes"), cli.ExitConflict, domain.CodeInvalidState)
	h.waited(h.cli("stop", "restored"), domain.OpStop)
	if d := h.cli("snapshot", "delete", "base", "--yes"); d.code != 0 || d.last().Event != "result" {
		t.Fatalf("snapshot delete: exit %d %+v", d.code, d.last().Error)
	}
	h.expectError(h.cli("snapshot", "delete", "base", "--yes"), cli.ExitNotFound, domain.CodeNotFound)

	// Saved context exists for interactive use only; scripts must be explicit.
	if c := h.cli("context", "use", "web"); c.code != 0 {
		t.Fatalf("context use: %+v", c.last().Error)
	}
	h.expectError(h.cli("info"), cli.ExitUsage, domain.CodeInvalidArgument)
	show := h.cli("context", "show")
	var shown map[string]any
	json.Unmarshal(show.raw[0], &shown)
	if shown["source"] != "none" || shown["saved"] == nil {
		t.Fatalf("context show (non-interactive): %v", shown)
	}
	if path := filepath.Join(h.dir, "config", "davit", "context.json"); fileMode(t, path) != 0o600 {
		t.Fatalf("context file must be owner-only")
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TestEndToEndInitDoctor onboards a sandboxed home: init writes a working
// config, the daemon starts from it without --config, and doctor reports
// every check as JSONL with the daemon reachable.
func TestEndToEndInitDoctor(t *testing.T) {
	home := t.TempDir()
	env := append(os.Environ(),
		"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "cfg"), "XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_RUNTIME_DIR="+shortSockDir(t), "DAVIT_API_URL=", "DAVIT_TS_AUTHKEY=")
	run := func(stdin string, args ...string) (int, []cli.Envelope, []json.RawMessage) {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"--json"}, args...)...)
		cmd.Env, cmd.Stdin = env, strings.NewReader(stdin)
		out, err := cmd.Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		var lines []cli.Envelope
		var raws []json.RawMessage
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			var e cli.Envelope
			var raw struct {
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal([]byte(l), &e) != nil || json.Unmarshal([]byte(l), &raw) != nil {
				t.Fatalf("%v: non-JSON stdout line %q", args, l)
			}
			lines, raws = append(lines, e), append(raws, raw.Data)
		}
		return code, lines, raws
	}

	// The control URL is a dead local port: the gateway this test enables
	// must never reach a real coordination server.
	code, lines, raws := run("tskey-auth-e2eSECRET\n", "init", "--auth-key-stdin", "--runtime", "fake", "--control-url", "http://127.0.0.1:9")
	var res struct {
		ConfigPath     string `json:"configPath"`
		ConfigWritten  bool   `json:"configWritten"`
		Runtime        string `json:"runtime"`
		AuthKeyWritten bool   `json:"authKeyWritten"`
		TailnetEnabled bool   `json:"tailnetEnabled"`
		Socket         string `json:"socket"`
	}
	json.Unmarshal(raws[len(raws)-1], &res)
	if code != 0 || len(lines) != 1 || !res.ConfigWritten || res.Runtime != "fake" || !res.AuthKeyWritten || res.TailnetEnabled {
		t.Fatalf("init: exit %d %+v", code, res)
	}
	if strings.Contains(string(raws[0]), "e2eSECRET") {
		t.Fatal("init echoed the auth key")
	}
	// Naming the owner on the existing config enables the gateway (key stored above).
	if code, _, raws := run("", "init", "--owner-login", "owner@example.com"); code != 0 ||
		!strings.Contains(string(raws[0]), `"tailnetEnabled":true`) || !strings.Contains(string(raws[0]), `"configWritten":true`) ||
		!strings.Contains(string(raws[0]), "Tailnet gateway enabled for owner@example.com") {
		t.Fatalf("init --owner-login: exit %d %s", code, raws[0])
	}
	if code, _, raws := run("", "init"); code != 0 || strings.Contains(string(raws[0]), `"configWritten":true`) {
		t.Fatalf("second init must keep the config: exit %d %s", code, raws[0])
	}
	if runtime.GOOS == "linux" {
		// The AppArmor profile is rendered for this binary's real path and
		// only printed commands do the root part.
		code, _, raws := run("", "init", "--apparmor")
		var aa struct {
			Binary, ProfileFile, Profile string
			Commands                     []string
		}
		json.Unmarshal(raws[len(raws)-1], &aa)
		real, _ := filepath.EvalSymlinks(bin)
		if code != 0 || aa.Binary != real || !strings.Contains(aa.Profile, real) || len(aa.Commands) != 2 ||
			!strings.HasPrefix(aa.Commands[0], "sudo install -m 0644 -o root -g root ") {
			t.Fatalf("init --apparmor: exit %d %s", code, raws[len(raws)-1])
		}
		if b, err := os.ReadFile(aa.ProfileFile); err != nil || string(b) != aa.Profile {
			t.Fatalf("profile file %s: %v", aa.ProfileFile, err)
		}
		if code, _, _ := run("", "init", "--apparmor", "--service"); code == 0 {
			t.Fatal("init --apparmor must refuse full-init flags")
		}
	}

	d := exec.Command(bin, "daemon")
	d.Env = env
	var log syncBuffer // written by exec's copy goroutine while the test reads it
	d.Stdout, d.Stderr = &log, &log
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Process.Kill(); d.Wait() })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if c, err := net.Dial("unix", res.Socket); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not start from the init config:\n%s", log.String())
		}
	}
	if !strings.Contains(log.String(), "preflight") && !strings.Contains(log.String(), "daemon starting") {
		t.Fatalf("daemon log lacks startup/preflight lines:\n%s", log.String())
	}

	code, lines, raws = run("", "doctor")
	ids := map[string]string{}
	for i, l := range lines[:len(lines)-1] {
		var c struct{ ID, Status string }
		json.Unmarshal(raws[i], &c)
		if l.Event != "item" || c.ID == "" {
			t.Fatalf("doctor line %d: %+v", i, l)
		}
		ids[c.ID] = c.Status
	}
	for _, id := range []string{"config", "platform", "storage", "disk_space", "tailnet_state", "tailnet_auth", "tailnet_owner", "daemon"} {
		if ids[id] == "" {
			t.Errorf("doctor did not report %s", id)
		}
	}
	if ids["daemon"] != "pass" || ids["config"] != "pass" || ids["tailnet_auth"] != "pass" || ids["tailnet_owner"] != "pass" {
		t.Fatalf("doctor statuses: %v", ids)
	}
	last := lines[len(lines)-1]
	if failed := code != 0; failed != (last.Event == "error") || (failed && code != cli.ExitConflict) {
		t.Fatalf("doctor exit %d with terminal %q", code, last.Event)
	}
}

// shortSockDir returns a short-lived directory for Unix sockets. t.TempDir
// can exceed the 108-byte sun_path limit (macOS /var/folders, or GOTMPDIR,
// which t.TempDir follows since Go 1.26), so sockets live under TMPDIR.
func shortSockDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "slw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// syncBuffer is a bytes.Buffer safe for one writer goroutine and concurrent
// readers (a subprocess's output while the test polls it).
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
