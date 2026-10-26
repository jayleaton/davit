package pidfd

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Stat returns the state letter and start time (clock ticks since boot) of
// pid from /proc/<pid>/stat. The comm field may contain spaces and ')' so
// parsing starts after the last ')'.
func Stat(pid int) (state byte, start uint64, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); field 22 (starttime) is f[19].
	if len(f) < 20 {
		return 0, 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	start, err = strconv.ParseUint(f[19], 10, 64)
	return f[0][0], start, err
}

// Alive reports whether pid is still the process that started at start and
// is not a zombie.
func Alive(pid int, start uint64) bool {
	if pid <= 0 {
		return false
	}
	state, st, err := Stat(pid)
	return err == nil && st == start && state != 'Z' && state != 'X'
}

// Handle signals one process and never another that later gets its PID. It
// holds a pidfd, opened first and only then checked against the recorded
// start time, so the descriptor is bound to that exact process: once it
// exits, signals fail with ESRCH instead of reaching whoever reuses the PID.
type Handle struct {
	pid   int
	start uint64
	fd    int // -1 without pidfd support (kernels before 5.3)
}

// Open returns a handle on the process pid that started at start, or nil if
// it is gone or its PID now names a different process.
func Open(pid int, start uint64) (*Handle, error) {
	if pid <= 0 {
		return nil, nil
	}
	fd, err := unix.PidfdOpen(pid, 0)
	switch {
	case errors.Is(err, syscall.ESRCH):
		return nil, nil
	case errors.Is(err, syscall.ENOSYS):
		fd = -1 // see Signal
	case err != nil:
		return nil, fmt.Errorf("pidfd_open %d: %w", pid, err)
	}
	h := &Handle{pid: pid, start: start, fd: fd}
	if !Alive(pid, start) { // exited, a zombie, or the PID was reused before the open
		h.Close()
		return nil, nil
	}
	return h, nil
}

// Signal sends sig. A process that has exited since the open is not an
// error.
func (h *Handle) Signal(sig syscall.Signal) error {
	var err error
	if h.fd >= 0 {
		err = unix.PidfdSendSignal(h.fd, sig, nil, 0)
	} else if Alive(h.pid, h.start) {
		// No pidfd: recheck the identity right before the signal. A reuse
		// between the two remains possible; kernels this old are not supported
		// for VMs anyway.
		err = syscall.Kill(h.pid, sig)
	}
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal pid %d: %w", h.pid, err)
	}
	return nil
}

// Close releases the pidfd.
func (h *Handle) Close() {
	if h.fd >= 0 {
		_ = unix.Close(h.fd)
		h.fd = -1
	}
}

// Kill sends sig to the process pid that started at start, if it is still
// that process.
func Kill(pid int, start uint64, sig syscall.Signal) error {
	h, err := Open(pid, start)
	if err != nil || h == nil {
		return err
	}
	defer h.Close()
	return h.Signal(sig)
}
