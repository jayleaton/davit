//go:build linux

package main_test

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"davit/api"
	"davit/internal/cli"
	"davit/internal/domain"
)

// background runs `davit args...` and returns once its stdout has a line
// containing ready; wait reaps it and returns its exit code and output.
func (h *host) background(ready string, args ...string) (cmd *exec.Cmd, wait func() (int, string, string)) {
	h.t.Helper()
	cmd = exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=",
		"XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"))
	cmd.Env = append(cmd.Env, h.env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		h.t.Fatal(err)
	}
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { cmd.Process.Kill() })
	seen := make(chan struct{})
	var out strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		found := false
		for sc.Scan() {
			out.WriteString(sc.Text() + "\n")
			if !found && strings.Contains(sc.Text(), ready) {
				found = true
				close(seen)
			}
		}
	}()
	// The windows only bound a hang; under heavy load a CLI can take
	// seconds to start or to finish.
	select {
	case <-seen:
	case <-done:
		cmd.Wait()
		h.t.Fatalf("davit %v exited (%v) before printing %q; stderr %q", args, cmd.ProcessState, ready, stderr.String())
	case <-time.After(60 * time.Second):
		h.t.Fatalf("davit %v never printed %q; stderr %q", args, ready, stderr.String())
	}
	return cmd, func() (int, string, string) {
		h.t.Helper()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			h.t.Fatalf("davit %v did not exit; stdout %q stderr %q", args, out.String(), stderr.String())
		}
		fatalIfSignaled(h.t, args, cmd.Wait())
		return cmd.ProcessState.ExitCode(), out.String(), stderr.String()
	}
}

func lastEnvelope(t *testing.T, out string) (cli.Envelope, json.RawMessage) {
	t.Helper()
	ls := strings.Split(strings.TrimSpace(out), "\n")
	var e cli.Envelope
	var raw struct{ Data json.RawMessage }
	if json.Unmarshal([]byte(ls[len(ls)-1]), &e) != nil || json.Unmarshal([]byte(ls[len(ls)-1]), &raw) != nil {
		t.Fatalf("last line not JSON: %q", out)
	}
	return e, raw.Data
}

// TestExecCwdKillAndSignals drives exec paths through the real binary and
// daemon: --cwd, --kill of a detached process, mutually exclusive modes,
// Ctrl-C (exit 130) and a daemon lost mid-stream (exit 255).
func TestExecCwdKillAndSignals(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(20*time.Millisecond, 0)
	h.waited(h.cli("new", "box"), domain.OpCreate)

	if r := h.cli("exec", "box", "--cwd", "/tmp", "--", "pwd"); r.code != 0 || output(t, r, "stdout") != "/tmp\n" {
		t.Fatalf("--cwd: exit %d %+v (error %+v)", r.code, r.lines, r.last().Error)
	}

	r := h.cli("exec", "box", "--detach", "--", "sleep", "30")
	var p api.Process
	json.Unmarshal(r.raw[len(r.raw)-1], &p)
	if r = h.cli("exec", "box", "--kill", p.ID); r.code != 0 || r.last().Event != "result" {
		t.Fatalf("--kill: exit %d %+v", r.code, r.last())
	}
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		json.Unmarshal(h.cli("exec", "box", "--status", p.ID).raw[0], &p)
		if p.Status != api.ProcessRunning && p.Status != api.ProcessStarting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed process still running: %+v", p)
		}
	}
	if p.Signal != "TERM" || p.ExitCode != nil {
		t.Fatalf("after --kill: %+v", p)
	}
	h.expectError(h.cli("exec", "box", "--status", p.ID, "--kill", p.ID), cli.ExitUsage, domain.CodeInvalidArgument)

	// Ctrl-C forwards SIGINT to the guest process and exits 130, also when it
	// lands right after process.started, before the stream is attached (the
	// handler is installed before the process is created).
	var code int
	var out, stderr string
	for i := 0; i < 3; i++ {
		cmd, wait := h.background(`"event":"process.started"`, "--json", "exec", "box", "--", "sh", "-c", "echo go; exec sleep 30")
		cmd.Process.Signal(syscall.SIGINT)
		code, out, stderr = wait()
		if last, raw := lastEnvelope(t, out); code != cli.ExitInterrupt || last.Event != "process.exited" {
			t.Fatalf("Ctrl-C right after process.started (try %d): exit %d last %+v %s; stderr %q", i, code, last, raw, stderr)
		}
	}

	// The daemon dies mid-stream: exit 255 with an `unavailable` error.
	_, wait := h.background(`"event":"stdout"`, "--json", "exec", "box", "--", "sh", "-c", "echo go; exec sleep 30")
	h.kill()
	code, out, stderr = wait()
	if last, _ := lastEnvelope(t, out); code != cli.ExitLost || last.Event != "error" || last.Error == nil || last.Error.Code != domain.CodeUnavailable {
		t.Fatalf("stream lost: exit %d last %+v; stderr %q", code, last, stderr)
	}
}

// TestInfoWatchInterrupt: Ctrl-C ends `info --watch` quietly with exit 130.
func TestInfoWatchInterrupt(t *testing.T) {
	h := newHost(t)
	h.start(0, 0)
	h.waited(h.cli("new", "box"), domain.OpCreate)
	cmd, wait := h.background("state ", "info", "box", "--watch")
	cmd.Process.Signal(syscall.SIGINT)
	code, out, stderr := wait()
	if code != cli.ExitInterrupt || stderr != "" || !strings.Contains(out, "state      ready") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, stderr)
	}
}

// TestHistoryReadableWhileStopped: a detached job's output is retained by
// the host with no client attached; after the VM stops, `history` lists the
// job and `history ID PID` prints its output.
func TestHistoryReadableWhileStopped(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(0, 0)
	h.waited(h.cli("new", "box"), domain.OpCreate)
	r := h.cli("exec", "box", "--detach", "--", "sh", "-c", "echo out-line; echo err-line >&2")
	var p api.Process
	json.Unmarshal(r.raw[len(r.raw)-1], &p)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(50 * time.Millisecond) {
		json.Unmarshal(h.cli("exec", "box", "--status", p.ID).raw[0], &p)
		if p.Status == api.ProcessExited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never exited: %+v", p)
		}
	}
	h.waited(h.cli("stop", "box"), domain.OpStop)

	hist := h.cli("history", "box")
	var entry api.HistoryEntry
	json.Unmarshal(hist.raw[0], &entry)
	if hist.code != 0 || entry.Process.ID != p.ID || entry.StdoutBytes != 9 || entry.StderrBytes != 9 {
		t.Fatalf("history while stopped: exit %d %+v", hist.code, entry)
	}
	out := h.cli("history", "box", p.ID)
	// Each stream is checked on its own: the order between two pipes is
	// not defined, so stderr may legitimately come first.
	streams := map[string]string{}
	for i, l := range out.lines {
		if l.Event == "stdout" || l.Event == "stderr" {
			var c struct{ Text string }
			json.Unmarshal(out.raw[i], &c)
			streams[l.Event] += c.Text
		}
	}
	if out.code != 0 || streams["stdout"] != "out-line\n" || streams["stderr"] != "err-line\n" {
		t.Fatalf("retained output while stopped: exit %d %q", out.code, streams)
	}
}

// TestHistoryOutputNotRetained: on a VM with noOutputRetention, history
// lists the job and says its output was not retained by policy, which is
// not a retention drop.
func TestHistoryOutputNotRetained(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(0, 0)
	h.waited(h.cli("new", "quiet", "--no-output-retention"), domain.OpCreate)
	r := h.cli("exec", "quiet", "--detach", "--", "sh", "-c", "echo out-line; echo err-line >&2")
	var p api.Process
	json.Unmarshal(r.raw[len(r.raw)-1], &p)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(50 * time.Millisecond) {
		json.Unmarshal(h.cli("exec", "quiet", "--status", p.ID).raw[0], &p)
		if p.Status == api.ProcessExited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never exited: %+v", p)
		}
	}
	hist := h.cli("history", "quiet")
	var entry api.HistoryEntry
	json.Unmarshal(hist.raw[0], &entry)
	if hist.code != 0 || entry.Process.ID != p.ID || entry.OutputNotRetained != api.OutputNotRetainedPolicy ||
		entry.OutputDropped || entry.StdoutBytes+entry.StderrBytes != 0 {
		t.Fatalf("history under noOutputRetention: exit %d %+v", hist.code, entry)
	}
}

// TestGuestParityWiring: with the in-process guest agent, an environment's
// workspace is prepared by the real agent (feature workspace), ps samples
// the guest (feature ps) and a job's history carries its durable handle
// (feature jobs) from the host's records.
func TestGuestParityWiring(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(0, 0)
	manifest := filepath.Join(h.dir, "m.json")
	os.WriteFile(manifest, []byte(`{"variables":{"STAGE":"test"},"setup":[{"name":"ok","argv":["true"]}]}`), 0o600)
	if r := h.cli("env", "create", "web", "--file", manifest); r.code != 0 {
		t.Fatalf("env create: exit %d %+v", r.code, r.last())
	}
	h.waited(h.cli("new", "box", "--environment", "web"), domain.OpCreate)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(50 * time.Millisecond) {
		var vm api.VM
		r := h.cli("info", "box")
		json.Unmarshal(r.raw[len(r.raw)-1], &vm)
		if vm.Workspace != nil && vm.Workspace.State == domain.WorkspaceReady {
			break
		}
		if vm.Workspace != nil && vm.Workspace.State == domain.WorkspaceFailed || time.Now().After(deadline) {
			t.Fatalf("workspace: %+v", vm.Workspace)
		}
	}

	ps := h.cli("ps", "box")
	if ps.code != 0 || len(ps.lines) < 2 {
		t.Fatalf("ps: exit %d %d lines", ps.code, len(ps.lines))
	}

	r := h.cli("exec", "box", "--detach", "--", "true")
	var p api.Process
	json.Unmarshal(r.raw[len(r.raw)-1], &p)
	hist := h.cli("history", "box")
	var entry api.HistoryEntry
	json.Unmarshal(hist.raw[0], &entry)
	if hist.code != 0 || entry.Process.ID != p.ID || !strings.HasPrefix(entry.Handle, "pstart_") {
		t.Fatalf("history handle: exit %d %+v", hist.code, entry)
	}
}
