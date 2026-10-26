//go:build linux

package guestagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"davit/internal/guestproto"
)

// Workspace preparation (feature workspace). One preparation runs at a time
// per agent; another is refused while it runs. Steps run in order: clone
// each repo, write each file, run each setup step; the first failure ends
// the sequence with a failed WorkspaceResult naming the step.
//
// Clones and setup steps run as DefaultUser (setup steps as their own User
// when set) with the request's Env, no terminal prompts, and in their own
// process group, which is killed when a step times out or the session ends.
// A clone is `git clone` of URL into Path, then `git checkout Ref` when a
// Ref is given (a branch, tag or commit). Files are written atomically as
// DefaultUser with parent directories created; Mode 0 means 0600 and only
// the 0777 bits are applied.
//
// Secrets: file contents and Env values are never logged, and progress and
// result messages (which may quote a failing command's output) have every
// Env value and every file's contents (and each of their lines) replaced by
// "[redacted]" before they leave the agent.

const (
	// stepOutputTail bounds the command output quoted in a failure message.
	stepOutputTail = 2 << 10
	redacted       = "[redacted]"
	// minRedact is the shortest secret fragment redacted; shorter ones
	// would mangle ordinary words without protecting anything.
	minRedact = 4
)

// workspaceRun is one preparation's stream.
type workspaceRun struct {
	s       *session
	stream  uint64
	seq     uint64
	secrets []string // longest first
}

func (s *session) prepareWorkspace(id uint64, req guestproto.WorkspacePrepare) {
	if !s.a.workspace.TryLock() {
		s.fail(id, guestproto.Errorf(guestproto.CodeInvalidArgument, "a workspace preparation is already running"))
		return
	}
	w := &workspaceRun{s: s, stream: id, secrets: secretsOf(req)}
	res := func() guestproto.WorkspaceResult {
		defer s.a.workspace.Unlock()
		return w.run(req)
	}()
	res.Revision = req.Revision
	res.Message = w.redact(res.Message)
	// The result is sent only after the lock is released: a client that has
	// seen it may start the next preparation at once and must not be refused.
	w.emit(res)
}

func (w *workspaceRun) run(req guestproto.WorkspacePrepare) guestproto.WorkspaceResult {
	a := w.s.a
	for _, r := range req.Repos {
		if res, ok := w.step(r.Path, guestproto.WorkspacePhaseClone, func(out *tailBuffer) (*int, error) {
			return nil, a.clone(w.s.ctx, r, req.Env, out)
		}); !ok {
			return res
		}
	}
	for _, f := range req.Files {
		if res, ok := w.step(f.Path, guestproto.WorkspacePhaseFile, func(*tailBuffer) (*int, error) {
			return nil, a.writeWorkspaceFile(f)
		}); !ok {
			return res
		}
	}
	for _, st := range req.Setup {
		if res, ok := w.step(st.Name, guestproto.WorkspacePhaseSetup, func(out *tailBuffer) (*int, error) {
			return a.runStep(w.s.ctx, guestproto.Exec{Argv: st.Argv, Cwd: st.Cwd, Env: req.Env, User: st.User},
				time.Duration(st.TimeoutSeconds)*time.Second, out)
		}); !ok {
			return res
		}
	}
	return guestproto.WorkspaceResult{State: guestproto.WorkspaceReady}
}

// step reports running, runs fn (which may write command output to out),
// and reports done or failed; ok false carries the failed result.
func (w *workspaceRun) step(name string, phase guestproto.WorkspacePhase, fn func(out *tailBuffer) (*int, error)) (guestproto.WorkspaceResult, bool) {
	w.emit(guestproto.WorkspaceProgress{Step: name, Phase: phase, State: guestproto.WorkspaceStepRunning})
	// Keep enough output that a secret cut by the tail can be dropped whole
	// (see quote).
	buf := &tailBuffer{max: stepOutputTail}
	if len(w.secrets) > 0 {
		buf.max += len(w.secrets[0])
	}
	code, err := fn(buf)
	if err == nil {
		w.emit(guestproto.WorkspaceProgress{Step: name, Phase: phase, State: guestproto.WorkspaceStepDone})
		return guestproto.WorkspaceResult{}, true
	}
	msg := err.Error()
	var pe *guestproto.Error
	if errors.As(err, &pe) {
		msg = pe.Message
	}
	msg = w.redact(msg)
	if out := strings.TrimSpace(w.quote(buf)); out != "" {
		msg += ": " + out
	}
	w.emit(guestproto.WorkspaceProgress{Step: name, Phase: phase, State: guestproto.WorkspaceStepFailed, Message: msg})
	return guestproto.WorkspaceResult{State: guestproto.WorkspaceFailed, FailedStep: name, ExitCode: code, Message: msg}, false
}

func (w *workspaceRun) emit(m guestproto.Message) {
	w.seq++
	f, err := guestproto.EncodeStream(w.stream, w.seq, m)
	if err == nil {
		w.s.sendStream(f)
	}
}

// secretsOf lists the strings no message may carry: Env values and file
// contents, whole and line by line.
func secretsOf(req guestproto.WorkspacePrepare) []string {
	var out []string
	add := func(v string) {
		if v = strings.TrimSpace(v); len(v) >= minRedact {
			out = append(out, v)
		}
	}
	for _, kv := range req.Env {
		_, v, _ := strings.Cut(kv, "=")
		add(v)
	}
	for _, f := range req.Files {
		add(string(f.Data))
		for _, line := range strings.Split(string(f.Data), "\n") {
			add(line)
		}
	}
	// Longest first, so a secret containing another is replaced whole.
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

func (w *workspaceRun) redact(msg string) string {
	for _, sec := range w.secrets {
		msg = strings.ReplaceAll(msg, sec, redacted)
	}
	return msg
}

// quote returns the redacted tail of a step's output. When the buffer
// dropped older output, a secret may have been cut there, leaving a suffix
// redaction cannot recognise; such a suffix is shorter than the longest
// secret, so that many leading bytes are dropped (moved back to the start
// of any secret straddling the cut, which is then redacted whole).
func (w *workspaceRun) quote(t *tailBuffer) string {
	out, cut := t.contents()
	start := 0
	if cut && len(w.secrets) > 0 {
		start = min(len(w.secrets[0]), len(out))
		for moved := true; moved; {
			moved = false
			for _, sec := range w.secrets {
				for off := 0; off < start; {
					i := strings.Index(out[off:], sec)
					if i < 0 || off+i >= start {
						break
					}
					if i += off; i+len(sec) > start {
						start, moved = i, true
						break
					}
					off = i + 1
				}
			}
		}
	}
	q := w.redact(out[start:])
	if cut || len(q) > stepOutputTail {
		q = "..." + q[max(0, len(q)-stepOutputTail):]
	}
	return q
}

// clone checks out r as DefaultUser, writing git's output to out.
func (a *Agent) clone(ctx context.Context, r guestproto.RepoCheckout, env []string, out *tailBuffer) error {
	if strings.HasPrefix(r.Ref, "-") {
		return guestproto.Errorf(guestproto.CodeInvalidArgument, "invalid ref %q", r.Ref)
	}
	if _, err := a.runStep(ctx, guestproto.Exec{Argv: []string{"git", "clone", "--quiet", "--", r.URL, r.Path}, Env: env}, 0, out); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	if r.Ref == "" {
		return nil
	}
	if _, err := a.runStep(ctx, guestproto.Exec{Argv: []string{"git", "-c", "advice.detachedHead=false", "checkout", "--quiet", r.Ref},
		Cwd: r.Path, Env: env}, 0, out); err != nil {
		return fmt.Errorf("git checkout %s: %w", r.Ref, err)
	}
	return nil
}

// writeWorkspaceFile writes f atomically as DefaultUser.
func (a *Agent) writeWorkspaceFile(f guestproto.WorkspaceFile) error {
	mode := os.FileMode(f.Mode) & 0o777
	if mode == 0 {
		mode = 0o600
	}
	err := asUser(a.cfg.DefaultUser, func() error {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return err
		}
		return writeAtomic(f.Path, f.Data, mode)
	})
	if err != nil {
		return fsError(f.Path, err)
	}
	return nil
}

// runStep runs req to completion in its own process group, writing its
// combined output to out, and returns its exit code when it exited
// non-zero. timeout 0 means none; the session ending kills it too.
func (a *Agent) runStep(ctx context.Context, req guestproto.Exec, timeout time.Duration, out *tailBuffer) (*int, error) {
	req.Env = append([]string{"GIT_TERMINAL_PROMPT=0"}, req.Env...)
	cmd, err := a.command(req)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = out, out
	// A daemon the step started may keep the output pipe open; the step
	// still ends when its own process does.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return nil, guestproto.Errorf(guestproto.CodeNotFound, "start %s: %v", req.Argv[0], unwrapExecErr(err))
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// The group is signalled only while its leader is unreaped (an exited
	// but unreaped leader keeps the group ID reserved), as for exec.
	var mu sync.Mutex
	exited := false
	timedOut := make(chan bool, 1)
	stop := context.AfterFunc(ctx, func() {
		mu.Lock()
		killed := !exited
		if killed {
			_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		}
		mu.Unlock()
		timedOut <- killed && errors.Is(ctx.Err(), context.DeadlineExceeded)
	})
	waitExited(cmd.Process.Pid)
	mu.Lock()
	exited = true
	mu.Unlock()
	err = cmd.Wait()
	if !stop() && <-timedOut {
		return nil, fmt.Errorf("timed out after %v", timeout)
	}
	if err == nil {
		return nil, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return &code, fmt.Errorf("exit status %d", code)
		}
		return nil, fmt.Errorf("%v", ee.ProcessState)
	}
	return nil, err
}

// tailBuffer keeps the last max bytes written to it; cut records that
// older bytes were dropped.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
	cut bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append(t.b[:0], t.b[len(t.b)-t.max:]...)
		t.cut = true
	}
	return len(p), nil
}

func (t *tailBuffer) contents() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b), t.cut
}
