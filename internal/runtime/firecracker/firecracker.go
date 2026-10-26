// Package firecracker is the Linux runtime adapter (SPEC §5, §6) over raw
// Firecracker microVMs. It runs rootless: /dev/kvm access is enough.
//
// Layout under Options.Dir:
//
//	instances/<vm>.g<gen>/  one directory per instance key; the VMM runs with
//	                        it as cwd, so drive (root.img -> storage lease),
//	                        API (api.sock) and vsock (vsock.sock) paths are
//	                        relative and a golden snapshot restores into any
//	                        instance directory.
//	golden/<id>/            immutable golden checkpoints: vmstate, mem and the
//	                        exact disk generation they were captured with.
//
// Networking belongs to lifecycle and internal/network: an instance runs
// inside the namespaces its runtime.NetworkHandle names (EnterArgv prefix)
// with the handle's TAP as eth0.
//
// VMM processes are started in their own session and outlive the daemon. An
// instance is identified by pid + kernel start time + host boot ID, never by
// pid alone; a restarted daemon re-adopts live instances from their records
// (or, if the daemon died before writing one, by the VMM's cwd).
package firecracker

import (
	"context"
	"log/slog"
	"time"

	"davit/internal/domain"
	"davit/internal/runtime"
)

// Engine is the engine name reported in capabilities and handles.
const Engine = "firecracker"

// AgentPort is the guest agent's vsock port (images README: guest contract).
const AgentPort uint32 = 1024

// Options configures the adapter.
type Options struct {
	// Dir holds instance directories and goldens. Required. It must be on
	// the same filesystem as storage for golden disks to reflink.
	Dir string
	// Binary is the verified firecracker binary (see EnsureBinary). Required.
	Binary string
	// CaptureNetwork provisions the network a golden capture VM runs in
	// (lifecycle provisions every instance's network; captures have no
	// instance). It must give the same device layout instances get, since a
	// golden restores only into the layout it was captured with. Nil
	// captures without a NIC, so goldens then match instances that have no
	// network.
	CaptureNetwork func(ctx context.Context) (runtime.NetworkHandle, func(), error)
	// NIC is the guest NIC layout every instance's network handle (and
	// CaptureNetwork) provides; zero means no guest NIC. It is fixed by
	// configuration so a golden's fingerprint is known before any network
	// exists.
	NIC NIC
	// GuestReady gates golden capture: the snapshot is taken once it returns
	// nil. Nil waits until the guest agent port accepts a connection. The
	// guest-agent workstream supplies a handshake-based gate.
	GuestReady func(ctx context.Context, d runtime.Dialer) error
	// GuestShutdown asks the guest to shut down cleanly (the agent's
	// Shutdown RPC); Stop retries it until the guest answers. A guest reboot
	// ends the VMM (reboot=k); a power-off halts it and Stop reaps it once
	// the console shows the kernel finished. Stop also sends Ctrl+Alt+Del,
	// which works only if the guest kernel has the i8042 keyboard driver
	// (the M1 image kernel does not).
	GuestShutdown func(ctx context.Context, key runtime.Key, d runtime.Dialer) error
	// CheckpointDir holds live-fork RAM checkpoints; empty means
	// Dir/checkpoints. They are written while the source is paused and read
	// once, so a tmpfs here makes the pause short and predictable but pins
	// each child's RAM image in host memory (unreclaimable) for its life;
	// on disk those pages stay reclaimable page cache.
	CheckpointDir string
	// GoldenTimeout bounds golden preparation (boot until settled, then the
	// snapshot write). Zero scales with the guest: goldenBudget.
	GoldenTimeout time.Duration
	// GoldenMinFree is the free space to keep on Dir's filesystem by evicting
	// least-recently-used goldens (and before a capture, room for it). Zero
	// means DefaultGoldenMinFree; negative disables eviction.
	GoldenMinFree int64
	Log           *slog.Logger
}

// DefaultGoldenMinFree is the golden free-space floor when none is set.
const DefaultGoldenMinFree = 10 * domain.GiB

// goldenBudget is the time a golden capture of shape may take when
// Options.GoldenTimeout is zero: 60 s for booting until the guest settles,
// plus 20 s per GiB of guest memory for writing the memory file (a 3 GiB
// guest on nested KVM overran a flat 60 s). An explicit timeout wins.
func goldenBudget(explicit time.Duration, shape domain.Shape) time.Duration {
	if explicit > 0 {
		return explicit
	}
	gib := (shape.MemoryBytes + domain.GiB - 1) / domain.GiB
	return 60*time.Second + time.Duration(gib)*20*time.Second
}

// NIC is a guest NIC layout: the TAP the VMM attaches as eth0 and its MAC.
type NIC struct {
	Tap string
	MAC string
}
