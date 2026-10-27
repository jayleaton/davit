//go:build linux || darwin

package preflight

import (
	"golang.org/x/sys/unix"

	"davit/internal/domain"
)

// FreeBytes is the space available to unprivileged users under path.
func FreeBytes(path string) (int64, error) {
	d, err := DiskSpace(path)
	return d.FreeBytes, err
}

// DiskSpace measures the filesystem holding path: the space available to
// unprivileged users and its size.
func DiskSpace(path string) (domain.DiskUsage, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return domain.DiskUsage{}, err
	}
	return domain.DiskUsage{FreeBytes: int64(st.Bavail) * int64(st.Bsize), TotalBytes: int64(st.Blocks) * int64(st.Bsize)}, nil
}

// SyncFS flushes the filesystem holding path, so that its free space
// counts blocks a delete released (XFS frees them in the background).
func SyncFS(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return syncfs(fd)
}

// SameFilesystem reports whether a and b are on one filesystem (false if
// either cannot be read).
func SameFilesystem(a, b string) bool {
	var sa, sb unix.Stat_t
	return unix.Stat(a, &sa) == nil && unix.Stat(b, &sb) == nil && sa.Dev == sb.Dev
}
