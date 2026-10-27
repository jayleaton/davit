//go:build linux

package lifecycle

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// preallocate reserves size bytes of real blocks for f.
func preallocate(f *os.File, size int64) error {
	err := unix.Fallocate(int(f.Fd()), 0, 0, size)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		return errors.ErrUnsupported
	}
	return err
}
