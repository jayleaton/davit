// Package mac is the macOS runtime adapter (G8, docs/research/MAC-BACKEND.md):
// a CGO-free Go adapter that runs each VM in its own signed helper process
// (Swift, owning one VZVirtualMachine) and controls it over private Unix
// sockets with helperproto.
//
// The adapter itself is platform-neutral and runs on Linux against the Go
// fake helper (fakehelper), where it passes runtimetest; only process
// identity (proc_*.go) and peer credentials (in helperproto) are per-OS.
//
// Capabilities are honest for the first Mac release: cold boot and disk
// fork only. Golden restore, RAM checkpoints and live fork are unsupported
// until they pass on Mac hardware (milestone M5). A zero NetworkHandle (no
// NIC, vsock only) is the only network until the packet-link contract and
// the Darwin netstack land; any other handle is refused.
package mac

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"davit/internal/domain"
	"davit/internal/runtime"
	hp "davit/internal/runtime/mac/helperproto"
)

// Engine names this adapter in handles and capabilities.
const Engine = "mac-vz"

// AgentPort is the guest agent's vsock port.
const AgentPort = 1024

// maxSocketPath is the longest Unix socket path macOS accepts (sun_path
// is 104 bytes with the terminating NUL).
const maxSocketPath = 103

// Options configure the adapter.
type Options struct {
	// Dir holds per-VM helper directories (Dir/vm/<hash>/). It must be
	// short enough for socket paths (New checks).
	Dir string
	// Helper is the signed helper executable.
	Helper string
	// HelperEnv is passed to helpers in addition to a minimal environment
	// (tests set fake-helper knobs here).
	HelperEnv []string
	// Client names the daemon build in helper handshakes.
	Client string
	// StartTimeout bounds a helper's start (record and sockets ready).
	StartTimeout time.Duration
	Log          *slog.Logger
}

// Runtime is the Mac adapter.
type Runtime struct {
	opts  Options
	probe hp.ProbeResult
	log   *slog.Logger

	mu    sync.Mutex
	keyMu map[runtime.Key]*sync.Mutex
}

// Probe runs `helper probe` and validates its answer.
func Probe(ctx context.Context, helper string, env []string) (hp.ProbeResult, error) {
	var p hp.ProbeResult
	cmd := exec.CommandContext(ctx, helper, "probe")
	cmd.Env = helperEnv(env)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return p, fmt.Errorf("mac: %s probe: %v: %s", helper, err, strings.TrimSpace(string(ee.Stderr)))
		}
		return p, fmt.Errorf("mac: %s probe: %w", helper, err)
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return p, fmt.Errorf("mac: %s probe: %w", helper, err)
	}
	if err := p.Validate(); err != nil {
		return p, fmt.Errorf("mac: %s probe: %w", helper, err)
	}
	if _, err := hp.Negotiate(p.MinVersion, p.MaxVersion); err != nil {
		return p, fmt.Errorf("mac: helper %s: %w", p.HelperVersion, err)
	}
	return p, nil
}

func helperEnv(extra []string) []string {
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if h := os.Getenv("HOME"); h != "" {
		env = append(env, "HOME="+h)
	}
	return append(env, extra...)
}

// New probes the helper and returns an adapter over opts.Dir. It fails
// when the helper is missing, speaks no common protocol version, or cannot
// run VMs here (no VZ support or no virtualization entitlement).
func New(opts Options) (*Runtime, error) {
	if opts.Helper == "" {
		return nil, errors.New("mac: no helper configured")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.StartTimeout == 0 {
		opts.StartTimeout = 15 * time.Second
	}
	if opts.Client == "" {
		opts.Client = "davit"
	}
	if err := os.MkdirAll(filepath.Join(opts.Dir, "vm"), 0o700); err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(opts.Dir)
	if err != nil {
		return nil, err
	}
	opts.Dir = dir
	if n := len(filepath.Join(opts.Dir, "vm", strings.Repeat("x", 12), hp.ControlSocket)); n > maxSocketPath {
		return nil, fmt.Errorf("mac: runtime dir %s is too long for helper sockets (%d > %d bytes); configure a shorter one", opts.Dir, n, maxSocketPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := Probe(ctx, opts.Helper, opts.HelperEnv)
	if err != nil {
		return nil, err
	}
	switch {
	case !p.VZSupported:
		return nil, fmt.Errorf("mac: Virtualization.framework reports VMs unsupported on this Mac (helper %s, macOS %s)", p.HelperVersion, p.OSVersion)
	case !p.Entitled:
		return nil, fmt.Errorf("mac: helper %s lacks the com.apple.security.virtualization entitlement (unsigned or ad-hoc build?)", opts.Helper)
	}
	return &Runtime{opts: opts, probe: p, log: opts.Log, keyMu: map[runtime.Key]*sync.Mutex{}}, nil
}

// Capabilities reports what this adapter actually supports.
func (r *Runtime) Capabilities() domain.RuntimeCapabilities {
	p := r.probe
	arch := domain.Arch(p.Arch)
	if p.Arch == "arm64" {
		arch = domain.ArchARM64
	}
	return domain.RuntimeCapabilities{
		Engine:        Engine,
		EngineVersion: p.HelperVersion + " (macOS " + p.OSVersion + " " + p.OSBuild + ")",
		Arch:          []domain.Arch{arch},
		ColdBoot:      true,
		DiskFork:      true,
		// Golden restore, RAM checkpoints and live fork: not until they
		// pass on Mac hardware (MAC-BACKEND §5, milestone M5).
		GuestTransport: "vsock",
		Shape: domain.ShapeConstraints{
			MaxCPUs: p.Limits.MaxCPUs, MinMemoryBytes: p.Limits.MinMemoryBytes, MaxMemoryBytes: p.Limits.MaxMemoryBytes,
			MemoryAlignBytes: p.Limits.MemoryAlignBytes, MinDiskBytes: 64 * domain.MiB,
		},
		HostChecks: []domain.HostCheck{
			{Name: "mac_helper", OK: true, Detail: fmt.Sprintf("%s build %s, protocol %d..%d", p.HelperVersion, p.Build, p.MinVersion, p.MaxVersion)},
			{Name: "virtualization", OK: p.VZSupported && p.Entitled, Detail: fmt.Sprintf("VZ supported %v, entitled %v, macOS %s (%s) %s", p.VZSupported, p.Entitled, p.OSVersion, p.OSBuild, p.Arch)},
		},
	}
}

// Prepare checks the image and shape; there is no golden on this engine.
func (r *Runtime) Prepare(ctx context.Context, img domain.Image, shape domain.Shape) (runtime.Prepared, error) {
	if err := r.checkImage(img); err != nil {
		return runtime.Prepared{}, err
	}
	if err := shape.Validate(r.Capabilities().Shape); err != nil {
		return runtime.Prepared{}, err
	}
	return runtime.Prepared{Image: img, Shape: shape}, nil
}

func (r *Runtime) checkImage(img domain.Image) error {
	if img.Arch != "" && img.Arch != domain.ArchARM64 {
		return domain.Errorf(domain.CodeUnsupportedCapability, "mac runtime runs arm64 guests, image %s is %s", img.ID, img.Arch).
			WithDetail("capability", "arch")
	}
	if !filepath.IsAbs(img.Kernel) {
		return domain.Errorf(domain.CodeInvalidArgument, "mac: image %s has no kernel", img.ID)
	}
	return nil
}

var keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func validKey(k runtime.Key) error {
	if !keyRE.MatchString(string(k.VMID)) || k.Generation < 1 {
		return domain.Errorf(domain.CodeInvalidArgument, "mac: invalid key %v", k)
	}
	return nil
}

// instDir is the helper directory of k: a fixed-length name, so socket
// paths stay short whatever the VM ID.
func (r *Runtime) instDir(k runtime.Key) string {
	sum := sha256.Sum256([]byte(string(k.VMID) + "\x00" + strconv.FormatInt(k.Generation, 10)))
	return filepath.Join(r.opts.Dir, "vm", hex.EncodeToString(sum[:6]))
}

func (r *Runtime) lockKey(k runtime.Key) func() {
	r.mu.Lock()
	m := r.keyMu[k]
	if m == nil {
		m = &sync.Mutex{}
		r.keyMu[k] = m
	}
	r.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// launch is launch.json: the helper process this adapter started, written
// right after the spawn so its identity survives a daemon restart.
type launch struct {
	PID   int    `json:"pid"`
	Birth uint64 `json:"birth"`
	Boot  string `json:"boot"`
}

// instance is everything on disk about one helper directory.
type instance struct {
	dir    string
	rec    *hp.Record
	launch *launch
	exit   *hp.Exit
}

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func loadInstance(dir string) (*instance, error) {
	in := &instance{dir: dir}
	var rec hp.Record
	if ok, err := readJSON(filepath.Join(dir, hp.RecordFile), &rec); err != nil {
		return nil, fmt.Errorf("mac: %s: %w", dir, err)
	} else if ok {
		if err := rec.Validate(); err != nil {
			return nil, fmt.Errorf("mac: %s: %w", dir, err)
		}
		in.rec = &rec
	}
	var l launch
	if ok, err := readJSON(filepath.Join(dir, "launch.json"), &l); err == nil && ok {
		in.launch = &l
	}
	var e hp.Exit
	if ok, err := readJSON(filepath.Join(dir, hp.ExitFile), &e); err == nil && ok && in.rec != nil && e.Incarnation == in.rec.Incarnation {
		in.exit = &e
	}
	return in, nil
}

// proc is the helper's process identity: from launch.json, or (a helper
// whose launch record was lost) its pid from helper.json, with the birth
// time read now; the caller authenticates such a helper by handshake.
func (in *instance) proc() (pid int, start uint64, ok bool) {
	if in.launch != nil && in.launch.Boot == hostBootID {
		return in.launch.PID, in.launch.Birth, true
	}
	if in.rec != nil && in.launch == nil {
		if b, err := birth(in.rec.PID); err == nil {
			return in.rec.PID, b, true
		}
	}
	return 0, 0, false
}

// alive reports whether the helper process is still running.
func (in *instance) alive() bool {
	if in.exit != nil {
		return false
	}
	pid, start, ok := in.proc()
	return ok && running(pid, start)
}

func (in *instance) handle(k runtime.Key) runtime.Handle {
	pid, start, _ := in.proc()
	data, _ := json.Marshal(handleData{V: 1, Incarnation: in.rec.Incarnation})
	return runtime.Handle{
		Engine: Engine, Key: k, BootMode: domain.BootModeColdBoot, Data: data,
		ProcessIdentity: fmt.Sprintf("%s:%d:%d:%s:%s", Engine, pid, start, hostBootID, in.rec.Incarnation),
	}
}

type handleData struct {
	V           int    `json:"v"`
	Incarnation string `json:"incarnation"`
}

func (r *Runtime) control(ctx context.Context, in *instance) (*hp.Client, error) {
	return hp.Open(ctx, filepath.Join(in.dir, hp.ControlSocket), r.opts.Client, in.rec.Incarnation)
}

// current loads h's instance; a different incarnation is ErrStaleHandle.
// It returns nil when nothing is recorded for h's key.
func (r *Runtime) current(h runtime.Handle) (*instance, error) {
	if err := validKey(h.Key); err != nil {
		return nil, err
	}
	in, err := loadInstance(r.instDir(h.Key))
	if err != nil || in.rec == nil {
		return nil, err
	}
	if in.rec.Key != (hp.Key{VMID: string(h.VMID), Generation: h.Generation}) {
		return nil, fmt.Errorf("mac: %s records key %v, not %v", in.dir, in.rec.Key, h.Key)
	}
	var d handleData
	if json.Unmarshal(h.Data, &d) != nil || d.Incarnation != in.rec.Incarnation {
		return nil, fmt.Errorf("%w: %v is now helper %s", runtime.ErrStaleHandle, h.Key, in.rec.Incarnation)
	}
	return in, nil
}

func checkNetwork(n runtime.NetworkHandle) error {
	if n.Namespace != "" || n.TapName != "" || n.UserNamespace != "" || len(n.EnterArgv) > 0 {
		return runtime.Unsupported(Engine, "guest network attachment (only no-NIC VMs until the packet-link network contract lands)")
	}
	return nil
}

// ColdBoot starts inst in a new helper, or returns the live helper's
// handle for the same key (a replayed boot never starts a second VM).
func (r *Runtime) ColdBoot(ctx context.Context, inst runtime.Instance, disks runtime.DiskSet, n runtime.NetworkHandle) (runtime.Handle, error) {
	if err := validKey(inst.Key); err != nil {
		return runtime.Handle{}, err
	}
	if err := checkNetwork(n); err != nil {
		return runtime.Handle{}, err
	}
	if err := r.checkImage(inst.Image); err != nil {
		return runtime.Handle{}, err
	}
	if err := inst.Shape.Validate(r.Capabilities().Shape); err != nil {
		return runtime.Handle{}, err
	}
	if !filepath.IsAbs(disks.Root) {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "mac: root disk must be an absolute path")
	}
	if st, err := os.Lstat(disks.Root); err != nil || !st.Mode().IsRegular() {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "mac: root disk %s is not a regular file", disks.Root)
	}
	unlock := r.lockKey(inst.Key)
	defer unlock()
	dir := r.instDir(inst.Key)
	in, err := loadInstance(dir)
	if err != nil {
		return runtime.Handle{}, err
	}
	cfg := hp.VMConfig{
		Key: hp.Key{VMID: string(inst.VMID), Generation: inst.Generation}, CPUs: inst.Shape.CPUs, MemoryBytes: inst.Shape.MemoryBytes,
		Kernel: inst.Image.Kernel, CommandLine: inst.Image.BootArgs, RootDisk: disks.Root, Network: hp.NetworkNone, VsockPort: AgentPort,
	}
	if in.rec != nil && in.alive() {
		// A live helper for this key: finish its boot (idempotent) and
		// return it. A helper that does not answer as the recorded
		// incarnation is a recovery error, never a reason to start a
		// second VM on the same disk.
		if err := r.boot(ctx, in, cfg); err != nil {
			return runtime.Handle{}, fmt.Errorf("mac: live helper for %v cannot be resumed (recover it before booting again): %w", inst.Key, err)
		}
		return in.handle(inst.Key), nil
	}
	if in.launch != nil && in.rec == nil {
		if pid, start, ok := in.proc(); ok && running(pid, start) {
			return runtime.Handle{}, domain.Errorf(domain.CodeUnavailable, "mac: a helper for %v is still starting (pid %d)", inst.Key, pid)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return runtime.Handle{}, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return runtime.Handle{}, err
	}
	in, err = r.spawn(ctx, dir, inst.Key)
	if err == nil {
		err = r.boot(ctx, in, cfg)
	}
	if err != nil {
		if in != nil {
			r.kill(ctx, in)
		}
		os.RemoveAll(dir)
		return runtime.Handle{}, mapErr(err)
	}
	r.log.Debug("mac helper started", "key", inst.Key, "incarnation", in.rec.Incarnation)
	return in.handle(inst.Key), nil
}

// spawn starts a helper in dir and waits for its record and sockets.
func (r *Runtime) spawn(ctx context.Context, dir string, k runtime.Key) (*instance, error) {
	logf, err := os.OpenFile(filepath.Join(dir, "helper.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(r.opts.Helper, "serve", "--dir", dir, "--vm", string(k.VMID), "--generation", strconv.FormatInt(k.Generation, 10))
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, helperEnv(r.opts.HelperEnv), logf, logf
	// Its own session: the helper outlives the daemon and its signals.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mac: start helper: %w", err)
	}
	pid := cmd.Process.Pid
	go cmd.Wait() // reap: the helper's exit is read from exit.json
	b, err := birth(pid)
	if err != nil {
		cmd.Process.Kill()
		return nil, fmt.Errorf("mac: helper exited at start: %s", logTail(dir))
	}
	l := &launch{PID: pid, Birth: b, Boot: hostBootID}
	if err := writeJSON(filepath.Join(dir, "launch.json"), l); err != nil {
		cmd.Process.Kill()
		return nil, err
	}
	deadline := time.Now().Add(r.opts.StartTimeout)
	for {
		in, err := loadInstance(dir)
		if err == nil && in.rec != nil {
			if in.rec.PID != pid || in.rec.Key != (hp.Key{VMID: string(k.VMID), Generation: k.Generation}) {
				return in, fmt.Errorf("mac: helper record %+v does not match its launch (pid %d, %v)", in.rec, pid, k)
			}
			if _, err := os.Stat(filepath.Join(dir, hp.StreamSocket)); err == nil {
				return in, nil
			}
		}
		if !running(pid, b) {
			return &instance{dir: dir, launch: l}, fmt.Errorf("mac: helper exited at start: %s", logTail(dir))
		}
		if time.Now().After(deadline) {
			return &instance{dir: dir, launch: l}, fmt.Errorf("mac: helper not ready after %s: %s", r.opts.StartTimeout, logTail(dir))
		}
		select {
		case <-ctx.Done():
			return &instance{dir: dir, launch: l}, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// boot sends Create (idempotent for the same configuration) and Boot,
// which answers once the VM runs.
func (r *Runtime) boot(ctx context.Context, in *instance, cfg hp.VMConfig) error {
	c, err := r.control(ctx, in)
	if err != nil {
		return err
	}
	defer c.Close()
	var st hp.StateResult
	if err := c.Call(ctx, hp.TypeState, nil, &st); err != nil {
		return err
	}
	if st.State == hp.StateCreated {
		if err := c.Call(ctx, hp.TypeCreate, cfg, nil); err != nil {
			return err
		}
	}
	if err := c.Call(ctx, hp.TypeBoot, nil, &st); err != nil {
		return err
	}
	if st.State != hp.StateRunning && st.State != hp.StatePaused {
		return fmt.Errorf("mac: VM is %s after boot", st.State)
	}
	return nil
}

func logTail(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "helper.log"))
	if len(b) > 1500 {
		b = b[len(b)-1500:]
	}
	return strings.TrimSpace(string(b))
}

// mapErr turns protocol errors into domain errors.
func mapErr(err error) error {
	var e *hp.Error
	if !errors.As(err, &e) {
		if errors.Is(err, context.DeadlineExceeded) {
			de := domain.Errorf(domain.CodeTimeout, "mac helper: %v", err)
			de.Retryable = true
			return de
		}
		return err
	}
	code := map[hp.Code]domain.Code{
		hp.CodeInvalidArgument: domain.CodeInvalidArgument, hp.CodeInvalidState: domain.CodeInvalidState,
		hp.CodeConflict: domain.CodeInvalidState, hp.CodeTimeout: domain.CodeTimeout, hp.CodeUnsupported: domain.CodeUnsupportedCapability,
		hp.CodeRefused: domain.CodeUnavailable, hp.CodeVersion: domain.CodeUnsupportedCapability,
	}[e.Code]
	if code == "" {
		code = domain.CodeInternal
	}
	de := domain.Errorf(code, "mac helper: %s", e.Message)
	de.Retryable = e.Retryable
	return de
}

// stateCall runs one control request on a live helper.
func (r *Runtime) stateCall(ctx context.Context, h runtime.Handle, t hp.MsgType, req any) error {
	in, err := r.current(h)
	if err != nil {
		return err
	}
	if in == nil || !in.alive() {
		return domain.Errorf(domain.CodeInvalidState, "mac: %v is not running", h.Key)
	}
	c, err := r.control(ctx, in)
	if err != nil {
		return err
	}
	defer c.Close()
	return mapErr(c.Call(ctx, t, req, nil))
}

func (r *Runtime) Pause(ctx context.Context, h runtime.Handle) error {
	return r.stateCall(ctx, h, hp.TypePause, nil)
}

func (r *Runtime) Unpause(ctx context.Context, h runtime.Handle) error {
	return r.stateCall(ctx, h, hp.TypeResume, nil)
}

// waitGone waits for the helper process to exit.
func waitGone(in *instance, d time.Duration) bool {
	pid, start, ok := in.proc()
	if !ok {
		return true
	}
	for deadline := time.Now().Add(d); running(pid, start); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			return false
		}
	}
	return true
}

// Stop asks the guest to shut down; it returns once the helper has exited,
// or a retryable timeout with the VM still running (forcing is Kill).
func (r *Runtime) Stop(ctx context.Context, h runtime.Handle, grace time.Duration) error {
	in, err := r.current(h)
	if err != nil || in == nil || !in.alive() {
		return err
	}
	c, err := r.control(ctx, in)
	if err != nil {
		if !in.alive() {
			return nil
		}
		return err
	}
	defer c.Close()
	cctx, cancel := context.WithTimeout(ctx, grace+10*time.Second)
	defer cancel()
	if err := c.Call(cctx, hp.TypeStop, hp.Stop{GraceMillis: grace.Milliseconds()}, nil); err != nil {
		return mapErr(err)
	}
	if !waitGone(in, 10*time.Second) {
		return domain.Errorf(domain.CodeUnavailable, "mac: helper for %v stopped its VM but did not exit", h.Key)
	}
	return nil
}

// Kill stops the VM at once (crash-consistent disk) and ends its helper.
func (r *Runtime) Kill(ctx context.Context, h runtime.Handle) error {
	in, err := r.current(h)
	if err != nil || in == nil {
		return err
	}
	return r.kill(ctx, in)
}

// kill asks the helper to force-stop; a helper that does not answer or
// exit is signalled (its identity verified first).
func (r *Runtime) kill(ctx context.Context, in *instance) error {
	if !in.alive() {
		return nil
	}
	if in.rec != nil {
		kctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if c, err := r.control(kctx, in); err == nil {
			c.Call(kctx, hp.TypeKill, nil, nil)
			c.Close()
		}
		cancel()
		if waitGone(in, 5*time.Second) {
			return nil
		}
	}
	pid, start, ok := in.proc()
	if !ok {
		return nil
	}
	r.log.Warn("mac helper did not exit: SIGKILL", "dir", in.dir, "pid", pid)
	if err := signal(pid, start, syscall.SIGKILL); err != nil {
		return err
	}
	if !waitGone(in, 10*time.Second) {
		return domain.Errorf(domain.CodeUnavailable, "mac: helper %d survived SIGKILL", pid)
	}
	return nil
}

// Inspect reports the instance's phase from its helper, or from the
// helper's exit record once it has gone.
func (r *Runtime) Inspect(ctx context.Context, h runtime.Handle) (runtime.Status, error) {
	in, err := r.current(h)
	if errors.Is(err, runtime.ErrStaleHandle) || (err == nil && in == nil) {
		return runtime.Status{Phase: runtime.PhaseGone}, nil
	}
	if err != nil {
		return runtime.Status{}, err
	}
	st := runtime.Status{StartedAt: in.rec.StartedAt}
	if in.alive() {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		c, err := r.control(cctx, in)
		if err == nil {
			defer c.Close()
			var s hp.StateResult
			if err = c.Call(cctx, hp.TypeState, nil, &s); err == nil {
				if !s.StartedAt.IsZero() {
					st.StartedAt = s.StartedAt
				}
				switch s.State {
				case hp.StateRunning, hp.StateStopping:
					st.Phase = runtime.PhaseRunning
				case hp.StatePaused:
					st.Phase = runtime.PhasePaused
				case hp.StateCreated, hp.StateStarting:
					st.Phase = runtime.PhaseStarting
				default:
					st.Phase, st.ExitReason = runtime.PhaseExited, s.Reason
				}
				return st, nil
			}
		}
		if in.alive() {
			return runtime.Status{}, fmt.Errorf("mac: helper for %v is alive but does not answer: %w", h.Key, err)
		}
		in, _ = loadInstance(in.dir) // it exited meanwhile: read its exit
	}
	st.Phase, st.ExitReason = runtime.PhaseExited, "crashed"
	if in != nil && in.exit != nil {
		st.ExitReason = in.exit.Reason
	}
	return st, nil
}

// Discover lists every helper directory with a record, live or exited.
// Spawns that never wrote a record and whose process is gone are removed.
func (r *Runtime) Discover(ctx context.Context) ([]runtime.Handle, error) {
	ents, err := os.ReadDir(filepath.Join(r.opts.Dir, "vm"))
	if err != nil {
		return nil, err
	}
	var out []runtime.Handle
	for _, e := range ents {
		dir := filepath.Join(r.opts.Dir, "vm", e.Name())
		in, err := loadInstance(dir)
		if err != nil {
			r.log.Warn("mac: unreadable helper directory", "dir", dir, "err", err)
			continue
		}
		if in.rec == nil {
			if pid, start, ok := in.proc(); !ok || !running(pid, start) {
				os.RemoveAll(dir)
			}
			continue
		}
		k := runtime.Key{VMID: domain.VMID(in.rec.Key.VMID), Generation: in.rec.Key.Generation}
		if validKey(k) != nil || r.instDir(k) != dir {
			r.log.Warn("mac: helper directory does not match its record", "dir", dir, "key", in.rec.Key)
			continue
		}
		out = append(out, in.handle(k))
	}
	return out, nil
}

// GuestTransport dials the guest's vsock ports through the helper.
func (r *Runtime) GuestTransport(ctx context.Context, h runtime.Handle) (runtime.Dialer, error) {
	in, err := r.current(h)
	if err != nil {
		return nil, err
	}
	if in == nil || !in.alive() {
		return nil, domain.Errorf(domain.CodeInvalidState, "mac: %v is not running", h.Key)
	}
	return dialer{socket: filepath.Join(in.dir, hp.StreamSocket), client: r.opts.Client, incarnation: in.rec.Incarnation}, nil
}

type dialer struct{ socket, client, incarnation string }

func (d dialer) DialGuest(ctx context.Context, port uint32) (conn net.Conn, err error) {
	conn, err = hp.DialGuest(ctx, d.socket, d.client, d.incarnation, port)
	return conn, mapErr(err)
}

// Destroy kills h's helper if it is live and removes its directory.
func (r *Runtime) Destroy(ctx context.Context, h runtime.Handle) error {
	in, err := r.current(h)
	if err != nil {
		return err
	}
	unlock := r.lockKey(h.Key)
	defer unlock()
	if in != nil {
		if err := r.kill(ctx, in); err != nil {
			return err
		}
	}
	// Re-check under the key lock: a replacement may have started.
	if cur, _ := loadInstance(r.instDir(h.Key)); cur != nil && cur.rec != nil && in != nil && cur.rec.Incarnation != in.rec.Incarnation {
		return fmt.Errorf("%w: %v was replaced during destroy", runtime.ErrStaleHandle, h.Key)
	}
	return os.RemoveAll(r.instDir(h.Key))
}

// Unsupported optional features (MAC-BACKEND §5).

func (r *Runtime) RestoreGolden(context.Context, runtime.Instance, runtime.Golden, runtime.DiskSet, runtime.NetworkHandle) (runtime.Handle, error) {
	return runtime.Handle{}, runtime.Unsupported(Engine, "golden restore")
}

func (r *Runtime) Checkpoint(context.Context, runtime.Handle, runtime.Quiescence) (runtime.Checkpoint, error) {
	return runtime.Checkpoint{}, runtime.Unsupported(Engine, "RAM checkpoints")
}

func (r *Runtime) RestoreCheckpoint(context.Context, runtime.Instance, runtime.Checkpoint, runtime.DiskSet, runtime.NetworkHandle) (runtime.Handle, error) {
	return runtime.Handle{}, runtime.Unsupported(Engine, "RAM checkpoints")
}

func (r *Runtime) DiscardCheckpoint(context.Context, runtime.Checkpoint) error {
	return runtime.Unsupported(Engine, "RAM checkpoints")
}

var (
	_ runtime.Runtime = (*Runtime)(nil)
	_ io.Closer       = (*hp.Client)(nil)
)
