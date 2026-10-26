//go:build linux

package guestagent_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"davit/internal/guestagent"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
)

// fakeSystem records what the barrier did to the "OS", in order.
type fakeSystem struct {
	mu       sync.Mutex
	events   []string
	clock    time.Time
	hostname string
	ssh      bool
	// sshGate, when set, holds an ssh.service restart until closed.
	sshGate chan struct{}
}

func (f *fakeSystem) log(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *fakeSystem) SetClock(t time.Time) error {
	f.mu.Lock()
	f.clock = t
	f.mu.Unlock()
	f.log("clock")
	return nil
}

func (f *fakeSystem) SetHostname(n string) error {
	f.mu.Lock()
	f.hostname = n
	f.mu.Unlock()
	f.log("hostname " + n)
	return nil
}

func (f *fakeSystem) RestartUnits(_ context.Context, units []string) error {
	f.mu.Lock()
	gate := f.sshGate
	f.mu.Unlock()
	if gate != nil && slices.Contains(units, "ssh.service") {
		<-gate
	}
	f.log("restart " + strings.Join(units, ","))
	return nil
}

func (f *fakeSystem) SystemState(context.Context) guestproto.ServiceState {
	return guestproto.ServiceReady
}
func (f *fakeSystem) UnitState(_ context.Context, u string) guestproto.ServiceState {
	if u == "docker" {
		return guestproto.ServiceStarting
	}
	return guestproto.ServiceReady
}
func (f *fakeSystem) SSHReady(context.Context) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.ssh }
func (f *fakeSystem) PowerOff(context.Context) error {
	f.log("poweroff")
	return nil
}

func (f *fakeSystem) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

const goldenMachineID = "0123456789abcdef0123456789abcdef"

type env struct {
	t      *testing.T
	root   string
	runDir string
	sys    *fakeSystem
	sock   string
	cancel context.CancelFunc
	done   chan struct{}
	user   string
	// tweak, when set, adjusts each agent's Config before New.
	tweak func(*guestagent.Config)
}

// newEnv starts an agent whose identity files live in a temp root seeded
// like a golden image (machine-id, an rsa + ed25519 host key, stamp of the
// golden VM).
func newEnv(t *testing.T) *env { t.Helper(); return newEnvWith(t, nil) }

// newEnvWith is newEnv with tweak applied to the agent's Config.
func newEnvWith(t *testing.T, tweak func(*guestagent.Config)) *env {
	t.Helper()
	root := t.TempDir()
	for p, c := range map[string]string{
		"etc/machine-id":                   goldenMachineID + "\n",
		"etc/hosts":                        "127.0.0.1\tlocalhost\n127.0.1.1\tgolden\n",
		"etc/ssh/ssh_host_rsa_key":         "golden-rsa-private",
		"etc/ssh/ssh_host_rsa_key.pub":     "ssh-rsa AAAAB3NzaC1yc2E= golden\n",
		"etc/ssh/ssh_host_ed25519_key":     "golden-ed-private",
		"etc/ssh/ssh_host_ed25519_key.pub": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 golden\n",
		"var/lib/davit/identity.json":      `{"vmId":"vm_golden"}`,
	} {
		writeFile(t, filepath.Join(root, p), c)
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, root: root, runDir: t.TempDir(), sys: &fakeSystem{ssh: true}, user: u.Username, tweak: tweak}
	e.start()
	return e
}

func (e *env) start() {
	e.t.Helper()
	cfg := guestagent.Config{Root: e.root, RunDir: e.runDir, System: e.sys, RetainOutput: 1 << 20}
	if e.tweak != nil {
		e.tweak(&cfg)
	}
	a, err := guestagent.New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	e.sock = filepath.Join(e.t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", e.sock)
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel, e.done = cancel, make(chan struct{})
	go func() { defer close(e.done); _ = a.Serve(ctx, l) }()
	e.t.Cleanup(e.stop)
}

// stop shuts the agent down (closing every session) and waits for it.
func (e *env) stop() {
	e.cancel()
	<-e.done
}

func (e *env) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", e.sock)
}

func writeFile(t *testing.T, p, c string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *env) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(e.root, rel))
	return string(b)
}

func secret() []byte {
	b := make([]byte, guestproto.MinSecretSize)
	_, _ = rand.Read(b)
	return b
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var hostTime = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func identity(vm string, gen uint64, rotate bool, next []byte) guestproto.SetIdentity {
	return guestproto.SetIdentity{VMID: vm, Generation: gen, Hostname: guestclient.HostnameFor(vm),
		WallClockUnixNano: hostTime.UnixNano(), RotateMachineID: rotate, RotateSSHHostKeys: rotate,
		NewAgentSecret: next, SSHUserCAKeys: []string{"ssh-ed25519 AAAACA user-ca"}}
}

// ready runs a bootstrap session plus the barrier and returns a client
// authenticated with the rotated secret.
func (e *env) ready(ctx context.Context, vm string, gen uint64) (*guestclient.Client, []byte) {
	e.t.Helper()
	c, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: vm, Generation: gen, Bootstrap: secret()})
	if err != nil {
		e.t.Fatal(err)
	}
	next := secret()
	if _, err := c.SetIdentity(ctx, identity(vm, gen, true, next)); err != nil {
		e.t.Fatal(err)
	}
	return c, next
}

func TestBarrierRotatesIdentityBeforeReadiness(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_fork1", Generation: 1, Bootstrap: secret()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	r, err := c.Readiness(ctx, nil)
	if err != nil || r.Agent {
		t.Fatalf("before barrier: readiness %+v err %v (agent must be false)", r, err)
	}
	if _, err := c.Exec(ctx, guestproto.Exec{Argv: []string{"true"}}, guestclient.ExecIO{}); !errors.Is(err, guestproto.ErrUnexpectedType) {
		t.Fatalf("exec before barrier: err = %v, want refusal", err)
	}
	if len(e.sys.snapshot()) != 0 {
		t.Fatalf("system touched before SetIdentity: %v", e.sys.snapshot())
	}

	oldKey, oldRSA := e.read("etc/ssh/ssh_host_ed25519_key"), e.read("etc/ssh/ssh_host_rsa_key")
	gate := make(chan struct{})
	e.sys.mu.Lock()
	e.sys.sshGate = gate
	e.sys.mu.Unlock()
	id, err := c.SetIdentity(ctx, identity("vm_fork1", 1, true, secret()))
	if err != nil {
		t.Fatal(err)
	}
	// The barrier answers before units restart, but SSH is not ready until
	// sshd restarted with the new keys (the old sshd still answers banners).
	if r, err := c.Readiness(ctx, nil); err != nil || !r.Agent || r.SSH {
		t.Fatalf("before sshd restart: %+v %v (agent ready, ssh not)", r, err)
	}
	close(gate)
	waitSSH(t, ctx, c)

	// Clock first, then identity, then the restarts (in any order).
	ev := e.sys.snapshot()
	if len(ev) != 4 || ev[0] != "clock" || ev[1] != "hostname vm-fork1" ||
		!slices.Contains(ev[2:], "restart systemd-journald.service") || !slices.Contains(ev[2:], "restart ssh.service") {
		t.Fatalf("events = %q", ev)
	}
	if !e.sys.clock.Equal(hostTime) {
		t.Fatalf("clock = %v, want host time %v", e.sys.clock, hostTime)
	}
	mid := strings.TrimSpace(e.read("etc/machine-id"))
	if mid == goldenMachineID || mid != id.MachineID || len(mid) != 32 {
		t.Fatalf("machine-id %q (reported %q) not rotated", mid, id.MachineID)
	}
	// The image shipped an RSA key, so a fresh one replaces it; ECDSA is
	// always generated (images configure it).
	if rsa := e.read("etc/ssh/ssh_host_rsa_key"); rsa == oldRSA || !strings.Contains(rsa, "OPENSSH PRIVATE KEY") {
		t.Fatal("golden rsa host key survived rotation")
	}
	if !strings.Contains(e.read("etc/ssh/ssh_host_ecdsa_key.pub"), "ecdsa-sha2-nistp256 ") {
		t.Fatal("no ecdsa host key")
	}
	newKey := e.read("etc/ssh/ssh_host_ed25519_key")
	if newKey == oldKey || !strings.Contains(newKey, "OPENSSH PRIVATE KEY") {
		t.Fatalf("ed25519 host key not regenerated: %q", newKey)
	}
	if len(id.SSHHostKeyFingerprints) != 3 || !strings.HasPrefix(id.SSHHostKeyFingerprints[0], "SHA256:") {
		t.Fatalf("fingerprints = %v", id.SSHHostKeyFingerprints)
	}
	if got := e.read("etc/hostname"); got != "vm-fork1\n" {
		t.Fatalf("hostname file %q", got)
	}
	if hosts := e.read("etc/hosts"); !strings.Contains(hosts, "127.0.1.1\tvm-fork1") || strings.Contains(hosts, "golden") {
		t.Fatalf("hosts = %q", hosts)
	}
	if conf := e.read("etc/ssh/sshd_config.d/50-davit.conf"); !strings.Contains(conf, "HostKey /etc/ssh/ssh_host_ed25519_key") ||
		!strings.Contains(conf, "TrustedUserCAKeys /etc/ssh/davit_user_ca.pub") {
		t.Fatalf("sshd drop-in = %q", conf)
	}
	if id.VMID != "vm_fork1" || id.Generation != 1 || id.Hostname != "vm-fork1" {
		t.Fatalf("identity result = %+v", id)
	}
	if r, err := c.Readiness(ctx, []string{"docker", "systemd"}); err != nil || !r.Agent || !r.SSH ||
		r.Services["docker"] != guestproto.ServiceStarting || r.Services["systemd"] != guestproto.ServiceReady {
		t.Fatalf("after barrier: %+v %v", r, err)
	}
}

func TestSSHHostKeyIsValidOpenSSH(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed")
	}
	e := newEnv(t) // the golden fixture ships an RSA key, so all three types are made
	ctx := ctxT(t)
	c, _ := e.ready(ctx, "vm_keys", 1)
	defer c.Close()
	c2, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_keys", Generation: 2, Bootstrap: secret()})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	id, err := c2.SetIdentity(ctx, identity("vm_keys", 2, false, nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"ed25519", "ecdsa", "rsa"} {
		key := filepath.Join(e.root, "etc/ssh/ssh_host_"+kind+"_key")
		derived, err := exec.Command(keygen, "-y", "-f", key).Output()
		if err != nil {
			t.Fatalf("%s: ssh-keygen rejected the private key: %v", kind, err)
		}
		pub := strings.Fields(e.read("etc/ssh/ssh_host_" + kind + "_key.pub"))
		if f := strings.Fields(string(derived)); len(f) < 2 || f[1] != pub[1] {
			t.Fatalf("%s: derived public key %q != %q", kind, derived, pub)
		}
		fp, err := exec.Command(keygen, "-l", "-E", "sha256", "-f", key+".pub").Output()
		if err != nil {
			t.Fatal(err)
		}
		if f := strings.Fields(string(fp)); len(f) < 2 || !slices.Contains(id.SSHHostKeyFingerprints, f[1]) {
			t.Fatalf("%s: ssh-keygen fingerprint %q not in reported %v", kind, fp, id.SSHHostKeyFingerprints)
		}
	}
}

func TestResumeKeepsIdentityForeignDiskRotates(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, _ := e.ready(ctx, "vm_a", 1)
	c.Close()
	mid1, key1 := e.read("etc/machine-id"), e.read("etc/ssh/ssh_host_ed25519_key")

	// Resume of the same disk VM: new generation, no rotation requested.
	c, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_a", Generation: 2, Bootstrap: secret()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetIdentity(ctx, identity("vm_a", 2, false, nil)); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if e.read("etc/machine-id") != mid1 || e.read("etc/ssh/ssh_host_ed25519_key") != key1 {
		t.Fatal("resume of the same VM rotated its identity")
	}

	// The same disk booted as another VM (fork whose host plan forgot to
	// rotate): the stamp names vm_a, so the agent rotates anyway.
	c, err = guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_b", Generation: 1, Bootstrap: secret()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.SetIdentity(ctx, identity("vm_b", 1, false, nil)); err != nil {
		t.Fatal(err)
	}
	if e.read("etc/machine-id") == mid1 || e.read("etc/ssh/ssh_host_ed25519_key") == key1 {
		t.Fatal("foreign disk kept the source VM's identity")
	}
}

func TestHandshakeAndSecretRotation(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	s1 := secret()
	c, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_s", Generation: 3, Bootstrap: s1})
	if err != nil {
		t.Fatal(err)
	}
	if c.Version() != guestproto.ProtocolVersion {
		t.Fatalf("version %d", c.Version())
	}

	// Same bootstrap again is idempotent (lost HelloAck retry); a different
	// one for a provisioned generation is refused.
	again, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_s", Generation: 3, Bootstrap: s1})
	if err != nil {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	again.Close()
	if _, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_s", Generation: 3, Bootstrap: secret()}); !errors.Is(err, guestclient.ErrRejected) {
		t.Fatalf("re-bootstrap with another secret: %v", err)
	}
	// Possession of the held secret authenticates without a bootstrap.
	if c2, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_s", Generation: 3, Secrets: [][]byte{s1}}); err != nil {
		t.Fatal(err)
	} else {
		c2.Close()
	}

	// Rotation: SetIdentity.NewAgentSecret replaces s1.
	s2 := secret()
	if _, err := c.SetIdentity(ctx, identity("vm_s", 3, true, s2)); err != nil {
		t.Fatal(err)
	}
	if _, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_s", Generation: 3, Secrets: [][]byte{s1}}); !errors.Is(err, guestproto.ErrUnauthenticated) {
		t.Fatalf("old secret after rotation: err = %v, want proof failure", err)
	}
	c3, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_s", Generation: 3, Secrets: [][]byte{secret(), s2}})
	if err != nil {
		t.Fatalf("rotated secret (second candidate): %v", err)
	}
	// Identity claims are bound to the session: the guest cannot be told,
	// and cannot answer for, another VM or generation.
	if _, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_other", Generation: 3, Secrets: [][]byte{s2}}); !errors.Is(err, guestclient.ErrRejected) {
		t.Fatalf("other vm without bootstrap: %v", err)
	}
	if _, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_s", Generation: 2, Bootstrap: secret()}); !errors.Is(err, guestclient.ErrRejected) {
		t.Fatalf("older generation: %v", err)
	}
	if _, err := c3.SetIdentity(ctx, identity("vm_other", 3, true, nil)); !errors.Is(err, guestproto.ErrInvalid) {
		t.Fatalf("SetIdentity for another vm: %v", err)
	}

	// Agent restart in the same boot: the secret survives in RunDir.
	e.stop()
	e.start()
	c4, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_s", Generation: 3, Secrets: [][]byte{s2}})
	if err != nil {
		t.Fatalf("after agent restart: %v", err)
	}
	if r, err := c4.Readiness(ctx, nil); err != nil || !r.Agent {
		t.Fatalf("readiness after agent restart: %+v %v", r, err)
	}

	// A new generation (restore) discards old sessions and resets readiness.
	c5, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_s", Generation: 4, Bootstrap: secret()})
	if err != nil {
		t.Fatal(err)
	}
	defer c5.Close()
	select {
	case <-c4.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("old-generation session not discarded")
	}
	if r, err := c5.Readiness(ctx, nil); err != nil || r.Agent {
		t.Fatalf("new generation readiness before barrier: %+v %v", r, err)
	}
}

func TestVersionMismatchIsTyped(t *testing.T) {
	e := newEnv(t)
	conn, err := e.dial(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pc := guestproto.NewConn(conn)
	h := guestproto.Hello{Versions: guestproto.VersionRange{Min: 7, Max: 9}, VMID: "vm_v", Generation: 1,
		Challenge: guestproto.NewChallenge(), BootstrapSecret: secret()}
	if err := pc.Send(1, h); err != nil {
		t.Fatal(err)
	}
	f, err := pc.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guestproto.Decode[guestproto.HelloAck](f); !errors.Is(err, guestproto.ErrVersionMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestExecStreamingAndExitCodes(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, _ := e.ready(ctx, "vm_x", 1)
	defer c.Close()

	var out, errb bytes.Buffer
	res, err := c.Exec(ctx, guestproto.Exec{Argv: []string{"sh", "-c", `echo out; echo err >&2; echo "$FOO"; exit 3`},
		Env: []string{"FOO=bar"}, Cwd: "/"}, guestclient.ExecIO{Stdout: &out, Stderr: &errb})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != guestproto.StatusExited || res.ExitCode != 3 || out.String() != "out\nbar\n" || errb.String() != "err\n" {
		t.Fatalf("res %+v stdout %q stderr %q", res, out.String(), errb.String())
	}

	// Stdin round trip larger than a chunk, the pipe buffer and the
	// retained-output bound: exercises chunking and backpressure.
	in := make([]byte, 3<<20)
	_, _ = rand.Read(in)
	out.Reset()
	res, err = c.Exec(ctx, guestproto.Exec{Argv: []string{"cat"}}, guestclient.ExecIO{Stdin: bytes.NewReader(in), Stdout: &out})
	if err != nil || res.ExitCode != 0 || !bytes.Equal(out.Bytes(), in) {
		t.Fatalf("cat: res %+v err %v, got %d bytes want %d", res, err, out.Len(), len(in))
	}

	// Missing binary is a typed error, not a crash.
	if _, err := c.Exec(ctx, guestproto.Exec{Argv: []string{"/nonexistent/bin"}}, guestclient.ExecIO{}); !errors.Is(err, guestproto.Errorf(guestproto.CodeNotFound, "")) {
		t.Fatalf("missing binary: %v", err)
	}
	// Killed by a signal.
	res, err = c.Exec(ctx, guestproto.Exec{Argv: []string{"sh", "-c", "kill -TERM $$"}}, guestclient.ExecIO{})
	if err != nil || res.Status != guestproto.StatusKilled || res.Signal != "TERM" || res.ExitCode != -1 {
		t.Fatalf("signalled: %+v %v", res, err)
	}
}

func TestExecDeadlineKillsProcessGroup(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, _ := e.ready(ctx, "vm_t", 1)
	defer c.Close()
	start := time.Now()
	var out bytes.Buffer
	// The child sleep inherits stdout; killing only sh would leave it holding
	// the pipe. The deadline kills the whole group.
	res, err := c.Exec(ctx, guestproto.Exec{Argv: []string{"sh", "-c", "echo started; sleep 30; echo never"}, DeadlineSeconds: 1},
		guestclient.ExecIO{Stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != guestproto.StatusKilled || res.Signal != "KILL" || !res.TimedOut || out.String() != "started\n" {
		t.Fatalf("res %+v out %q", res, out.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("deadline took %v", d)
	}
}

func TestDetachedSurvivesSessionAndAttaches(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, s := e.ready(ctx, "vm_d", 1)
	p, err := c.Start(ctx, guestproto.Exec{Argv: []string{"sh", "-c", "echo a; sleep 0.3; echo b; exit 7"}})
	if err != nil {
		t.Fatal(err)
	}
	// A non-detached process dies with its session.
	pidFile := filepath.Join(t.TempDir(), "pid")
	go func() {
		_, _ = c.Exec(ctx, guestproto.Exec{Argv: []string{"sh", "-c", `echo $$ > "$0"; exec sleep 30`, pidFile}}, guestclient.ExecIO{})
	}()
	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("non-detached process never started")
	}
	c.Close()

	c2, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_d", Generation: 1, Secrets: [][]byte{s}})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	var out bytes.Buffer
	ex, err := c2.Attach(ctx, p.ProcessID, 0, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "a\nb\n" || ex.ExitCode == nil || *ex.ExitCode != 7 {
		t.Fatalf("attach: out %q exit %+v", out.String(), ex)
	}
	st, err := c2.Status(ctx, p.ProcessID)
	if err != nil || st.Status != guestproto.StatusExited || *st.ExitCode != 7 || st.OutputSeq != 2 {
		t.Fatalf("status %+v %v", st, err)
	}
	// Attach from a cursor replays only later output.
	out.Reset()
	if _, err := c2.Attach(ctx, p.ProcessID, 1, &out, io.Discard); err != nil || out.String() != "b\n" {
		t.Fatalf("attach after 1: %q %v", out.String(), err)
	}
	// Attaching at the last output seq still delivers the exit; past it is
	// rejected rather than hanging.
	if ex, err := c2.Attach(ctx, p.ProcessID, 2, io.Discard, io.Discard); err != nil || *ex.ExitCode != 7 {
		t.Fatalf("attach at last output: %+v %v", ex, err)
	}
	if _, err := c2.Attach(ctx, p.ProcessID, 3, io.Discard, io.Discard); !errors.Is(err, guestproto.ErrInvalid) {
		t.Fatalf("attach past output: %v", err)
	}
	alive := func() bool { return syscall.Kill(pid, 0) == nil }
	for time.Now().Before(deadline) && alive() {
		time.Sleep(10 * time.Millisecond)
	}
	if alive() {
		t.Fatalf("non-detached process %d survived its session", pid)
	}
}

func TestFileRoundTrip(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, _ := e.ready(ctx, "vm_f", 1)
	defer c.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	data := make([]byte, 3<<20+17)
	_, _ = rand.Read(data)

	n, err := c.WriteFile(ctx, path, e.user, 0o600, bytes.NewReader(data))
	if err != nil || n != int64(len(data)) {
		t.Fatalf("write: %d %v", n, err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("stat: %v %v", fi, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".davit-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	var got bytes.Buffer
	res, err := c.ReadFile(ctx, path, e.user, 0, 0, &got)
	if err != nil || res.Size != int64(len(data)) || res.Mode != 0o600 || !bytes.Equal(got.Bytes(), data) {
		t.Fatalf("read: %+v %v (%d bytes)", res, err, got.Len())
	}
	got.Reset()
	if _, err := c.ReadFile(ctx, path, e.user, 10, 100, &got); err != nil || !bytes.Equal(got.Bytes(), data[10:110]) {
		t.Fatalf("ranged read: %v", err)
	}
	// Replace keeps the mode when Mode is 0.
	if _, err := c.WriteFile(ctx, path, e.user, 0, strings.NewReader("small")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "small" {
		t.Fatalf("replace: %q", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after replace: %v", fi.Mode())
	}
	if _, err := c.ReadFile(ctx, filepath.Join(dir, "missing"), e.user, 0, 0, io.Discard); !errors.Is(err, guestproto.Errorf(guestproto.CodeNotFound, "")) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := c.ReadFile(ctx, dir, e.user, 0, 0, io.Discard); !errors.Is(err, guestproto.ErrInvalid) {
		t.Fatalf("directory read: %v", err)
	}
	if os.Geteuid() != 0 {
		if _, err := c.ReadFile(ctx, path, "root", 0, 0, io.Discard); !errors.Is(err, guestproto.ErrInvalid) {
			t.Fatalf("unprivileged agent acting as root: %v", err)
		}
	}
}

// transportRuntime gives the fake runtime a guest transport to the agent.
type transportRuntime struct {
	*fake.Runtime
	dial func(ctx context.Context) (net.Conn, error)
}

type dialer func(ctx context.Context) (net.Conn, error)

func (d dialer) DialGuest(ctx context.Context, port uint32) (net.Conn, error) {
	if port != guestagent.Port {
		return nil, errors.New("wrong port")
	}
	return d(ctx)
}

func (r transportRuntime) GuestTransport(context.Context, runtime.Handle) (runtime.Dialer, error) {
	return dialer(r.dial), nil
}

func TestProberRunsFullBarrier(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	fr, err := fake.New(fake.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	rt := transportRuntime{Runtime: fr, dial: e.dial}
	disk := filepath.Join(t.TempDir(), "root.img")
	writeFile(t, disk, "")
	h, err := rt.ColdBoot(ctx, runtime.Instance{Key: runtime.Key{VMID: "vm_probe", Generation: 1}}, runtime.DiskSet{Root: disk}, runtime.NetworkHandle{})
	if err != nil {
		t.Fatal(err)
	}
	store := &guestclient.MemorySecrets{}
	var registered []string
	p := &guestclient.Prober{Secrets: store, Now: func() time.Time { return hostTime }, Poll: time.Millisecond,
		OnIdentity: func(_ context.Context, _ runtime.Key, id guestproto.IdentityResult) error {
			registered = id.SSHHostKeyFingerprints
			return nil
		}}
	rd, err := p.Probe(ctx, rt, h)
	if err != nil {
		t.Fatal(err)
	}
	if !rd.Agent || !rd.SSH || len(registered) != 3 {
		t.Fatalf("readiness %+v registered %v", rd, registered)
	}
	if !e.sys.clock.Equal(hostTime) || strings.TrimSpace(e.read("etc/machine-id")) == goldenMachineID {
		t.Fatal("prober did not run the barrier")
	}
	// One durable write held {rotated, bootstrap}: the rotated secret
	// authenticates, the bootstrap one no longer does.
	sec, _ := store.Load(ctx, h.Key)
	if len(sec) != 2 {
		t.Fatalf("store holds %d secrets", len(sec))
	}
	c, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_probe", Generation: 1, Secrets: sec[:1]})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_probe", Generation: 1, Secrets: sec[1:]}); !errors.Is(err, guestproto.ErrUnauthenticated) {
		t.Fatalf("bootstrap secret after rotation: %v", err)
	}

	// A daemon restart re-probes the same generation: no bootstrap, the
	// stored secret authenticates, the barrier runs again without rotating.
	mid := e.read("etc/machine-id")
	p2 := &guestclient.Prober{Secrets: store, Poll: time.Millisecond,
		Plan: func(context.Context, runtime.Key) (guestclient.Plan, error) {
			return guestclient.Plan{Hostname: "renamed", WaitSSH: true}, nil
		}}
	if _, err := p2.Probe(ctx, rt, h); err != nil {
		t.Fatal(err)
	}
	if e.read("etc/machine-id") != mid || e.read("etc/hostname") != "renamed\n" {
		t.Fatal("re-probe rotated identity or ignored the plan")
	}

	// The host forgot its secret while the guest holds one: the bootstrap
	// is refused rather than re-keying a live generation.
	p3 := &guestclient.Prober{Secrets: &guestclient.MemorySecrets{}, Poll: time.Millisecond}
	if _, err := p3.Probe(ctx, rt, h); err == nil {
		t.Fatal("bootstrap of an already provisioned generation succeeded")
	}
}

func TestArchiveRoundTripAndEscapes(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, _ := e.ready(ctx, "vm_tar", 1)
	defer c.Close()
	if !c.Has(guestproto.FeatureFilesTar) {
		t.Fatal("files.tar not negotiated")
	}
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "top.txt"), "top")
	writeFile(t, filepath.Join(src, "sub/deep/run.sh"), "#!/bin/sh\n")
	os.Chmod(filepath.Join(src, "sub/deep/run.sh"), 0o750)
	os.Chmod(filepath.Join(src, "sub"), 0o710)
	os.Symlink("top.txt", filepath.Join(src, "link"))

	var arch bytes.Buffer
	res, err := c.ReadArchive(ctx, src, "", &arch)
	if err != nil || res.Size != -1 {
		t.Fatalf("read archive: %+v %v", res, err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	if _, err := c.WriteArchive(ctx, dst, "", 0, bytes.NewReader(arch.Bytes())); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub/deep/run.sh")); string(b) != "#!/bin/sh\n" {
		t.Fatalf("nested file %q", b)
	}
	for p, want := range map[string]os.FileMode{"sub/deep/run.sh": 0o750, "sub": 0o710, "top.txt": 0o600} {
		if fi, err := os.Stat(filepath.Join(dst, p)); err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s mode %v %v, want %v", p, fi.Mode(), err, want)
		}
	}
	if l, err := os.Readlink(filepath.Join(dst, "link")); err != nil || l != "top.txt" {
		t.Fatalf("symlink %q %v", l, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(dst), ".davit-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}

	// Hostile archives: escape via .., and writing through a symlink.
	outside := t.TempDir()
	evil := func(entries ...func(*tar.Writer)) []byte {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		for _, e := range entries {
			e(tw)
		}
		tw.Close()
		return b.Bytes()
	}
	file := func(name string) func(*tar.Writer) {
		return func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
			tw.Write([]byte("x"))
		}
	}
	link := func(name, to string) func(*tar.Writer) {
		return func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: name, Linkname: to, Typeflag: tar.TypeSymlink})
		}
	}
	for name, a := range map[string][]byte{
		"dotdot":  evil(file("../escaped")),
		"symlink": evil(link("out", outside), file("out/escaped")),
	} {
		if _, err := c.WriteArchive(ctx, filepath.Join(t.TempDir(), name), "", 0, bytes.NewReader(a)); !errors.Is(err, guestproto.ErrInvalid) {
			t.Errorf("%s archive: err = %v, want invalid_argument", name, err)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(outside, "*")); len(m) != 0 {
		t.Fatalf("archive escaped: %v", m)
	}
}

// halfCloseEcho accepts one connection, reads it to EOF (the host's
// half-close) and only then writes everything back and closes.
func halfCloseEcho(t *testing.T) (port uint16, done <-chan error) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	ch := make(chan error, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			ch <- err
			return
		}
		defer c.Close()
		data, err := io.ReadAll(c)
		if err == nil {
			_, err = c.Write(data)
		}
		ch <- err
	}()
	return uint16(l.Addr().(*net.TCPAddr).Port), ch
}

func TestDialTCP(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	port, served := halfCloseEcho(t)
	s1 := secret()
	c, err := guestclient.Connect(ctx, e.dial, guestclient.Session{VMID: "vm_tcp", Generation: 1, Bootstrap: s1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !c.Has(guestproto.FeatureTCP) {
		t.Fatal("tcp feature not negotiated")
	}
	if _, err := guestclient.DialTCP(ctx, e.dial, guestclient.Session{VMID: "vm_tcp", Generation: 1, Secrets: [][]byte{s1}}, port); !errors.Is(err, guestproto.ErrUnexpectedType) {
		t.Fatalf("dial before barrier: %v", err)
	}
	next := secret()
	if _, err := c.SetIdentity(ctx, identity("vm_tcp", 1, true, next)); err != nil {
		t.Fatal(err)
	}
	sess := guestclient.Session{VMID: "vm_tcp", Generation: 1, Secrets: [][]byte{next}}

	if _, err := guestclient.DialTCP(ctx, e.dial, sess, 0); !errors.Is(err, guestproto.ErrInvalid) {
		t.Fatalf("port 0: %v", err)
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := uint16(closed.Addr().(*net.TCPAddr).Port)
	closed.Close()
	if _, err := guestclient.DialTCP(ctx, e.dial, sess, closedPort); !errors.Is(err, &guestproto.Error{Code: guestproto.CodeNotFound}) {
		t.Fatalf("closed port: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("agent after failed dial: %v", err)
	}

	conn, err := guestclient.DialTCP(ctx, e.dial, sess, port)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := make([]byte, 3*guestproto.MaxChunkSize+17) // larger than any frame
	_, _ = rand.Read(payload)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(conn) // EOF only after the server closed its side
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("echo: %d of %d bytes, %v", len(got), len(payload), err)
	}
	if err := <-served; err != nil {
		t.Fatalf("server: %v", err)
	}
}

// TestDialTCPMustBeOnlyRequest: a session that already made a request
// cannot become a raw stream (another goroutine could still write frames).
func TestDialTCPMustBeOnlyRequest(t *testing.T) {
	e := newEnv(t)
	ctx := ctxT(t)
	c, next := e.ready(ctx, "vm_tcp1", 1)
	defer c.Close()
	port, _ := halfCloseEcho(t)

	conn, err := e.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	pc := guestproto.NewConn(conn)
	h := guestproto.Hello{Versions: guestproto.SupportedVersions, Features: guestclient.Features, VMID: "vm_tcp1",
		Generation: 1, Challenge: guestproto.NewChallenge()}
	if err := pc.Send(1, h); err != nil {
		t.Fatal(err)
	}
	f, err := pc.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	ack, err := guestproto.Decode[guestproto.HelloAck](f)
	if err != nil {
		t.Fatal(err)
	}
	if err := guestproto.VerifyHelloAck(next, h, ack); err != nil {
		t.Fatal(err)
	}
	pc.SetVersion(ack.Version)
	for id, m := range []guestproto.Message{guestproto.Ping{}, guestproto.DialTCP{Port: port}} {
		if err := pc.Send(uint64(id+2), m); err != nil {
			t.Fatal(err)
		}
	}
	if f, err = pc.ReadFrame(); err != nil || f.Type != guestproto.TypePong {
		t.Fatalf("ping: %+v %v", f, err)
	}
	if f, err = pc.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if _, err := guestproto.Decode[guestproto.Ack](f); !errors.Is(err, guestproto.ErrInvalid) {
		t.Fatalf("dial after another request: %v", err)
	}
}

func waitSSH(t *testing.T, ctx context.Context, c *guestclient.Client) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if r, err := c.Readiness(ctx, nil); err == nil && r.SSH {
			return
		}
	}
	t.Fatal("ssh never became ready")
}
