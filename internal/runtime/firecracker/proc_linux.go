//go:build linux

package firecracker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"davit/internal/pidfd"
)

// procID identifies one process across pid reuse and host reboots.
type procID struct {
	PID   int    `json:"pid"`
	Start uint64 `json:"start"` // /proc/<pid>/stat starttime (clock ticks since boot)
	Boot  string `json:"boot"`  // host boot ID
}

func (p procID) String() string {
	return fmt.Sprintf("fc:%d:%d:%s", p.PID, p.Start, p.Boot)
}

var hostBootID = func() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(b))
}()

// identify reads pid's identity.
func identify(pid int) (procID, error) {
	_, start, err := pidfd.Stat(pid)
	return procID{PID: pid, Start: start, Boot: hostBootID}, err
}

// alive reports whether exactly this process still runs (zombies are dead).
func (p procID) alive() bool {
	return p.Boot == hostBootID && pidfd.Alive(p.PID, p.Start)
}

// signal delivers sig to exactly this process, through a pidfd bound to it
// (pidfd.Handle), so a recycled pid is never signalled.
func (p procID) signal(sig syscall.Signal) error {
	if p.Boot != hostBootID {
		return nil
	}
	return pidfd.Kill(p.PID, p.Start, sig)
}

// waitGone polls until the process has exited or d elapses.
func (p procID) waitGone(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for p.alive() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// findByCwd returns live processes whose working directory is dir. It finds
// a VMM whose launcher started but whose record was never written (daemon
// crash between spawn and record).
func findByCwd(dir string) []procID {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []procID
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil || cwd != dir {
			continue
		}
		if id, err := identify(pid); err == nil && id.alive() {
			out = append(out, id)
		}
	}
	return out
}
