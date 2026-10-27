//go:build linux

package preflight

import "golang.org/x/sys/unix"

func syncfs(fd int) error { return unix.Syncfs(fd) }
