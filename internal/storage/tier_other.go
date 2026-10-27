//go:build !linux

package storage

import (
	"context"
	"os"
)

// idleIO is a no-op: only Linux has a per-thread I/O class.
func idleIO() func() { return func() {} }

func dropCache(*os.File) {}

// punchHole writes zeros over [off, off+n) of f.
func punchHole(f *os.File, off, n int64) error {
	_, err := f.WriteAt(make([]byte, n), off)
	return err
}

// networkFS is false: the cold tier is a Linux host feature.
func networkFS(string) bool { return false }

func allocatedOf(fi os.FileInfo) int64 { return fi.Size() }

// deltaBytes is data's size: without extent reporting nothing is known to
// be shared.
func deltaBytes(_ context.Context, data, _ *os.File) (int64, error) {
	fi, err := data.Stat()
	if err != nil {
		return 0, err
	}
	return allocatedOf(fi), nil
}
