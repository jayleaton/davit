//go:build !linux

package preflight

import "errors"

// FreeMemory is not measured on this OS; callers treat errors.ErrUnsupported
// as "no memory bound".
func FreeMemory() (int64, error) { return 0, errors.ErrUnsupported }

// TotalMemory is not measured on this OS.
func TotalMemory() (int64, error) { return 0, errors.ErrUnsupported }
