//go:build darwin

package mac

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

var hostBootID = func() string {
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil || id == "" {
		return "unknown"
	}
	return id
}()

// birth returns pid's start time in microseconds since the epoch
// (kern.proc.pid p_starttime).
func birth(pid int) (uint64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if kp.Proc.P_pid != int32(pid) {
		return 0, syscall.ESRCH
	}
	t := kp.Proc.P_starttime
	return uint64(t.Sec)*1e6 + uint64(t.Usec), nil
}

const sZomb = 5 // SZOMB in <sys/proc.h>

// running reports whether pid is still the process born at start.
func running(pid int, start uint64) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid != int32(pid) || kp.Proc.P_stat == sZomb {
		return false
	}
	t := kp.Proc.P_starttime
	return uint64(t.Sec)*1e6+uint64(t.Usec) == start
}

// signal delivers sig to pid if it is still the process born at start.
// macOS has no pidfd: the identity is rechecked right before kill(2), which
// leaves only the window between the two calls.
func signal(pid int, start uint64, sig syscall.Signal) error {
	if !running(pid, start) {
		return nil
	}
	if err := unix.Kill(pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}
