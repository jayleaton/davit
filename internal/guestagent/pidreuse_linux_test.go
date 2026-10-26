package guestagent

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"davit/internal/guestproto"
	"davit/internal/pidns"
)

// startProc runs argv the way exec does (own process group, piped output)
// and returns the proc once run is following it.
func startProc(t *testing.T, argv ...string) *proc {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	outW.Close()
	errW.Close()
	p := &proc{id: newProcID(), pid: cmd.Process.Pid, limit: 1 << 20, subs: map[*subscriber]struct{}{}, exitedCh: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	t.Cleanup(func() { p.kill(unix.SIGKILL) }) // the safe kill: never a reused group
	go p.run(cmd, outR, errR, 0, func() {})
	return p
}

func exitReport(t *testing.T, p *proc) guestproto.ProcessExited {
	t.Helper()
	select {
	case <-p.exitedCh:
	case <-time.After(30 * time.Second):
		t.Fatal("no exit report")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return *p.exited
}

func reaped(p *proc) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reaped
}

func waitReaped(t *testing.T, p *proc) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !reaped(p); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("leader never reaped")
		}
	}
}

// running reports whether pid is a live, non-zombie process.
func running(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	f := strings.Fields(string(b)[strings.LastIndexByte(string(b), ')')+1:])
	return len(f) > 0 && f[0] != "Z" && f[0] != "X"
}

// TestExitReportedWhileGroupLingers: the exit is reported at once from the
// unreaped leader (status decoded from waitid), a lingering child keeps the
// group signalable, and once the leader is reaped kill sends nothing.
func TestExitReportedWhileGroupLingers(t *testing.T) {
	if ex := exitReport(t, startProc(t, "sh", "-c", "kill -KILL $$")); ex.Status != guestproto.StatusKilled || ex.Signal != "KILL" {
		t.Fatalf("killed leader: %+v", ex)
	}

	p := startProc(t, "sh", "-c", "sleep 60 & exit 3")
	if ex := exitReport(t, p); ex.Status != guestproto.StatusExited || ex.ExitCode == nil || *ex.ExitCode != 3 {
		t.Fatalf("exit 3: %+v", ex)
	}
	if reaped(p) || !groupHasOthers(p.pid) {
		t.Fatal("the leader was reaped while its background child still runs")
	}
	p.kill(unix.SIGKILL) // reaches the lingering sleep through the group
	waitReaped(t, p)

	var calls []int
	orig := killGroup
	killGroup = func(pid int, sig unix.Signal) error { calls = append(calls, pid); return nil }
	defer func() { killGroup = orig }()
	p.kill(unix.SIGKILL)
	if len(calls) != 0 {
		t.Fatalf("kill after the reap signalled %v", calls)
	}
}

// TestGroupKillNeverReachesAReusedPID: after a process exited and its PID
// went to a stranger that leads its own group, killing the old process
// (deadline, Ctrl-C, session close) leaves the stranger alone.
func TestGroupKillNeverReachesAReusedPID(t *testing.T) {
	if !pidns.Isolated(t) {
		return
	}
	p := startProc(t, "true")
	exitReport(t, p)
	waitReaped(t, p)

	stranger := exec.Command("sleep", "60")
	stranger.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // leads group p.pid
	pidns.Reuse(t, p.pid, stranger)
	p.kill(unix.SIGKILL)
	time.Sleep(50 * time.Millisecond)
	if !running(stranger.Process.Pid) {
		t.Fatal("killing an exited process reached the group of the process that reused its PID")
	}
}
