//go:build linux

package main_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/cli"
	"davit/internal/domain"
)

// output concatenates the text of an exec stream's stdout/stderr events.
func output(t *testing.T, r run, event string) string {
	t.Helper()
	var b strings.Builder
	var seq int64
	for i, l := range r.lines {
		if l.Event != "stdout" && l.Event != "stderr" {
			continue
		}
		if l.Seq <= seq {
			t.Fatalf("output seq %d after %d", l.Seq, seq)
		}
		seq = l.Seq
		if l.Event == event {
			var c struct{ Text string }
			json.Unmarshal(r.raw[i], &c)
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

func exited(t *testing.T, r run) api.Process {
	t.Helper()
	if l := r.last(); l.Event != "process.exited" {
		t.Fatalf("last event %q (%+v), want process.exited; stderr %q", l.Event, l.Error, r.stderr)
	}
	var p api.Process
	if err := json.Unmarshal(r.raw[len(r.raw)-1], &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestExecEndToEnd runs the daemon on the fake runtime with in-process
// guest agents: new runs the full restore barrier, then exec streams
// output and preserves the guest exit status.
func TestExecEndToEnd(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(20*time.Millisecond, 0)
	vm := h.waited(h.cli("new", "box"), domain.OpCreate)
	if !vm.Readiness.Agent || vm.Readiness.SSH {
		t.Fatalf("readiness %+v: agent must be verified by the barrier, ssh must not be claimed", vm.Readiness)
	}

	r := h.cli("exec", "box", "--", "sh", "-c", "echo hi; echo oops >&2; exit 3")
	if r.code != 3 || r.lines[0].Event != "process.started" || output(t, r, "stdout") != "hi\n" || output(t, r, "stderr") != "oops\n" {
		t.Fatalf("exec: exit %d lines %+v stderr %q", r.code, r.lines, r.stderr)
	}
	if p := exited(t, r); p.Status != api.ProcessExited || p.ExitCode == nil || *p.ExitCode != 3 || p.VMID != vm.ID {
		t.Fatalf("exited process %+v", p)
	}

	// Human mode: raw passthrough of stdout, stderr and stdin.
	cmd := exec.Command(bin, "exec", "box", "--stdin", "-e", "GREETING=hello", "--", "sh", "-c", `cat; echo "$GREETING" >&2`)
	cmd.Env = append(cmd.Environ(), "DAVIT_API_URL=unix://"+h.sock)
	cmd.Stdin = strings.NewReader("from stdin\n")
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil || so.String() != "from stdin\n" || se.String() != "hello\n" {
		t.Fatalf("human exec: %v stdout %q stderr %q", err, so.String(), se.String())
	}

	// The process deadline maps to exit 124.
	start := time.Now()
	r = h.cli("exec", "box", "--timeout", "1", "--", "sleep", "30")
	if p := exited(t, r); r.code != cli.ExitDeadline || !p.TimedOut || time.Since(start) > 10*time.Second {
		t.Fatalf("deadline: exit %d process %+v", r.code, p)
	}

	// Detached: returns the ID; status and attach replay work afterwards.
	r = h.cli("exec", "box", "--detach", "--", "sh", "-c", "echo one; sleep 0.2; echo two")
	if r.code != 0 || r.last().Event != "result" {
		t.Fatalf("detach: exit %d %+v", r.code, r.last())
	}
	var p api.Process
	json.Unmarshal(r.raw[len(r.raw)-1], &p)
	if p.ID == "" || !p.Detached {
		t.Fatalf("detached process %+v", p)
	}
	r = h.cli("exec", "box", "--attach", p.ID)
	if r.code != 0 || output(t, r, "stdout") != "one\ntwo\n" {
		t.Fatalf("attach: exit %d %+v", r.code, r.lines)
	}
	r = h.cli("exec", "box", "--attach", p.ID, "--after", "1")
	if output(t, r, "stdout") != "two\n" {
		t.Fatalf("attach --after 1: %+v", r.lines)
	}
	// From the final cursor only the exit remains.
	if r = h.cli("exec", "box", "--attach", p.ID, "--after", "2"); r.code != 0 || output(t, r, "stdout") != "" {
		t.Fatalf("attach --after 2: exit %d %+v", r.code, r.lines)
	}
	r = h.cli("exec", "box", "--status", p.ID)
	var st api.Process
	json.Unmarshal(r.raw[len(r.raw)-1], &st)
	if r.code != 0 || st.Status != api.ProcessExited || st.OutputCursor != 2 || *st.ExitCode != 0 {
		t.Fatalf("status: exit %d %+v", r.code, st)
	}

	h.expectError(h.cli("exec", "box", "--status", "proc_nope"), cli.ExitNotFound, domain.CodeNotFound)
	h.expectError(h.cli("exec", "box", "--tty", "--", "sh"), cli.ExitConflict, domain.CodeUnsupportedCapability)
	h.expectError(h.cli("exec", "box"), cli.ExitUsage, domain.CodeInvalidArgument)

	// Stopped VMs refuse guest access.
	h.waited(h.cli("stop", "box"), domain.OpStop)
	h.expectError(h.cli("exec", "box", "--", "true"), cli.ExitConflict, domain.CodeInvalidState)
}

// TestExecRecordsSurviveDaemonRestart SIGKILLs the daemon: process records
// are durable, so status still answers with argv and the recorded outcome.
// (The fake's in-process agents die with the daemon, so a process that was
// still running is reported lost; a real guest keeps running it.)
func TestExecRecordsSurviveDaemonRestart(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(20*time.Millisecond, 0)
	h.waited(h.cli("new", "box"), domain.OpCreate)
	done := exited(t, h.cli("exec", "box", "--", "sh", "-c", "exit 5"))
	r := h.cli("exec", "box", "--detach", "--", "sleep", "2")
	var running api.Process
	json.Unmarshal(r.raw[len(r.raw)-1], &running)

	h.kill()
	h.start(20*time.Millisecond, 0)

	status := func(id string) api.Process {
		t.Helper()
		r := h.cli("exec", "box", "--status", id)
		if r.code != 0 {
			t.Fatalf("status %s after restart: exit %d %+v", id, r.code, r.last().Error)
		}
		var p api.Process
		json.Unmarshal(r.raw[len(r.raw)-1], &p)
		return p
	}
	if p := status(done.ID); p.Status != api.ProcessExited || *p.ExitCode != 5 || strings.Join(p.Argv, " ") != "sh -c exit 5" {
		t.Fatalf("finished process after restart: %+v", p)
	}
	if p := status(running.ID); p.Status != api.ProcessLost || strings.Join(p.Argv, " ") != "sleep 2" || !p.CreatedAt.Equal(running.CreatedAt) {
		t.Fatalf("running process after restart: %+v", p)
	}
	// The VM itself is still reachable for new work.
	if r := h.cli("exec", "box", "--", "true"); r.code != 0 {
		t.Fatalf("exec after restart: exit %d %+v", r.code, r.last().Error)
	}
}
