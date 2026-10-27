//go:build darwin

package preflight

import "golang.org/x/sys/unix"

// syncfs flushes the file; macOS has no per-filesystem sync.
func syncfs(fd int) error { return unix.Fsync(fd) }
