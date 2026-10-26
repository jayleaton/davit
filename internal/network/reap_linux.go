package network

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Reap kills rootless helper processes leaked by an earlier daemon: every
// live process whose cwd is inside Config.StateDir and that is not the
// holder or egress process (PID and start time) of a handle in keep. It
// waits for them to exit, removes their handle directories and any other
// empty directory under StateDir that keep does not reference, and returns
// how many processes it killed. Processes outside StateDir are never
// touched; without a StateDir there is nothing to find and Reap returns 0.
//
// Run it once at daemon start, before any Create: a Create in flight has a
// holder in StateDir that is not yet in keep and would be killed.
//
// slirp4netns keeps the handle directory as cwd and is found like the
// holder. pasta may leave it when it isolates itself on daemonizing, so it
// is not relied on to be found; it exits by itself once the holder's
// namespace is gone.
func (r *Rootless) Reap(ctx context.Context, keep []Handle) (int, error) {
	if r.cfg.StateDir == "" {
		return 0, nil
	}
	root, err := filepath.EvalSymlinks(r.cfg.StateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("network: reap: %w", err)
	}
	kept := map[Process]bool{}
	busy := map[string]bool{} // handle dirs that must stay
	for _, h := range keep {
		for _, p := range []*Process{h.Holder, h.EgressProc} {
			if p != nil {
				kept[*p] = true
			}
		}
		if name, ok := r.handleDir(h.StateDir); ok {
			busy[name] = true
		}
	}
	found, err := procsUnder(root)
	if err != nil {
		return 0, fmt.Errorf("network: reap: %w", err)
	}
	var errs []error
	var killed []Process
	reapedDirs := map[string]bool{}
	for p, dir := range found {
		if kept[p] {
			busy[dir] = true
			continue
		}
		ok, err := killProcess(p)
		if err != nil {
			errs = append(errs, err)
			busy[dir] = true
			continue
		}
		if ok {
			killed = append(killed, p)
			reapedDirs[dir] = true
		}
	}
	n := 0
	for _, p := range killed {
		if err := waitGone(ctx, p, stopGrace); err != nil {
			errs = append(errs, err)
			busy[found[p]] = true
			continue
		}
		n++
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return n, errors.Join(append(errs, err)...)
	}
	for _, e := range entries {
		if !e.IsDir() || busy[e.Name()] {
			continue
		}
		path := filepath.Join(root, e.Name())
		if reapedDirs[e.Name()] {
			if err := os.RemoveAll(path); err != nil {
				errs = append(errs, err)
			}
		} else {
			_ = os.Remove(path) // only succeeds when empty
		}
	}
	return n, errors.Join(errs...)
}

// handleDir reports the name of a handle's state directory if it lies
// directly under this provider's StateDir.
func (r *Rootless) handleDir(dir string) (string, bool) {
	if r.cfg.StateDir == "" || dir == "" {
		return "", false
	}
	dir = filepath.Clean(dir)
	name := filepath.Base(dir)
	if filepath.Dir(dir) != filepath.Clean(r.cfg.StateDir) || !strings.HasPrefix(name, "h-") {
		return "", false
	}
	return name, true
}

// procsUnder returns every live, non-zombie process (other than this one)
// whose cwd is root or below it, mapped to the first path component under
// root ("" for root itself). Unreadable processes are skipped.
func procsUnder(root string) (map[Process]string, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	out := map[Process]string{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		_, start, err := procStat(pid)
		if err != nil {
			continue
		}
		cwd, err := os.Readlink("/proc/" + e.Name() + "/cwd")
		if err != nil {
			continue
		}
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		rel, err := filepath.Rel(root, cwd)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		p := Process{PID: pid, StartTime: start}
		if !alive(&p) { // gone, zombie, or PID reused since the first read
			continue
		}
		dir := ""
		if rel != "." {
			dir, _, _ = strings.Cut(rel, "/")
		}
		out[p] = dir
	}
	return out, nil
}

// killProcess sends SIGKILL to p if it is still the same process, through
// a pidfd bound to it (see pidfd.Handle), so the signal cannot reach a process
// that reused the PID. It reports whether a signal was sent.
func killProcess(p Process) (bool, error) {
	h, err := openProcess(&p)
	if err != nil || h == nil {
		return false, err
	}
	defer h.Close()
	if err := h.Signal(syscall.SIGKILL); err != nil {
		return false, fmt.Errorf("network: %w", err)
	}
	return true, nil
}

// waitGone polls until p has exited (or is a zombie) or the timeout passes.
func waitGone(ctx context.Context, p Process, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for alive(&p) {
		if time.Now().After(deadline) {
			return fmt.Errorf("network: pid %d did not exit", p.PID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}
