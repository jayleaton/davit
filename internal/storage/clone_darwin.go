//go:build darwin

package storage

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// platformClone clones with APFS clonefile(2) via fclonefileat: the source
// is the already open file (no source pathname to swap) and the destination
// is a new name relative to an O_NOFOLLOW directory descriptor of the private
// temp dir. clonefile requires the destination to be absent and fails with
// EXDEV across volumes and ENOTSUP on filesystems without cloning. The clone
// inherits the source's mode, flags and ACLs; openPrivate fixes that up.
var platformClone = cloneStrategy{name: "clonefile", clone: clonefileInto}

func clonefileInto(src *os.File, dir, name string) error {
	dfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open clone dir: %w", err)
	}
	defer unix.Close(dfd)
	for {
		err = unix.Fclonefileat(int(src.Fd()), dfd, name, unix.CLONE_NOFOLLOW)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// fullSync flushes f to stable storage with F_FULLFSYNC (fsync on macOS only
// reaches the drive cache). Filesystems that do not implement F_FULLFSYNC
// (some network mounts) fall back to fsync; any other failure is returned so
// publication fails.
func fullSync(f *os.File) error {
	for {
		_, err := unix.FcntlInt(f.Fd(), unix.F_FULLFSYNC, 0)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EINVAL):
			return f.Sync()
		default:
			return fmt.Errorf("F_FULLFSYNC: %w", err)
		}
	}
}

// clearWriteFlags removes owner-settable immutable/append-only flags a clone
// inherited from its source. System flags cannot be cleared by the store
// owner, so they fail the clone instead of leaving an unwritable disk.
func clearWriteFlags(f *os.File) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Flags&(unix.SF_IMMUTABLE|unix.SF_APPEND) != 0 {
		return fmt.Errorf("inherited system file flags 0x%x block writes", st.Flags)
	}
	const user = unix.UF_IMMUTABLE | unix.UF_APPEND
	if st.Flags&user == 0 {
		return nil
	}
	return unix.Fchflags(int(f.Fd()), int(st.Flags&^user))
}

// fsName names the filesystem holding path (e.g. "apfs") for probe details.
func fsName(path string) string {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "unknown filesystem"
	}
	b := st.Fstypename[:]
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	if len(b) == 0 {
		return "unknown filesystem"
	}
	return string(b)
}

// nextData treats the whole remainder of f as data; sparseCopy still skips
// writing all-zero blocks, so the copy stays sparse.
func nextData(_ *os.File, off, size int64) (int64, int64, bool, error) {
	return off, size, off < size, nil
}
