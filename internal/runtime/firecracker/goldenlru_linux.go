package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"davit/internal/runtime"
)

var _ runtime.GoldenEvictor = (*Runtime)(nil)

// errNoRoom: a capture would take the data dir below the golden floor even
// after evicting every evictable golden. Not remembered as a failure: the
// next create tries again (space may have been freed).
var errNoRoom = errors.New("firecracker: not enough free space for a golden capture")

// OnGoldenEvicted implements runtime.GoldenEvictor.
func (r *Runtime) OnGoldenEvicted(f func(runtime.GoldenEviction)) {
	r.gmu.Lock()
	r.onEvict = f
	r.gmu.Unlock()
}

// EnforceGoldenFloor implements runtime.GoldenEvictor.
func (r *Runtime) EnforceGoldenFloor(ctx context.Context) error {
	_, err := r.enforceFloor(ctx, 0)
	return err
}

// goldenFloor is the free space to keep, or -1 when eviction is off.
func (r *Runtime) goldenFloor() int64 {
	switch {
	case r.opts.GoldenMinFree < 0:
		return -1
	case r.opts.GoldenMinFree == 0:
		return DefaultGoldenMinFree
	}
	return r.opts.GoldenMinFree
}

// freeBytes is the space available to this user on Dir's filesystem.
func (r *Runtime) freeBytes() (int64, error) {
	if r.statFree != nil {
		return r.statFree()
	}
	var st unix.Statfs_t
	if err := unix.Statfs(r.opts.Dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// touchGolden marks golden id as used now (its directory's mtime is the
// LRU clock).
func (r *Runtime) touchGolden(id string) {
	now := time.Now()
	_ = os.Chtimes(r.goldenDir(id), now, now)
}

type lruGolden struct {
	id, dir string
	used    time.Time
	in      goldenInputs
}

// enforceFloor evicts least-recently-used goldens until need more bytes fit
// above the floor, keeping the most recently used golden of each image
// (ImageDigest) so fresh boxes of every image stay fast. It reports whether
// the floor (plus need) is met. Goldens a Prepare holds are skipped.
func (r *Runtime) enforceFloor(ctx context.Context, need int64) (bool, error) {
	floor := r.goldenFloor()
	if floor < 0 {
		return true, nil
	}
	free, err := r.freeBytes()
	if err != nil {
		return false, err
	}
	if free-need >= floor {
		return true, nil
	}
	dirs, err := filepath.Glob(filepath.Join(r.opts.Dir, "golden", "g*"))
	if err != nil {
		return false, err
	}
	var all []lruGolden
	for _, d := range dirs {
		m, err := readGolden(d)
		fi, serr := os.Stat(d)
		if err != nil || m == nil || serr != nil {
			continue // incomplete or unreadable: GCGoldens removes those
		}
		all = append(all, lruGolden{id: filepath.Base(d), dir: d, used: fi.ModTime(), in: m.Inputs})
	}
	// Newest first, so the first of each image is the one it keeps.
	slices.SortFunc(all, func(a, b lruGolden) int {
		if c := b.used.Compare(a.used); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	kept := map[string]bool{}
	var victims []lruGolden
	for _, g := range all {
		if !kept[g.in.ImageDigest] {
			kept[g.in.ImageDigest] = true
			continue
		}
		victims = append(victims, g)
	}
	slices.Reverse(victims) // least recently used first
	for _, g := range victims {
		if free-need >= floor {
			break
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		l := r.goldenLock(g.id)
		if !l.TryLock() {
			continue // a Prepare is using it
		}
		// What the eviction frees is the golden's own files (its memory file
		// above all); the root disk is mostly a reflink of its base, shared.
		// Counted before the delete: XFS releases an unlinked file's blocks
		// in the background, so statfs right after can still miss them, and
		// trusting it evicted a second golden on the test host.
		freed := ownBytes(g.dir)
		err := os.RemoveAll(g.dir)
		l.Unlock()
		if err != nil {
			return false, err
		}
		ev := runtime.GoldenEviction{ID: g.id, Root: g.in.Rootfs, ImageDigest: g.in.ImageDigest, Bytes: freed,
			Reason: fmt.Sprintf("free space %d MiB below the %d MiB golden floor; least recently used (%s)", (free-need)>>20, floor>>20, g.used.UTC().Format(time.RFC3339))}
		free += freed
		r.log.Info("golden evicted", "id", ev.ID, "root", ev.Root, "freed", ev.Bytes, "reason", ev.Reason)
		r.gmu.Lock()
		f := r.onEvict
		delete(r.gFail, g.id)
		r.gmu.Unlock()
		if f != nil {
			f(ev)
		}
	}
	return free-need >= floor, nil
}

// ownBytes is the space a golden's files other than its root disk take on
// disk (allocated blocks): what deleting it gives back for sure.
func ownBytes(dir string) int64 {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var n int64
	for _, e := range ents {
		if e.Name() == "root.img" || e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				n += st.Blocks * 512
			}
		}
	}
	return n
}
