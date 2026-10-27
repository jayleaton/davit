package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"syscall"

	"davit/internal/domain"
)

// The save reserve (Limits.SaveReserve): space on the data dir's
// filesystem set aside so that stopping a running VM, whose guest flushes
// its page cache to disk and whose disk is then committed, never fails for
// lack of space. Admission refuses new work at the floor (disk.go), long
// before free space reaches the reserve; what can still eat into it is
// running guests writing. So a stop that starts with less free space than
// the reserve releases it first, and a commit that fails with ENOSPC
// releases it and tries once more. The sweeper arms it again once no stop
// is in flight and at least twice its size is free.

// SaveReserve is space held back for stops.
type SaveReserve interface {
	Size() int64
	Armed() bool
	// Arm sets the space aside (a no-op when armed), giving up when ctx
	// ends.
	Arm(ctx context.Context) error
	// Release frees it, reporting whether it was armed.
	Release() bool
}

// FileReserve is a SaveReserve kept as a preallocated file.
type FileReserve struct {
	Path  string
	Bytes int64
	mu    sync.Mutex
}

func (r *FileReserve) Size() int64 { return r.Bytes }

func (r *FileReserve) Armed() bool {
	fi, err := os.Stat(r.Path)
	return err == nil && fi.Size() == r.Bytes
}

// Arm preallocates the file (fallocate where the filesystem supports it,
// else incompressible bytes, so compressing filesystems really set the
// space aside) and moves it into place.
func (r *FileReserve) Arm(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fi, err := os.Stat(r.Path); err == nil && fi.Size() == r.Bytes {
		return nil
	}
	tmp := r.Path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err = preallocate(f, r.Bytes); errors.Is(err, errors.ErrUnsupported) {
		err = fillRandom(ctx, f, r.Bytes)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, r.Path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func (r *FileReserve) Release() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return os.Remove(r.Path) == nil
}

func fillRandom(ctx context.Context, f *os.File, size int64) error {
	buf := make([]byte, 1<<20)
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	for written := int64(0); written < size; written += int64(len(buf)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		for i := 0; i+8 <= len(buf); i += 8 {
			v := rng.Uint64()
			for j := range 8 {
				buf[i+j] = byte(v >> (8 * j))
			}
		}
		if _, err := f.Write(buf[:min(int64(len(buf)), size-written)]); err != nil {
			return err
		}
	}
	return nil
}

// diskUsage measures the data dir's filesystem; ok is false when it is not
// measured (no probe, or unsupported on this OS).
func (s *Service) diskUsage() (d domain.DiskUsage, ok bool, err error) {
	if s.cfg.DiskSpace == nil {
		return d, false, nil
	}
	d, err = s.cfg.DiskSpace()
	if errors.Is(err, errors.ErrUnsupported) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("measure free space: %w", err)
	}
	return d, true, nil
}

// beginSave marks a stop in flight and releases the save reserve when free
// space is below its size. Call the returned func when the stop is done.
func (s *Service) beginSave(vm domain.VMID) (done func()) {
	s.mu.Lock()
	s.saving++
	s.mu.Unlock()
	done = func() {
		s.mu.Lock()
		s.saving--
		s.mu.Unlock()
	}
	r := s.cfg.SaveReserve
	if r == nil || !r.Armed() {
		return done
	}
	if d, ok, err := s.diskUsage(); err == nil && ok && d.FreeBytes < r.Size() {
		s.releaseSaveReserve(vm, d.FreeBytes, "free space below the save reserve")
	}
	return done
}

// releaseSaveReserve frees the save reserve for vm's stop.
func (s *Service) releaseSaveReserve(vm domain.VMID, free int64, why string) bool {
	r := s.cfg.SaveReserve
	if r == nil || !r.Release() {
		return false
	}
	s.cfg.Log.Warn("released the save reserve for a stop", "vm", vm, "reason", why,
		"freeBytes", free, "reserveBytes", r.Size())
	return true
}

// rearmSaveReserve arms the save reserve again once no stop is in flight
// and at least twice its size is free (so arming leaves a stop its size).
func (s *Service) rearmSaveReserve() {
	r := s.cfg.SaveReserve
	if r == nil || r.Armed() {
		return
	}
	s.mu.Lock()
	saving := s.saving
	s.mu.Unlock()
	if saving > 0 {
		return
	}
	if d, ok, err := s.diskUsage(); err != nil || !ok || d.FreeBytes < 2*r.Size() {
		return
	}
	if err := r.Arm(s.ctx); err != nil {
		s.cfg.Log.Warn("save reserve not armed", "err", err)
		return
	}
	s.cfg.Log.Info("save reserve armed", "bytes", r.Size())
}

// isNoSpace reports whether err is a filesystem out of space.
func isNoSpace(err error) bool { return errors.Is(err, syscall.ENOSPC) }
