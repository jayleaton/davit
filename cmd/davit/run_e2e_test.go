//go:build linux

package main_test

import (
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

// liveVMs counts the VMs `list` shows (provisioning, ready, running).
func (h *host) liveVMs() int {
	h.t.Helper()
	r := h.cli("list")
	var res struct{ Count int }
	if r.code != 0 || json.Unmarshal(r.raw[len(r.raw)-1], &res) != nil {
		h.t.Fatalf("list: exit %d %+v", r.code, r.last())
	}
	return res.Count
}

// finalRun decodes the result line of `run`.
func finalRun(t *testing.T, r run) api.Run {
	t.Helper()
	if l := r.last(); l.Event != "result" {
		t.Fatalf("last event %q (%+v), want result; stderr %q", l.Event, l.Error, r.stderr)
	}
	var out api.Run
	if err := json.Unmarshal(r.raw[len(r.raw)-1], &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRunEndToEnd drives `run` and `push` through the real binary and the
// fake runtime's in-process guest agent, whose "guest" is this host.
func TestRunEndToEnd(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(20*time.Millisecond, 0)

	// The command's exit status is the CLI's, and the VM is gone.
	r := h.cli("run", "--", "sh", "-c", "echo hi; echo oops >&2; exit 3")
	if r.code != 3 || output(t, r, "stdout") != "hi\n" || output(t, r, "stderr") != "oops\n" {
		t.Fatalf("run: exit %d lines %+v stderr %q", r.code, r.lines, r.stderr)
	}
	if run := finalRun(t, r); run.State != api.RunFailed || run.ExitCode == nil || *run.ExitCode != 3 || run.EndedAt == nil {
		t.Fatalf("run %+v", run)
	}
	if n := h.liveVMs(); n != 0 {
		t.Fatalf("%d VMs left after run", n)
	}

	// --keep leaves the VM.
	r = h.cli("run", "--keep", "--", "true")
	kept := finalRun(t, r)
	if r.code != 0 || kept.State != api.RunSucceeded || !kept.Keep || kept.VMID == "" {
		t.Fatalf("run --keep: exit %d %+v", r.code, kept)
	}
	if vm := h.info(string(kept.VMID)); vm.State != domain.StateReady {
		t.Fatalf("kept VM is %s", vm.State)
	}

	// The command's deadline maps to 124, and still cleans up.
	r = h.cli("run", "--timeout", "1", "--", "sleep", "30")
	if run := finalRun(t, r); r.code != cli.ExitDeadline || run.Error == nil || run.Error.Code != domain.CodeTimeout {
		t.Fatalf("run --timeout: exit %d %+v", r.code, run)
	}
	// Setup failures are errors, not exit statuses.
	h.expectError(h.cli("run", "--image", "no-such-image", "--", "true"), cli.ExitNotFound, domain.CodeNotFound)
	h.expectError(h.cli("run", "--keep"), cli.ExitUsage, domain.CodeInvalidArgument)
	if n := h.liveVMs(); n != 1 {
		t.Fatalf("%d VMs, want only the kept one", n)
	}

	// --push uploads the git selection before the command runs in it.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := gitRepo(t)
	guest := filepath.Join(t.TempDir(), "work")
	r = h.cli("run", "--push", repo+":"+guest, "--", "sh", "-c", "ls -A; cat tracked.txt")
	if r.code != 0 || output(t, r, "stdout") != ".gitignore\ntracked.txt\nuntracked.txt\ntracked\n" {
		t.Fatalf("run --push: exit %d stdout %q stderr %q", r.code, output(t, r, "stdout"), r.stderr)
	}
	if _, err := os.Stat(filepath.Join(guest, "ignored.env")); err == nil {
		t.Fatal("an ignored file was pushed")
	}

	// push to an existing VM: merge, JSON result.
	dst := filepath.Join(t.TempDir(), "tree")
	r = h.cli("push", string(kept.VMID), repo+":"+dst)
	var res api.PushResult
	if r.code != 0 || json.Unmarshal(r.raw[len(r.raw)-1], &res) != nil || res.Files != 3 || res.Path != dst || len(res.SHA256) != 64 {
		t.Fatalf("push: exit %d %+v %+v", r.code, r.last(), res)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "tracked.txt")); string(b) != "tracked\n" {
		t.Fatalf("pushed tracked.txt: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dst, "ignored.env")); err == nil {
		t.Fatal("push uploaded an ignored file")
	}
	h.expectError(h.cli("push", string(kept.VMID), t.TempDir()+":"+dst), cli.ExitUsage, domain.CodeInvalidArgument) // not a git tree
}

// TestRunInterrupt: Ctrl-C cancels the run; the VM is deleted and the CLI
// exits 130.
func TestRunInterrupt(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(20*time.Millisecond, 0)
	cmd, wait := h.background("started", "--json", "run", "--", "sh", "-c", "echo started; sleep 30")
	cmd.Process.Signal(syscall.SIGINT)
	code, out, stderr := wait()
	env, raw := lastEnvelope(t, out)
	var run api.Run
	json.Unmarshal(raw, &run)
	if code != cli.ExitInterrupt || env.Event != "result" || run.State != api.RunCancelled {
		t.Fatalf("interrupted run: exit %d %s %+v; stderr %q", code, env.Event, run, stderr)
	}
	if n := h.liveVMs(); n != 0 {
		t.Fatalf("%d VMs left after a cancelled run", n)
	}
}

// gitRepo makes a work tree with a tracked, an untracked and an ignored
// file under a hermetic git configuration.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(dir, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := filepath.Join(dir, "repo")
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(repo, 0o755)
	git("init", "-q")
	for name, data := range map[string]string{".gitignore": "*.env\n", "tracked.txt": "tracked\n"} {
		os.WriteFile(filepath.Join(repo, name), []byte(data), 0o644)
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("new\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "ignored.env"), []byte("TOKEN=x\n"), 0o600)
	if !strings.HasPrefix(repo, "/") {
		t.Fatal("repo path is not absolute")
	}
	return repo
}
