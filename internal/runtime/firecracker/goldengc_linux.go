package firecracker

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// goldenGCEvery bounds how often Prepare runs the golden sweep.
const goldenGCEvery = 10 * time.Minute

// GoldenGCItem is one golden the sweep would remove (or removed).
type GoldenGCItem struct {
	ID     string `json:"id"`
	Bytes  int64  `json:"bytes"` // allocated on disk
	Reason string `json:"reason"`
}

// GoldenGCReport is a golden sweep's result.
type GoldenGCReport struct {
	DryRun  bool           `json:"dryRun"`
	Removed []GoldenGCItem `json:"removed"`
	Kept    int            `json:"kept"`
	Bytes   int64          `json:"bytes"` // removed (or removable) bytes
}

// stale says why a golden can never be restored again, or "" if it can: its
// root (an image rootfs, or a snapshot's committed disk for a project
// image's instances) or kernel is gone (a deleted snapshot's commit is
// collected; a replaced image's blobs are pruned), or it was captured by a
// different engine, host CPU, device layout or network layout, which its
// fingerprint never matches again.
func (r *Runtime) stale(in goldenInputs) string {
	switch {
	case in.Layout != goldenLayout:
		return "device layout changed"
	case in.Engine != Engine+" "+r.version || in.EngineSHA != r.binSHA:
		return "captured by another Firecracker build"
	case in.HostCPU != r.cpu:
		return "captured on another host CPU"
	case in.Network != r.opts.NIC.layout():
		return "network layout changed"
	}
	for what, p := range map[string]string{"root disk": in.Rootfs, "kernel": in.Kernel} {
		if _, err := os.Stat(p); err != nil {
			return what + " " + p + " is gone"
		}
	}
	return ""
}

// GCGoldens removes (with dryRun, only reports) every golden that can never
// be restored again (stale). Instances already restored from one keep
// running: they map their own copy of its memory file's pages.
func (r *Runtime) GCGoldens(ctx context.Context, dryRun bool) (GoldenGCReport, error) {
	rep := GoldenGCReport{DryRun: dryRun, Removed: []GoldenGCItem{}}
	dirs, err := filepath.Glob(filepath.Join(r.opts.Dir, "golden", "g*"))
	if err != nil {
		return rep, err
	}
	slices.Sort(dirs)
	for _, dir := range dirs {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		id := filepath.Base(dir)
		l := r.goldenLock(id)
		l.Lock() // never under a capture or Prepare of the same golden
		m, err := readGolden(dir)
		reason := ""
		switch {
		case err != nil:
			reason = "unreadable golden.json: " + err.Error()
		case m == nil:
			reason = "incomplete (no golden.json)"
		default:
			reason = r.stale(m.Inputs)
		}
		if reason == "" {
			rep.Kept++
			l.Unlock()
			continue
		}
		item := GoldenGCItem{ID: id, Bytes: allocated(dir), Reason: reason}
		if !dryRun {
			if err := os.RemoveAll(dir); err != nil {
				l.Unlock()
				return rep, err
			}
			r.gmu.Lock()
			delete(r.gFail, id)
			r.gmu.Unlock()
			r.log.Info("golden removed", "id", id, "reason", reason, "bytes", item.Bytes)
		}
		l.Unlock()
		rep.Removed = append(rep.Removed, item)
		rep.Bytes += item.Bytes
	}
	return rep, nil
}

// maybeGCGoldens runs the sweep at most every goldenGCEvery (Prepare calls it).
func (r *Runtime) maybeGCGoldens(ctx context.Context) {
	r.gmu.Lock()
	due := time.Since(r.gcAt) >= goldenGCEvery
	if due {
		r.gcAt = time.Now()
	}
	r.gmu.Unlock()
	if !due {
		return
	}
	if rep, err := r.GCGoldens(ctx, false); err != nil {
		r.log.Warn("golden sweep failed", "err", err)
	} else if len(rep.Removed) > 0 {
		r.log.Info("golden sweep", "removed", len(rep.Removed), "bytes", rep.Bytes, "kept", rep.Kept)
	}
	if _, err := r.enforceFloor(ctx, 0); err != nil {
		r.log.Warn("golden floor failed", "err", err)
	}
}

// goldenLock is the per-golden mutex Prepare holds.
func (r *Runtime) goldenLock(id string) *sync.Mutex {
	r.gmu.Lock()
	defer r.gmu.Unlock()
	l := r.gLock[id]
	if l == nil {
		l = &sync.Mutex{}
		r.gLock[id] = l
	}
	return l
}

// allocated is the space dir's files take on disk (sparse files count only
// their written blocks).
func allocated(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				n += st.Blocks * 512
			}
		}
		return nil
	})
	return n
}

// SweepGoldens implements runtime.GoldenSweeper: the golden sweep now.
func (r *Runtime) SweepGoldens(ctx context.Context) (int, int64, error) {
	r.gmu.Lock()
	r.gcAt = time.Now()
	r.gmu.Unlock()
	rep, err := r.GCGoldens(ctx, false)
	if err == nil && len(rep.Removed) > 0 {
		r.log.Info("golden sweep", "removed", len(rep.Removed), "bytes", rep.Bytes, "kept", rep.Kept)
	}
	return len(rep.Removed), rep.Bytes, err
}
