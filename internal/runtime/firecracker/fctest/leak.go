//go:build linux

package fctest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"davit/internal/pidfd"
	"davit/internal/runtime"
)

// Recorder wraps a runtime and logs every lifecycle call it forwards with
// its result, for leak reports.
type Recorder struct {
	runtime.Runtime
	mu  sync.Mutex
	log []string
}

// Record wraps rt.
func Record(rt runtime.Runtime) *Recorder { return &Recorder{Runtime: rt} }

func (r *Recorder) note(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log = append(r.log, time.Now().Format("15:04:05.000")+" "+fmt.Sprintf(format, args...))
}

// Calls returns the recorded calls, one per line.
func (r *Recorder) Calls() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.log, "\n")
}

func (r *Recorder) ColdBoot(ctx context.Context, inst runtime.Instance, d runtime.DiskSet, n runtime.NetworkHandle) (runtime.Handle, error) {
	h, err := r.Runtime.ColdBoot(ctx, inst, d, n)
	r.note("ColdBoot %v -> %s %v", inst.Key, h.ProcessIdentity, err)
	return h, err
}

func (r *Recorder) RestoreGolden(ctx context.Context, inst runtime.Instance, g runtime.Golden, d runtime.DiskSet, n runtime.NetworkHandle) (runtime.Handle, error) {
	h, err := r.Runtime.RestoreGolden(ctx, inst, g, d, n)
	r.note("RestoreGolden %v -> %s %v", inst.Key, h.ProcessIdentity, err)
	return h, err
}

func (r *Recorder) RestoreCheckpoint(ctx context.Context, inst runtime.Instance, cp runtime.Checkpoint, d runtime.DiskSet, n runtime.NetworkHandle) (runtime.Handle, error) {
	h, err := r.Runtime.RestoreCheckpoint(ctx, inst, cp, d, n)
	r.note("RestoreCheckpoint %v -> %s %v", inst.Key, h.ProcessIdentity, err)
	return h, err
}

func (r *Recorder) Stop(ctx context.Context, h runtime.Handle, grace time.Duration) error {
	err := r.Runtime.Stop(ctx, h, grace)
	r.note("Stop %v %s grace=%s -> %v", h.Key, h.ProcessIdentity, grace, err)
	return err
}

func (r *Recorder) Kill(ctx context.Context, h runtime.Handle) error {
	err := r.Runtime.Kill(ctx, h)
	r.note("Kill %v %s -> %v", h.Key, h.ProcessIdentity, err)
	return err
}

func (r *Recorder) Destroy(ctx context.Context, h runtime.Handle) error {
	err := r.Runtime.Destroy(ctx, h)
	r.note("Destroy %v %s -> %v", h.Key, h.ProcessIdentity, err)
	return err
}

// Guard fails t at the very end (after every other cleanup, including the
// Destroy calls tests register) if any process still runs with its working
// directory under dir, with a report of each and the runtime calls rec saw
// (rec may be nil). Leftovers are killed either way.
func Guard(t testing.TB, dir string, rec *Recorder) {
	t.Helper()
	t.Cleanup(func() {
		calls := ""
		if rec != nil {
			calls = rec.Calls()
		}
		if report := Sweep(dir, calls); report != "" {
			t.Errorf("LEAKED PROCESSES under %s (killed):\n%s", dir, report)
		}
	})
}

// Sweep kills every live process whose working directory is dir or below it
// (including a deleted one) and returns a report of what it found, or "" if
// nothing was running there.
func Sweep(dir, calls string) string {
	dir, _ = filepath.Abs(dir)
	var b strings.Builder
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil {
			continue
		}
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		if cwd != dir && !strings.HasPrefix(cwd, dir+"/") {
			continue
		}
		state, start, err := pidfd.Stat(pid)
		if err != nil || state == 'Z' || state == 'X' {
			continue // already dead
		}
		stat, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		fmt.Fprintf(&b, "== pid %d cwd %s\n   cmd: %s\n   stat: %s\n", pid, cwd,
			strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " "), strings.TrimSpace(string(stat)))
		if rec, err := os.ReadFile(filepath.Join(cwd, "instance.json")); err == nil {
			fmt.Fprintf(&b, "   instance.json: %s\n", rec)
		}
		if log, err := os.ReadFile(filepath.Join(cwd, "vmm.log")); err == nil {
			lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
			if len(lines) > 50 {
				lines = lines[len(lines)-50:]
			}
			fmt.Fprintf(&b, "   vmm.log (last %d lines):\n      %s\n", len(lines), strings.Join(lines, "\n      "))
		}
		// The process that was scanned, never a later one with its PID.
		pidfd.Kill(pid, start, syscall.SIGKILL)
	}
	if b.Len() == 0 {
		return ""
	}
	if calls != "" {
		fmt.Fprintf(&b, "== runtime calls:\n%s\n", calls)
	}
	return b.String()
}
