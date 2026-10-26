package pidns

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const envKey = "DAVIT_PIDNS_TEST"

// Isolated reports whether t runs inside its private namespaces. Outside, it
// runs t's test function again in them (the child's output is logged), fails
// t if that run fails, and returns false: the caller returns too. It skips t
// where unprivileged user namespaces are not allowed, or are created without
// the right to mount (Ubuntu 24.04's AppArmor policy,
// kernel.apparmor_restrict_unprivileged_userns).
//
//	func TestX(t *testing.T) {
//		if !pidns.Isolated(t) {
//			return
//		}
//		... // runs as PID 1 of a new PID namespace
//	}
func Isolated(t *testing.T) bool {
	t.Helper()
	if os.Getenv(envKey) == t.Name() {
		// Inside: make mounts private, then give /proc to this PID namespace.
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
				t.Skipf("pidns: user namespace without mount rights (AppArmor userns restriction?): %v", err)
			}
			t.Fatalf("pidns: private mounts: %v", err)
		}
		if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			t.Fatalf("pidns: mount /proc: %v", err)
		}
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), envKey+"="+t.Name())
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState == nil { // never started: namespaces not allowed here
		t.Skipf("pidns: no unprivileged user+PID namespaces: %v", err)
	}
	if err == nil && strings.Contains(string(out), "--- SKIP: "+t.Name()+" ") {
		t.Skipf("pidns: skipped in private namespaces:\n%s", out)
	}
	t.Logf("in private namespaces:\n%s", out)
	if err != nil {
		t.Fatalf("pidns: isolated run failed: %v", err)
	}
	return false
}

// Reuse starts cmd as PID pid: the number of a process that has exited and
// been reaped. Only valid inside Isolated, where nothing else forks.
func Reuse(t *testing.T, pid int, cmd *exec.Cmd) {
	t.Helper()
	if err := os.WriteFile("/proc/sys/kernel/ns_last_pid", []byte(strconv.Itoa(pid-1)), 0); err != nil {
		t.Fatalf("pidns: ns_last_pid: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if cmd.Process.Pid != pid {
		t.Fatalf("pidns: got PID %d, want the reused %d", cmd.Process.Pid, pid)
	}
}
