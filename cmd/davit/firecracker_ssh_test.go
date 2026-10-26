//go:build linux

package main_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"davit/internal/domain"
	"davit/internal/runtime/firecracker/fctest"
)

// TestFirecrackerSSHScpForward proves the interactive loop on a real VM:
// `davit ssh` runs OpenSSH through the API tunnel with a 5-minute
// certificate for this VM generation and the barrier-pinned host key,
// `davit scp` copies both ways over SFTP, and `davit forward` reaches a
// guest-loopback TCP service (the guest's own sshd).
func TestFirecrackerSSHScpForward(t *testing.T) {
	user := fctest.Setup(t).Image.DefaultUser
	for _, tool := range []string{"ssh", "scp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no OpenSSH %s in PATH", tool)
		}
	}
	h := newFCHost(t, nil)
	h.waited(h.davit("new", "box"), domain.OpCreate)
	env := sshEnv(h.host)
	run := func(stdin string, args ...string) (string, string, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env, cmd.Stdin = env, strings.NewReader(stdin)
		var so, se strings.Builder
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return so.String(), se.String(), code
	}

	t0 := time.Now()
	out, errOut, code := run("", "ssh", "box", "--", "uname", "-a")
	if code != 0 || !strings.HasPrefix(out, "Linux ") || strings.Contains(errOut, "Permanently added") {
		t.Fatalf("davit ssh box -- uname -a: exit %d stdout %q stderr %q", code, out, errOut)
	}
	t.Logf("davit ssh box -- uname -a: %s (%s)", strings.TrimSpace(out), time.Since(t0).Round(time.Millisecond))
	if out, _, code := run("", "ssh", "box", "--", "id", "-un"); code != 0 || strings.TrimSpace(out) != user {
		t.Fatalf("login user: %q exit %d (want the image's user)", out, code)
	}

	// scp both ways (OpenSSH's SFTP protocol to the guest sshd).
	local := t.TempDir()
	src := filepath.Join(local, "up.txt")
	payload := strings.Repeat("davit scp\n", 4096)
	os.WriteFile(src, []byte(payload), 0o644)
	if _, errOut, code := run("", "scp", src, "box:/tmp/up.txt"); code != 0 {
		t.Fatalf("scp up: exit %d %s", code, errOut)
	}
	back := filepath.Join(local, "back.txt")
	if _, errOut, code := run("", "scp", "box:/tmp/up.txt", back); code != 0 {
		t.Fatalf("scp down: exit %d %s", code, errOut)
	}
	if b, _ := os.ReadFile(back); string(b) != payload {
		t.Fatalf("scp round trip: %d bytes back, want %d", len(b), len(payload))
	}

	// forward: a local port to the guest's sshd on its loopback.
	cmd := exec.Command(bin, "--json", "forward", "box", "0:22")
	cmd.Env = env
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() {
		t.Fatalf("forward ended early: %s", stderr.String())
	}
	var ev struct {
		Event string
		Data  struct{ Local string }
	}
	json.Unmarshal(lines.Bytes(), &ev)
	if ev.Event != "forward.listening" {
		t.Fatalf("forward: %s", lines.Text())
	}
	c, err := net.Dial("tcp", ev.Data.Local)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(30 * time.Second))
	banner := make([]byte, 8)
	_, err = io.ReadFull(c, banner)
	c.Close()
	if err != nil || string(banner) != "SSH-2.0-" {
		t.Fatalf("forwarded guest port 22: %q %v", banner, err)
	}
	cmd.Process.Signal(syscall.SIGINT)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("forward exit: %v %s", err, stderr.String())
	}
}
