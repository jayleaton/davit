package guestclient

import (
	"context"
	"errors"
	"net"
	"time"

	"davit/internal/guestproto"
	"davit/internal/runtime"
)

// GoldenCaptureVMID is the throwaway identity a golden-capture gate binds.
// Restores always present a different VM ID, so the agent re-bootstraps
// and the barrier rotates everything; the capture secret is random, never
// stored on the host, and useless after capture.
const GoldenCaptureVMID = "vm_golden_capture"

// SettledGate is a golden-capture gate (runtime GuestReady): it returns once
// the guest agent authenticates and systemd reports that boot finished
// (running or degraded; unknown without systemd). A golden captured mid-boot
// resumes with boot jobs still queued, and the restore barrier's sshd
// restart waits behind them. poll defaults to 50ms.
func SettledGate(port uint32, poll time.Duration) func(ctx context.Context, d runtime.Dialer) error {
	if poll == 0 {
		poll = 50 * time.Millisecond
	}
	return func(ctx context.Context, d runtime.Dialer) error {
		dial := func(ctx context.Context) (net.Conn, error) { return d.DialGuest(ctx, port) }
		s := Session{VMID: GoldenCaptureVMID, Generation: 1, Bootstrap: newSecret()}
		var c *Client
		for {
			var err error
			if c, err = Connect(ctx, dial, s); err == nil {
				break
			}
			if errors.Is(err, ErrRejected) || ctx.Err() != nil {
				return err
			}
			if err := sleep(ctx, poll); err != nil {
				return err
			}
		}
		defer c.Close()
		for {
			r, err := c.Readiness(ctx, []string{"systemd"})
			if err != nil {
				return err
			}
			switch r.Services["systemd"] {
			case guestproto.ServiceReady, guestproto.ServiceFailed, guestproto.ServiceUnknown:
				return nil // finished (failed = degraded: boot is over either way)
			}
			if err := sleep(ctx, poll); err != nil {
				return err
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
