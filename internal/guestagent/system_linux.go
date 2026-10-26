//go:build linux

package guestagent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"davit/internal/guestproto"
)

// OSSystem is the production System: syscalls for clock and hostname,
// systemctl for services (skipped when systemd is not PID 1).
type OSSystem struct {
	// SSHAddr is probed for an SSH banner; default 127.0.0.1:22.
	SSHAddr string
}

var _ System = OSSystem{}

func (OSSystem) SetClock(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

func (OSSystem) SetHostname(name string) error { return unix.Sethostname([]byte(name)) }

func systemdRunning() bool {
	fi, err := os.Stat("/run/systemd/system")
	return err == nil && fi.IsDir()
}

// validUnit rejects names systemctl could parse as options or globs.
func validUnit(u string) bool {
	if u == "" || len(u) > 256 || u[0] == '-' {
		return false
	}
	for _, r := range u {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(":_.@-\\", r)) {
			return false
		}
	}
	return true
}

func systemctl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func (OSSystem) RestartUnits(ctx context.Context, units []string) error {
	if !systemdRunning() {
		return nil
	}
	seen := map[string]bool{}
	for _, u := range units {
		if seen[u] {
			continue
		}
		seen[u] = true
		if !validUnit(u) {
			return fmt.Errorf("invalid unit name %q", u)
		}
		if out, err := systemctl(ctx, "restart", "--", u); err != nil {
			return fmt.Errorf("restart %s: %v: %s", u, err, out)
		}
	}
	return nil
}

func (OSSystem) SystemState(ctx context.Context) guestproto.ServiceState {
	if !systemdRunning() {
		return guestproto.ServiceUnknown
	}
	out, _ := systemctl(ctx, "is-system-running") // non-zero unless "running"
	switch out {
	case "running":
		return guestproto.ServiceReady
	case "initializing", "starting":
		return guestproto.ServiceStarting
	case "degraded", "maintenance":
		return guestproto.ServiceFailed
	}
	return guestproto.ServiceUnknown
}

func (OSSystem) UnitState(ctx context.Context, unit string) guestproto.ServiceState {
	if !systemdRunning() || !validUnit(unit) {
		return guestproto.ServiceUnknown
	}
	out, _ := systemctl(ctx, "is-active", "--", unit)
	switch out {
	case "active":
		return guestproto.ServiceReady
	case "activating", "reloading", "inactive":
		return guestproto.ServiceStarting
	case "failed":
		return guestproto.ServiceFailed
	}
	return guestproto.ServiceUnknown
}

func (o OSSystem) SSHReady(ctx context.Context) bool {
	addr := o.SSHAddr
	if addr == "" {
		addr = "127.0.0.1:22"
	}
	return sshBanner(ctx, addr)
}

// sshBanner reports whether addr greets with an SSH identification string.
func sshBanner(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	var b [4]byte
	_, err = io.ReadFull(c, b[:])
	return err == nil && string(b[:]) == "SSH-"
}

func (OSSystem) PowerOff(ctx context.Context) error {
	if systemdRunning() {
		_, err := systemctl(ctx, "poweroff")
		return err
	}
	unix.Sync()
	return unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
}
