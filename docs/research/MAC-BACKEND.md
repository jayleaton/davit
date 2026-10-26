# Native Apple-silicon Mac backend (G8)

Design proposal, **2026-10-22**. No Mac prototype or performance
measurement was run for this document. This specifies the implementation and release gates;
it does not report a shipped backend.

**Status:** the Go side is implemented: the CGO-free adapter
`internal/runtime/mac` over [helper protocol v1](MAC-HELPER-PROTOCOL.md), passing `runtimetest`
against a Go fake helper; APFS `clonefile` storage (`internal/storage/clone_darwin.go`); daemon
selection on Apple silicon and the `macos`/`mac_helper` doctor checks. Not done: the signed Swift
helper, any run on a real Mac, the packet-link network contract and Darwin netstack (Mac VMs are
network-none), and every measurement in §6.

## Decision and scope

Implement `internal/runtime/mac` as a CGO-free Go adapter controlling a small, signed Swift helper
over private Unix sockets. Each VM has its own helper process owning its `VZVirtualMachine`.
Use Virtualization.framework (VZ), a native arm64 Ubuntu guest, virtio block/network/vsock, and
APFS file clones for private raw ext4 disks. Keep lifecycle, metadata, ownership, admission,
snapshot names and tailnet authorization in the existing Go services.

Ship **cold boot + disk-only stop/resume/fork first**. Use the same userspace egress policy on
Linux and Mac; VZ NAT is a diagnostic baseline, not the production policy boundary. Enable
golden restore, RAM checkpoints or live fork only after separate configuration, identity,
concurrency and performance gates pass on actual Mac hardware.

This is feature G8, which depends on G1 (arm64 release artifacts). Native hosting,
APFS disk lifecycle, Docker, tailnet access and measured first-command/fork/resume times are
required for a Mac release. A Darwin CLI talking to a Linux server does not satisfy G8.
Follow [SPEC §§5–8](SPEC.md#5-host-daemon-and-adapter-boundaries); Linux delivery does not wait
for Mac RAM restore.

## 1. Engine selection and speed

### Alternatives

| Engine/integration | Cold path and disk clones | RAM/golden path | Decision |
| --- | --- | --- | --- |
| VZ via signed Swift helper | Direct Linux kernel boot avoids EFI work; APFS clones avoid full disk copies. Actual Ubuntu readiness and helper startup cost remain unmeasured. | macOS 14+ save/restore APIs; exact Linux bootloader/devices and concurrent reuse must pass validation and execution tests. | Selected: smallest native bridge while preserving the CGO-free daemon. |
| VZ linked into Go through Code-Hex/vz | Same VZ engine and APFS primitives; fewer control IPC boundaries, but no evidence that this materially improves first-command time. | Same Apple restrictions; a binding cannot add missing VZ capabilities. | Violates the daemon's CGO-free boundary. A separate Go/cgo helper is viable, but Swift accesses new Apple APIs directly. |
| Lima or vfkit as the external VZ process | Useful boot/network baselines and reusable implementation knowledge; full Lima orchestration adds another lifecycle/configuration owner. | Must inspect the pinned release; a snapshot command is not proof of reusable Linux RAM templates. | Benchmark/reference before writing unnecessary bridge code; do not nest a second VM control plane. |
| Tart | Native VZ, raw disk cloning and CI automation provide useful comparisons; no comparable local timing established. | Current source has Linux suspension support, with device and identity restrictions. | Reference only; current FSL license is not an unrestricted OSS foundation. |
| UTM / QEMU with HVF | UTM supplies both Apple virtualization and QEMU paths. QEMU brings a broader device model and image machinery; direct-kernel/HVF versus VZ speed needs measurement. | QEMU snapshot support is a different contract and format, not VZ save files. | Useful compatibility baseline; too much engine/device surface for the initial backend. |
| Hypervisor.framework directly; libkrun/microsandbox; Apple Containerization | HVF alone requires owning devices/boot. Smaller container-oriented guests may reduce boot work, but must prove Ubuntu/systemd, persistent full disk and Docker semantics. | No inferred Linux snapshot/fork portability. | Revisit if the VZ cold path misses product targets; compare the same guest workload. |

Primary references: [Code-Hex/vz](https://github.com/Code-Hex/vz),
[lima-vm/vz](https://github.com/lima-vm/vz) (the historical fork is cancelled in favor of revived
upstream), [Lima VZ](https://lima-vm.io/docs/config/vmtype/vz/),
[vfkit](https://github.com/crc-org/vfkit), [UTM](https://github.com/utmapp/UTM),
[QEMU HVF](https://www.qemu.org/docs/master/system/introduction.html),
[libkrun](https://github.com/libkrun/libkrun),
[microsandbox](https://github.com/superradcompany/microsandbox), and
[Apple Containerization](https://github.com/apple/containerization). These are alternatives and
implementation precedents, not measured speed rankings.

**Tart and Linux suspend:** earlier reports that Tart rejects Linux suspend are stale relative
to source inspected here. At Tart commit
[`6063c8e`](https://github.com/openai/tart/tree/6063c8e710d93e1f7b813b611e2c40d64af532c7),
[`Linux.swift`](https://github.com/openai/tart/blob/6063c8e710d93e1f7b813b611e2c40d64af532c7/Sources/tart/Platform/Linux.swift)
implements `PlatformSuspendable`, persists a VZ generic machine identifier, and omits a pointing
device whose restore fails despite validation. The
[`Run.swift`](https://github.com/openai/tart/blob/6063c8e710d93e1f7b813b611e2c40d64af532c7/Sources/tart/Commands/Run.swift)
path checks that identifier, validates save support, and handles concurrent identifier collisions
by regenerating identity/resetting state. Its
[`VM.swift`](https://github.com/openai/tart/blob/6063c8e710d93e1f7b813b611e2c40d64af532c7/Sources/tart/VM.swift)
removes entropy/audio devices in suspendable configurations. This warrants a Linux prototype;
it does not establish Davit's direct-kernel + vsock + socket-network configuration or reusable
goldens. Consult the pinned [FSL-1.1-ALv2 license](https://github.com/openai/tart/blob/6063c8e710d93e1f7b813b611e2c40d64af532c7/LICENSE)
before any code reuse.

### Signed helper boundary

Proposed placement: `internal/runtime/mac/` for Go, `internal/runtime/mac/helper/` for native
source/build metadata, and `packaging/` for the signed artifact and launchd packaging. Swift is
preferred; Objective-C is an acceptable small shim where API/FD handling is clearer. Neither
language belongs in the Go daemon's address space. Build native artifacts on a Mac builder;
the existing CGO-free Darwin cross build remains useful but cannot produce/sign the helper.

The daemon's user launchd service and per-VM helpers run as the same dedicated user, without
root. The helper needs [`com.apple.security.virtualization`](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.security.virtualization).
Check `VZVirtualMachine.isSupported` and `configuration.validate()` in the actual signed process.
Do not request `com.apple.security.hypervisor` just because VZ uses hardware virtualization.
Neither the selected socket attachment nor VZ NAT needs the restricted `com.apple.vm.networking`
entitlement; bridged/vmnet options have separate requirements and are outside this release.
Apple's [entitlement instructions](https://developer.apple.com/documentation/virtualization/adding-the-virtualization-entitlement-to-your-project)
allow removing the App Sandbox entitlement when otherwise unnecessary. Start with hardened
runtime and a narrow protocol; do not claim App Sandbox confinement that has not been validated.

Official distribution signs all executables with Developer ID, secure timestamps and hardened
runtime, notarizes the distribution and staples its supported container/bundle. Pin helper
digest, signing identity and IPC version to the daemon release; verify before launch. Local
development may use an explicitly selected ad-hoc signed build; that is not release trust.
Never ask users to disable Gatekeeper or strip quarantine as installation procedure.
[Apple notarization requirements](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution).

The helper owns VZ objects on one serial dispatch queue with a live run loop, translating
asynchronous completions into bounded responses. Use versioned, length-limited control RPC
with request IDs, `(vmId, generation)`, expected process identity and typed errors. The first
exchange checks protocol range, helper build and supported operations; incompatible versions
fail before starting a VM. Control has separate sockets from guest byte streams and packet I/O
so a blocked exec cannot block Stop/Inspect. No arbitrary selectors, shell commands, URLs or
unchecked host paths. Bound queues, message sizes, concurrent guest streams and connection timeouts.

Per-instance runtime directories are owner-only (0700), sockets 0600; check Unix peer UID
using Darwin peer credentials. Resolve only daemon-approved regular disk/kernel artifacts,
reject symlinks/escapes, and bind disk device/inode plus generation in the launch record. Same-user
host processes remain inside the trusted host boundary; guests never see the control sockets.
SCM_RIGHTS transfers packet FDs on authenticated control sessions. VZ vsock streams initially
use a dedicated helper-to-Go Unix byte-stream proxy with backpressure, EOF/half-close, deadlines
and cancellation; this avoids depending on undocumented lifetime behavior of exported VZ FDs.

### Runtime semantics and supervision

[`runtime.Runtime`](../../internal/runtime/runtime.go) is the contract, not a VZ API facade.
`Prepare` verifies image architecture/digests, bootloader/devices and shape; it returns no golden
until certified. `ColdBoot` obtains a per-key filesystem lock, reconnects to a live helper first,
or spawns one and returns only after VZ is running. It does not publish product readiness;
lifecycle still runs the authenticated guest barrier.

Each helper durably records a JSON-round-trippable handle before acknowledging boot. Use engine
`mac-vz`, key `(VMID, generation)`, helper incarnation UUID, OS process birth identity and host
boot identity; never PID alone. `Handle.Data` contains only versioned internal reconnection/config
references, no guest bootstrap secrets. The worker retains a lifetime lock and control socket.
Duplicate boot of a live key returns the same identity. A replacement after exit gets a new
identity. Stale Stop/Kill/Destroy requests return wrapped `runtime.ErrStaleHandle` and cannot
affect the replacement. New generations remain distinct keys; lifecycle governs overlap.

Helpers survive API-daemon disconnects, including launches whose response was lost; do not
use parent-death termination. `Discover` scans the owned runtime root, authenticates live
helpers and compares identity, not just record/socket existence. Reconcile starting, running,
paused and exited records on daemon restart before allowing another boot or GC. A wedged or
unverifiable helper is an explicit recovery error, not permission to open its disk again.
Helper crash terminates that VM; host restart cold-boots retained committed disks only through
lifecycle recovery. launchd must not automatically respawn a VM with an already leased disk.

`Pause`/`Unpause` wait for confirmed VZ state transitions. `Stop(grace)` requests guest shutdown
and waits for stopped state, closes VZ disk/device resources and exits the worker before success.
Timeout returns an error and retains the disk/last good commit; it never escalates silently.
`Kill` verifies identity then terminates/reaps the worker, yielding crash-consistent disk only.
`Destroy` additionally removes runtime records/sockets after proving exit. Stop/Kill/Destroy
are idempotent for a gone instance; Inspect reports exited/gone rather than inventing running
state from an old record. Zero `NetworkHandle` means **no NIC**, with vsock still available.

### macOS 14+ save/restore: optional experiment, not an assumption

Apple's [`saveMachineStateTo`](https://developer.apple.com/documentation/virtualization/vzvirtualmachine/savemachinestateto%28url%3Acompletionhandler%3A%29)
and [`restoreMachineStateFrom`](https://developer.apple.com/documentation/virtualization/vzvirtualmachine/restoremachinestatefrom%28url%3Acompletionhandler%3A%29)
are available since macOS 14. Save needs a paused VM; restore needs a stopped VM and produces a
paused VM that must be resumed. Call
[`validateSaveRestoreSupport()`](https://developer.apple.com/documentation/virtualization/vzvirtualmachineconfiguration/validatesaverestoresupport%28%29)
on the **exact** shipping configuration. API availability and successful validation alone are
insufficient: perform save/restore and the guest workload. Apple's
[WWDC23 explanation](https://developer.apple.com/videos/play/wwdc2023/10007/) requires consistent
external resources/configuration and describes saved state as tied to the originating Mac/user.
Treat it as local opaque state, not a portable image or a Firecracker memory mapping.

Prototype direct-kernel and EFI Linux separately. Tart's EFI route is useful fallback evidence;
it does not prove `VZLinuxBootLoader` is savable. Start with minimal headless devices; test vsock,
network attachment, entropy and balloon devices individually. Do not remove entropy to pass
validation without proving host-supplied reseeding and clone-safe guest randomness before workloads.
Persist [`VZGenericPlatformConfiguration.machineIdentifier`](https://developer.apple.com/documentation/virtualization/vzgenericplatformconfiguration/machineidentifier)
as runtime configuration, distinct from the guest machine-id. Test whether a restore permits a
new VZ identifier and whether the same saved identifier can run concurrently. If it cannot,
same-VM suspend may work while reusable goldens and live fork remain unavailable.

Checkpoint ordering, if supported: quiesce applications/filesystems while guest code can still
run; pause; drain/flush host block I/O; save RAM/device state; commit the exact disk generation
while paused; durably publish the pair; then resume/thaw the source. The current guest protocol
has no filesystem-freeze RPC: any such addition needs a dedicated contract change. Never send
a freeze RPC after pausing CPUs. A VZ save file does not include external disks. Disk-only stop
continues to use clean shutdown; a plain APFS clone of a running disk is not a clean snapshot.

For `Checkpoint`, reject a running handle and leave the source paused. For restore, create a new
VZ VM with matching configuration and a fresh private disk clone, restore, reconnect vsock and
network, resume under the barrier and return a running handle. Pin RAM/disk resources until no
runtime depends on them; idempotent `DiscardCheckpoint` cannot break existing children.
Fingerprint helper/IPC format, host Mac/user identity, macOS build, CPU/platform, VZ identifier,
bootloader/kernel/initrd/cmdline, image and paired disk manifest, device layout/MAC/MTU, CPU count
and RAM size. Reject incompatible or corrupt state before readiness. Require explicit disk-only
recovery for lost user execution state; never silently discard a requested RAM restore.

A trusted golden contains no owner work, node state or reusable secrets. Capture it at a tested
identity bootstrap barrier and restore **only onto its paired disk generation**. `resume` and
disk forks cold-boot their saved disks; never overlay golden RAM on modified disk state. Neutral
pre-mount bootstrap restore is a separate experiment. VZ file save/restore might eagerly process
large RAM state; APFS-cloning the save file does not prove lazy/shared RAM or faster first command.

## 2. APFS storage adapter

Keep the [`storage.Store`](../../internal/storage/storage.go) contract and raw image/manifest
layout in [`fs.go`](../../internal/storage/fs.go). Add a Darwin clone strategy inside
`internal/storage`, sharing publication, ID validation, leases and GC rather than duplicating
the Store. Before this design, [`clone_other.go`](../../internal/storage/clone_other.go) reported
unsupported and Darwin copied (since implemented in `clone_darwin.go`). Refactor the internal clone/publication boundary: Linux `reflink(dst, src)`
receives a precreated destination, but APFS `clonefile` requires an absent destination pathname.
Use a private temporary directory and clone into its nonexistent child, then open/grow/sync it
and publish. Keep Linux FICLONE behavior and copy fallback intact.

APFS clones inherit source metadata: a clone of a read-only commit must explicitly become
owner-writable before opening for writes, growing or handing it to VZ. Preserve the source's
immutable permissions and validate/remove inherited flags or ACLs that would prevent private
writes. Set owner-only permissions on the private clone; do not rely on the Linux path's
precreated destination permissions.

Use the already pinned `golang.org/x/sys/unix` Darwin `Clonefile`/`Fclonefileat` wrappers in Go;
their Darwin arm64 definitions were inspected locally. No cgo or helper round trip is needed
for disk cloning. Prefer source/destination-directory FDs and no-follow validation to avoid
pathname replacement. [Apple APFS cloning](https://developer.apple.com/library/archive/documentation/FileManagement/Conceptual/APFS_Guide/Features/Features.html)
and the [Apple clonefile manual source](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/man/man2/clonefile.2)
describe CoW semantics, nonexistent destinations and delayed ENOSPC on later writes.

| Store operation | Required APFS behavior |
| --- | --- |
| `Probe` | Actually clone known bytes in the store volume, verify content and independent writes, then remove probe files. Report `ModeReflink`, `FastClone=true`, Detail naming APFS/clonefile only after success. The current wire mode remains `reflink`; do not invent `apfs` without a shared-contract change. |
| `CreateWritable` | Clone immutable base into temporary storage, grow sparsely, never shrink, publish under caller's ID. An existing ID is returned unchanged even if the retry requests a new size. Empty base creates a sparse disk. Cross-volume base import may explicitly copy once into the store; surface that cost. |
| `CloneCommitted` | Clone immutable commit data into a new writable file, grow if requested. Existing target ID is unchanged; missing source is typed not_found. All internal generations stay on one APFS volume. |
| `Commit` / `GetCommit` | After VMM exit, or a validated paused checkpoint flush, clone the stable disk into temporary commit storage, make it read-only, persist data and manifest, then atomically publish and sync the parent. Never replace a published commit. Retries return identical manifest digest/time; digest hashes the manifest, not all disk bytes. Missing/invalid manifests remain invisible. |
| `OpenForRuntime` / `Release` | Lease the private disk before the helper opens it; release only after confirmed VMM exit and closed descriptors. Releasing an unknown lease succeeds. A lease pins against GC, not a guest-consistency barrier. |
| `Collect` | Respect keep sets and active leases; collect unreferenced disks/commits and unpublished partial work. Never unlink a live helper's backing disk because the API daemon restarted. |

The store is concurrent-safe, with serialized publication and the daemon's exclusive store lock.
On Darwin add a deliberate durability path for file/manifest sync, directory sync and device-cache
flush (`F_FULLFSYNC` where appropriate); failures fail publication. Measure it rather than dropping
flushes for speed. APFS atomic cloning is not a durable multi-file transaction and not a substitute
for flushing guest writes. Keep the last published generation and the recoverable working disk
on any clone/sync/rename/ENOSPC failure. On reopen, reconcile any visible publication against
the journal before declaring success or deleting its resources.

Forced `reflink` fails if cloning is unsupported; forced `copy` always reports slow copy. Auto
mode may choose copy after a failed probe and must expose the reason. Internal clone errors
after a successful probe fail the operation rather than silently copying; cross-volume base
imports are the explicit exception. Do not infer support from an APFS volume name alone.
Logical `SizeBytes` is not exclusive physical allocation; shared extents do not reserve space
for future writes. Keep host free-space admission and resource_exhausted error mapping, plus
report host free/allocated bytes separately from logical guest capacity. Guest ext4 growth is
an image/guest step; truncating a host raw file alone does not enlarge its filesystem.

Existing FS leases are memory-only. Before startup GC, use durable lifecycle/runtime records
to build conservative keep sets, discover surviving helpers, and reopen their leases. Any
unresolved live disk stays pinned. APFS extent sharing survives origin unlinking, but RAM-state
or auxiliary-resource dependencies still require explicit lifecycle references.

## 3. Networking and tailnet access

VZ [`NAT`](https://developer.apple.com/documentation/virtualization/vznatnetworkdeviceattachment)
is convenient for a first boot and a speed baseline. It does not implement Davit's off,
filtered, allowlist, DNS pinning or host/LAN/metadata denial semantics. Guest iptables cannot be
the host security boundary. Do not expose VZ NAT as the default supported policy implementation,
attach a second NAT NIC, or fall back to NAT when the policy stack fails.

Use [`VZFileHandleNetworkDeviceAttachment`](https://developer.apple.com/documentation/virtualization/vzfilehandlenetworkdeviceattachment):
a connected `AF_UNIX/SOCK_DGRAM` pair carries Ethernet frames, one packet per datagram. The helper
receives one FD; a per-VM Go netstack worker owns the other. Preserve packet boundaries, bound
MTU/buffers/queues, detect truncation and malformed frames, and close the link on worker failure.
Each VM has an independent virtual link/stack, so the existing constant guest IP/MAC can remain
isolated without Linux namespaces. Do not connect unrelated VMs to one shared virtual switch.

Reuse [`internal/network/userspace`](../../internal/network/userspace/), not a second Swift
policy implementation. This is **porting work**, not code that already runs on Darwin:
`stack_linux.go` uses gVisor's Linux fd-based link endpoint; main/control/DNS code is Linux-gated,
controls use abstract Unix sockets and SO_PEERCRED, and holder liveness uses `/proc` birth times.
Extract common TCP/UDP forwarding, DNS/dnsallow pins, policy swaps and flow teardown behind a
packet endpoint and host resolver interface. Supply a Darwin datagram/channel endpoint, filesystem
control sockets with Darwin peer credentials, native resolver discovery, and supervisor identity
instead of `/proc`. Prove the pinned gVisor dependency works on Darwin; changing build tags alone
is insufficient. Linux retains its TAP/netns endpoint and existing behavior.

Initially carry IPv4 TCP/UDP and DNS, matching the existing userspace stack. Drop IPv6 and
unsupported L3 traffic rather than forwarding around policy; disclose absent ICMP/IPv6 support.
Configure the Mac guest-facing gateway and routes for the existing `172.16.0.2/30` guest, not
the Linux stack's `10.0.2.0/24` uplink defaults (which the Linux guest never sees). Implement
ARP/gateway/DNS service or an equivalent explicit static image configuration. No DHCP wait is
required for the static first path. Test Docker bridge/NAT and MTU behavior through this link.

Policy order remains host-management addresses denied unconditionally, explicit administrator
exceptions, default deny prefixes, then public internet. Egress `off` also denies DNS; allowlist
admits only approved CIDRs or TTL-pinned approved DNS answers. Apply DNS pinning before replying;
policy changes must close newly forbidden TCP/UDP flows before acknowledgement. Refresh host
addresses/resolvers on interface/VPN changes; stale host addresses must not become a bypass.
DNS forwarding is a narrowly bounded resolver exception, not arbitrary access to host DNS ports.
On any missing/failed policy worker, link traffic is denied while vsock control remains usable.

**Contract prerequisite:** [`runtime.NetworkHandle`](../../internal/runtime/runtime.go) currently
contains only namespace/TAP/user-namespace/argv fields. In a dedicated shared-contract PR, add
a tagged native packet-link reference owned by the network provider. It names a private control
endpoint plus incarnation; the runtime redeems it for an FD, never serializes an FD integer as
a durable identity. Define attach/reconnect/destroy ownership and restart behavior together.
Do not overload `TapName` or `Namespace` with a socket path. Runtime/helper consumes the attachment;
network owns policy and worker supervision. Until this and the Darwin port land, only explicitly
network-none VM experiments qualify, not the Mac release.

Keep one persistent host-resident tsnet gateway, existing WhoIs/token authorization and
guest-agent `tcp.dial` for guest localhost services. SSH/SCP/forwards work through authenticated
vsock, independent of guest egress; no host TUN, root, public listener or tailscaled in the image
is required. A user forwarding grant is separate from guest egress permission. Optional per-VM
tsnet nodes retain their own state outside cloned disks; forks get new endpoint identity.
One gateway cannot synthesize per-VM MagicDNS names. Enrollment/endpoint readiness stays separate
from VM/first-command readiness. See [SPEC tailnet design](SPEC.md#tailnet-gateway-and-per-vm-names)
and [tsnet API](https://tailscale.com/docs/reference/tsnet-server-api).

## 4. Native Ubuntu guest and transport

Images owns a digest-pinned **linux/arm64 Ubuntu systemd** image, raw ext4 rootfs and matching
kernel/initrd manifest; G1 supplies verified prebuilt artifacts so installation never builds an
image locally. Prefer [`VZLinuxBootLoader`](https://developer.apple.com/documentation/virtualization/vzlinuxbootloader)
with `VZGenericPlatformConfiguration` for cold boot. Keep an EFI image/config experiment separate
if required by save support; EFI variable stores then become private, committed external resources
and need a contract/layout decision beyond today's single `DiskSet.Root`.

Kernel requirements: virtio MMIO/PCI as used by VZ, block, console, net and vsock, ext4, namespaces,
cgroups, seccomp, overlayfs, bridge/veth, nftables/NAT/conntrack and Docker's required modules.
Run the existing Linux guest agent compiled for arm64, early under systemd, root only inside
the guest with workload user `davit`. Native arm64 Docker containers require no nested KVM.
x86 kernels are unsupported; Rosetta/emulation, GUI and nested virtualization are optional
future capabilities, not implied by Apple silicon or by the Mac Studio product name.

Configure a VZ virtio socket device. Host helper uses
[`VZVirtioSocketDevice.connect(toPort:)`](https://developer.apple.com/documentation/virtualization/vzvirtiosocketdevice)
to guest port **1024**; it does not dial Darwin AF_VSOCK or send Firecracker's `CONNECT` preamble.
`GuestTransport` binds its dialer to a checked handle/key/incarnation, and the helper proxies
only that VM's stream. Verify that Linux sees host peer **CID 2**, as required by
[`guestagent/vsock_linux.go`](../../internal/guestagent/vsock_linux.go). Preserve rejection of
CID 1/guest-local or other guests and expose no generic host listener to guests.

Reuse [`guestproto`](../../internal/guestproto/doc.go) unchanged: SW length framing, 1 MiB maximum
frame, negotiated protocol/features, host-created VM/generation/challenge and HMAC proof, fresh
per-generation secret. Helper transports opaque guest bytes; authentication/barrier logic stays
in Go. No reusable secret in a template; bootstrap only over the private per-VM host-owned vsock.
Reconnect in the same generation after daemon restart with its existing secret, not a newly
invented bootstrap. Close all old sessions on a generation transition.

Run `SetIdentity` before admitting exec/files/tunnels: host wall clock, VM ID/generation/hostname,
machine-id and SSH key rotation for new/fork, fresh agent secret, service restart and host-key
verification. Disk resume of the same VM keeps persistent machine-id/SSH identity. Agent readiness,
SSH readiness and Docker/application readiness remain separate. Existing agent code explicitly
does not reset PID 1 or D-Bus cached identity merely by writing machine-id; validate an early
image identity gate or a complete restart sequence for cloned disks as well as RAM templates.
Test entropy, TLS, clocks/timers, DNS and concurrent sibling identities. A restored kernel boot ID
may be shared; `instanceGeneration` is the authoritative run identity.

## 5. Honest capability report

The first release, **after** cold/disk/vsock acceptance, reports the following using existing
[`RuntimeCapabilities`](../../internal/domain/capabilities.go) fields. Before implementation,
none of these are claims about the current binary.

| Field | Initial Mac value / condition |
| --- | --- |
| `Engine`, `EngineVersion`, `Arch` | `mac-vz`; exact helper release plus macOS build identifying the system framework; `[arm64]`. |
| `ColdBoot`, `DiskFork` | `true`; disk fork means booting an independent cloned committed disk, including explicit slow-copy storage. FastClone is a separate storage result. |
| `GuestTransport` | `vsock`, after private stream/authentication acceptance. |
| `GoldenRestore` | `false` until reusable trusted-template restores, including concurrent identity/config compatibility, pass. |
| `RAMSnapshot`, `RAMCheckpointFormats` | `false`, empty list. Enable only after the current runtime checkpoint contract passes, not merely a same-VM suspend demo. |
| `LiveFork` | `false`. APFS disk cloning, Pause and even same-VM RAM restore do not establish it. |
| `Shape`, `HostChecks` | VZ-reported CPU/memory limits/alignment, image minimum disk and actual host checks: arm64/OS, helper signature/IPC, VZ support/config, storage, capacity and policy worker. No hardcoded invented maximum. |
| `FingerprintRules` | Document exact inputs above if RAM paths are enabled; cold-only support never implies portable checkpoint compatibility. |

`BootModes()` initially exposes only `cold_boot`. Unsupported golden/checkpoint/restore methods
return `runtime.Unsupported` / domain `unsupported_capability`; `--require-golden` fails rather
than cold-booting under a golden label. Pause is a runtime control, not suspend or live fork.
Do not add nesting/desktop/Rosetta claims to a capability structure that has no fields for them;
any new public field needs the dedicated domain/OpenAPI wire-contract work.

Important constraint: today's `runtimetest.CheckpointRestore` resumes the source and restores
the checkpoint into a different key while the source remains running. Consequently a VZ path
that supports **only same-VM suspend** cannot honestly set `RAMSnapshot=true` under this contract.
Keep it experimental or propose a separate same-instance capability in a dedicated contract PR.
Golden support likewise requires more than restoring one stopped VM once. Enabling `LiveFork`
also requires the lifecycle identity/network/process barrier, identical supported shape and
isolation/rollback proof; it remains a separate release decision even if checkpoint conformance passes.

## 6. Milestones and measurements on real Macs

All execution below belongs on a Mac runner, not a Linux development machine. Initial supported
floor proposal: macOS 14+, arm64; certify exact supported OS builds before release. API deployment
floor is not evidence that Linux save works on every OS at or above it. Test an M1/M2 Studio
without nesting and a newer available Studio separately; record exact model/SoC, cores, unified
RAM, SSD/volume/free space/FileVault, macOS build, SDK/Xcode/helper revision, guest digests,
shape and concurrency. No hardware is assumed available by this document.

| Milestone | Acceptance on hardware | Measurements/artifacts |
| --- | --- | --- |
| M0: native feasibility | Signed minimal helper boots arm64 Ubuntu, vsock host CID/auth proof succeeds, validates shipping device configuration. Execute Linux save/restore probe; record unsupported errors as findings. | Helper launch, VZ start, agent connect/auth and first command; direct-kernel versus EFI only where needed; exact save-support/error transcript. |
| M1: APFS storage | Store contract in auto/copy/forced-clone modes; sibling writes independent, commits survive process restart, partial publication/low-space recovery safe. | Clone-only and durable commit latency, allocation before/after first writes, sparse growth, copy/import cost, GC reclaim and flush cost. |
| M2: runtime + image | Full runtimetest with Reopen; same guest exec/SSH/files, disk stop/resume/fork, clean and forced shutdown, daemon/helper crash recovery and identity barrier work. | Cold versus cached-image new, disk fork/resume, SSH command and first authenticated command; shutdown/commit durations; peak worker+daemon RSS. |
| M3: portable network + gateway | Shared contract and Darwin netstack land; denied host/LAN/metadata/other-VM traffic stays denied, allowlist/off/live revocation work, approved internet/LAN exception and Docker container work. Tailnet client uses existing API/SSH/forward. | First remote command with persistent gateway, endpoint enrollment separately, TCP/UDP throughput and CPU, DNS latency, tunnel half-close/backpressure, VPN/interface changes. |
| M4: Mac release gate | Signed/notarized G1 artifacts install on a clean supported Mac with no compiler/image build; complete v1 Mac user journey, restart/GC/space evidence and honest capabilities. | End-to-end distributions and failures under idle and declared load, cold-install cost, 10-way disk fan-out wall time, resource/admission limits. Publish limitations and measured results. |
| M5: optional RAM/golden | Exact configuration passes real restore and runtime checkpoint contract, matching disk/identity/clock/randomness/network tests and concurrent reuse. If not, ship M4 with false flags. | Save latency/source pause, state bytes, restore/barrier/first command, 10-way reuse, memory pressure, identifier collisions and OS-update invalidation. Compare to cold boot; enable golden only when beneficial. |

For each timing configuration collect **at least 100 successful attempts**, plus every failure,
with raw per-attempt timestamps and count; report p50/p95, failures and timeout policy without
discarding slow outliers. Keep failed attempts visible rather than rerunning until clean. Include
at least 10 independent clean-install trials when measuring installation. Label cached image,
warm page cache, cold page cache after controlled runner restart, prestarted helper and any warm
pool separately; never call a prebooted VM cold. Start with no pool and report its cost if added.

Measure from accepted create/fork/resume through storage, helper launch/attach, VZ running,
guest authentication, identity barrier and **actual successful command returning expected bytes**.
Also measure client request-to-result and remote SSH command separately. Docker readiness means
`docker info` plus a real arm64 container doing permitted networking, not daemon PID existence.
Use identical guest workload/shape for VZ/helper, Lima/VZ and any alternative; publish deviations.
Do not import Linux Firecracker timing numbers as Mac estimates.

SPEC's cached compatible golden-new target remains **<1 s p50, <2 s p95**, not a finding or an
unconditional cold/disk promise. Set a measured cold/disk budget after M0/M2, before calling the
Mac release fast. M4 requires an explicit published budget/result; missing a chosen speed gate
requires optimization or a clearly revised release decision, not relabeling VZ-running as ready.
Budget helper IPC/startup only from measurement; persistent helper reuse may help, but does not
justify a pool that quietly retains guest RAM. Clean up benchmark disks/state/log bulk after
retaining compact manifests/raw timing records and failure evidence.

## 7. Test plan and proof

Reuse [`runtimetest.Run`](../../internal/runtime/runtimetest/suite.go) unchanged with a Mac
Harness: real bootable `Image`, private `Disk`, valid explicit `Shape` (not the tiny fake default),
`New`, and mandatory `Reopen` because helpers outlive the daemon. Provide `CloneDisk` if RAM
is ever advertised. Its existing checks cover capability completeness, boot/inspect/stop,
duplicate boot, kill/destroy, stale replacement handles, discovery, restart/replay, explicit
unsupported operations, and optional paused checkpoint/concurrent restore/discard semantics.
The suite's no-network boot path must work. Passing it does not prove guest readiness or egress.

Reuse [`fs_test.go`](../../internal/storage/fs_test.go)'s `runContract` in package storage after
the shared clone-strategy refactor. It is package-private, not an exported suite callable from
an arbitrary new subpackage. Keep forced copy and auto coverage; add Darwin-specific real
clone success/failure, absent-destination, sparse/growth, write isolation and durability cases.
In particular, clone a read-only commit, grow/write the private target and verify the source
remains unchanged and read-only.
Linux FICLONE and copy contracts must still pass on their runners.

Keep enduring tests at meaningful boundaries:

- Helper RPC: version mismatch, bounded/malformed inputs, peer credentials, path/FD validation,
  idempotent request replay after response loss, cancellation during boot/stop and stale identity.
- Crash/durability: kill daemon after helper start before acknowledgement, during stop/commit,
  and around publication; reopen the real store, read guest sentinel files and verify last good
  commit. Kill helper during writes; no false clean stop. Test disk full/flush failure, simultaneous
  duplicate boot, runtime/network worker loss and GC after reconciliation.
- Guest/gateway: actual argv exec and file bytes, SSH host key verification, independent sibling
  machine IDs/keys/secrets, same-VM identity retention, rejected stale-generation credentials,
  denied guest-local CID bootstrap, working localhost HTTP forward with half-close. Admission
  occurs only after the barrier, including on disk forks.
- Network business policy: run the same destination/DNS/TTL/revocation cases against Linux and
  Darwin endpoints; actual internet and forbidden-host attempts from guest and Docker, literal
  IP/domain/IPv4-mapped/IPv6 bypasses, DNS rebinding, interface changes and worker crash fail-closed.
- Optional RAM: in-memory counter/process survives, disk bytes match checkpoint, siblings change
  independently, no old secret/session/flow reuse, entropy/TLS/timers correct, mismatched shape,
  macOS/user/hardware/config and damaged state rejected. Repeat after supported OS updates.
- Release: quarantined downloaded artifact on a clean supported Mac, signature/notarization and
  digest failure, helper mismatch, launchd restart/login/logout behavior, full CLI/SDK user journey.

Implementation PRs run the full project `make check` plus Mac native build,
signing verification, conformance and real-VM tiers. Signing success or cross-compilation alone
does not certify hosting. Keep compact reproducible evidence with hardware/sample counts;
remove VM/image scratch and test output after use. **None of these execution tests had run
when this design was written.**

## 8. Risks and exit criteria

| Risk | Mitigation / consequence |
| --- | --- |
| Linux save validates but fails with a device, bootloader or new VZ identity | Test exact minimal configuration and restored workload. Preserve a cold-only release; no workaround may remove vsock isolation or safe entropy. |
| VZ machine identifier prevents concurrent golden/child restore | Distinguish same-instance suspend from reusable template restore; current checkpoint suite is a hard gate. Keep RAMSnapshot/GoldenRestore/LiveFork false if it fails. |
| Opaque state tied to user/Mac or invalidated by OS update | Local versioned fingerprints, explicit incompatibility errors and retained disk recovery. Never promise checkpoint export or cross-host migration. |
| APFS fast metadata clone hides flush or first-write cost; space exhaustion later | Measure durable commit and write amplification separately, use free-space admission/reserve, preserve last good state, never count logical shared bytes as reserved capacity. |
| Helper crash/restart or stale PID opens a disk twice | Per-key lifetime locks, incarnation/birth identity, authenticated discovery and conservative leases/GC. Kill only verified process identity; unknown means recovery error. |
| Darwin gVisor endpoint/resolver/control port is larger than expected | Extract portable policy with Linux regression coverage; prototype link first. No VZ NAT fallback. Network-none is only an experiment, not G8 completion. |
| Guest identity cached before barrier, cloned randomness or retained user secrets | Early image/service gating and artifact-level sibling tests. Do not claim arbitrary user disk secrets are scrubbed; keep same-owner fork semantics. |
| macOS signing, unattended launchd session, logout or FileVault availability | Test installed artifacts in the actual user service context and document supported login/unlock behavior; no root helper or weakened platform security by default. |
| VZ lacks Linux cgroup-style host resource enforcement or stable fast restore | Validate VZ shape limits, admission and memory pressure under concurrency, measure helper CPU/RSS. Treat stronger host limits as explicit work, not inherited Linux behavior. |
| License or reference drift | Pin implementation references and review licenses before copying; Tart is evidence, not a dependency. Apple/project main branches do not establish a supported release matrix. |

G8 is complete for the v1 Mac release only after M4 evidence exists. M5 is independently
optional. This design intentionally leaves performance and save/restore feasibility as measured
engineering gates, with concrete cold/disk behavior that can ship without speculative RAM support.
