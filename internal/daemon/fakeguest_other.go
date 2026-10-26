//go:build !linux

package daemon

import (
	"context"
	"log/slog"

	"davit/internal/runtime"
)

// newFakeGuests is unavailable off Linux: the guest agent is Linux-only, so
// the fake runtime keeps guest transport "none".
func newFakeGuests(string, *slog.Logger) func(context.Context, runtime.Handle) (runtime.Dialer, error) {
	return nil
}
