//go:build linux || darwin

package preflight

import "golang.org/x/sys/unix"

// FreeBytes is the space available to unprivileged users under path.
func FreeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
