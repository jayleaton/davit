//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"davit/internal/domain"
	"davit/internal/runtime"
)

// Runtime is the Firecracker adapter.
type Runtime struct {
	opts    Options
	version string // e.g. "v1.17.0"
	binSHA  string
	cpu     string
	checks  []domain.HostCheck
	log     *slog.Logger

	mu    sync.Mutex
	keyMu map[runtime.Key]*sync.Mutex
	// recMu serializes instance-record read-modify-writes between API calls
	// and exit reapers.
	recMu sync.Mutex

	gmu      sync.Mutex
	gLock    map[string]*sync.Mutex
	gFail    map[string]error
	gcAt     time.Time // last golden sweep
	onEvict  func(runtime.GoldenEviction)
	statFree func() (int64, error) // tests: free space of Dir
}

var _ runtime.Runtime = (*Runtime)(nil)

// record is instances/<key>/instance.json. Proc is zero while the VMM is
// being spawned.
type record struct {
	Handle    runtime.Handle `json:"handle"`
	Proc      procID         `json:"proc"`
	StartedAt time.Time      `json:"startedAt"`
	Exit      string         `json:"exit,omitempty"`
	// Fingerprint (and its inputs) decide which RAM checkpoints of this
	// instance may restore where, exactly like goldens.
	Fingerprint string        `json:"fingerprint,omitempty"`
	Inputs      *goldenInputs `json:"inputs,omitempty"`
}

// snapSource is the RAM/device snapshot an instance restores from.
type snapSource struct {
	Dir  string // holds vmstate and mem
	Mode domain.BootMode
}

// New returns an adapter over opts.Dir. It fails when this host cannot run
// Firecracker (no KVM, unsupported architecture, unusable binary).
func New(opts Options) (*Runtime, error) {
	if opts.Dir == "" || !filepath.IsAbs(opts.Dir) {
		return nil, errors.New("firecracker: Dir must be an absolute path")
	}
	if opts.Binary == "" {
		return nil, errors.New("firecracker: Binary is required")
	}
	if goruntime.GOARCH != "amd64" {
		// Clean stop uses SendCtrlAltDel, which Firecracker has only on x86.
		return nil, runtime.Unsupported(Engine, "this host architecture ("+goruntime.GOARCH+") in M1")
	}
	if opts.CheckpointDir == "" {
		opts.CheckpointDir = filepath.Join(opts.Dir, "checkpoints")
	} else {
		// A configured location (often a shared tmpfs) gets a private
		// subdirectory: this adapter sweeps and re-flags what it owns only.
		sum := sha256.Sum256([]byte(opts.Dir))
		opts.CheckpointDir = filepath.Join(opts.CheckpointDir, "davit-"+hex.EncodeToString(sum[:6]))
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	for _, d := range []string{"instances", "golden"} {
		if err := os.MkdirAll(filepath.Join(opts.Dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	// VMMs are found by their cwd as the kernel reports it: no symlinks.
	dir, err := filepath.EvalSymlinks(opts.Dir)
	if err != nil {
		return nil, err
	}
	opts.Dir = dir
	r := &Runtime{
		opts: opts, log: opts.Log, cpu: cpuFingerprint(),
		keyMu: map[runtime.Key]*sync.Mutex{}, gLock: map[string]*sync.Mutex{}, gFail: map[string]error{},
	}
	kvm := domain.HostCheck{Name: "kvm", OK: true, Detail: "/dev/kvm read-write"}
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		kvm = domain.HostCheck{Name: "kvm", Detail: err.Error()}
	} else {
		f.Close()
	}
	r.checks = append(r.checks, kvm)
	if !kvm.OK {
		return nil, fmt.Errorf("firecracker: KVM unavailable: %s", kvm.Detail)
	}
	out, err := exec.Command(opts.Binary, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("firecracker: %s --version: %w", opts.Binary, err)
	}
	if f := strings.Fields(string(out)); len(f) >= 2 && f[0] == "Firecracker" {
		r.version = f[1]
	} else {
		return nil, fmt.Errorf("firecracker: unexpected --version output %q", out)
	}
	if r.binSHA, err = hashFile(opts.Binary); err != nil {
		return nil, err
	}
	r.checks = append(r.checks, domain.HostCheck{Name: "firecracker", OK: true, Detail: r.version + " sha256:" + r.binSHA[:12]})

	// Checkpoints are consumed right after they are taken. Any left over
	// belong to live forks a crashed daemon interrupted; their replays fail
	// cleanly (the child's disk may already be cloned, so a checkpoint is
	// never retaken) and can be retried.
	if err := os.RemoveAll(r.opts.CheckpointDir); err != nil {
		return nil, err
	}
	if n, err := r.reapStrays(); err != nil {
		return nil, err
	} else if n > 0 {
		r.log.Warn("killed stray firecracker processes no instance record accounts for", "count", n)
	}
	if err := r.sweepAllPrep(); err != nil {
		return nil, err
	}
	r.maybeGCGoldens(context.Background())
	return r, nil
}

// Version is the Firecracker release in use.
func (r *Runtime) Version() string { return r.version }

func (r *Runtime) Capabilities() domain.RuntimeCapabilities {
	return domain.RuntimeCapabilities{
		Engine:        Engine,
		EngineVersion: r.version,
		Arch:          []domain.Arch{domain.ArchAMD64},
		ColdBoot:      true,
		GoldenRestore: true,
		RAMSnapshot:   true,
		// Firecracker snapshots restore only into the same release.
		RAMCheckpointFormats: []string{"firecracker-" + r.version},
		LiveFork:             true,
		DiskFork:             true,
		GuestTransport:       "hybrid-vsock-uds",
		Shape: domain.ShapeConstraints{
			MaxCPUs: 32, MinMemoryBytes: 128 * domain.MiB, MaxMemoryBytes: 256 * domain.GiB,
			MemoryAlignBytes: domain.MiB, MinDiskBytes: 64 * domain.MiB,
		},
		FingerprintRules: []string{"engine-version", "engine-binary", "host-cpu", "image-digest", "boot-args", "shape", "network-layout", "device-layout"},
		HostChecks:       append([]domain.HostCheck(nil), r.checks...),
	}
}

var keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func validKey(k runtime.Key) error {
	if !keyRE.MatchString(string(k.VMID)) || k.Generation < 1 {
		return domain.Errorf(domain.CodeInvalidArgument, "firecracker: invalid instance key %v", k)
	}
	return nil
}

func (r *Runtime) instDir(k runtime.Key) string {
	return filepath.Join(r.opts.Dir, "instances", fmt.Sprintf("%s.g%d", k.VMID, k.Generation))
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

// load reads dir's record, completing a spawn the previous daemon did not
// record by finding the VMM through its cwd. Nil means no instance.
func (r *Runtime) load(dir string) (*record, error) {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	rec, err := readRecord(dir)
	if err != nil || rec == nil || rec.Proc.PID != 0 {
		return rec, err
	}
	if ps := findByCwd(dir); len(ps) == 1 {
		rec.Proc, rec.Handle.ProcessIdentity = ps[0], ps[0].String()
		return rec, writeRecord(dir, rec)
	}
	return rec, nil
}

func readRecord(dir string) (*record, error) {
	b, err := os.ReadFile(filepath.Join(dir, "instance.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("firecracker: %s: %w", dir, err)
	}
	return &rec, nil
}

func writeRecord(dir string, rec *record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dir, "instance.json"), b, 0o600)
}

// writeFileSync atomically replaces path with b, durably.
func writeFileSync(path string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	return syncDir(filepath.Dir(path))
}

// removeDir deletes an instance dir under the record lock, so an exit
// reaper cannot write a record back into it.
func (r *Runtime) removeDir(dir string) {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	os.RemoveAll(dir)
}

// current returns h's record, nil if the instance is gone, or
// ErrStaleHandle if another process now holds h's key.
func (r *Runtime) current(h runtime.Handle) (*record, error) {
	if h.Engine != Engine {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "firecracker: handle from engine %q", h.Engine)
	}
	if err := validKey(h.Key); err != nil {
		return nil, err
	}
	rec, err := r.load(r.instDir(h.Key))
	if err != nil || rec == nil {
		return nil, err
	}
	if rec.Handle.ProcessIdentity != h.ProcessIdentity {
		return nil, fmt.Errorf("%w: %v is now %s, handle names %s", runtime.ErrStaleHandle, h.Key, rec.Handle.ProcessIdentity, h.ProcessIdentity)
	}
	return rec, nil
}

// start spawns argv in dir and reaps it in the background, recording its
// exit reason if it is still the process recorded there.
func (r *Runtime) start(dir string, argv []string) (procID, error) {
	logf, err := os.OpenFile(filepath.Join(dir, "vmm.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return procID{}, err
	}
	defer logf.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, []string{}
	// Own session: the VMM outlives the daemon and its signals.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return procID{}, fmt.Errorf("firecracker: start: %w", err)
	}
	id, err := identify(cmd.Process.Pid)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return procID{}, fmt.Errorf("firecracker: VMM exited at start: %s", logTail(dir))
	}
	go func() {
		cmd.Wait()
		reason := exitReason(cmd.ProcessState)
		r.recMu.Lock()
		defer r.recMu.Unlock()
		if rec, err := readRecord(dir); err == nil && rec != nil && rec.Proc == id && rec.Exit == "" {
			rec.Exit = reason
			writeRecord(dir, rec)
		}
	}()
	return id, nil
}

func exitReason(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
		switch {
		case ws.Signaled() && ws.Signal() == syscall.SIGKILL:
			return "killed"
		case ws.Signaled():
			return "crashed: " + ws.Signal().String()
		case ws.ExitStatus() == 0:
			return "shutdown"
		default:
			return "crashed: exit " + strconv.Itoa(ws.ExitStatus())
		}
	}
	return "exited"
}

// logTail returns the end of dir's VMM log for error messages.
func logTail(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "vmm.log"))
	if len(b) > 1500 {
		b = b[len(b)-1500:]
	}
	return strings.TrimSpace(string(b))
}

// waitState polls the VMM API until it reports want.
func waitState(ctx context.Context, api *apiClient, id procID, dir, want string) error {
	sock := filepath.Join(dir, "api.sock")
	for {
		if !id.alive() {
			return fmt.Errorf("firecracker: VMM exited: %s", logTail(dir))
		}
		if _, err := os.Stat(sock); err == nil {
			if st, err := api.state(ctx); err == nil && (st == want || want == "") {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (r *Runtime) ColdBoot(ctx context.Context, inst runtime.Instance, disks runtime.DiskSet, nh runtime.NetworkHandle) (runtime.Handle, error) {
	return r.boot(ctx, inst, disks, nh, nil)
}

// boot starts inst cold, or restored from snap when non-nil. A live instance
// of the same key is returned as is.
func (r *Runtime) boot(ctx context.Context, inst runtime.Instance, disks runtime.DiskSet, nh runtime.NetworkHandle, snap *snapSource) (runtime.Handle, error) {
	if err := validKey(inst.Key); err != nil {
		return runtime.Handle{}, err
	}
	if err := inst.Shape.Validate(r.Capabilities().Shape); err != nil {
		return runtime.Handle{}, err
	}
	root, err := filepath.Abs(disks.Root)
	if err != nil {
		return runtime.Handle{}, err
	}
	if _, err := os.Stat(root); err != nil {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "firecracker: root disk: %v", err)
	}
	if nh.TapName != "" && len(nh.EnterArgv) == 0 {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "firecracker: network handle has a TAP but no namespace to enter")
	}
	if got := nicOf(nh); got.layout() != r.opts.NIC.layout() {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "firecracker: network gives guest NIC %s, runtime is configured for %s", got.layout(), r.opts.NIC.layout())
	}
	unlock := r.lockKey(inst.Key)
	defer unlock()
	dir := r.instDir(inst.Key)
	rec, err := r.load(dir)
	if err != nil {
		return runtime.Handle{}, err
	}
	if rec != nil && rec.Proc.alive() {
		started, err := vmmStarted(ctx, dir)
		if err != nil {
			return runtime.Handle{}, err
		}
		if started {
			return rec.Handle, nil
		}
		// Its spawner died before the VM ran (or before a snapshot load):
		// the replay starts over.
		if err := r.kill(rec.Proc, "replay: spawn never started its VM"); err != nil {
			return runtime.Handle{}, err
		}
	}

	h := runtime.Handle{Engine: Engine, Key: inst.Key, BootMode: domain.BootModeColdBoot}
	argv := append(slices.Clone(nh.EnterArgv), r.opts.Binary, "--api-sock", "api.sock", "--level", "Warning")
	if snap != nil {
		h.BootMode = snap.Mode
	}
	in, fp := r.fingerprint(inst.Image, inst.Shape)
	rec = &record{Handle: h, StartedAt: time.Now().UTC(), Fingerprint: fp, Inputs: &in}
	r.recMu.Lock()
	err = os.RemoveAll(dir)
	if err == nil {
		err = os.Mkdir(dir, 0o700)
	}
	if err == nil {
		err = os.Symlink(root, filepath.Join(dir, "root.img"))
	}
	if err == nil && snap == nil {
		var cfg []byte
		if cfg, err = json.Marshal(r.vmConfig(inst.Image, inst.Shape, nicOf(nh))); err == nil {
			err = os.WriteFile(filepath.Join(dir, "vm.json"), cfg, 0o600)
			argv = append(argv, "--config-file", "vm.json")
		}
	}
	if err == nil {
		err = writeRecord(dir, rec)
	}
	r.recMu.Unlock()
	if err != nil {
		return runtime.Handle{}, err
	}

	id, err := r.start(dir, argv)
	if err != nil {
		r.removeDir(dir)
		return runtime.Handle{}, err
	}
	r.recMu.Lock()
	rec.Proc, rec.Handle.ProcessIdentity = id, id.String()
	err = writeRecord(dir, rec)
	r.recMu.Unlock()
	if err == nil {
		err = r.bring(ctx, dir, id, snap)
	}
	if err != nil {
		r.logKill(id, "boot failed: "+err.Error())
		id.signal(syscall.SIGKILL)
		id.waitGone(5 * time.Second)
		r.removeDir(dir)
		return runtime.Handle{}, err
	}
	r.log.Debug("firecracker instance started", "key", inst.Key, "mode", h.BootMode, "proc", id)
	return rec.Handle, nil
}

// vmmStarted reports whether the live VMM in dir has a running or paused
// VM, as opposed to one still waiting for its configuration. It retries a
// busy API briefly and never guesses.
func vmmStarted(ctx context.Context, dir string) (bool, error) {
	api := newAPI(filepath.Join(dir, "api.sock"))
	var err error
	for range 50 {
		var st string
		actx, cancel := context.WithTimeout(ctx, time.Second)
		st, err = api.state(actx)
		cancel()
		if err == nil {
			return st == "Running" || st == "Paused", nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return false, fmt.Errorf("firecracker: live VMM in %s does not answer its API: %w", dir, err)
}

// bring takes a spawned VMM to Running, loading snap first if set.
func (r *Runtime) bring(ctx context.Context, dir string, id procID, snap *snapSource) error {
	api := newAPI(filepath.Join(dir, "api.sock"))
	if snap != nil {
		if err := waitState(ctx, api, id, dir, ""); err != nil {
			return err
		}
		if err := api.snapshotLoad(ctx, filepath.Join(snap.Dir, "vmstate"), filepath.Join(snap.Dir, "mem")); err != nil {
			return fmt.Errorf("%w: %s", err, logTail(dir))
		}
	}
	return waitState(ctx, api, id, dir, "Running")
}

func (r *Runtime) vmConfig(img domain.Image, shape domain.Shape, nic NIC) vmConfig {
	var c vmConfig
	c.BootSource.Kernel = img.Kernel
	c.BootSource.BootArgs = img.BootArgs
	if c.BootSource.BootArgs == "" {
		c.BootSource.BootArgs = defaultBootArgs
	}
	c.Drives = []fcDrive{{ID: "rootfs", Path: "root.img", Root: true}}
	c.Machine.VCPUs, c.Machine.MemMiB = shape.CPUs, shape.MemoryBytes/domain.MiB
	if nic.Tap != "" {
		mac := nic.MAC
		if mac == "" {
			mac = defaultGuestMAC
		}
		c.NICs = []fcNIC{{ID: "eth0", MAC: mac, Tap: nic.Tap}}
	}
	c.Vsock.CID, c.Vsock.Path = 3, "vsock.sock"
	return c
}

// checkpointMeta is checkpoints/<id>/checkpoint.json.
type checkpointMeta struct {
	ID          string       `json:"id"`
	Fingerprint string       `json:"fingerprint"`
	Inputs      goldenInputs `json:"inputs"`
	DiskBytes   int64        `json:"diskBytes"`
	Source      runtime.Key  `json:"source"`
	CreatedAt   time.Time    `json:"createdAt"`
}

var checkpointRE = regexp.MustCompile(`^cp-[0-9a-f]{24}$`)

func (r *Runtime) checkpointDir(id string) string {
	return filepath.Join(r.opts.CheckpointDir, id)
}

// Checkpoint snapshots a paused instance's RAM and device state into
// checkpoints/<id>. The files are not fsynced: a checkpoint is consumed by
// RestoreCheckpoint right away and is never needed across a host crash
// (lifecycle retakes it), so durability would only cost latency.
func (r *Runtime) Checkpoint(ctx context.Context, h runtime.Handle, _ runtime.Quiescence) (runtime.Checkpoint, error) {
	rec, err := r.live(h)
	if err != nil {
		return runtime.Checkpoint{}, err
	}
	if rec.Inputs == nil {
		return runtime.Checkpoint{}, domain.Errorf(domain.CodeUnsupportedCapability, "firecracker: instance %v predates checkpoint support; restart it", h.Key).
			WithDetail("capability", "ramSnapshot")
	}
	dir := r.instDir(h.Key)
	api := newAPI(filepath.Join(dir, "api.sock"))
	if st, err := api.state(ctx); err != nil || st != "Paused" {
		return runtime.Checkpoint{}, domain.Errorf(domain.CodeInvalidState, "firecracker: checkpoint needs a paused instance (%q, %v)", st, err)
	}
	st, err := os.Stat(filepath.Join(dir, "root.img"))
	if err != nil {
		return runtime.Checkpoint{}, err
	}
	var b [12]byte
	rand.Read(b[:])
	id := "cp-" + hex.EncodeToString(b[:])
	cdir := r.checkpointDir(id)
	if err := r.ensureCheckpointRoot(); err != nil {
		return runtime.Checkpoint{}, err
	}
	if err := os.Mkdir(cdir, 0o700); err != nil {
		return runtime.Checkpoint{}, err
	}
	if err := api.snapshotCreateNoSync(ctx, filepath.Join(cdir, "vmstate"), filepath.Join(cdir, "mem")); err != nil {
		os.RemoveAll(cdir)
		return runtime.Checkpoint{}, err
	}
	m := checkpointMeta{ID: id, Fingerprint: rec.Fingerprint, Inputs: *rec.Inputs, DiskBytes: st.Size(), Source: h.Key, CreatedAt: time.Now().UTC()}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(cdir, "checkpoint.json"), raw, 0o400); err != nil {
		os.RemoveAll(cdir)
		return runtime.Checkpoint{}, err
	}
	return runtime.Checkpoint{ID: id, Fingerprint: m.Fingerprint}, nil
}

// RestoreCheckpoint starts inst from checkpoint cp: same engine, host CPU,
// image, shape and NIC layout (the fingerprint), and a disk of the size the
// checkpoint saw.
func (r *Runtime) RestoreCheckpoint(ctx context.Context, inst runtime.Instance, cp runtime.Checkpoint, disks runtime.DiskSet, nh runtime.NetworkHandle) (runtime.Handle, error) {
	if !checkpointRE.MatchString(cp.ID) {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "firecracker: invalid checkpoint ID %q", cp.ID)
	}
	cdir := r.checkpointDir(cp.ID)
	raw, err := os.ReadFile(filepath.Join(cdir, "checkpoint.json"))
	if errors.Is(err, os.ErrNotExist) {
		return runtime.Handle{}, domain.Errorf(domain.CodeNotFound, "firecracker: checkpoint %s not found", cp.ID)
	}
	if err != nil {
		return runtime.Handle{}, err
	}
	var m checkpointMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return runtime.Handle{}, err
	}
	if _, fp := r.fingerprint(inst.Image, inst.Shape); fp != m.Fingerprint || cp.Fingerprint != m.Fingerprint {
		return runtime.Handle{}, domain.Errorf(domain.CodeUnsupportedCapability, "firecracker: checkpoint %s is not compatible with this instance, image, shape or host", cp.ID).
			WithDetail("capability", "ramSnapshot")
	}
	st, err := os.Stat(disks.Root)
	if err != nil {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "firecracker: root disk: %v", err)
	}
	if st.Size() != m.DiskBytes {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "firecracker: root disk is %d bytes, checkpoint %s expects %d", st.Size(), cp.ID, m.DiskBytes)
	}
	return r.boot(ctx, inst, disks, nh, &snapSource{Dir: cdir, Mode: domain.BootModeRAMRestore})
}

// ensureCheckpointRoot creates checkpoints/ and marks it no-copy-on-write
// and uncompressed where the filesystem supports it (btrfs): checkpoint
// memory files are written whole, read back once and deleted, so
// compressing them at writeback only adds dirty-page pressure that stalls
// the next checkpoint (and so the paused source).
func (r *Runtime) ensureCheckpointRoot() error {
	root := r.opts.CheckpointDir
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	f, err := os.Open(root)
	if err != nil {
		return err
	}
	defer f.Close()
	if flags, err := unix.IoctlGetInt(int(f.Fd()), unix.FS_IOC_GETFLAGS); err == nil {
		const fsNoCOW, fsNoComp = 0x00800000, 0x00000400 // FS_NOCOW_FL, FS_NOCOMP_FL (linux/fs.h)
		want := flags | fsNoCOW | fsNoComp
		if want != flags {
			unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, want)
		}
	}
	return nil
}

// DiscardCheckpoint deletes a checkpoint. Restored instances keep running:
// their guest memory maps the (unlinked) file privately.
func (r *Runtime) DiscardCheckpoint(_ context.Context, cp runtime.Checkpoint) error {
	if !checkpointRE.MatchString(cp.ID) {
		return domain.Errorf(domain.CodeInvalidArgument, "firecracker: invalid checkpoint ID %q", cp.ID)
	}
	return os.RemoveAll(r.checkpointDir(cp.ID))
}

// live returns h's record if its VMM is running, else an invalid_state error.
func (r *Runtime) live(h runtime.Handle) (*record, error) {
	rec, err := r.current(h)
	if err != nil {
		return nil, err
	}
	if rec == nil || !rec.Proc.alive() {
		return nil, domain.Errorf(domain.CodeInvalidState, "firecracker: instance %v is not running", h.Key)
	}
	return rec, nil
}

func (r *Runtime) Pause(ctx context.Context, h runtime.Handle) error {
	if _, err := r.live(h); err != nil {
		return err
	}
	return newAPI(filepath.Join(r.instDir(h.Key), "api.sock")).setPaused(ctx, true)
}

func (r *Runtime) Unpause(ctx context.Context, h runtime.Handle) error {
	if _, err := r.live(h); err != nil {
		return err
	}
	return newAPI(filepath.Join(r.instDir(h.Key), "api.sock")).setPaused(ctx, false)
}

// Stop asks the guest to shut down (GuestShutdown, plus Ctrl+Alt+Del for
// kernels with i8042): systemd stops every service and unmounts, then a
// guest reboot ends the VMM, or a guest power-off is seen on the console
// and the halted VMM is reaped. Processes end; the disk is consistent once the
// VMM is gone. If the VMM is still running at grace, Stop fails and leaves
// it running: forcing is the caller's explicit choice (Kill).
func (r *Runtime) Stop(ctx context.Context, h runtime.Handle, grace time.Duration) error {
	rec, err := r.current(h)
	if err != nil || rec == nil || !rec.Proc.alive() {
		return err
	}
	dir := r.instDir(h.Key)
	api := newAPI(filepath.Join(dir, "api.sock"))
	if st, err := api.state(ctx); err == nil && st == "Paused" {
		if err := api.setPaused(ctx, false); err != nil {
			return err
		}
	}
	// Only console output written after this point counts as this stop's
	// power-down.
	logPath := filepath.Join(dir, "vmm.log")
	var logFrom int64
	if st, err := os.Stat(logPath); err == nil {
		logFrom = st.Size()
	}
	ctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	api.action(ctx, "SendCtrlAltDel")
	if r.opts.GuestShutdown != nil {
		d, _ := r.dialer(dir)
		go func() {
			// Retry while the guest is still booting or the agent restarts.
			for ctx.Err() == nil {
				err := r.opts.GuestShutdown(ctx, h.Key, d)
				if err == nil {
					return
				}
				r.log.Debug("guest shutdown request failed; retrying", "key", h.Key, "err", err)
				select {
				case <-ctx.Done():
				case <-time.After(100 * time.Millisecond):
				}
			}
		}()
	}
	for tick := 0; rec.Proc.alive(); tick++ {
		// A guest that powered off (the agent's shutdown) halts instead of
		// exiting: Firecracker has no power-off device. Its kernel has
		// finished shutting down once it prints this, so reap the VMM.
		if tick%20 == 0 && poweredOff(logPath, logFrom) {
			return r.kill(rec.Proc, "stop: guest powered off")
		}
		select {
		case <-ctx.Done():
			if !rec.Proc.alive() {
				return nil
			}
			if poweredOff(logPath, logFrom) {
				return r.kill(rec.Proc, "stop: guest powered off")
			}
			r.log.Warn("guest did not shut down", "key", h.Key, "grace", grace, "console", logTail(dir))
			e := domain.Errorf(domain.CodeTimeout, "guest %v did not shut down within %s", h.Key, grace)
			e.Retryable = true
			return e
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}

// poweredOff reports whether the guest console logged a completed power-off
// or halt after offset from. The guest can only stop itself this way, and
// only while the host is stopping it.
func poweredOff(path string, from int64) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b, err := io.ReadAll(io.NewSectionReader(f, from, 1<<20))
	if err != nil {
		return false
	}
	// kernel/reboot.c: "reboot: Power down", "reboot: System halted", and
	// without a power-off device "reboot: Power off not available: System
	// halted instead" (Firecracker).
	return bytes.Contains(b, []byte("reboot: Power down")) || bytes.Contains(b, []byte("System halted"))
}

func (r *Runtime) Kill(ctx context.Context, h runtime.Handle) error {
	rec, err := r.current(h)
	if err != nil || rec == nil {
		return err
	}
	return r.kill(rec.Proc, "kill requested")
}

// kill SIGKILLs a VMM of this adapter and logs why first: every VMM the
// adapter ends is in the daemon log, so a VMM that died "killed" without
// such a line was killed from outside (OOM killer, another process).
func (r *Runtime) kill(p procID, why string) error {
	r.logKill(p, why)
	return kill(p)
}

func (r *Runtime) logKill(p procID, why string) {
	if p.alive() {
		r.log.Debug("SIGKILL firecracker VMM", "proc", p, "why", why)
	}
}

func kill(p procID) error {
	if err := p.signal(syscall.SIGKILL); err != nil {
		return err
	}
	if !p.waitGone(10 * time.Second) {
		return domain.Errorf(domain.CodeUnavailable, "firecracker: %s survived SIGKILL", p)
	}
	return nil
}

func (r *Runtime) Inspect(ctx context.Context, h runtime.Handle) (runtime.Status, error) {
	rec, err := r.current(h)
	if errors.Is(err, runtime.ErrStaleHandle) || (err == nil && rec == nil) {
		return runtime.Status{Phase: runtime.PhaseGone}, nil
	}
	if err != nil {
		return runtime.Status{}, err
	}
	st := runtime.Status{StartedAt: rec.StartedAt}
	if !rec.Proc.alive() {
		st.Phase, st.ExitReason = runtime.PhaseExited, rec.Exit
		if st.ExitReason == "" {
			st.ExitReason = "exited"
		}
		return st, nil
	}
	actx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	switch s, _ := newAPI(filepath.Join(r.instDir(h.Key), "api.sock")).state(actx); s {
	case "Running":
		st.Phase = runtime.PhaseRunning
	case "Paused":
		st.Phase = runtime.PhasePaused
	default:
		st.Phase = runtime.PhaseStarting
	}
	return st, nil
}

// Discover lists every instance with a record, live or exited. Spawns that
// never got a process are removed.
func (r *Runtime) Discover(ctx context.Context) ([]runtime.Handle, error) {
	ents, err := os.ReadDir(filepath.Join(r.opts.Dir, "instances"))
	if err != nil {
		return nil, err
	}
	var out []runtime.Handle
	for _, e := range ents {
		k, ok := parseKey(e.Name())
		if !ok {
			continue
		}
		unlock := r.lockKey(k)
		dir := r.instDir(k)
		rec, err := r.load(dir)
		switch {
		case err != nil:
			unlock()
			return nil, err
		case rec == nil || rec.Proc.PID == 0:
			r.removeDir(dir)
		default:
			out = append(out, rec.Handle)
		}
		unlock()
	}
	return out, nil
}

func parseKey(name string) (runtime.Key, bool) {
	i := strings.LastIndex(name, ".g")
	if i <= 0 {
		return runtime.Key{}, false
	}
	gen, err := strconv.ParseInt(name[i+2:], 10, 64)
	k := runtime.Key{VMID: domain.VMID(name[:i]), Generation: gen}
	return k, err == nil && validKey(k) == nil
}

func (r *Runtime) GuestTransport(ctx context.Context, h runtime.Handle) (runtime.Dialer, error) {
	if _, err := r.live(h); err != nil {
		return nil, err
	}
	return r.dialer(r.instDir(h.Key))
}

func (r *Runtime) dialer(dir string) (VsockDialer, error) {
	return VsockDialer{Path: filepath.Join(dir, "vsock.sock")}, nil
}

func (r *Runtime) Destroy(ctx context.Context, h runtime.Handle) error {
	rec, err := r.current(h)
	if err != nil {
		return err
	}
	unlock := r.lockKey(h.Key)
	defer unlock()
	if rec != nil {
		if err := r.kill(rec.Proc, "destroy"); err != nil {
			return err
		}
	}
	r.recMu.Lock()
	defer r.recMu.Unlock()
	// Re-check under the record lock: a replacement may have started.
	if cur, _ := readRecord(r.instDir(h.Key)); cur != nil && cur.Handle.ProcessIdentity != h.ProcessIdentity {
		return fmt.Errorf("%w: %v was replaced during destroy", runtime.ErrStaleHandle, h.Key)
	}
	return os.RemoveAll(r.instDir(h.Key))
}

// reapStrays kills VMM processes (this adapter's firecracker binary) running
// under Dir that no instance record accounts for: an instance dir that was
// deleted, a record naming another process or none yet (its spawner died
// before recording it), or anything outside instances/ (captures are swept
// separately). Live recorded instances are left for lifecycle
// reconciliation, which destroys those it has no VM for. It runs in New,
// before any boot, so no spawn is in flight; that holds only while New is
// the one adapter on Dir. The daemon guarantees it by opening the runtime
// after taking the exclusive state lock (state.Open), so a second daemon
// fails before it can reap the first one's spawns. A live adopted VMM is
// never touched: its instance record names exactly its PID, start time and
// boot.
func (r *Runtime) reapStrays() (int, error) {
	bin, err := filepath.EvalSymlinks(r.opts.Binary)
	if err != nil {
		bin = r.opts.Binary
	}
	instances := filepath.Join(r.opts.Dir, "instances") + "/"
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil {
			continue
		}
		deleted := strings.HasSuffix(cwd, " (deleted)")
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		if cwd != r.opts.Dir && !strings.HasPrefix(cwd, r.opts.Dir+"/") {
			continue
		}
		if exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe")); err != nil || strings.TrimSuffix(exe, " (deleted)") != bin {
			continue
		}
		id, err := identify(pid)
		if err != nil || !id.alive() {
			continue
		}
		if !deleted && strings.HasPrefix(cwd, instances) && !strings.Contains(strings.TrimPrefix(cwd, instances), "/") {
			if rec, err := readRecord(cwd); err == nil && rec != nil && rec.Proc == id {
				continue // recorded: lifecycle decides
			}
		} else if strings.HasPrefix(cwd, filepath.Join(r.opts.Dir, "golden", ".prep-")) && !deleted {
			continue // sweepAllPrep handles captures
		}
		r.log.Warn("killing stray firecracker", "pid", pid, "cwd", cwd, "deletedCwd", deleted)
		if err := r.kill(id, "stray: no instance record accounts for it"); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
