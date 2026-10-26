package network

import (
	"fmt"
	"syscall"
	"time"

	"davit/internal/pidfd"
)

// procStat returns the state letter and start time of pid.
func procStat(pid int) (state byte, start uint64, err error) { return pidfd.Stat(pid) }

// identify captures pid's identity for later liveness checks.
func identify(pid int) (*Process, error) {
	_, start, err := procStat(pid)
	if err != nil {
		return nil, err
	}
	return &Process{PID: pid, StartTime: start}, nil
}

// alive reports whether p still names the same, non-zombie process.
func alive(p *Process) bool {
	return p != nil && pidfd.Alive(p.PID, p.StartTime)
}

// stopProcess terminates p if it is still the same process and waits until
// it is gone. A process that is already gone (or whose PID was reused) is
// success: Destroy must be idempotent and must never signal a stranger.
func stopProcess(p *Process, grace time.Duration) error {
	h, err := openProcess(p)
	if err != nil || h == nil {
		return err
	}
	defer h.Close()
	if err := h.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(grace)
	killed := false
	for alive(p) {
		if time.Now().After(deadline) {
			if killed {
				return fmt.Errorf("pid %d did not exit", p.PID)
			}
			if err := h.Signal(syscall.SIGKILL); err != nil {
				return err
			}
			killed, deadline = true, time.Now().Add(grace)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// openProcess returns a pidfd handle on p (see pidfd.Handle), or nil if p
// is gone or its PID now names a different process.
func openProcess(p *Process) (*pidfd.Handle, error) {
	if p == nil {
		return nil, nil
	}
	return pidfd.Open(p.PID, p.StartTime)
}
