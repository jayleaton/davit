//go:build !linux

package firecracker

import (
	"errors"

	"davit/internal/runtime"
)

// Runtime is unavailable off Linux.
type Runtime struct{ runtime.Runtime }

// New fails: Firecracker needs Linux KVM.
func New(Options) (*Runtime, error) {
	return nil, errors.New("firecracker: runtime requires Linux with KVM")
}

// Version is empty off Linux.
func (r *Runtime) Version() string { return "" }
