//go:build !linux && !darwin

package mac

import (
	"errors"
	"syscall"
)

var hostBootID = "unknown"

var errNoProc = errors.New("mac runtime: process identity is not supported on this platform")

func birth(int) (uint64, error)                { return 0, errNoProc }
func running(int, uint64) bool                 { return false }
func signal(int, uint64, syscall.Signal) error { return errNoProc }
