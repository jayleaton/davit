//go:build linux

package firecracker

import (
	"context"
	"fmt"
	"net"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// dialUnix connects to a Unix socket even when its path exceeds sun_path
// (108 bytes): it reaches the socket through /proc/self/fd/<dir fd>/<name>.
// Firecracker binds its sockets relative to its cwd, so only dialers care.
func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	if len(path) < 100 {
		return d.DialContext(ctx, "unix", path)
	}
	fd, err := unix.Open(filepath.Dir(path), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	return d.DialContext(ctx, "unix", fmt.Sprintf("/proc/self/fd/%d/%s", fd, filepath.Base(path)))
}
