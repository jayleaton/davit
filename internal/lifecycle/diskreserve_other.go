//go:build !linux

package lifecycle

import (
	"errors"
	"os"
)

// preallocate is unsupported here; Arm writes the bytes instead.
func preallocate(*os.File, int64) error { return errors.ErrUnsupported }
