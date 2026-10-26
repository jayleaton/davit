//go:build linux

package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// platformClone creates dir/name and makes it share src's extents via
// FICLONE. FICLONE fails (typically EOPNOTSUPP, EINVAL or EXDEV) unless both
// files are on the same filesystem and it supports reflinks (btrfs, XFS with
// reflink=1, ...).
var platformClone = cloneStrategy{name: "FICLONE", clone: ficloneInto}

func ficloneInto(src *os.File, dir, name string) error {
	path := filepath.Join(dir, name)
	dst, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// fullSync makes f's data and metadata durable (fsync).
func fullSync(f *os.File) error { return f.Sync() }

// clearWriteFlags is a no-op on Linux: FICLONE clones into a file this
// process created, so no source attributes are inherited.
func clearWriteFlags(*os.File) error { return nil }

// fsName names the filesystem holding path for probe details.
func fsName(path string) string {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "unknown filesystem"
	}
	switch uint64(st.Type) {
	case unix.BTRFS_SUPER_MAGIC:
		return "btrfs"
	case unix.XFS_SUPER_MAGIC:
		return "xfs"
	case unix.EXT4_SUPER_MAGIC:
		return "ext4"
	case unix.TMPFS_MAGIC:
		return "tmpfs"
	case unix.OVERLAYFS_SUPER_MAGIC:
		return "overlayfs"
	case 0x2fc12fc1:
		return "zfs"
	case 0x6969:
		return "nfs"
	}
	return fmt.Sprintf("filesystem 0x%x", uint64(st.Type))
}

// nextData returns the first data extent [start, end) of f at or after off,
// using SEEK_DATA/SEEK_HOLE. Filesystems without extent reporting treat the
// whole file as data, which is still correct.
func nextData(f *os.File, off, size int64) (start, end int64, ok bool, err error) {
	start, err = unix.Seek(int(f.Fd()), off, unix.SEEK_DATA)
	if errors.Is(err, unix.ENXIO) {
		return 0, 0, false, nil
	}
	if err != nil {
		return off, size, true, nil
	}
	end, err = unix.Seek(int(f.Fd()), start, unix.SEEK_HOLE)
	if err != nil || end > size {
		end = size
	}
	return start, end, start < size, nil
}
