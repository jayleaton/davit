//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"davit/internal/domain"
	"davit/internal/runtime"
)

// goldenLayout versions the device layout this adapter gives guests (drive,
// NIC, vsock CID/paths). Bump it whenever vmConfig changes shape.
const goldenLayout = 2

// goldenInputs are everything a golden checkpoint's validity depends on.
// Their hash is the golden's fingerprint and (shortened) its ID, so a cache
// hit is by construction compatible.
type goldenInputs struct {
	Layout      int    `json:"layout"`
	Engine      string `json:"engine"`
	EngineSHA   string `json:"engineSha256"`
	HostCPU     string `json:"hostCpu"`
	ImageDigest string `json:"imageDigest"`
	// Kernel and Rootfs are the blob paths, which the manifest catalog names
	// by content digest, so a rebuilt image never reuses a stale golden.
	Kernel   string       `json:"kernel"`
	Rootfs   string       `json:"rootfs"`
	BootArgs string       `json:"bootArgs"`
	Shape    domain.Shape `json:"shape"`
	Network  string       `json:"network"`
}

// goldenMeta is golden/<id>/golden.json.
type goldenMeta struct {
	ID          string       `json:"id"`
	Fingerprint string       `json:"fingerprint"`
	Inputs      goldenInputs `json:"inputs"`
	DiskBytes   int64        `json:"diskBytes"`
	CreatedAt   time.Time    `json:"createdAt"`
	// PrepareMs is how long cold boot + readiness + snapshot took.
	PrepareMs int64 `json:"prepareMs"`
}

func (r *Runtime) fingerprint(img domain.Image, shape domain.Shape) (goldenInputs, string) {
	in := goldenInputs{
		Layout: goldenLayout, Engine: Engine + " " + r.version, EngineSHA: r.binSHA, HostCPU: r.cpu,
		ImageDigest: img.Digest, Kernel: img.Kernel, Rootfs: img.Rootfs, BootArgs: img.BootArgs, Shape: shape, Network: r.opts.NIC.layout(),
	}
	if in.BootArgs == "" {
		in.BootArgs = defaultBootArgs
	}
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return in, hex.EncodeToString(sum[:])
}

func (r *Runtime) goldenDir(id string) string { return filepath.Join(r.opts.Dir, "golden", id) }

func readGolden(dir string) (*goldenMeta, error) {
	b, err := os.ReadFile(filepath.Join(dir, "golden.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m goldenMeta
	return &m, json.Unmarshal(b, &m)
}

func (m *goldenMeta) runtimeGolden(dir string) *runtime.Golden {
	return &runtime.Golden{ID: m.ID, Fingerprint: m.Fingerprint, Disk: filepath.Join(dir, "root.img")}
}

// Prepare returns the golden checkpoint for image+shape on this host,
// capturing it on first use: cold boot a reflink of the image rootfs, wait
// for GuestReady, pause, snapshot RAM+device state, and keep the exact disk.
// A failed capture is remembered until restart so every `new` does not
// re-pay the timeout; callers fall back to cold boot.
func (r *Runtime) Prepare(ctx context.Context, img domain.Image, shape domain.Shape) (runtime.Prepared, error) {
	if err := shape.Validate(r.Capabilities().Shape); err != nil {
		return runtime.Prepared{}, err
	}
	if img.Kernel == "" || img.Rootfs == "" || img.Digest == "" {
		return runtime.Prepared{}, domain.Errorf(domain.CodeInvalidArgument, "firecracker: image %s has no kernel/rootfs", img.ID)
	}
	in, fp := r.fingerprint(img, shape)
	id := "g" + fp[:24]
	dir := r.goldenDir(id)
	out := runtime.Prepared{Image: img, Shape: shape}

	r.maybeGCGoldens(ctx)
	l := r.goldenLock(id)
	l.Lock()
	defer l.Unlock()

	if m, err := readGolden(dir); err != nil {
		return out, err
	} else if m != nil && m.Fingerprint == fp {
		r.touchGolden(id)
		out.Golden = m.runtimeGolden(dir)
		return out, nil
	}
	r.gmu.Lock()
	ferr := r.gFail[id]
	r.gmu.Unlock()
	if ferr != nil {
		return out, ferr
	}
	// Room for the capture's memory file above the golden floor, evicting
	// least-recently-used goldens if needed; a capture never fills the disk.
	if ok, err := r.enforceFloor(ctx, shape.MemoryBytes); err != nil {
		return out, err
	} else if !ok {
		return out, fmt.Errorf("%w (%d MiB guest memory above the %d MiB floor)", errNoRoom, shape.MemoryBytes>>20, r.goldenFloor()>>20)
	}
	m, err := r.capture(ctx, img, shape, in, fp, id)
	if err != nil {
		// A capture that ran out of time (a loaded host) is retried by the
		// next create; only a deterministic failure is remembered, so it
		// does not cost a boot attempt per create.
		if ctx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
			r.gmu.Lock()
			r.gFail[id] = err
			r.gmu.Unlock()
		}
		return out, err
	}
	r.log.Info("golden prepared", "id", id, "image", img.Ref, "ms", m.PrepareMs)
	out.Golden = m.runtimeGolden(dir)
	return out, nil
}

func (r *Runtime) capture(ctx context.Context, img domain.Image, shape domain.Shape, in goldenInputs, fp, id string) (*goldenMeta, error) {
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(ctx, goldenBudget(r.opts.GoldenTimeout, shape))
	defer cancel()
	final := r.goldenDir(id)
	tmp := filepath.Join(r.opts.Dir, "golden", ".prep-"+id)
	if err := r.sweepPrep(tmp); err != nil {
		return nil, err
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()
	disk := filepath.Join(tmp, "root.img")
	if err := cloneFile(img.Rootfs, disk, shape.DiskBytes); err != nil {
		return nil, fmt.Errorf("firecracker: golden disk: %w", err)
	}
	var nh runtime.NetworkHandle
	if r.opts.CaptureNetwork != nil {
		h, release, err := r.opts.CaptureNetwork(ctx)
		if err != nil {
			return nil, fmt.Errorf("firecracker: golden capture network: %w", err)
		}
		defer release() // after the capture VMM is gone (deferred kill runs first)
		nh = h
	}
	if nicOf(nh).layout() != r.opts.NIC.layout() {
		return nil, fmt.Errorf("firecracker: capture network gives guest NIC %s, runtime is configured for %s", nicOf(nh).layout(), r.opts.NIC.layout())
	}
	cfg, _ := json.Marshal(r.vmConfig(img, shape, nicOf(nh)))
	if err := os.WriteFile(filepath.Join(tmp, "vm.json"), cfg, 0o600); err != nil {
		return nil, err
	}
	argv := append(slices.Clone(nh.EnterArgv), r.opts.Binary, "--api-sock", "api.sock", "--level", "Warning", "--config-file", "vm.json")
	proc, err := r.start(tmp, argv)
	if err != nil {
		return nil, err
	}
	defer r.kill(proc, "golden capture ended")
	api := newAPI(filepath.Join(tmp, "api.sock"))
	if err := waitState(ctx, api, proc, tmp, "Running"); err != nil {
		return nil, err
	}
	d, err := r.dialer(tmp)
	if err != nil {
		return nil, err
	}
	ready := r.opts.GuestReady
	if ready == nil {
		ready = func(ctx context.Context, d runtime.Dialer) error {
			return waitGuestPort(ctx, d.(VsockDialer), AgentPort)
		}
	}
	if err := ready(ctx, d); err != nil {
		return nil, fmt.Errorf("firecracker: golden guest never became ready: %w", err)
	}
	if err := api.setPaused(ctx, true); err != nil {
		return nil, err
	}
	if err := api.snapshotCreate(ctx, filepath.Join(tmp, "vmstate"), filepath.Join(tmp, "mem")); err != nil {
		return nil, err
	}
	// Paused, then killed: the disk can no longer change, so it is exactly
	// the generation the RAM image expects.
	if err := r.kill(proc, "golden capture: snapshot taken"); err != nil {
		return nil, err
	}
	for _, f := range []string{"api.sock", "vsock.sock", "vm.json"} {
		os.Remove(filepath.Join(tmp, f))
	}
	for _, f := range []string{"root.img", "mem", "vmstate"} {
		if err := syncReadOnly(filepath.Join(tmp, f)); err != nil {
			return nil, err
		}
	}
	m := &goldenMeta{ID: id, Fingerprint: fp, Inputs: in, DiskBytes: shape.DiskBytes, CreatedAt: time.Now().UTC(), PrepareMs: time.Since(t0).Milliseconds()}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := writeFileSync(filepath.Join(tmp, "golden.json"), b, 0o400); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return nil, err
	}
	ok = true
	return m, syncDir(filepath.Dir(final))
}

// sweepPrep kills a capture VMM a crashed daemon left running in dir and
// removes the partial capture.
func (r *Runtime) sweepPrep(dir string) error {
	for _, p := range findByCwd(dir) {
		if err := r.kill(p, "partial golden capture of an earlier daemon"); err != nil {
			return err
		}
	}
	return os.RemoveAll(dir)
}

// sweepAllPrep runs sweepPrep on every partial capture.
func (r *Runtime) sweepAllPrep() error {
	dirs, _ := filepath.Glob(filepath.Join(r.opts.Dir, "golden", ".prep-*"))
	for _, d := range dirs {
		if err := r.sweepPrep(d); err != nil {
			return err
		}
	}
	return nil
}

func syncReadOnly(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Chmod(0o400)
}

// cloneFile makes dst a reflink of src (full copy if the filesystem cannot
// reflink), grown sparsely to size.
func cloneFile(src, dst string, size int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		if _, err := out.ReadFrom(in); err != nil {
			return err
		}
	}
	st, err := out.Stat()
	if err != nil {
		return err
	}
	if size > st.Size() {
		if err := out.Truncate(size); err != nil {
			return err
		}
	}
	return out.Sync()
}

// RestoreGolden starts inst from golden g. disks.Root must be a private
// clone of g.Disk that no VMM has opened since (SPEC §6: never restore
// golden RAM onto a modified filesystem); lifecycle guarantees that.
func (r *Runtime) RestoreGolden(ctx context.Context, inst runtime.Instance, g runtime.Golden, disks runtime.DiskSet, nh runtime.NetworkHandle) (runtime.Handle, error) {
	if !keyRE.MatchString(g.ID) {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "firecracker: invalid golden ID %q", g.ID)
	}
	m, err := readGolden(r.goldenDir(g.ID))
	if err != nil {
		return runtime.Handle{}, err
	}
	if m == nil {
		return runtime.Handle{}, domain.Errorf(domain.CodeNotFound, "firecracker: golden %s is not prepared", g.ID)
	}
	if _, fp := r.fingerprint(inst.Image, inst.Shape); fp != m.Fingerprint || g.Fingerprint != m.Fingerprint {
		return runtime.Handle{}, domain.Errorf(domain.CodeUnsupportedCapability, "firecracker: golden %s is not compatible with this instance, image or host", g.ID).
			WithDetail("capability", "goldenRestore")
	}
	r.touchGolden(g.ID)
	st, err := os.Stat(disks.Root)
	if err != nil {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "firecracker: root disk: %v", err)
	}
	if st.Size() != m.DiskBytes {
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "firecracker: root disk is %d bytes, golden %s expects %d", st.Size(), g.ID, m.DiskBytes)
	}
	return r.boot(ctx, inst, disks, nh, &snapSource{Dir: r.goldenDir(m.ID), Mode: domain.BootModeGoldenRestore})
}

// cpuFingerprint names the host CPU closely enough that a RAM snapshot
// taken here restores here (vendor, family/model/stepping, feature flags).
func cpuFingerprint() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	want := map[string]string{"vendor_id": "", "cpu family": "", "model": "", "stepping": "", "model name": "", "flags": ""}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break // first processor only
		}
		k, v, ok := strings.Cut(line, ":")
		k = strings.TrimSpace(k)
		if _, w := want[k]; ok && w {
			want[k] = strings.TrimSpace(v)
		}
	}
	flags := sha256.Sum256([]byte(want["flags"]))
	return fmt.Sprintf("%s/%s/%s/%s %s flags:%x", want["vendor_id"], want["cpu family"], want["model"], want["stepping"], want["model name"], flags[:6])
}

// waitGuestPort polls until port accepts a connection.
func waitGuestPort(ctx context.Context, d VsockDialer, port uint32) error {
	for {
		c, err := d.DialGuest(ctx, port)
		if err == nil {
			return c.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest port %d never accepted: %w (last: %v)", port, ctx.Err(), err)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
