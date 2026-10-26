package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// cloneStrategy is how this host makes a copy-on-write clone. Every strategy
// uses the same contract, the strictest one (APFS clonefile(2)): clone the
// open regular file src into a NEW file called name inside dir, a private
// directory on the store volume, where name does not exist yet. On failure it
// must leave no file at dir/name. The new file may inherit src's metadata
// (permissions, flags); callers fix that up with openPrivate before writing.
//
// FS opens, grows, syncs and publishes the result, so publication, ID
// validation, leases and GC are shared by every strategy.
type cloneStrategy struct {
	name  string // label for probe details and errors: "FICLONE", "clonefile"
	clone func(src *os.File, dir, name string) error
}

// workFile makes the new file dir/name holding src's bytes (src nil = empty)
// and returns it open read-write with owner-only permissions. dir is a
// private temp directory; name must not exist.
//
// In reflink mode the probed strategy clones src and any clone error fails
// the call: a store that proved cloning works never silently copies. The one
// exception is crossFSOK (importing a base image from another filesystem),
// where EXDEV falls back to one sparse copy into the store.
func (f *FS) workFile(ctx context.Context, src *os.File, dir, name string, crossFSOK bool) (*os.File, error) {
	path := filepath.Join(dir, name)
	if src != nil && f.probe.Mode == ModeReflink {
		err := f.clone.clone(src, dir, name)
		if err == nil {
			return openPrivate(path)
		}
		if !(crossFSOK && errors.Is(err, syscall.EXDEV)) {
			return nil, fmt.Errorf("%s: %w", f.clone.name, err)
		}
		// A strategy leaves nothing behind on failure; make sure of it.
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return nil, rerr
		}
	}
	out, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if src != nil {
		if err := sparseCopy(ctx, out, src); err != nil {
			out.Close()
			return nil, err
		}
	}
	return out, nil
}

// openPrivate opens a freshly cloned regular file read-write after making it
// owner-only writable (0600). A clone may inherit a read-only commit's 0444
// mode or write-blocking file flags; the source keeps its own metadata. The
// path must not be a symlink or be replaced while it is opened.
func openPrivate(path string) (*os.File, error) {
	lfi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !lfi.Mode().IsRegular() {
		return nil, fmt.Errorf("clone %s is not a regular file (%v)", path, lfi.Mode().Type())
	}
	ro, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer ro.Close()
	fi, err := ro.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(fi, lfi) {
		return nil, fmt.Errorf("clone %s was replaced while opening", path)
	}
	if err := clearWriteFlags(ro); err != nil {
		return nil, fmt.Errorf("clone %s: %w", path, err)
	}
	if fi.Mode().Perm() != 0o600 {
		if err := ro.Chmod(0o600); err != nil {
			return nil, err
		}
	}
	rw, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("clone %s not writable after chmod 0600: %w", path, err)
	}
	if rfi, err := rw.Stat(); err != nil || !os.SameFile(rfi, fi) {
		rw.Close()
		if err == nil {
			err = fmt.Errorf("clone %s was replaced while opening", path)
		}
		return nil, err
	}
	return rw, nil
}

// probeClone clones a small read-only file inside parent with s, exactly as
// the store will, then verifies the clone's bytes and that a write to the
// clone leaves the source unchanged. All probe files are removed.
func probeClone(parent string, s cloneStrategy) error {
	dir, err := os.MkdirTemp(parent, ".davit-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	want := bytes.Repeat([]byte("davit-probe\n"), 512) // > one 4 KiB block
	srcPath := filepath.Join(dir, "src")
	if err := os.WriteFile(srcPath, want, 0o444); err != nil {
		return err
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := s.clone(src, dir, "dst"); err != nil {
		return err
	}
	dst, err := openPrivate(filepath.Join(dir, "dst"))
	if err != nil {
		return err
	}
	defer dst.Close()
	got, err := io.ReadAll(dst)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("%s succeeded but clone contents differ", s.name)
	}
	if _, err := dst.WriteAt([]byte("independent"), 0); err != nil {
		return err
	}
	if got, err := os.ReadFile(srcPath); err != nil {
		return err
	} else if !bytes.Equal(got, want) {
		return fmt.Errorf("%s: write to clone changed the source", s.name)
	}
	return nil
}
