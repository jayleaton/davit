// Package runtime is the semantic engine-adapter contract (SPEC §5). It is
// not an engine API passthrough: adapters do not own user policy, SQLite or
// snapshot names; the lifecycle service composes runtime + storage.
//
// Every adapter must pass runtimetest.Run. Behaviour the suite pins down:
//   - Instances are keyed by (VM ID, generation). ColdBoot of a key that is
//     already live returns the existing handle, so a restarted daemon can
//     safely replay an interrupted boot without spawning a second VMM.
//   - Discover lists every instance the adapter supervises, including those
//     started by a previous daemon process. Lifecycle reconciles them.
//   - Handles carry generation and process identity; a stale handle (old
//     generation or replaced process) is rejected with ErrStaleHandle and
//     can never mutate a replacement instance.
//   - Stop, Kill and Destroy are idempotent; Destroy of a gone instance is ok.
//   - Unsupported optional features return a CodeUnsupportedCapability
//     *domain.Error and are reported false in Capabilities.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"time"

	"davit/internal/domain"
)

// ErrStaleHandle is returned (wrapped) when a handle no longer identifies the
// live instance for its key.
var ErrStaleHandle = errors.New("runtime: stale handle")

// Key identifies one run of a VM.
type Key struct {
	VMID       domain.VMID `json:"vmId"`
	Generation int64       `json:"generation"`
}

// Instance is what the lifecycle asks the runtime to run.
type Instance struct {
	Key
	Shape domain.Shape
	Image domain.Image
}

// DiskSet is the host-side disk layout for one instance.
type DiskSet struct {
	Root string // private writable root disk path (from a storage lease)
}

// NetworkHandle is the per-instance network attachment prepared by the
// network workstream (netns/TAP on Linux). The zero value means "no guest
// network": the guest is reachable only over its guest transport.
type NetworkHandle struct {
	// Namespace is the VM's netns path: /proc/<holder>/ns/net (rootless) or
	// /run/netns/slw-<id> (privileged).
	Namespace string
	// TapName is the guest NIC's TAP inside Namespace ("tap0" everywhere).
	TapName string
	// UserNamespace owns Namespace in rootless mode; the VMM joins it before
	// the netns.
	UserNamespace string
	// GuestMAC is the virtio-net MAC, constant in every netns so restored
	// snapshots and forks need no device reconfiguration.
	GuestMAC string
	// EnterArgv is the argv prefix that runs the VMM inside the namespaces
	// (network.EnterArgv), for adapters without a jailer.
	EnterArgv []string
}

// Handle is the durable reference to a running instance. It is persisted by
// lifecycle and must round-trip through JSON.
type Handle struct {
	Engine string `json:"engine"`
	Key
	// ProcessIdentity distinguishes this VMM process from any other that has
	// held the same key (never a bare PID).
	ProcessIdentity string          `json:"processIdentity"`
	BootMode        domain.BootMode `json:"bootMode"`
	Data            json.RawMessage `json:"data,omitempty"`
}

// Phase is the observed VMM state.
type Phase string

const (
	PhaseStarting Phase = "starting"
	PhaseRunning  Phase = "running"
	PhasePaused   Phase = "paused"
	PhaseExited   Phase = "exited"
	PhaseGone     Phase = "gone" // no trace of the instance remains
)

// Status is the result of Inspect.
type Status struct {
	Phase     Phase
	StartedAt time.Time
	// ExitReason explains PhaseExited ("shutdown", "killed", "crashed").
	ExitReason string
}

// Prepared is an image prepared for a shape (kernel/rootfs checks, golden).
type Prepared struct {
	Image  domain.Image
	Shape  domain.Shape
	Golden *Golden
}

// Golden is a trusted booted checkpoint with its exact disk generation.
// Disk is that generation (host path, read-only): lifecycle clones it into
// the private disk it passes to RestoreGolden.
type Golden struct {
	ID          string
	Fingerprint string
	Disk        string
}

// Checkpoint is an optional RAM/device checkpoint.
type Checkpoint struct {
	ID          string
	Fingerprint string
}

// Quiescence describes how a checkpoint is taken.
type Quiescence struct {
	FreezeFilesystems bool
}

// Dialer opens authenticated-capable streams to the guest agent. It is never
// a raw global guest address.
type Dialer interface {
	DialGuest(ctx context.Context, port uint32) (net.Conn, error)
}

// Runtime is the engine adapter interface.
type Runtime interface {
	Capabilities() domain.RuntimeCapabilities
	// Prepare checks image and shape and returns the golden for them, if the
	// engine has one. A golden is a boot of image.Rootfs, and its identity
	// covers that path: lifecycle passes a snapshot's committed disk as Rootfs
	// for a create from that snapshot, which must get a golden of its own.
	Prepare(ctx context.Context, image domain.Image, shape domain.Shape) (Prepared, error)
	ColdBoot(ctx context.Context, inst Instance, disks DiskSet, net NetworkHandle) (Handle, error)
	RestoreGolden(ctx context.Context, inst Instance, golden Golden, disks DiskSet, net NetworkHandle) (Handle, error)
	// Checkpoint captures h's RAM and device state. h must be paused and
	// stays paused: the caller captures the matching disk generation before
	// Unpause (RAM and disk must be from the same instant, SPEC §6).
	Checkpoint(ctx context.Context, h Handle, q Quiescence) (Checkpoint, error)
	// RestoreCheckpoint starts inst from cp. disks.Root must be a private
	// clone of the disk generation captured with cp that no VMM has opened.
	RestoreCheckpoint(ctx context.Context, inst Instance, cp Checkpoint, disks DiskSet, net NetworkHandle) (Handle, error)
	// DiscardCheckpoint releases cp's artifacts; instances already restored
	// from it keep running. Idempotent.
	DiscardCheckpoint(ctx context.Context, cp Checkpoint) error
	Pause(ctx context.Context, h Handle) error
	Unpause(ctx context.Context, h Handle) error
	// Stop asks the guest to shut down cleanly and returns once the VMM has
	// exited, or an error if it did not exit within grace.
	Stop(ctx context.Context, h Handle, grace time.Duration) error
	// Kill terminates the VMM immediately (crash-consistent disk).
	Kill(ctx context.Context, h Handle) error
	Inspect(ctx context.Context, h Handle) (Status, error)
	// Discover returns every instance this adapter supervises.
	Discover(ctx context.Context) ([]Handle, error)
	GuestTransport(ctx context.Context, h Handle) (Dialer, error)
	// Destroy kills h if still live and releases all its runtime resources;
	// idempotent.
	Destroy(ctx context.Context, h Handle) error
}

// Unsupported returns the standard error for a missing optional capability.
func Unsupported(engine, feature string) error {
	return domain.Errorf(domain.CodeUnsupportedCapability, "%s runtime does not support %s", engine, feature).
		WithDetail("capability", feature)
}

// GoldenEviction is a golden an engine removed to keep the free-space floor
// of its data dir (GoldenEvictor). The next create that needs it captures
// it again; instances already restored from it keep running.
type GoldenEviction struct {
	ID string
	// Root is the disk the golden was a boot of: an image rootfs, or a
	// snapshot's committed disk (a project image).
	Root        string
	ImageDigest string
	// Bytes is the free space the eviction gave back.
	Bytes  int64
	Reason string
}

// GoldenSweeper is implemented by engines that keep goldens: SweepGoldens
// removes those that can never be used again (their root disk was
// collected), at once rather than on the engine's own schedule.
type GoldenSweeper interface {
	SweepGoldens(ctx context.Context) (removed int, bytes int64, err error)
}

// GoldenEvictor is implemented by engines that evict least-recently-used
// goldens when their data dir runs low on space.
type GoldenEvictor interface {
	// OnGoldenEvicted sets the function told about each eviction.
	OnGoldenEvicted(func(GoldenEviction))
	// EnforceGoldenFloor evicts until the floor is met (or only protected
	// goldens are left).
	EnforceGoldenFloor(ctx context.Context) error
}
