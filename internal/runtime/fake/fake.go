// Package fake is an in-process runtime adapter for tests and M0 end-to-end
// runs. It models a VMM supervisor whose instances outlive the daemon: each
// instance is a JSON record in Dir, so a restarted daemon (a new Runtime over
// the same Dir) rediscovers instances that were booting or running when the
// old one died, exactly as a real adapter would rediscover VMM processes.
package fake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"davit/internal/domain"
	"davit/internal/runtime"
)

// Engine is the engine name reported in capabilities and handles.
const Engine = "fake"

// Options configures the fake.
type Options struct {
	// Dir holds instance records. Required.
	Dir string
	// BootDelay is how long a cold boot takes. The instance keeps "booting"
	// in the background if the caller goes away, like a real VMM.
	BootDelay time.Duration
	// StopDelay is how long a clean shutdown takes. Longer than a Stop's
	// grace models a guest that ignores the request: Stop then times out
	// after the grace.
	StopDelay time.Duration
	// Golden enables golden restore. Prepare "captures" a golden whose disk
	// is a copy of the image rootfs under Dir; RestoreGolden insists the
	// private disk is byte-identical to it (the SPEC §6 invariant).
	Golden bool
	// LiveFork enables RAM checkpoints (Checkpoint/RestoreCheckpoint) and
	// live fork. A checkpoint records a digest of the source disk, and
	// RestoreCheckpoint insists the new disk still matches it.
	LiveFork bool
	// Now overrides the clock.
	Now func() time.Time
	// GuestTransport, when set, gives running instances a guest transport
	// (e.g. an in-process guest agent) and Capabilities reports it as
	// "in-process". Nil keeps transport "none".
	GuestTransport func(ctx context.Context, h runtime.Handle) (runtime.Dialer, error)
}

// Runtime is the fake adapter.
type Runtime struct {
	opts    Options
	mu      sync.Mutex
	faults  map[string]error
	onEvict func(runtime.GoldenEviction)
	floors  int // EnforceGoldenFloor calls
}

// OnGoldenEvicted implements runtime.GoldenEvictor.
func (r *Runtime) OnGoldenEvicted(f func(runtime.GoldenEviction)) {
	r.mu.Lock()
	r.onEvict = f
	r.mu.Unlock()
}

// EnforceGoldenFloor implements runtime.GoldenEvictor: it only counts calls
// (tests evict with EvictGolden).
func (r *Runtime) EnforceGoldenFloor(context.Context) error {
	r.mu.Lock()
	r.floors++
	r.mu.Unlock()
	return nil
}

// FloorChecks reports how often EnforceGoldenFloor ran.
func (r *Runtime) FloorChecks() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.floors
}

// EvictGolden removes the golden Prepare gives image+shape, as a real engine
// does under disk pressure, and reports it; the next Prepare recaptures it.
func (r *Runtime) EvictGolden(image domain.Image, shape domain.Shape) {
	sum := goldenSum(image, shape)
	id := "golden-" + hex.EncodeToString(sum[:8])
	os.Remove(filepath.Join(r.opts.Dir, id+".img"))
	r.mu.Lock()
	f := r.onEvict
	r.mu.Unlock()
	if f != nil {
		f(runtime.GoldenEviction{ID: id, Root: image.Rootfs, ImageDigest: image.Digest, Bytes: 1, Reason: "test"})
	}
}

type record struct {
	Handle     runtime.Handle `json:"handle"`
	DiskPath   string         `json:"diskPath"`
	Shape      domain.Shape   `json:"shape"`
	StartedAt  time.Time      `json:"startedAt"`
	ReadyAt    time.Time      `json:"readyAt"`
	Paused     bool           `json:"paused,omitempty"`
	Exited     bool           `json:"exited,omitempty"`
	ExitReason string         `json:"exitReason,omitempty"`
}

var _ runtime.Runtime = (*Runtime)(nil)

// New returns a fake runtime over opts.Dir, creating it if needed.
func New(opts Options) (*Runtime, error) {
	if opts.Dir == "" {
		return nil, errors.New("fake: Dir is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, err
	}
	return &Runtime{opts: opts, faults: map[string]error{}}, nil
}

// InjectFault makes the next call to method (e.g. "ColdBoot", "Stop") fail
// with err. A nil err clears it.
func (r *Runtime) InjectFault(method string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.faults, method)
	} else {
		r.faults[method] = err
	}
}

// Crash marks a live instance as having exited unexpectedly.
func (r *Runtime) Crash(key runtime.Key) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, err := r.load(key)
	if err != nil || rec == nil {
		return fmt.Errorf("fake: crash %v: no instance", key)
	}
	rec.Exited, rec.ExitReason = true, "crashed"
	return r.save(rec)
}

// InstanceShape returns the shape key booted with, if it has a record.
func (r *Runtime) InstanceShape(key runtime.Key) (domain.Shape, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, err := r.load(key)
	if err != nil || rec == nil {
		return domain.Shape{}, false
	}
	return rec.Shape, true
}

func (r *Runtime) fault(method string) error {
	if err, ok := r.faults[method]; ok {
		delete(r.faults, method)
		return err
	}
	return nil
}

func (r *Runtime) path(k runtime.Key) string {
	return filepath.Join(r.opts.Dir, fmt.Sprintf("%s.g%d.json", k.VMID, k.Generation))
}

func (r *Runtime) load(k runtime.Key) (*record, error) {
	b, err := os.ReadFile(r.path(k))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rec record
	return &rec, json.Unmarshal(b, &rec)
}

func (r *Runtime) save(rec *record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	p := r.path(rec.Handle.Key)
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// current loads h's record and checks h still identifies it. A missing
// record yields (nil, nil).
func (r *Runtime) current(h runtime.Handle) (*record, error) {
	rec, err := r.load(h.Key)
	if err != nil || rec == nil {
		return nil, err
	}
	if rec.Handle.ProcessIdentity != h.ProcessIdentity {
		return nil, fmt.Errorf("%w: %v identity %s is not live %s", runtime.ErrStaleHandle, h.Key, h.ProcessIdentity, rec.Handle.ProcessIdentity)
	}
	return rec, nil
}

func (r *Runtime) phase(rec *record) runtime.Phase {
	switch {
	case rec == nil:
		return runtime.PhaseGone
	case rec.Exited:
		return runtime.PhaseExited
	case rec.Paused:
		return runtime.PhasePaused
	case r.opts.Now().Before(rec.ReadyAt):
		return runtime.PhaseStarting
	}
	return runtime.PhaseRunning
}

func (r *Runtime) Capabilities() domain.RuntimeCapabilities {
	return domain.RuntimeCapabilities{
		Engine:         Engine,
		Arch:           []domain.Arch{domain.Arch(goruntime.GOARCH)},
		ColdBoot:       true,
		GoldenRestore:  r.opts.Golden,
		RAMSnapshot:    r.opts.LiveFork,
		LiveFork:       r.opts.LiveFork,
		DiskFork:       true,
		GuestTransport: r.transportName(),
		Shape: domain.ShapeConstraints{
			MaxCPUs: 64, MinMemoryBytes: 128 * domain.MiB, MaxMemoryBytes: 256 * domain.GiB,
			MemoryAlignBytes: domain.MiB, MinDiskBytes: domain.MiB,
		},
		FingerprintRules: []string{},
	}
}

func (r *Runtime) Prepare(ctx context.Context, image domain.Image, shape domain.Shape) (runtime.Prepared, error) {
	if err := shape.Validate(r.Capabilities().Shape); err != nil {
		return runtime.Prepared{}, err
	}
	out := runtime.Prepared{Image: image, Shape: shape}
	if !r.opts.Golden {
		return out, nil
	}
	r.mu.Lock()
	err := r.fault("Prepare")
	r.mu.Unlock()
	if err != nil {
		return out, err
	}
	// Like a real engine, the golden is a boot of image.Rootfs: a create from
	// a snapshot passes the snapshot's disk there and gets its own golden.
	sum := goldenSum(image, shape)
	id := "golden-" + hex.EncodeToString(sum[:8])
	disk := filepath.Join(r.opts.Dir, id+".img")
	if _, err := os.Stat(disk); err != nil {
		b, err := textPrefix(image.Rootfs)
		if err != nil {
			return out, err
		}
		if err := os.WriteFile(disk, append(b, "golden\n"...), 0o400); err != nil {
			return out, err
		}
	}
	out.Golden = &runtime.Golden{ID: id, Fingerprint: hex.EncodeToString(sum[:]), Disk: disk}
	return out, nil
}

func (r *Runtime) ColdBoot(ctx context.Context, inst runtime.Instance, disks runtime.DiskSet, _ runtime.NetworkHandle) (runtime.Handle, error) {
	return r.boot(ctx, "ColdBoot", domain.BootModeColdBoot, inst, disks)
}

func (r *Runtime) boot(ctx context.Context, method string, mode domain.BootMode, inst runtime.Instance, disks runtime.DiskSet) (runtime.Handle, error) {
	r.mu.Lock()
	if err := r.fault(method); err != nil {
		r.mu.Unlock()
		return runtime.Handle{}, err
	}
	if inst.VMID == "" || inst.Generation < 1 {
		r.mu.Unlock()
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "fake: invalid instance key %v", inst.Key)
	}
	if _, err := os.Stat(disks.Root); err != nil {
		r.mu.Unlock()
		return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "fake: root disk not accessible")
	}
	rec, err := r.load(inst.Key)
	if err != nil {
		r.mu.Unlock()
		return runtime.Handle{}, err
	}
	if rec == nil || rec.Exited {
		now := r.opts.Now()
		rec = &record{
			Handle: runtime.Handle{
				Engine: Engine, Key: inst.Key, BootMode: mode,
				ProcessIdentity: strings.TrimPrefix(domain.NewID("proc"), "proc_"),
			},
			DiskPath: disks.Root, Shape: inst.Shape, StartedAt: now, ReadyAt: now.Add(r.opts.BootDelay),
		}
		if err := r.save(rec); err != nil {
			r.mu.Unlock()
			return runtime.Handle{}, err
		}
	}
	r.mu.Unlock()
	// Wait for boot outside the lock; the record already exists, so a caller
	// that dies here leaves a discoverable, still-booting instance.
	if wait := rec.ReadyAt.Sub(r.opts.Now()); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return runtime.Handle{}, ctx.Err()
		}
	}
	return rec.Handle, nil
}

func (r *Runtime) RestoreGolden(ctx context.Context, inst runtime.Instance, g runtime.Golden, disks runtime.DiskSet, _ runtime.NetworkHandle) (runtime.Handle, error) {
	if !r.opts.Golden {
		return runtime.Handle{}, runtime.Unsupported(Engine, "goldenRestore")
	}
	// Like a real engine: the golden must be the one Prepare gives this
	// instance's image (its root included) and shape.
	if sum := goldenSum(inst.Image, inst.Shape); hex.EncodeToString(sum[:]) != g.Fingerprint {
		return runtime.Handle{}, domain.Errorf(domain.CodeUnsupportedCapability, "fake: golden %s is not compatible with this instance's image or shape", g.ID)
	}
	r.mu.Lock()
	rec, err := r.load(inst.Key)
	r.mu.Unlock()
	if err != nil {
		return runtime.Handle{}, err
	}
	if rec == nil || rec.Exited {
		want, err := os.ReadFile(g.Disk)
		if err != nil {
			return runtime.Handle{}, domain.Errorf(domain.CodeNotFound, "fake: golden %s not prepared", g.ID)
		}
		// The private disk is the golden disk grown sparsely: compare the
		// golden-sized prefix.
		got := make([]byte, len(want))
		f, err := os.Open(disks.Root)
		if err == nil {
			_, err = io.ReadFull(f, got)
			f.Close()
		}
		if err != nil || !bytes.Equal(got, want) {
			return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "fake: disk is not golden %s's generation", g.ID)
		}
	}
	return r.boot(ctx, "RestoreGolden", domain.BootModeGoldenRestore, inst, disks)
}

// cpFile is a fake checkpoint: the source key and its disk digest.
type cpFile struct {
	Key    runtime.Key `json:"key"`
	Digest string      `json:"digest"`
}

// diskDigest hashes the leading part of a disk (disks grow sparsely).
func diskDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, 16<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (r *Runtime) Checkpoint(ctx context.Context, h runtime.Handle, _ runtime.Quiescence) (runtime.Checkpoint, error) {
	if !r.opts.LiveFork {
		return runtime.Checkpoint{}, runtime.Unsupported(Engine, "ramSnapshot")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fault("Checkpoint"); err != nil {
		return runtime.Checkpoint{}, err
	}
	rec, err := r.current(h)
	if err != nil {
		return runtime.Checkpoint{}, err
	}
	if !rec.Paused || r.phase(rec) != runtime.PhasePaused {
		return runtime.Checkpoint{}, domain.Errorf(domain.CodeInvalidState, "fake: checkpoint needs a paused instance")
	}
	d, err := diskDigest(rec.DiskPath)
	if err != nil {
		return runtime.Checkpoint{}, err
	}
	id := domain.NewID("cp")
	b, _ := json.Marshal(cpFile{Key: h.Key, Digest: d})
	if err := os.WriteFile(filepath.Join(r.opts.Dir, id+".cp.json"), b, 0o600); err != nil {
		return runtime.Checkpoint{}, err
	}
	return runtime.Checkpoint{ID: id, Fingerprint: d}, nil
}

func (r *Runtime) DiscardCheckpoint(_ context.Context, cp runtime.Checkpoint) error {
	if cp.ID == "" || strings.ContainsAny(cp.ID, "/.") {
		return domain.Errorf(domain.CodeInvalidArgument, "fake: invalid checkpoint %q", cp.ID)
	}
	if err := os.Remove(filepath.Join(r.opts.Dir, cp.ID+".cp.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (r *Runtime) RestoreCheckpoint(ctx context.Context, inst runtime.Instance, cp runtime.Checkpoint, disks runtime.DiskSet, _ runtime.NetworkHandle) (runtime.Handle, error) {
	if !r.opts.LiveFork {
		return runtime.Handle{}, runtime.Unsupported(Engine, "ramSnapshot")
	}
	r.mu.Lock()
	rec, err := r.load(inst.Key)
	r.mu.Unlock()
	if err != nil {
		return runtime.Handle{}, err
	}
	if rec == nil || rec.Exited {
		if cp.ID == "" || strings.ContainsAny(cp.ID, "/.") {
			return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "fake: invalid checkpoint %q", cp.ID)
		}
		b, err := os.ReadFile(filepath.Join(r.opts.Dir, cp.ID+".cp.json"))
		if err != nil {
			return runtime.Handle{}, domain.Errorf(domain.CodeNotFound, "fake: checkpoint %s not found", cp.ID)
		}
		var c cpFile
		if err := json.Unmarshal(b, &c); err != nil {
			return runtime.Handle{}, err
		}
		if d, err := diskDigest(disks.Root); err != nil || d != c.Digest {
			return runtime.Handle{}, domain.Errorf(domain.CodeInvalidArgument, "fake: disk is not checkpoint %s's generation", cp.ID)
		}
	}
	return r.boot(ctx, "RestoreCheckpoint", domain.BootModeRAMRestore, inst, disks)
}

func (r *Runtime) setPaused(h runtime.Handle, paused bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, err := r.current(h)
	if err != nil {
		return err
	}
	if p := r.phase(rec); p != runtime.PhaseRunning && p != runtime.PhasePaused {
		return domain.Errorf(domain.CodeInvalidState, "fake: instance %v is %s", h.Key, p)
	}
	rec.Paused = paused
	return r.save(rec)
}

func (r *Runtime) Pause(_ context.Context, h runtime.Handle) error   { return r.setPaused(h, true) }
func (r *Runtime) Unpause(_ context.Context, h runtime.Handle) error { return r.setPaused(h, false) }

func (r *Runtime) Stop(ctx context.Context, h runtime.Handle, grace time.Duration) error {
	r.mu.Lock()
	if err := r.fault("Stop"); err != nil {
		r.mu.Unlock()
		return err
	}
	rec, err := r.current(h)
	r.mu.Unlock()
	if err != nil || rec == nil || rec.Exited {
		return err
	}
	if r.opts.StopDelay > grace {
		// Like a real guest that ignores the request: the grace runs out.
		select {
		case <-time.After(grace):
		case <-ctx.Done():
			return ctx.Err()
		}
		return domain.Errorf(domain.CodeTimeout, "fake: guest did not shut down within %s", grace)
	}
	select {
	case <-time.After(r.opts.StopDelay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.exit(h, "shutdown")
}

func (r *Runtime) Kill(_ context.Context, h runtime.Handle) error { return r.exit(h, "killed") }

func (r *Runtime) exit(h runtime.Handle, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, err := r.current(h)
	if err != nil || rec == nil || rec.Exited {
		return err
	}
	rec.Exited, rec.ExitReason = true, reason
	return r.save(rec)
}

func (r *Runtime) Inspect(_ context.Context, h runtime.Handle) (runtime.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, err := r.current(h)
	if err != nil {
		return runtime.Status{}, err
	}
	st := runtime.Status{Phase: r.phase(rec)}
	if rec != nil {
		st.StartedAt, st.ExitReason = rec.StartedAt, rec.ExitReason
	}
	return st, nil
}

func (r *Runtime) Discover(context.Context) ([]runtime.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	paths, err := filepath.Glob(filepath.Join(r.opts.Dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []runtime.Handle
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var rec record
		if json.Unmarshal(b, &rec) == nil {
			out = append(out, rec.Handle)
		}
	}
	return out, nil
}

func (r *Runtime) transportName() string {
	if r.opts.GuestTransport != nil {
		return "in-process"
	}
	return "none"
}

// GuestTransport returns a dialer for h's instance. Each dial re-checks
// that h is still the live instance and running, as a real VMM's vsock only
// accepts connections once the guest is up; callers retry while booting.
func (r *Runtime) GuestTransport(ctx context.Context, h runtime.Handle) (runtime.Dialer, error) {
	if r.opts.GuestTransport == nil {
		return nil, runtime.Unsupported(Engine, "guestTransport")
	}
	r.mu.Lock()
	_, err := r.current(h)
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	inner, err := r.opts.GuestTransport(ctx, h)
	if err != nil {
		return nil, err
	}
	return guestDialer{r: r, h: h, inner: inner}, nil
}

type guestDialer struct {
	r     *Runtime
	h     runtime.Handle
	inner runtime.Dialer
}

func (d guestDialer) DialGuest(ctx context.Context, port uint32) (net.Conn, error) {
	d.r.mu.Lock()
	rec, err := d.r.current(d.h)
	phase := d.r.phase(rec)
	d.r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if phase != runtime.PhaseRunning {
		return nil, domain.Errorf(domain.CodeUnavailable, "instance %v is %s", d.h.Key, phase)
	}
	return d.inner.DialGuest(ctx, port)
}

func (r *Runtime) Destroy(_ context.Context, h runtime.Handle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, err := r.current(h)
	if err != nil || rec == nil {
		return err
	}
	// A live instance is killed first: Destroy always leaves nothing behind.
	err = os.Remove(r.path(h.Key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// textPrefix reads a fake disk's content: the text before the first NUL. A
// committed disk is grown sparsely to the VM's disk size (GiBs of zeros),
// which a golden of a snapshot must not copy.
func textPrefix(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, 1<<20)
	n, err := io.ReadFull(f, b)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	b = b[:n]
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return b, nil
}

// goldenSum is a golden's fingerprint: the image (its root included) and shape.
func goldenSum(image domain.Image, shape domain.Shape) [32]byte {
	return sha256.Sum256([]byte(image.Digest + "\x00" + image.Rootfs + "\x00" + fmt.Sprint(shape)))
}
