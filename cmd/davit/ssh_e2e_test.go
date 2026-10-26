package main_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"davit/internal/cli"
	"davit/internal/domain"
)

// sshEnv sandboxes OpenSSH (no user ~/.ssh config or keys) and points the
// CLI at the test daemon.
func sshEnv(h *host) []string {
	home := filepath.Join(h.dir, "home")
	os.MkdirAll(home, 0o700)
	return append(os.Environ(), "HOME="+home, "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=",
		"XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"))
}

// TestSSHEndToEnd runs real OpenSSH through the API tunnel (ProxyCommand =
// `davit ssh --proxy`) to the fake guest's in-process sshd.
func TestSSHEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no OpenSSH client in PATH")
	}
	h := newHost(t)
	h.guestAgent = true
	h.start(20*time.Millisecond, 0)
	vm := h.waited(h.cli("new", "box"), domain.OpCreate)
	other := h.waited(h.cli("new", "box2"), domain.OpCreate)
	env := sshEnv(h)
	if len(vm.Endpoint.SSHHostKeyFingerprints) == 0 {
		t.Fatalf("endpoint has no host-key fingerprints: %+v", vm.Endpoint)
	}

	run := func(args ...string) (string, string, int) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = env
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

	out, errOut, code := run(bin, "ssh", "box", "--", "echo", "hello-from-guest")
	if code != 0 || out != "hello-from-guest\n" || strings.Contains(errOut, "Permanently added") {
		t.Fatalf("davit ssh: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if _, _, code := run(bin, "ssh", "box", "--", "exit 7"); code != 7 {
		t.Fatalf("guest exit status: got %d, want 7", code)
	}
	// The pinned host key is exactly the one the barrier reported.
	sshDir := filepath.Join(h.dir, "config", "davit", "ssh")
	kh, _ := os.ReadFile(filepath.Join(sshDir, "davit-"+string(vm.ID)+".known_hosts"))
	f := strings.Fields(string(kh))
	if len(f) != 3 || f[0] != "davit-"+string(vm.ID) {
		t.Fatalf("pinned known_hosts: %q", kh)
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(f[1] + " " + f[2]))
	if err != nil || !slices.Contains(vm.Endpoint.SSHHostKeyFingerprints, ssh.FingerprintSHA256(pk)) {
		t.Fatalf("pinned key %q not in endpoint fingerprints %v (%v)", kh, vm.Endpoint.SSHHostKeyFingerprints, err)
	}

	// A certificate is scoped to its VM generation: box's certificate does
	// not log in to box2, even through box2's own tunnel and pinned key.
	if _, errOut, code := run(bin, "ssh", "box2", "--", "true"); code != 0 {
		t.Fatalf("ssh box2: exit %d %s", code, errOut)
	}
	_, errOut, code = run("ssh", "-F", "/dev/null", "-o", "BatchMode=yes",
		"-o", "ProxyCommand="+bin+" ssh --proxy "+string(other.ID),
		"-o", "UserKnownHostsFile="+filepath.Join(sshDir, "davit-"+string(other.ID)+".known_hosts"),
		"-o", "StrictHostKeyChecking=yes", "-o", "IdentitiesOnly=yes",
		"-o", "IdentityFile="+filepath.Join(sshDir, "id_ed25519"),
		"-o", "CertificateFile="+filepath.Join(sshDir, "davit-"+string(vm.ID)+"-cert.pub"),
		"user@davit-"+string(other.ID), "true")
	if code == 0 || !strings.Contains(errOut, "Permission denied") {
		t.Fatalf("box's certificate on box2: exit %d stderr %q", code, errOut)
	}

	// ssh-config makes plain OpenSSH work by VM name.
	r := h.cli("ssh-config")
	var res struct{ Config string }
	json.Unmarshal(r.raw[len(r.raw)-1], &res)
	if r.code != 0 || !strings.Contains(res.Config, "Host davit-*") || !strings.Contains(res.Config, "ssh --proxy %n") {
		t.Fatalf("ssh-config: exit %d %q", r.code, res.Config)
	}
	cfgPath := filepath.Join(h.dir, "ssh_config")
	os.WriteFile(cfgPath, []byte(res.Config), 0o600)
	if out, errOut, code := run("ssh", "-F", cfgPath, "davit-box", "echo", "via-config"); code != 0 || out != "via-config\n" {
		t.Fatalf("ssh -F ssh-config davit-box: exit %d stdout %q stderr %q", code, out, errOut)
	}

	if r := h.cli("ssh", "box"); r.code != cli.ExitUsage {
		t.Fatalf("ssh --json must be refused, got exit %d", r.code)
	}
}

// TestForwardEndToEnd forwards a local port to a TCP port on the fake guest
// (whose loopback is the host's) and checks the --json event stream.
func TestForwardEndToEnd(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(20*time.Millisecond, 0)
	h.waited(h.cli("new", "box"), domain.OpCreate)

	guest, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	go func() {
		for {
			c, err := guest.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	port := guest.Addr().(*net.TCPAddr).Port

	cmd := exec.Command(bin, "--json", "forward", "box", "0:"+strconv.Itoa(port))
	cmd.Env = sshEnv(h)
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	lines := bufio.NewScanner(stdout)
	next := func() (cli.Envelope, json.RawMessage) {
		t.Helper()
		if !lines.Scan() {
			t.Fatalf("forward ended early; stderr %q", stderr.String())
		}
		var e cli.Envelope
		var raw struct{ Data json.RawMessage }
		json.Unmarshal(lines.Bytes(), &e)
		json.Unmarshal(lines.Bytes(), &raw)
		return e, raw.Data
	}
	e, data := next()
	var listening struct {
		Local  string
		Remote int
	}
	json.Unmarshal(data, &listening)
	if e.Event != "forward.listening" || listening.Remote != port || listening.Local == "" {
		t.Fatalf("first event %+v %s", e, data)
	}

	payload := strings.Repeat("ping-", 20000) // several tunnel messages
	c, err := net.Dial("tcp", listening.Local)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(15 * time.Second))
	go func() { io.WriteString(c, payload); c.(*net.TCPConn).CloseWrite() }()
	got, err := io.ReadAll(c)
	c.Close()
	if err != nil || string(got) != payload {
		t.Fatalf("echo through forward: %d bytes, err %v", len(got), err)
	}
	if e, _ := next(); e.Event != "forward.connection" {
		t.Fatalf("want forward.connection, got %+v", e)
	}
	e, data = next()
	var closed struct{ BytesIn, BytesOut int }
	json.Unmarshal(data, &closed)
	if e.Event != "forward.closed" || closed.BytesIn != len(payload) || closed.BytesOut != len(payload) {
		t.Fatalf("forward.closed %+v %s", e, data)
	}

	cmd.Process.Signal(syscall.SIGINT)
	e, data = next()
	if e.Event != "result" || !strings.Contains(string(data), `"connections":1`) {
		t.Fatalf("result after Ctrl-C: %+v %s", e, data)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("forward exit: %v; stderr %q", err, stderr.String())
	}
}
