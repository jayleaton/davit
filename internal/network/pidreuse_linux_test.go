package network

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"davit/internal/pidns"
)

// TestSignalsNeverReachAReusedPID: once a recorded process has exited and
// its PID belongs to a stranger, stopProcess and killProcess leave the
// stranger alone, also when the PID is reused after the handle was opened
// (the window a bare kill(pid) after an identity check leaves open).
func TestSignalsNeverReachAReusedPID(t *testing.T) {
	if !pidns.Isolated(t) {
		return
	}
	// exited starts a process, records it, and lets it exit and be reaped.
	exited := func() *Process {
		t.Helper()
		c := exec.Command("sleep", "60")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		p, err := identify(c.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Process.Kill()
		_ = c.Wait()
		// Start times count clock ticks (10 ms): a stranger started in the
		// same tick under the same PID is indistinguishable by design, and
		// outside this test PIDs only come back after wrapping pid_max.
		time.Sleep(30 * time.Millisecond)
		return p
	}
	// A killed stranger is an unreaped zombie of this test, which a signal 0
	// would still reach; alive reads /proc and counts zombies as dead.
	strangerAlive := func(c *exec.Cmd) bool {
		p, err := identify(c.Process.Pid)
		return err == nil && alive(p)
	}

	// Reused before the identity check.
	old := exited()
	stranger := exec.Command("sleep", "60")
	pidns.Reuse(t, old.PID, stranger)
	if err := stopProcess(old, 100*time.Millisecond); err != nil {
		t.Fatalf("stopProcess of a gone process: %v", err)
	}
	if sent, err := killProcess(*old); err != nil || sent {
		t.Fatalf("killProcess of a gone process: sent %v, %v", sent, err)
	}
	if !strangerAlive(stranger) {
		t.Fatal("the stranger that reused the PID was signalled")
	}

	// Reused after the handle was opened: the pidfd still names the old
	// process, so the signal fails quietly instead of reaching the stranger.
	victim := exec.Command("sleep", "60")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	p, err := identify(victim.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	h, err := openProcess(p)
	if err != nil || h == nil {
		t.Fatalf("openProcess of a live process: %v, %v", h, err)
	}
	defer h.Close()
	_ = victim.Process.Kill()
	_ = victim.Wait()
	stranger2 := exec.Command("sleep", "60")
	pidns.Reuse(t, p.PID, stranger2)
	if err := h.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("signal after the process exited: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if !strangerAlive(stranger2) {
		t.Fatal("a signal through an open handle reached the process that reused the PID")
	}
}
