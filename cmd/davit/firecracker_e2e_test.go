//go:build linux

package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"davit/api"
	"davit/internal/cli"
	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/gateway"
	"davit/internal/gateway/gatewaytest"
	"davit/internal/runtime/firecracker"
	"davit/internal/runtime/firecracker/fctest"
	"davit/internal/storage"
)

// fcHost is the real davit daemon on the Firecracker runtime: no test
// hooks; guests run the real agent from this checkout (fctest).
type fcHost struct {
	*host
	fx   fctest.Fixture
	data string
	cfg  config.Daemon
	path string
}

func newFCHost(t *testing.T, edit func(*config.Daemon)) *fcHost {
	return newFCHostIn(t, "", edit)
}

// newFCHostIn is newFCHost with the daemon's data dir in parent (default
// the fixture scratch).
func newFCHostIn(t *testing.T, parent string, edit func(*config.Daemon)) *fcHost {
	fx := fctest.Setup(t)
	if parent == "" {
		parent = fx.Scratch
	}
	data, err := os.MkdirTemp(parent, "e2e-")
	if err != nil {
		t.Fatal(err)
	}
	h := &fcHost{host: newHost(t), fx: fx, data: data, path: filepath.Join(t.TempDir(), "config.json")}
	h.cfg = config.Defaults()
	h.cfg.DataDir, h.cfg.Socket, h.cfg.Runtime, h.cfg.LogLevel = data, h.sock, "firecracker", "debug"
	h.cfg.Images = []config.ImageSource{{Manifest: fx.Manifest, BlobDir: fx.BlobDir}}
	h.cfg.Firecracker.Binary = fx.Binary.Path
	h.cfg.StopGrace = config.Duration(60 * time.Second)
	if edit != nil {
		edit(&h.cfg)
	}
	t.Cleanup(func() {
		// Tear down through the product: delete whatever VMs the test left,
		// then stop the daemon. Any process still running under the data dir
		// afterwards (VMM, namespace holder, egress helper) is a leak.
		h.deleteAll()
		h.stopDaemon()
		if report := fctest.Sweep(data, ""); report != "" {
			t.Errorf("LEAKED PROCESSES under %s after delete + daemon stop (killed):\n%s\ndaemon log tail:\n%s", data, report, tail(h.log.String(), 4000))
		}
		os.RemoveAll(data)
	})
	h.startDaemon()
	return h
}

// deleteAll deletes every VM the daemon still has (best effort).
func (h *fcHost) deleteAll() {
	if h.daemon == nil || h.daemon.ProcessState != nil {
		return
	}
	r := h.cli("list", "--all")
	for i, l := range r.lines {
		var vm api.VM
		if l.Event == "item" && json.Unmarshal(r.raw[i], &vm) == nil && vm.State != domain.StateDeleted {
			h.cli("delete", string(vm.ID), "--yes")
		}
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func (h *fcHost) startDaemon() {
	h.t.Helper()
	b, _ := json.Marshal(h.cfg)
	if err := os.WriteFile(h.path, b, 0o600); err != nil {
		h.t.Fatal(err)
	}
	h.launch(h.path, 60*time.Second)
}

// stopDaemon ends the daemon gracefully (SIGTERM); VMMs keep running.
func (h *fcHost) stopDaemon() {
	if h.daemon == nil {
		return
	}
	select {
	case <-h.daemonDone:
		return
	default:
	}
	h.killed = true // the test ended it
	h.daemon.Process.Signal(syscall.SIGTERM)
	<-h.daemonDone
}

// holders returns the rootless network namespace holder processes (the
// `sleep` that owns the netns) of VMM pid.
func holders(pid int) []int {
	ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid))
	if err != nil {
		return nil
	}
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		p, err := strconv.Atoi(e.Name())
		if err != nil || p == pid {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if n, _ := os.Readlink(filepath.Join("/proc", e.Name(), "ns/net")); n == ns && strings.TrimSpace(string(comm)) == "sleep" {
			out = append(out, p)
		}
	}
	return out
}

func alive(pid int) bool {
	st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	return err == nil && !strings.Contains(string(st), ") Z ")
}

// sh runs script in vm through `davit exec` as the image's default user
// and returns its stdout.
func (h *fcHost) sh(vm string, script string) string {
	h.t.Helper()
	return h.shAs(vm, "", script)
}

// root runs script as root (`davit exec --user root`).
func (h *fcHost) root(vm string, script string) string {
	h.t.Helper()
	return h.shAs(vm, "root", script)
}

func (h *fcHost) shAs(vm, user, script string) string {
	h.t.Helper()
	args := []string{"exec", vm}
	if user != "" {
		args = append(args, "--user", user)
	}
	r := h.cli(append(args, "--", "sh", "-c", script)...)
	if r.code != 0 {
		h.t.Fatalf("%s: %q: exit %d %+v %s", vm, script, r.code, r.last().Error, output(h.t, r, "stderr"))
	}
	return strings.TrimSpace(output(h.t, r, "stdout"))
}

// vmms lists live firecracker processes running under dir.
func vmms(dir string) []int {
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err == nil && strings.HasPrefix(cwd, dir+"/") {
			if st, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat")); err == nil && !strings.Contains(string(st), ") Z ") {
				out = append(out, pid)
			}
		}
	}
	return out
}

const vmShape = "--cpus=1 --memory=512MiB"

func (h *fcHost) davit(args ...string) run {
	return h.cli(append(args, strings.Fields(vmShape)...)...)
}

// tailnetSSH runs script in the guest as `ssh <login>@gateway` from tailnet
// peer p through the daemon's SSH front door, pinning the gateway host key.
func (h *fcHost) tailnetSSH(p gatewaytest.Peer, login, script string) (string, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	addr := net.JoinHostPort(gatewaytest.PeerAddr(h.t, ctx, p, h.cfg.Tailnet.Hostname).String(), "22")
	hostKey, err := gateway.LoadOrCreateHostKey(filepath.Join(h.cfg.TailnetStateDir(), "ssh_host_ed25519_key"))
	if err != nil {
		return "", err
	}
	var c net.Conn
	for range 60 { // the daemon's node may still be finishing DERP setup
		if c, err = p.Srv.Dial(ctx, "tcp", addr); err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return "", err
	}
	cc, chans, reqs, err := ssh.NewClientConn(c, addr, &ssh.ClientConfig{
		User: login, HostKeyCallback: ssh.FixedHostKey(hostKey.PublicKey()), Timeout: 60 * time.Second,
	})
	if err != nil {
		c.Close()
		return "", err
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(script)
	return strings.TrimSpace(string(out)), err
}

func (h *fcHost) mustSSH(p gatewaytest.Peer, login, script string) string {
	h.t.Helper()
	out, err := h.tailnetSSH(p, login, script)
	if err != nil {
		h.t.Fatalf("ssh %s %q: %v: %s", login, script, err, out)
	}
	return out
}

// TestM1Acceptance is the SPEC §8 M1 path on real Firecracker VMs, through
// the real daemon binary with its tailnet gateway on an in-process test
// tailnet (never the real one): a tailnet owner reaches the VM over SSH,
// another peer is refused, and the lifecycle survives a daemon kill.
func TestM1Acceptance(t *testing.T) {
	fctest.Setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	controlURL := gatewaytest.StartControl(t)
	alice := gatewaytest.StartPeer(t, ctx, controlURL, "alice-laptop")
	mallory := gatewaytest.StartPeer(t, ctx, controlURL, "mallory-laptop")
	h := newFCHost(t, func(c *config.Daemon) {
		c.Tailnet = config.Tailnet{Enabled: true, Hostname: "davit-m1", ControlURL: controlURL, OwnerLogins: []string{alice.LoginName}}
	})
	timing := map[string]float64{}
	step := func(name string, f func()) {
		t0 := time.Now()
		f()
		timing[name] = ms(time.Since(t0))
		t.Logf("%s: %s", name, time.Since(t0).Round(time.Millisecond))
	}

	r := h.cli("capabilities")
	var caps api.HostCapabilities
	if r.code != 0 || json.Unmarshal(r.raw[len(r.raw)-1], &caps) != nil {
		t.Fatalf("capabilities: exit %d %+v", r.code, r.last().Error)
	}
	if caps.Runtime.Engine != firecracker.Engine || caps.Runtime.EngineVersion != firecracker.FirecrackerVersion ||
		!caps.Runtime.GoldenRestore || !caps.Storage.FastClone {
		t.Fatalf("capabilities: %+v", caps)
	}

	// new (golden capture on this fresh data dir), then the owner's tailnet SSH.
	var vm api.VM
	step("newIncludingGoldenCapture", func() { vm = h.waited(h.davit("new", "web"), domain.OpCreate) })
	if vm.BootMode != domain.BootModeGoldenRestore {
		t.Fatalf("new booted %s, want golden_restore", vm.BootMode)
	}
	step("firstTailnetSSH", func() {
		if out := h.mustSSH(alice, "web", "id -un; hostname"); out != h.fx.Image.DefaultUser+"\nweb" {
			t.Fatalf("ssh web@gateway: %q", out)
		}
	})
	// The guest NIC sits on its own rootless netns tap (ARP resolves its side;
	// the ruleset drops guest-to-namespace IP); egress is off on this host.
	if out := h.mustSSH(alice, "web", "ping -c1 -W1 172.16.0.1 >/dev/null 2>&1; ip neigh show 172.16.0.1"); !strings.Contains(out, "lladdr") {
		t.Fatalf("guest NIC: %q", out)
	}
	// Warm path: another golden new straight to a tailnet SSH command.
	step("warmNewToTailnetSSH", func() {
		h.waited(h.davit("new", "warm"), domain.OpCreate)
		h.mustSSH(alice, "warm", "true")
	})
	h.waited(h.cli("delete", "warm", "--yes"), domain.OpDelete)

	// cp a file in through the guest agent.
	src := filepath.Join(t.TempDir(), "note.txt")
	os.WriteFile(src, []byte("hello from the host\n"), 0o644)
	step("cpIn", func() {
		if r := h.cli("cp", src, "web:/home/"+h.fx.Image.DefaultUser+"/note.txt"); r.code != 0 {
			t.Fatalf("cp: exit %d %+v", r.code, r.last().Error)
		}
	})
	if out := h.mustSSH(alice, "web", "cat ~/note.txt && sync"); out != "hello from the host" {
		t.Fatalf("cp'd file: %q", out)
	}

	// stop (agent power-off, VMM reaped, disk committed) -> resume keeps it.
	step("stop", func() { h.waited(h.cli("stop", "web"), domain.OpStop) })
	if p := vmms(h.data); len(p) != 0 {
		t.Fatalf("VMM still running after stop: %v", p)
	}
	step("resumeToTailnetSSH", func() {
		vm = h.waited(h.cli("resume", "web"), domain.OpResume)
		if out := h.mustSSH(alice, "web", "cat ~/note.txt"); out != "hello from the host" {
			t.Fatalf("file did not persist across stop/resume: %q", out)
		}
	})
	if vm.BootMode != domain.BootModeColdBoot || vm.InstanceGeneration != 2 {
		t.Fatalf("resume: %s gen %d", vm.BootMode, vm.InstanceGeneration)
	}

	// Three forks of the committed disk, isolated writes.
	for i := range 2 {
		name := fmt.Sprintf("f%d", i)
		step("fork"+name+"ToTailnetSSH", func() {
			h.waited(h.davit("fork", "web", name), domain.OpFork)
			h.mustSSH(alice, name, "echo "+name+" > ~/who && sync")
		})
	}
	// The third fork is interrupted: SIGKILL the daemon mid-operation, restart.
	r = h.cli("fork", "web", "f2", "--no-wait", "--cpus=1", "--memory=512MiB")
	if r.code != 0 || r.lines[0].OperationID == "" {
		t.Fatalf("fork --no-wait: exit %d %+v", r.code, r.last().Error)
	}
	forkOp := string(r.lines[0].OperationID)
	opPhase := func() string {
		var op api.Operation
		if r := h.cli("operations", forkOp); r.code == 0 {
			json.Unmarshal(r.raw[len(r.raw)-1], &op)
		}
		return op.Phase
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		if p := opPhase(); p == "runtime.boot" || p == "readiness" {
			break // the VMM is starting or the barrier is running
		}
		if time.Now().After(deadline) {
			t.Fatalf("fork never reached a boot phase (%q)", opPhase())
		}
	}
	h.daemon.Process.Signal(syscall.SIGKILL)
	h.daemon.Wait()
	step("restartAfterKillToForkReady", func() {
		h.startDaemon()
		for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(200 * time.Millisecond) {
			got := h.info("f2")
			if got.State == domain.StateReady {
				break
			}
			if got.State == domain.StateError || time.Now().After(deadline) {
				t.Fatalf("fork after daemon kill: %s %+v", got.State, got.LastError)
			}
		}
		h.mustSSH(alice, "f2", "echo f2 > ~/who && sync")
	})
	r = h.cli("operations", forkOp, "--watch")
	var journal strings.Builder
	for _, raw := range r.raw {
		journal.Write(raw)
	}
	if r.code != 0 || !strings.Contains(journal.String(), `"phase":"recovered"`) {
		t.Fatalf("killed fork was not replayed from its journal: exit %d", r.code)
	}
	for _, name := range []string{"f0", "f1", "f2"} {
		if out := h.mustSSH(alice, name, "cat ~/who; cat ~/note.txt"); out != name+"\nhello from the host" {
			t.Fatalf("fork %s: %q (writes not isolated or disk not inherited)", name, out)
		}
	}
	if out := h.mustSSH(alice, "web", "cat ~/who 2>/dev/null || echo none"); out != "none" {
		t.Fatalf("source sees fork writes: %q", out)
	}

	// Another tailnet user is refused, over SSH and the API.
	if out, err := h.tailnetSSH(mallory, "web", "true"); err == nil {
		t.Fatalf("mallory reached the guest: %q", out)
	}
	if code := tailnetGET(t, ctx, mallory, h.cfg.Tailnet.Hostname, "/api/v1/vms"); code != http.StatusForbidden {
		t.Fatalf("mallory API: %d", code)
	}

	// Revocation: the owner grant is withdrawn; the next connection fails.
	h.stopDaemon()
	h.cfg.Tailnet.OwnerLogins = []string{"someone-else@example.com"}
	h.startDaemon()
	if out, err := h.tailnetSSH(alice, "web", "true"); err == nil {
		t.Fatalf("revoked owner reached the guest: %q", out)
	}

	// delete leaves nothing: no VMM, namespace holder or instance dir.
	var hs []int
	for _, p := range vmms(h.data) {
		hs = append(hs, holders(p)...)
	}
	if len(hs) == 0 {
		t.Fatal("found no network namespace holders to check")
	}
	step("deleteAll", func() {
		for _, name := range []string{"web", "f0", "f1", "f2"} {
			h.waited(h.cli("delete", name, "--yes"), domain.OpDelete)
		}
	})
	if p := vmms(h.data); len(p) != 0 {
		t.Fatalf("VMMs left after delete: %v", p)
	}
	for _, p := range hs {
		if alive(p) {
			t.Fatalf("network namespace holder %d left after delete", p)
		}
	}
	ents, _ := os.ReadDir(filepath.Join(h.data, "runtime", "firecracker", "instances"))
	if len(ents) != 0 {
		t.Fatalf("instance dirs left after delete: %d", len(ents))
	}
	load, _ := os.ReadFile("/proc/loadavg")
	b, _ := json.MarshalIndent(map[string]any{"date": time.Now().UTC().Format(time.RFC3339), "shape": vmShape,
		"loadavgAfter": strings.TrimSpace(string(load)), "stepsMs": timing}, "", "  ")
	t.Logf("%s", b)
	if out := os.Getenv("DAVIT_M1_TIMINGS"); out != "" {
		os.WriteFile(out, b, 0o644)
	}
}

// tailnetGET returns the status of GET path on the gateway from peer p.
func tailnetGET(t *testing.T, ctx context.Context, p gatewaytest.Peer, host, path string) int {
	t.Helper()
	url := "http://" + gatewaytest.PeerAddr(t, ctx, p, host).String() + path
	for range 40 {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		res, err := p.Srv.HTTPClient().Do(req)
		if err == nil {
			res.Body.Close()
			return res.StatusCode
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("GET %s never answered", url)
	return 0
}

// TestFirecrackerBenchmark measures the M1 latency targets through the CLI.
// It runs only with DAVIT_BENCH=1 and writes JSON to DAVIT_BENCH_OUT.
func TestFirecrackerBenchmark(t *testing.T) {
	if os.Getenv("DAVIT_BENCH") != "1" {
		t.Skip("set DAVIT_BENCH=1")
	}
	h := newFCHost(t, func(c *config.Daemon) {
		// e.g. a tmpfs, to compare live-fork pause cost against the default.
		c.Firecracker.CheckpointDir = os.Getenv("DAVIT_BENCH_CHECKPOINT_DIR")
	})
	newN, forkN := envInt("DAVIT_BENCH_NEW", 20), envInt("DAVIT_BENCH_FORK", 10)

	// First new on a fresh data dir captures the golden: reported apart.
	t0 := time.Now()
	h.waited(h.davit("new", "warm"), domain.OpCreate)
	h.sh("warm", "true")
	capture := time.Since(t0)
	// Exec round trip on a running VM: `davit exec warm -- true`, CLI
	// start to exit (CLI process, API, guest agent over vsock, the command).
	type sample struct{ CLIMs, FirstCmdMs float64 }
	var execs []sample
	for range envInt("DAVIT_BENCH_EXEC", 20) {
		t0 := time.Now()
		h.sh("warm", "true")
		execs = append(execs, sample{FirstCmdMs: ms(time.Since(t0))})
	}
	h.waited(h.cli("stop", "warm"), domain.OpStop)

	measure := func(kind domain.OperationKind, args ...string) (api.VM, sample) {
		t0 := time.Now()
		vm := h.waited(h.davit(args...), kind)
		cli := time.Since(t0)
		h.sh(vm.Name, "true")
		return vm, sample{ms(cli), ms(time.Since(t0))}
	}
	var news, forks, resumes []sample
	for i := range newN {
		vm, s := measure(domain.OpCreate, "new", fmt.Sprintf("n%d", i))
		if vm.BootMode != domain.BootModeGoldenRestore {
			t.Fatalf("sample %d booted %s", i, vm.BootMode)
		}
		news = append(news, s)
		h.waited(h.cli("delete", vm.Name, "--yes"), domain.OpDelete)
	}
	for range envInt("DAVIT_BENCH_RESUME", 10) {
		h.waited(h.cli("stop", "warm"), domain.OpStop)
		t0 := time.Now()
		h.waited(h.cli("resume", "warm"), domain.OpResume)
		cli := time.Since(t0)
		h.sh("warm", "true")
		resumes = append(resumes, sample{ms(cli), ms(time.Since(t0))})
	}
	// Disk forks (cold boot of warm's last committed disk) and live forks of
	// the running warm (RAM + disk at one instant), interleaved so both see
	// the same host load.
	var lives []sample
	var pauses []float64
	for i := range forkN {
		vm, s := measure(domain.OpFork, "fork", "warm", fmt.Sprintf("k%d", i))
		forks = append(forks, s)
		h.waited(h.cli("delete", vm.Name, "--yes"), domain.OpDelete)

		name := fmt.Sprintf("l%d", i)
		t0 := time.Now()
		r := h.davit("fork", "warm", name, "--live")
		vm = h.waited(r, domain.OpFork)
		cli := time.Since(t0)
		h.sh(name, "true")
		lives = append(lives, sample{ms(cli), ms(time.Since(t0))})
		if vm.BootMode != domain.BootModeRAMRestore {
			t.Fatalf("live fork %d booted %s", i, vm.BootMode)
		}
		pauses = append(pauses, sourcePause(t, h, string(r.lines[0].OperationID)))
		h.waited(h.cli("delete", name, "--yes"), domain.OpDelete)
	}
	stats := func(xs []sample, f func(sample) float64) map[string]float64 {
		v := make([]float64, len(xs))
		for i, x := range xs {
			v[i] = f(x)
		}
		slices.Sort(v)
		pct := func(p float64) float64 { return v[int(math.Ceil(p*float64(len(v))))-1] }
		return map[string]float64{"p50": pct(0.5), "p95": pct(0.95), "min": v[0], "max": v[len(v)-1], "n": float64(len(v))}
	}
	pauseSamples := make([]sample, len(pauses))
	for i, p := range pauses {
		pauseSamples[i].FirstCmdMs = p
	}
	load, _ := os.ReadFile("/proc/loadavg")
	res := map[string]any{
		"date": time.Now().UTC().Format(time.RFC3339), "shape": vmShape, "image": h.fx.Image.Ref,
		"checkpointDir": h.cfg.Firecracker.CheckpointDir,
		"firecracker":   h.fx.Binary.Version, "loadavgAfter": strings.TrimSpace(string(load)),
		"firstNewWithGoldenCaptureMs":   ms(capture),
		"newGoldenToFirstCommandMs":     stats(news, func(s sample) float64 { return s.FirstCmdMs }),
		"newGoldenCLIReturnedMs":        stats(news, func(s sample) float64 { return s.CLIMs }),
		"forkColdBootToFirstCommandMs":  stats(forks, func(s sample) float64 { return s.FirstCmdMs }),
		"forkColdBootCLIReturnedMs":     stats(forks, func(s sample) float64 { return s.CLIMs }),
		"newSamplesFirstCommandMs":      collect(news, func(s sample) float64 { return s.FirstCmdMs }),
		"forkSamplesFirstCommandMs":     collect(forks, func(s sample) float64 { return s.FirstCmdMs }),
		"resumeToFirstCommandMs":        stats(resumes, func(s sample) float64 { return s.FirstCmdMs }),
		"liveForkToFirstCommandMs":      stats(lives, func(s sample) float64 { return s.FirstCmdMs }),
		"liveForkCLIReturnedMs":         stats(lives, func(s sample) float64 { return s.CLIMs }),
		"liveForkSamplesFirstCommandMs": collect(lives, func(s sample) float64 { return s.FirstCmdMs }),
		"liveForkSourcePausedMs":        stats(pauseSamples, func(s sample) float64 { return s.FirstCmdMs }),
		"resumeSamplesFirstCommandMs":   collect(resumes, func(s sample) float64 { return s.FirstCmdMs }),
		"execToRunningVMMs":             stats(execs, func(s sample) float64 { return s.FirstCmdMs }),
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("%s", b)
	if out := os.Getenv("DAVIT_BENCH_OUT"); out != "" {
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.waited(h.cli("delete", "warm", "--yes"), domain.OpDelete)
}

// sourcePause reads how long a live fork paused its source from the
// operation's journal ("source paused 312ms").
func sourcePause(t *testing.T, h *fcHost, op string) float64 {
	t.Helper()
	r := h.cli("operations", op, "--watch")
	for _, raw := range r.raw {
		var ev struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &ev) == nil && strings.HasPrefix(ev.Message, "source paused ") {
			t.Logf("%s", ev.Message)
			total, _, _ := strings.Cut(strings.TrimPrefix(ev.Message, "source paused "), " ")
			if d, err := time.ParseDuration(total); err == nil {
				return ms(d)
			}
		}
	}
	t.Fatalf("operation %s has no source pause event", op)
	return 0
}

// TestLiveFork forks a running VM on real Firecracker: the child has the
// parent's processes and unsynced page-cache state, a fresh identity, and
// isolated writes; the parent keeps running.
func TestLiveFork(t *testing.T) {
	h := newFCHost(t, nil)
	h.waited(h.davit("new", "parent"), domain.OpCreate)
	// A service (exec'd processes end with their session) and page-cache
	// state that was never synced to disk.
	h.root("parent", "systemd-run --quiet --unit=sleeper sleep 1000 && echo in-ram > /root/unsynced")
	pmid := h.sh("parent", "cat /etc/machine-id")

	r := h.davit("fork", "parent", "kid", "--live")
	kid := h.waited(r, domain.OpFork)
	if kid.BootMode != domain.BootModeRAMRestore {
		t.Fatalf("live fork booted %s", kid.BootMode)
	}
	t.Logf("source paused %.0f ms", sourcePause(t, h, string(r.lines[0].OperationID)))
	if out := h.root("kid", "cat /root/unsynced; systemctl is-active sleeper; hostname"); out != "in-ram\nactive\nkid" {
		t.Fatalf("child state: %q", out)
	}
	if mid := h.sh("kid", "cat /etc/machine-id"); mid == pmid || mid == "" {
		t.Fatalf("child kept the parent's machine-id %q", mid)
	}
	h.root("kid", "echo kid > /root/who")
	if out := h.root("parent", "cat /root/who 2>/dev/null || echo none; systemctl is-active sleeper; hostname"); out != "none\nactive\nparent" {
		t.Fatalf("parent after live fork: %q", out)
	}
	// Live fork needs a running source and is explicit about it.
	h.waited(h.cli("stop", "kid"), domain.OpStop)
	h.expectError(h.davit("fork", "kid", "kid2", "--live"), cli.ExitConflict, domain.CodeInvalidState)
	for _, n := range []string{"parent", "kid"} {
		h.waited(h.cli("delete", n, "--yes"), domain.OpDelete)
	}
	if m, _ := filepath.Glob(filepath.Join(h.data, "runtime", "firecracker", "checkpoints", "*")); len(m) != 0 {
		t.Fatalf("checkpoints left: %v", m)
	}
}

func ms(d time.Duration) float64 { return math.Round(float64(d.Microseconds())/100) / 10 }

func collect[T any](xs []T, f func(T) float64) []float64 {
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = f(x)
	}
	return out
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n
	}
	return def
}

// TestForkIdentityAndStopDurability (SPEC §5, §6): disk forks get their own
// machine-id and SSH host keys while a resume keeps the VM's own identity,
// and a clean stop persists writes the guest never synced.
func TestForkIdentityAndStopDurability(t *testing.T) {
	h := newFCHost(t, nil)
	const identity = "cat /etc/machine-id; ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub | cut -d' ' -f2"
	h.waited(h.davit("new", "src"), domain.OpCreate)
	// exec defaults to the image's user, who has passwordless sudo (agents
	// apt install); root is an explicit --user root.
	if out := h.sh("src", "id -un; sudo -n true && echo sudo-ok"); out != h.fx.Image.DefaultUser+"\nsudo-ok" {
		t.Fatalf("default exec user: %q", out)
	}
	if out := h.root("src", "id -un"); out != "root" {
		t.Fatalf("exec --user root: %q", out)
	}
	before := h.sh("src", identity)
	// No sync: only the clean shutdown (agent power-off) may flush it.
	h.sh("src", "echo durable > $HOME/unsynced")

	h.waited(h.cli("stop", "src"), domain.OpStop)
	h.waited(h.cli("resume", "src"), domain.OpResume)
	if out := h.sh("src", "cat $HOME/unsynced"); out != "durable" {
		t.Fatalf("unsynced write lost across a clean stop: %q", out)
	}
	if after := h.sh("src", identity); after != before {
		t.Fatalf("resume of the same disk changed its identity:\n%s\n->\n%s", before, after)
	}

	h.waited(h.cli("stop", "src"), domain.OpStop)
	seen := map[string]string{"src": before}
	for _, name := range []string{"f1", "f2"} {
		h.waited(h.davit("fork", "src", name), domain.OpFork)
		id := h.sh(name, identity)
		mid, key, _ := strings.Cut(id, "\n")
		for other, oid := range seen {
			omid, okey, _ := strings.Cut(oid, "\n")
			if mid == omid || key == okey || mid == "" || key == "" {
				t.Fatalf("%s shares identity with %s: %q vs %q", name, other, id, oid)
			}
		}
		seen[name] = id
		if out := h.sh(name, "cat $HOME/unsynced"); out != "durable" {
			t.Fatalf("fork %s lost the committed write: %q", name, out)
		}
	}
	for _, n := range []string{"src", "f1", "f2"} {
		h.waited(h.cli("delete", n, "--yes"), domain.OpDelete)
	}
}

// vmmOf returns the VMM pid of vm's current instance under data.
func vmmOf(t *testing.T, data string, vm domain.VMID) int {
	t.Helper()
	for _, p := range vmms(data) {
		cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", p))
		if strings.Contains(filepath.Base(cwd), string(vm)+".g") {
			return p
		}
	}
	t.Fatalf("no VMM for %s", vm)
	return 0
}

// smaps reads Pss, Private_Dirty, Rss, Swap and SwapPss (MiB) of pid.
func smaps(t *testing.T, pid int) map[string]float64 {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch k {
		case "Pss", "Private_Dirty", "Rss", "Swap", "SwapPss":
			kb, _ := strconv.ParseFloat(strings.Fields(v)[0], 64)
			out[k] = math.Round(kb/1024*10) / 10
		}
	}
	return out
}

// TestDensityBenchmark measures per-VM host memory (SPEC §1: PSS and private
// dirty, not RSS claims) for idle golden VMs, idle disk forks and idle
// live-fork children, n at a time, and the full-copy clone fallback against
// reflink. Opt-in with DAVIT_BENCH=1; JSON to DAVIT_BENCH_OUT.
func TestDensityBenchmark(t *testing.T) {
	if os.Getenv("DAVIT_BENCH") != "1" {
		t.Skip("set DAVIT_BENCH=1")
	}
	h := newFCHost(t, nil)
	n := envInt("DAVIT_BENCH_DENSITY", 5)
	idle := time.Duration(envInt("DAVIT_BENCH_IDLE_SECONDS", 30)) * time.Second
	res := map[string]any{"date": time.Now().UTC().Format(time.RFC3339), "shape": vmShape, "n": n, "idleSeconds": idle.Seconds()}
	load := func() string { b, _ := os.ReadFile("/proc/loadavg"); return strings.TrimSpace(string(b)) }

	measure := func(group string, vms []api.VM) {
		time.Sleep(idle)
		var rows []map[string]float64
		for _, vm := range vms {
			rows = append(rows, smaps(t, vmmOf(t, h.data, vm.ID)))
		}
		agg := map[string]any{"load": load(), "samples": rows}
		for _, k := range []string{"Pss", "Private_Dirty", "Rss", "Swap"} {
			v := make([]float64, len(rows))
			for i, r := range rows {
				v[i] = r[k]
			}
			slices.Sort(v)
			sum := 0.0
			for _, x := range v {
				sum += x
			}
			agg[k] = map[string]float64{"min": v[0], "p50": v[len(v)/2], "max": v[len(v)-1], "mean": math.Round(sum/float64(len(v))*10) / 10}
		}
		res[group] = agg
		t.Logf("%s: %v", group, agg)
	}
	del := func(vms []api.VM) {
		for _, vm := range vms {
			h.waited(h.cli("delete", string(vm.ID), "--yes"), domain.OpDelete)
		}
	}

	// Golden restores share the golden memory file's clean pages.
	h.waited(h.davit("new", "warmup"), domain.OpCreate) // captures the golden
	h.waited(h.cli("delete", "warmup", "--yes"), domain.OpDelete)
	var gold []api.VM
	for i := range n {
		gold = append(gold, h.waited(h.davit("new", fmt.Sprintf("g%d", i)), domain.OpCreate))
	}
	measure("goldenNew", gold)
	del(gold)

	// Disk forks cold boot their own copy of the kernel and userspace.
	h.waited(h.davit("new", "base"), domain.OpCreate)
	h.waited(h.cli("stop", "base"), domain.OpStop)
	var forks []api.VM
	for i := range n {
		forks = append(forks, h.waited(h.davit("fork", "base", fmt.Sprintf("d%d", i)), domain.OpFork))
	}
	measure("diskFork", forks)
	del(forks)

	// Live-fork children each map their own checkpoint (one per fork).
	base := h.waited(h.cli("resume", "base"), domain.OpResume)
	var lives []api.VM
	for i := range n {
		lives = append(lives, h.waited(h.davit("fork", "base", fmt.Sprintf("l%d", i), "--live"), domain.OpFork))
	}
	measure("liveForkChild", lives)
	measure("liveForkSource", []api.VM{base})
	del(lives)
	del([]api.VM{base})

	// Full-copy clone (storage mode "copy", the non-reflink fallback) vs
	// reflink, of the image rootfs grown to the VM disk size.
	size := domain.DefaultShape.DiskBytes
	for _, mode := range []storage.Mode{storage.ModeCopy, storage.ModeReflink} {
		dir, err := os.MkdirTemp(h.fx.Scratch, "clone-")
		if err != nil {
			t.Fatal(err)
		}
		st, err := storage.Open(dir, mode)
		if err != nil {
			t.Fatal(err)
		}
		var ms_ []float64
		for i := range n {
			t0 := time.Now()
			d, err := st.CreateWritable(context.Background(), storage.DiskID(fmt.Sprintf("d%d", i)), h.fx.Image.Rootfs, size)
			if err != nil {
				t.Fatal(err)
			}
			ms_ = append(ms_, ms(time.Since(t0)))
			os.Remove(d.Path)
		}
		os.RemoveAll(dir)
		res["clone_"+string(mode)] = map[string]any{"ms": ms_, "load": load(), "imageApparentBytes": h.fx.Image.RootfsSize, "diskBytes": size}
		t.Logf("clone %s: %v ms", mode, ms_)
	}

	b, _ := json.MarshalIndent(res, "", "  ")
	t.Logf("%s", b)
	if out := os.Getenv("DAVIT_BENCH_OUT"); out != "" {
		os.WriteFile(out, b, 0o644)
	}
}
