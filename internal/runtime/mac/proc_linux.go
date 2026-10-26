//go:build linux

package mac

import (
	"os"
	"strings"
	"syscall"

	"davit/internal/pidfd"
)

// Linux identities serve the fake-helper tests; the runtime ships on macOS.

var hostBootID = func() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}()

// birth returns pid's start time (clock ticks since boot).
func birth(pid int) (uint64, error) {
	_, start, err := pidfd.Stat(pid)
	return start, err
}

// running reports whether pid is still the process born at start.
func running(pid int, start uint64) bool { return pidfd.Alive(pid, start) }

// signal delivers sig to exactly the process born at start (pidfd).
func signal(pid int, start uint64, sig syscall.Signal) error { return pidfd.Kill(pid, start, sig) }
