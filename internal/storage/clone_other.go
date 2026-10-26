//go:build !linux && !darwin

package storage

import (
	"errors"
	"fmt"
	"os"
)

// platformClone reports cloning as unsupported so the probe falls back to
// copy honestly. Linux uses FICLONE, Darwin APFS clonefile.
var platformClone = cloneStrategy{name: "clone", clone: func(*os.File, string, string) error { return errNoClone }}

// errNoClone is returned by the strategy on hosts without clone support.
var errNoClone = fmt.Errorf("no copy-on-write clone on this OS: %w", errors.ErrUnsupported)

func fullSync(f *os.File) error { return f.Sync() }

func clearWriteFlags(*os.File) error { return nil }

func fsName(path string) string { return "unknown filesystem" }

// nextData treats the whole remainder of f as data on this OS.
func nextData(_ *os.File, off, size int64) (int64, int64, bool, error) {
	return off, size, off < size, nil
}
