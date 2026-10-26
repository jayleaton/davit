//go:build unix

package onboarding

import "golang.org/x/sys/unix"

// userWritable reports whether this user may write (or replace entries in) p.
func userWritable(p string) bool { return unix.Access(p, unix.W_OK) == nil }
