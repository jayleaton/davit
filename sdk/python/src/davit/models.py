"""Wire models generated from api/openapi.yaml by scripts/gen_models.py.

DO NOT EDIT. Regenerate with ``.build/py-venv/bin/python sdk/python/scripts/gen_models.py``.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from typing import Any

from ._model import Model, OpenEnum

__all__ = [
    "VM",
    "AddMemberRequest",
    "AppEndpoint",
    "AppEndpointKind",
    "AppEndpointList",
    "AppEndpointState",
    "Arch",
    "AutoDeletion",
    "BatchItem",
    "BootMode",
    "CapacityWaiter",
    "ColdCapacity",
    "Consistency",
    "Conversation",
    "ConversationList",
    "CreateAppEndpointRequest",
    "CreateCredentialGrantRequest",
    "CreateCredentialProfileRequest",
    "CreateEnvironmentRequest",
    "CreateEnvironmentRevisionRequest",
    "CreateReverseForwardRequest",
    "CreateShareRequest",
    "CreateTokenRequest",
    "CreateVMRequest",
    "CreateVMsRequest",
    "CreateVMsResult",
    "CreateWebhookRequest",
    "CreatedToken",
    "Credential",
    "CredentialGrant",
    "CredentialGrantList",
    "CredentialKind",
    "CredentialProfile",
    "CredentialProfileList",
    "Egress",
    "EgressMode",
    "Endpoint",
    "EnvManifest",
    "Environment",
    "EnvironmentList",
    "EnvironmentPin",
    "EnvironmentRef",
    "EnvironmentRevision",
    "EnvironmentRevisionList",
    "Error",
    "ErrorCode",
    "ErrorInfo",
    "ErrorResponse",
    "Event",
    "EventList",
    "ExecRequest",
    "Feature",
    "FeatureStatus",
    "FileInfo",
    "ForkVMRequest",
    "GrantState",
    "GuestProcess",
    "GuestProcessList",
    "HarnessCapabilities",
    "HistoryEntry",
    "HistoryList",
    "HostCapabilities",
    "HostCapacity",
    "HostCheck",
    "Image",
    "ImageList",
    "InterruptRequest",
    "InterruptResult",
    "IssueSSHCredentialRequest",
    "Limits",
    "Me",
    "MeMember",
    "Member",
    "MemberList",
    "MemberRole",
    "NetworkCapabilities",
    "OpResult",
    "OpenDesktopRequest",
    "OpenTunnelRequest",
    "Operation",
    "OperationAccepted",
    "OperationEvent",
    "OperationEventList",
    "OperationKind",
    "OperationState",
    "OutputNotRetainedReason",
    "PrivacyPolicy",
    "Process",
    "ProcessList",
    "ProcessOutput",
    "ProcessStatus",
    "ProcessStreamMessage",
    "Profile",
    "Prompt",
    "PromptAttachment",
    "PromptError",
    "PromptEvent",
    "PromptEventList",
    "PromptFeed",
    "PromptFeedItem",
    "PromptOptions",
    "PromptRequest",
    "PromptState",
    "PromptUsage",
    "PruneImagesRequest",
    "PruneImagesResult",
    "PrunedBlob",
    "PushResult",
    "PutCredentialRequest",
    "Readiness",
    "ReclaimVMsRequest",
    "ReclaimVMsResult",
    "ReclaimedVM",
    "RecoverVMRequest",
    "RepoSpec",
    "ResumeVMRequest",
    "ReverseForward",
    "ReverseForwardList",
    "ReverseForwardState",
    "ReverseSessionEvent",
    "ReverseSessionEventType",
    "ReverseTarget",
    "ReverseTargetKind",
    "RevokeReason",
    "RotateTokenRequest",
    "RotateWebhookSecretRequest",
    "Run",
    "RunList",
    "RunRequest",
    "RunState",
    "RuntimeCapabilities",
    "SSHCredential",
    "SecretFileRef",
    "SetupAction",
    "SetupError",
    "Shape",
    "ShapeConstraints",
    "ShapeOverride",
    "Share",
    "ShareKind",
    "ShareList",
    "Snapshot",
    "SnapshotEntry",
    "SnapshotKind",
    "SnapshotList",
    "SnapshotTree",
    "SnapshotVMRequest",
    "SteerRequest",
    "SteerResult",
    "SteerSupport",
    "StopVMRequest",
    "StorageCapabilities",
    "StorageInfo",
    "StorageTier",
    "Token",
    "TokenList",
    "TokenScope",
    "Tunnel",
    "UpdateAppEndpointRequest",
    "UpdateMemberRequest",
    "UpdateVMRequest",
    "UpdateWebhookRequest",
    "VMList",
    "VMState",
    "Webhook",
    "WebhookDelivery",
    "WebhookDeliveryList",
    "WebhookList",
    "WebhookPayload",
    "WebhookSecret",
    "Workspace",
    "WorkspaceState",
]


class VMState(OpenEnum):
    """VM lifecycle state. Later versions add states (e.g. suspending, suspended) behind
    capabilities; clients MUST tolerate unknown values.
    """

    PROVISIONING = "provisioning"
    READY = "ready"
    RUNNING = "running"
    STOPPING = "stopping"
    STOPPED = "stopped"
    ERROR = "error"
    DELETING = "deleting"
    DELETED = "deleted"


class OperationState(OpenEnum):
    """Open enum of OperationState values."""

    QUEUED = "queued"
    RUNNING = "running"
    SUCCEEDED = "succeeded"
    FAILED = "failed"


class OperationKind(OpenEnum):
    """Open enum of OperationKind values."""

    CREATE = "create"
    FORK = "fork"
    STOP = "stop"
    RESUME = "resume"
    DELETE = "delete"
    SNAPSHOT = "snapshot"
    RECOVER = "recover"


class BootMode(OpenEnum):
    """How the instance actually started; the empty string means it has not booted."""

    EMPTY = ""
    COLD_BOOT = "cold_boot"
    GOLDEN_RESTORE = "golden_restore"
    BOOTSTRAP_RESTORE = "bootstrap_restore"
    RAM_RESTORE = "ram_restore"


class SnapshotKind(OpenEnum):
    """Open enum of SnapshotKind values."""

    DISK = "disk"
    RAM = "ram"


class Consistency(OpenEnum):
    """Whether a disk snapshot came from a clean shutdown."""

    CLEAN = "clean"
    CRASH = "crash"


class Arch(OpenEnum):
    """Open enum of Arch values."""

    AMD64 = "amd64"
    ARM64 = "arm64"


class StorageTier(OpenEnum):
    """Storage tier holding a VM's committed disk (docs/storage-tiers.md): `hot` is the data
    dir's filesystem, `cold` the optional slower, larger one stopped VMs move to when unused.
    VM.tier is `cold` once the host moved the stopped VM there, so the next resume copies it
    back first (VM.coldBytes); absent means hot.
    """

    HOT = "hot"
    COLD = "cold"


class ErrorCode(OpenEnum):
    """Stable machine-readable error code. HTTP status mapping: 400 invalid_argument; 401
    unauthenticated; 403 permission_denied; 404 not_found; 409 operation_in_progress,
    idempotency_conflict, invalid_state, stop_required; 412 revision_mismatch; 413
    payload_too_large; 422 unsupported_capability; 429 resource_exhausted; 500 internal; 501
    not_implemented; 503 unavailable; 504 timeout.
    """

    INVALID_ARGUMENT = "invalid_argument"
    UNAUTHENTICATED = "unauthenticated"
    PERMISSION_DENIED = "permission_denied"
    NOT_FOUND = "not_found"
    OPERATION_IN_PROGRESS = "operation_in_progress"
    IDEMPOTENCY_CONFLICT = "idempotency_conflict"
    INVALID_STATE = "invalid_state"
    STOP_REQUIRED = "stop_required"
    REVISION_MISMATCH = "revision_mismatch"
    PAYLOAD_TOO_LARGE = "payload_too_large"
    UNSUPPORTED_CAPABILITY = "unsupported_capability"
    RESOURCE_EXHAUSTED = "resource_exhausted"
    UNAVAILABLE = "unavailable"
    NOT_IMPLEMENTED = "not_implemented"
    TIMEOUT = "timeout"
    INTERNAL = "internal"


@dataclass(frozen=True, kw_only=True)
class ErrorResponse(Model):
    """Body of every non-2xx JSON response."""

    error: Error

    _wire_ = (("error", "error", "Error", True),)


@dataclass(frozen=True, kw_only=True)
class Error(Model):
    """Error envelope. Secrets and host paths are redacted."""

    code: ErrorCode

    message: str
    """Human-readable; may change. Do not match on it."""

    retryable: bool
    details: dict[str, Any] | None = None
    request_id: str

    operation_id: str | None = None
    """Set for `operation_in_progress` and errors tied to an operation."""

    _wire_ = (
        ("code", "code", "ErrorCode", True),
        ("message", "message", "str", True),
        ("retryable", "retryable", "bool", True),
        ("details", "details", "map[any]", False),
        ("request_id", "requestId", "str", True),
        ("operation_id", "operationId", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class ErrorInfo(Model):
    """Error recorded on an operation or VM (`Operation.error`, `VM.lastError`)."""

    code: ErrorCode
    message: str
    retryable: bool
    details: dict[str, Any] | None = None
    operation_id: str | None = None

    _wire_ = (
        ("code", "code", "ErrorCode", True),
        ("message", "message", "str", True),
        ("retryable", "retryable", "bool", True),
        ("details", "details", "map[any]", False),
        ("operation_id", "operationId", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class HostCapabilities(Model):
    """The ``HostCapabilities`` wire object."""

    version: str
    runtime: RuntimeCapabilities
    boot_modes: list[BootMode]
    storage: StorageCapabilities
    network: NetworkCapabilities

    states: list[VMState]
    """VM states this host can report."""

    services: list[str]

    profiles: list[Profile]
    """Administrator-defined shapes accepted by `type` on createVM/forkVM."""

    ttl_warning_seconds: int
    """How long before an auto-stop the host starts warning (VM.autoStopWarningAt)."""

    features: list[Feature]
    """Contract surfaces and whether this host provides them. Operations of a `not_implemented`
    feature answer `not_implemented` (HTTP 501) naming the feature.
    """

    harnesses: list[HarnessCapabilities]
    capacity: HostCapacity | None = None

    _wire_ = (
        ("version", "version", "str", True),
        ("runtime", "runtime", "RuntimeCapabilities", True),
        ("boot_modes", "bootModes", "list[BootMode]", True),
        ("storage", "storage", "StorageCapabilities", True),
        ("network", "network", "NetworkCapabilities", True),
        ("states", "states", "list[VMState]", True),
        ("services", "services", "list[str]", True),
        ("profiles", "profiles", "list[Profile]", True),
        ("ttl_warning_seconds", "ttlWarningSeconds", "int", True),
        ("features", "features", "list[Feature]", True),
        ("harnesses", "harnesses", "list[HarnessCapabilities]", True),
        ("capacity", "capacity", "HostCapacity", False),
    )


@dataclass(frozen=True, kw_only=True)
class HostCapacity(Model):
    """The host's room for more VMs, measured when capabilities are read (feature hostCapacity).
    Admission of new, fork, resume and recover checks the same numbers.
    """

    free_memory_bytes: int
    """Available host memory (Linux MemAvailable)."""

    free_disk_bytes: int
    """Free space of the data dir."""

    total_disk_bytes: int | None = None
    """Size of the data dir's filesystem; absent where not measured."""

    disk_floor_bytes: int | None = None
    """Free-space floor admission keeps: the larger of `limits.minFreeDiskBytes` and
    `limits.minFreeDiskPercent` of the filesystem. Below it new, fork and resume are refused
    or queued and the low-disk cleanup runs. Absent when disabled or not measured.
    """

    disk_reserved_bytes: int | None = None
    """Growth headroom reserved for VMs still provisioning, counted as used by admission."""

    disk_headroom_bytes: int | None = None
    """Free space admission reserves for a VM of the default shape to grow into
    (`limits.diskHeadroomPercent` of its disk size).
    """

    save_reserve_bytes: int | None = None
    """Space set aside so that stopping (committing) a running VM never fails for lack of space."""

    save_reserve_armed: bool | None = None
    """False while a stop is using the save reserve (or it could not be set aside)."""

    allocated: Shape
    limits: Limits

    live_vms: int
    """VMs holding a slot (provisioning to stopping)."""

    slots: int
    """How many more VMs of the default shape admission accepts now."""

    total_memory_bytes: int | None = None
    """Host memory (Linux MemTotal); absent where not measured."""

    memory_budget_bytes: int | None = None
    """What the live VMs' declared memory (`allocated.memoryBytes`) may sum to:
    `limits.memoryBytes` times `limits.memoryOvercommit`; absent when unlimited.
    """

    reclaimable: int
    """Idle VMs admission would stop now to make room (feature capacityReclaim)."""

    waiting: list[CapacityWaiter]
    """Requests queued for capacity (`waitSeconds`), first in line first."""

    cold: ColdCapacity | None = None

    _wire_ = (
        ("free_memory_bytes", "freeMemoryBytes", "int", True),
        ("free_disk_bytes", "freeDiskBytes", "int", True),
        ("total_disk_bytes", "totalDiskBytes", "int", False),
        ("disk_floor_bytes", "diskFloorBytes", "int", False),
        ("disk_reserved_bytes", "diskReservedBytes", "int", False),
        ("disk_headroom_bytes", "diskHeadroomBytes", "int", False),
        ("save_reserve_bytes", "saveReserveBytes", "int", False),
        ("save_reserve_armed", "saveReserveArmed", "bool", False),
        ("allocated", "allocated", "Shape", True),
        ("limits", "limits", "Limits", True),
        ("live_vms", "liveVms", "int", True),
        ("slots", "slots", "int", True),
        ("total_memory_bytes", "totalMemoryBytes", "int", False),
        ("memory_budget_bytes", "memoryBudgetBytes", "int", False),
        ("reclaimable", "reclaimable", "int", True),
        ("waiting", "waiting", "list[CapacityWaiter]", True),
        ("cold", "cold", "ColdCapacity", False),
    )


@dataclass(frozen=True, kw_only=True)
class ColdCapacity(Model):
    """The cold storage tier (docs/storage-tiers.md): the filesystem stopped VMs unused for
    `afterSeconds` move to. Absent from HostCapacity when none is configured and no VM is on
    one.
    """

    free_bytes: int
    """Free space of the cold filesystem."""

    total_bytes: int | None = None
    """Size of the cold filesystem; absent where not measured."""

    floor_bytes: int | None = None
    """Free space moving VMs to cold storage keeps there (`coldStorage.minFreeBytes` /
    `minFreePercent` of its size).
    """

    after_seconds: int
    """How long a stopped VM must be unused before the host moves it to cold storage."""

    vms: int
    """VMs on cold storage."""

    bytes: int
    """Data of the VMs on cold storage."""

    fast_clone: bool
    """Whether the cold filesystem supports reflink copies."""

    detail: str | None = None
    """The reflink probe's detail."""

    same_filesystem: bool | None = None
    """True when the cold directory is on the data dir's own filesystem: moving VMs there frees
    nothing.
    """

    draining: bool | None = None
    """True while the tier is being emptied (`coldStorage.drain`): nothing moves there and cold
    VMs come back.
    """

    unavailable: str | None = None
    """Why the cold tier is not in use although VMs are on it (its disk is missing, or it is no
    longer configured); they cannot resume until it is back.
    """

    _wire_ = (
        ("free_bytes", "freeBytes", "int", True),
        ("total_bytes", "totalBytes", "int", False),
        ("floor_bytes", "floorBytes", "int", False),
        ("after_seconds", "afterSeconds", "int", True),
        ("vms", "vms", "int", True),
        ("bytes", "bytes", "int", True),
        ("fast_clone", "fastClone", "bool", True),
        ("detail", "detail", "str", False),
        ("same_filesystem", "sameFilesystem", "bool", False),
        ("draining", "draining", "bool", False),
        ("unavailable", "unavailable", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class CapacityWaiter(Model):
    """The ``CapacityWaiter`` wire object."""

    ticket: str
    """The first 16 hex digits of SHA-256 of the request's Idempotency-Key, so a client can find
    its own place without the queue revealing anyone's key.
    """

    memory_bytes: int
    """Memory the request needs (0 if not known)."""

    resource: str | None = None
    """What the request's last refusal was for: `memory`, `vms`, `cpu` or `disk` (free space);
    absent before it was refused.
    """

    disk_bytes: int | None = None
    """Free space it needs (its growth headroom, the headroom reserved for VMs starting, and the
    floor) when it waits for disk space.
    """

    since: datetime

    _wire_ = (
        ("ticket", "ticket", "str", True),
        ("memory_bytes", "memoryBytes", "int", True),
        ("resource", "resource", "str", False),
        ("disk_bytes", "diskBytes", "int", False),
        ("since", "since", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class Limits(Model):
    """Administrator allocation bounds; zero means unlimited."""

    cpus: int
    memory_bytes: int
    disk_bytes: int

    min_free_disk_bytes: int | None = None
    """Free-space floor of the data dir, at least this many bytes (0 = default 5 GiB, negative =
    no floor: disk admission and the low-disk cleanup off).
    """

    min_free_disk_percent: int | None = None
    """Free-space floor as a share of the filesystem; the floor is the larger of the two (0 =
    default 10, negative = bytes only).
    """

    disk_headroom_percent: int | None = None
    """Share of a VM's disk size admission reserves for its private growth (0 = default 25,
    negative = none).
    """

    save_reserve_bytes: int | None = None
    """Space set aside (a reserve file) that only stops may use (0 = default 2 GiB, negative =
    none).
    """

    vms: int | None = None
    """Live VMs at once (0 = unlimited)."""

    min_free_memory_bytes: int | None = None
    """Host memory that must stay available after admitting a VM (0 = default, negative =
    disabled).
    """

    concurrent_boots: int | None = None
    """Boots (create, fork, resume, recover) running at once; the rest wait queued (0 = default
    8, negative = unlimited).
    """

    memory_overcommit: float | None = None
    """Scales `memoryBytes` into the memory reservation budget: live VMs' declared memory may sum
    to `memoryBytes` times this (0 = default 1.5; otherwise at least 1), because VMs restored
    from one golden share pages.
    """

    reclaim_idle_after_seconds: int | None = None
    """How long a VM with an idle timeout must be idle before admission may stop it to make room
    (0 = default 120, negative = never).
    """

    _wire_ = (
        ("cpus", "cpus", "int", True),
        ("memory_bytes", "memoryBytes", "int", True),
        ("disk_bytes", "diskBytes", "int", True),
        ("min_free_disk_bytes", "minFreeDiskBytes", "int", False),
        ("min_free_disk_percent", "minFreeDiskPercent", "int", False),
        ("disk_headroom_percent", "diskHeadroomPercent", "int", False),
        ("save_reserve_bytes", "saveReserveBytes", "int", False),
        ("vms", "vms", "int", False),
        ("min_free_memory_bytes", "minFreeMemoryBytes", "int", False),
        ("concurrent_boots", "concurrentBoots", "int", False),
        ("memory_overcommit", "memoryOvercommit", "float", False),
        ("reclaim_idle_after_seconds", "reclaimIdleAfterSeconds", "int", False),
    )


@dataclass(frozen=True, kw_only=True)
class RuntimeCapabilities(Model):
    """The ``RuntimeCapabilities`` wire object."""

    engine: str

    engine_version: str | None = None
    """Exact engine release in use, when known."""

    arch: list[Arch]
    cold_boot: bool
    golden_restore: bool
    disk_fork: bool
    ram_snapshot: bool

    ram_checkpoint_formats: list[str]
    """RAM checkpoint formats the engine can write and restore (for example `firecracker-1.x`).
    Empty when `ramSnapshot` is false. Checkpoints never convert across formats or engines.
    """

    live_fork: bool

    guest_transport: str
    """e.g. `vsock`, `hybrid-vsock-uds`, `none`."""

    shape_constraints: ShapeConstraints

    fingerprint_rules: list[str]
    """Inputs that decide RAM checkpoint compatibility."""

    host_checks: list[HostCheck] | None = None
    """Host preflight results the adapter measured (KVM access, engine binary)."""

    _wire_ = (
        ("engine", "engine", "str", True),
        ("engine_version", "engineVersion", "str", False),
        ("arch", "arch", "list[Arch]", True),
        ("cold_boot", "coldBoot", "bool", True),
        ("golden_restore", "goldenRestore", "bool", True),
        ("disk_fork", "diskFork", "bool", True),
        ("ram_snapshot", "ramSnapshot", "bool", True),
        ("ram_checkpoint_formats", "ramCheckpointFormats", "list[str]", True),
        ("live_fork", "liveFork", "bool", True),
        ("guest_transport", "guestTransport", "str", True),
        ("shape_constraints", "shapeConstraints", "ShapeConstraints", True),
        ("fingerprint_rules", "fingerprintRules", "list[str]", True),
        ("host_checks", "hostChecks", "list[HostCheck]", False),
    )


@dataclass(frozen=True, kw_only=True)
class HostCheck(Model):
    """The ``HostCheck`` wire object."""

    name: str
    ok: bool
    detail: str | None = None

    _wire_ = (
        ("name", "name", "str", True),
        ("ok", "ok", "bool", True),
        ("detail", "detail", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class ShapeConstraints(Model):
    """The ``ShapeConstraints`` wire object."""

    max_cpus: int
    min_memory_bytes: int
    max_memory_bytes: int
    memory_align_bytes: int
    min_disk_bytes: int

    _wire_ = (
        ("max_cpus", "maxCpus", "int", True),
        ("min_memory_bytes", "minMemoryBytes", "int", True),
        ("max_memory_bytes", "maxMemoryBytes", "int", True),
        ("memory_align_bytes", "memoryAlignBytes", "int", True),
        ("min_disk_bytes", "minDiskBytes", "int", True),
    )


@dataclass(frozen=True, kw_only=True)
class StorageCapabilities(Model):
    """The ``StorageCapabilities`` wire object."""

    mode: str
    """`reflink` or `copy`."""

    fast_clone: bool
    """True only when a real reflink probe succeeded."""

    detail: str | None = None

    _wire_ = (
        ("mode", "mode", "str", True),
        ("fast_clone", "fastClone", "bool", True),
        ("detail", "detail", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class NetworkCapabilities(Model):
    """The ``NetworkCapabilities`` wire object."""

    listeners: list[str]
    """e.g. `unix`, later `tsnet`."""

    endpoint_modes: list[str]
    forwarding: bool

    _wire_ = (
        ("listeners", "listeners", "list[str]", True),
        ("endpoint_modes", "endpointModes", "list[str]", True),
        ("forwarding", "forwarding", "bool", True),
    )


@dataclass(frozen=True, kw_only=True)
class Image(Model):
    """Immutable, architecture-specific kernel/rootfs identified by digest."""

    id: str
    ref: str
    digest: str
    arch: Arch
    rootfs_bytes: int

    default_user: str | None = None
    """The image's login user (what `ssh <vm>@<gateway>` and SSH credentials default to)."""

    created_at: datetime

    _wire_ = (
        ("id", "id", "str", True),
        ("ref", "ref", "str", True),
        ("digest", "digest", "str", True),
        ("arch", "arch", "Arch", True),
        ("rootfs_bytes", "rootfsBytes", "int", True),
        ("default_user", "defaultUser", "str", False),
        ("created_at", "createdAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class ImageList(Model):
    """The ``ImageList`` wire object."""

    items: list[Image]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[Image]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class Shape(Model):
    """The ``Shape`` wire object."""

    cpus: int
    memory_bytes: int
    disk_bytes: int

    _wire_ = (
        ("cpus", "cpus", "int", True),
        ("memory_bytes", "memoryBytes", "int", True),
        ("disk_bytes", "diskBytes", "int", True),
    )


@dataclass(frozen=True, kw_only=True)
class ShapeOverride(Model):
    """Partial shape; omitted or zero fields keep the default/source value."""

    cpus: int | None = None
    memory_bytes: int | None = None
    disk_bytes: int | None = None

    _wire_ = (
        ("cpus", "cpus", "int", False),
        ("memory_bytes", "memoryBytes", "int", False),
        ("disk_bytes", "diskBytes", "int", False),
    )


@dataclass(frozen=True, kw_only=True)
class Readiness(Model):
    """Reported separately from state; `ready` never implies services are healthy."""

    agent: bool
    ssh: bool
    services: dict[str, bool]

    _wire_ = (
        ("agent", "agent", "bool", True),
        ("ssh", "ssh", "bool", True),
        ("services", "services", "map[bool]", True),
    )


@dataclass(frozen=True, kw_only=True)
class Endpoint(Model):
    """The ``Endpoint`` wire object."""

    mode: str
    """`gateway` or `node`."""

    state: str
    """`pending`, `ready`, `error` or `none`."""

    gateway_fqdn: str | None = None
    vm_fqdn: str | None = None
    ssh_target: str | None = None

    ssh_host_key_fingerprints: list[str] | None = None
    """SHA256 fingerprints of the guest's SSH host keys as reported to the authenticated restore
    barrier of the current generation. Pin these; never trust a guest host key on first use.
    """

    _wire_ = (
        ("mode", "mode", "str", True),
        ("state", "state", "str", True),
        ("gateway_fqdn", "gatewayFQDN", "str", False),
        ("vm_fqdn", "vmFQDN", "str", False),
        ("ssh_target", "sshTarget", "str", False),
        ("ssh_host_key_fingerprints", "sshHostKeyFingerprints", "list[str]", False),
    )


@dataclass(frozen=True, kw_only=True)
class StorageInfo(Model):
    """The ``StorageInfo`` wire object."""

    mode: str
    """`reflink` or `copy`."""

    fast_clone: bool

    _wire_ = (
        ("mode", "mode", "str", True),
        ("fast_clone", "fastClone", "bool", True),
    )


@dataclass(frozen=True, kw_only=True)
class VM(Model):
    """Durable record of a machine. Environment values are never echoed."""

    id: str
    owner: str
    name: str
    state: VMState

    state_revision: int
    """Bumped on every state change; use with `If-Match`."""

    instance_generation: int
    shape: Shape
    image_id: str
    boot_mode: BootMode
    ttl_seconds: int
    auto_stop: bool
    expires_at: datetime | None = None

    auto_stop_warning_at: datetime | None = None
    """When the host starts warning about the auto-stop (expiresAt minus ttlWarningSeconds)."""

    active_operation_id: str | None = None
    last_error: ErrorInfo | None = None
    last_snapshot_id: str | None = None
    source_snapshot_id: str | None = None
    readiness: Readiness
    endpoint: Endpoint
    storage: StorageInfo
    privacy: PrivacyPolicy
    egress: Egress
    created_at: datetime
    updated_at: datetime
    deleted_at: datetime | None = None
    environment: EnvironmentPin | None = None
    workspace: Workspace | None = None

    labels: dict[str, str] | None = None
    """Owner key/value tags (at most 32). Keys: 1-63 of a-z 0-9 . _ -, starting with a letter or
    digit; values: at most 255 printable ASCII bytes. `davit.box` marks a project box. Forks
    do not inherit labels.
    """

    idle_timeout_seconds: int | None = None
    """Stops the VM (keeping its disk) after this long with no exec, attached process stream or
    tunnel (ssh) activity. Absent or 0: never. Independent of the TTL.
    """

    keep: bool | None = None
    """Pinned (`davit box pin`): never auto-deleted by the host's low-disk cleanup. On a project
    image's builder VM it pins the image. Moving to cold storage is separate (`keepHot`).
    """

    keep_hot: bool | None = None
    """Kept on hot storage (`davit box pin --hot`): the host never moves the VM to its cold
    storage tier (docs/storage-tiers.md).
    """

    tier: StorageTier | None = None

    cold_bytes: int | None = None
    """The data a resume of a cold VM copies back to hot storage."""

    cold_at: datetime | None = None
    """When the VM went cold."""

    delete_reason: str | None = None
    """On a deleted VM the host deleted on its own (the low-disk cleanup's auto-delete): why,
    with how long it was unused and the disk's state. Absent when its owner deleted it.
    """

    last_activity_at: datetime | None = None
    """Last activity the idle timeout counts from (this daemon run's exec, stream or tunnel use,
    or the VM's last state change); present on ready/running VMs with an idle timeout.
    """

    _wire_ = (
        ("id", "id", "str", True),
        ("owner", "owner", "str", True),
        ("name", "name", "str", True),
        ("state", "state", "VMState", True),
        ("state_revision", "stateRevision", "int", True),
        ("instance_generation", "instanceGeneration", "int", True),
        ("shape", "shape", "Shape", True),
        ("image_id", "imageId", "str", True),
        ("boot_mode", "bootMode", "BootMode", True),
        ("ttl_seconds", "ttlSeconds", "int", True),
        ("auto_stop", "autoStop", "bool", True),
        ("expires_at", "expiresAt", "datetime", False),
        ("auto_stop_warning_at", "autoStopWarningAt", "datetime", False),
        ("active_operation_id", "activeOperationId", "str", False),
        ("last_error", "lastError", "ErrorInfo", False),
        ("last_snapshot_id", "lastSnapshotId", "str", False),
        ("source_snapshot_id", "sourceSnapshotId", "str", False),
        ("readiness", "readiness", "Readiness", True),
        ("endpoint", "endpoint", "Endpoint", True),
        ("storage", "storage", "StorageInfo", True),
        ("privacy", "privacy", "PrivacyPolicy", True),
        ("egress", "egress", "Egress", True),
        ("created_at", "createdAt", "datetime", True),
        ("updated_at", "updatedAt", "datetime", True),
        ("deleted_at", "deletedAt", "datetime", False),
        ("environment", "environment", "EnvironmentPin", False),
        ("workspace", "workspace", "Workspace", False),
        ("labels", "labels", "map[str]", False),
        ("idle_timeout_seconds", "idleTimeoutSeconds", "int", False),
        ("keep", "keep", "bool", False),
        ("keep_hot", "keepHot", "bool", False),
        ("tier", "tier", "StorageTier", False),
        ("cold_bytes", "coldBytes", "int", False),
        ("cold_at", "coldAt", "datetime", False),
        ("delete_reason", "deleteReason", "str", False),
        ("last_activity_at", "lastActivityAt", "datetime", False),
    )


class EgressMode(OpenEnum):
    """`off` blocks all guest traffic leaving the VM (DNS included). `filtered` (default) allows
    the public internet; private/LAN, host, link-local/metadata and CGNAT/tailnet ranges stay
    blocked. `allowlist` allows only `allow` entries, on top of the `filtered` rules: domain
    names resolve through the built-in DNS resolver, which answers NXDOMAIN for other names
    and admits the answered IPv4 addresses for their TTL (clamped to 60 s - 1 h); CIDRs are
    admitted directly.
    """

    OFF = "off"
    FILTERED = "filtered"
    ALLOWLIST = "allowlist"


@dataclass(frozen=True, kw_only=True)
class PrivacyPolicy(Model):
    """A VM's ephemeral and privacy policy (feature privacyPolicy); every flag defaults to false.
    It limits what davit keeps on the host. It is not secure erasure: deleted data may survive
    in host backups and filesystem remnants, and a guest can always write to its own disk.
    """

    ephemeral: bool | None = None
    """Delete the VM when it stops (explicit, TTL or forced); no snapshot is committed."""

    no_snapshot: bool | None = None
    """A stop commits nothing and discards the working disk; a stopped VM cannot be resumed or
    forked, explicit snapshots are refused.
    """

    no_output_retention: bool | None = None
    """Keep no process output on the host (history lists processes without output)."""

    no_process_samples: bool | None = None
    """Store no whole-guest process samples (ps still answers live)."""

    _wire_ = (
        ("ephemeral", "ephemeral", "bool", False),
        ("no_snapshot", "noSnapshot", "bool", False),
        ("no_output_retention", "noOutputRetention", "bool", False),
        ("no_process_samples", "noProcessSamples", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class Egress(Model):
    """A VM's egress policy. On createVM, omitted means the host default (config
    `network.egress`, else `filtered`); on forkVM, omitted inherits the source VM's policy.
    """

    mode: EgressMode

    allow: list[str] | None = None
    """Allowlist entries (mode `allowlist` only): domain names (`github.com` also matches
    subdomains; `*.example.com` matches only subdomains) and IPv4 addresses/CIDRs. Returned
    normalized (lowercase, CIDRs masked, sorted, deduplicated).
    """

    _wire_ = (
        ("mode", "mode", "EgressMode", True),
        ("allow", "allow", "list[str]", False),
    )


@dataclass(frozen=True, kw_only=True)
class VMList(Model):
    """The ``VMList`` wire object."""

    items: list[VM]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[VM]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class CreateVMRequest(Model):
    """`image` and `snapshotId` are mutually exclusive."""

    name: str | None = None

    type: str | None = None
    """Administrator-defined profile (capabilities.profiles); `shape` fields override it."""

    image: str | None = None
    """Image digest or ref."""

    snapshot_id: str | None = None
    shape: ShapeOverride | None = None

    env: dict[str, str] | None = None
    """Guest environment. Never echoed back."""

    ttl_seconds: int | None = None
    auto_stop: bool | None = None

    require_golden: bool | None = None
    """Fail with `unsupported_capability` instead of falling back to a cold boot."""

    privacy: PrivacyPolicy | None = None
    egress: Egress | None = None
    environment: EnvironmentRef | None = None

    labels: dict[str, str] | None = None
    """Key/value tags (see VM.labels)."""

    idle_timeout_seconds: int | None = None
    """Stop the VM after this long without activity (see VM.idleTimeoutSeconds)."""

    keep: bool | None = None
    """Pin the VM (see VM.keep)."""

    _wire_ = (
        ("name", "name", "str", False),
        ("type", "type", "str", False),
        ("image", "image", "str", False),
        ("snapshot_id", "snapshotId", "str", False),
        ("shape", "shape", "ShapeOverride", False),
        ("env", "env", "map[str]", False),
        ("ttl_seconds", "ttlSeconds", "int", False),
        ("auto_stop", "autoStop", "bool", False),
        ("require_golden", "requireGolden", "bool", False),
        ("privacy", "privacy", "PrivacyPolicy", False),
        ("egress", "egress", "Egress", False),
        ("environment", "environment", "EnvironmentRef", False),
        ("labels", "labels", "map[str]", False),
        ("idle_timeout_seconds", "idleTimeoutSeconds", "int", False),
        ("keep", "keep", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class ForkVMRequest(Model):
    """The ``ForkVMRequest`` wire object."""

    snapshot_id: str | None = None
    """Fork from this snapshot of the VM instead of its current disk."""

    name: str | None = None

    type: str | None = None
    """Administrator-defined profile (capabilities.profiles); `shape` fields override it."""

    shape: ShapeOverride | None = None
    ttl_seconds: int | None = None
    auto_stop: bool | None = None
    egress: Egress | None = None

    live: bool | None = None
    """Fork the running VM itself (RAM, processes and disk captured at one instant, source paused
    briefly) instead of a committed disk snapshot. Requires capability liveFork; the child
    boots with bootMode ram_restore and gets a fresh identity. Mutually exclusive with
    snapshotId.
    """

    environment: EnvironmentRef | None = None

    _wire_ = (
        ("snapshot_id", "snapshotId", "str", False),
        ("name", "name", "str", False),
        ("type", "type", "str", False),
        ("shape", "shape", "ShapeOverride", False),
        ("ttl_seconds", "ttlSeconds", "int", False),
        ("auto_stop", "autoStop", "bool", False),
        ("egress", "egress", "Egress", False),
        ("live", "live", "bool", False),
        ("environment", "environment", "EnvironmentRef", False),
    )


@dataclass(frozen=True, kw_only=True)
class Profile(Model):
    """The ``Profile`` wire object."""

    name: str
    shape: Shape

    _wire_ = (
        ("name", "name", "str", True),
        ("shape", "shape", "Shape", True),
    )


@dataclass(frozen=True, kw_only=True)
class UpdateVMRequest(Model):
    """Every field is optional; only the fields present change."""

    name: str | None = None

    ttl_seconds: int | None = None
    """Sets the VM's TTL and, if it is ready or running, restarts the auto-stop countdown from
    now (`davit extend`).
    """

    auto_stop: bool | None = None
    privacy: PrivacyPolicy | None = None
    egress: Egress | None = None

    idle_timeout_seconds: int | None = None
    """Replaces the idle timeout (0 disables it); the countdown restarts now."""

    keep: bool | None = None
    """Pins (true) or unpins the VM (see VM.keep)."""

    keep_hot: bool | None = None
    """True keeps the VM on hot storage (never moved to cold storage); false lets it go cold
    again (see VM.keepHot).
    """

    _wire_ = (
        ("name", "name", "str", False),
        ("ttl_seconds", "ttlSeconds", "int", False),
        ("auto_stop", "autoStop", "bool", False),
        ("privacy", "privacy", "PrivacyPolicy", False),
        ("egress", "egress", "Egress", False),
        ("idle_timeout_seconds", "idleTimeoutSeconds", "int", False),
        ("keep", "keep", "bool", False),
        ("keep_hot", "keepHot", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class StopVMRequest(Model):
    """The ``StopVMRequest`` wire object."""

    force: bool | None = None
    """Skip graceful guest shutdown; the commit is then crash-consistent."""

    _wire_ = (("force", "force", "bool", False),)


@dataclass(frozen=True, kw_only=True)
class ResumeVMRequest(Model):
    """The ``ResumeVMRequest`` wire object."""

    ttl_seconds: int | None = None
    auto_stop: bool | None = None
    environment: EnvironmentRef | None = None

    type: str | None = None
    """Profile to resize to (feature resize)."""

    shape: ShapeOverride | None = None

    _wire_ = (
        ("ttl_seconds", "ttlSeconds", "int", False),
        ("auto_stop", "autoStop", "bool", False),
        ("environment", "environment", "EnvironmentRef", False),
        ("type", "type", "str", False),
        ("shape", "shape", "ShapeOverride", False),
    )


@dataclass(frozen=True, kw_only=True)
class RecoverVMRequest(Model):
    """Empty; reserved for options."""

    _wire_ = ()


@dataclass(frozen=True, kw_only=True)
class CreateVMsRequest(Model):
    """The ``CreateVMsRequest`` wire object."""

    count: int
    template: CreateVMRequest

    _wire_ = (
        ("count", "count", "int", True),
        ("template", "template", "CreateVMRequest", True),
    )


@dataclass(frozen=True, kw_only=True)
class ReclaimVMsRequest(Model):
    """The ``ReclaimVMsRequest`` wire object."""

    idle_seconds: int
    """Stop VMs with no activity for at least this long."""

    labels: dict[str, str] | None = None
    """Only VMs carrying all of these labels."""

    dry_run: bool | None = None
    """List the VMs without stopping them."""

    _wire_ = (
        ("idle_seconds", "idleSeconds", "int", True),
        ("labels", "labels", "map[str]", False),
        ("dry_run", "dryRun", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class ReclaimVMsResult(Model):
    """The ``ReclaimVMsResult`` wire object."""

    items: list[ReclaimedVM]
    """Oldest activity first."""

    _wire_ = (("items", "items", "list[ReclaimedVM]", True),)


@dataclass(frozen=True, kw_only=True)
class ReclaimedVM(Model):
    """The ``ReclaimedVM`` wire object."""

    vm_id: str
    name: str
    idle_seconds: int
    memory_bytes: int

    operation_id: str | None = None
    """The stop (absent with dryRun)."""

    _wire_ = (
        ("vm_id", "vmId", "str", True),
        ("name", "name", "str", True),
        ("idle_seconds", "idleSeconds", "int", True),
        ("memory_bytes", "memoryBytes", "int", True),
        ("operation_id", "operationId", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class PruneImagesRequest(Model):
    """The ``PruneImagesRequest`` wire object."""

    dry_run: bool | None = None
    """List what would be removed without removing it."""

    _wire_ = (("dry_run", "dryRun", "bool", False),)


@dataclass(frozen=True, kw_only=True)
class PruneImagesResult(Model):
    """The ``PruneImagesResult`` wire object."""

    dry_run: bool

    removed: list[PrunedBlob]
    """The removed (or removable) blobs, least recently changed first."""

    kept: int
    """Blobs that stay (referenced, or changed within the last hour)."""

    bytes: int
    """Space the removed blobs took on disk."""

    _wire_ = (
        ("dry_run", "dryRun", "bool", True),
        ("removed", "removed", "list[PrunedBlob]", True),
        ("kept", "kept", "int", True),
        ("bytes", "bytes", "int", True),
    )


@dataclass(frozen=True, kw_only=True)
class PrunedBlob(Model):
    """The ``PrunedBlob`` wire object."""

    digest: str
    bytes: int
    changed_at: datetime

    _wire_ = (
        ("digest", "digest", "str", True),
        ("bytes", "bytes", "int", True),
        ("changed_at", "changedAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class AutoDeletion(Model):
    """The data of a `vm.autodeleted` event: an unused box the host's low-disk cleanup deleted."""

    vm_id: str
    name: str

    box: str
    """PROJECT/INSTANCE, or PROJECT for a project image."""

    kind: str
    """`image` or `instance`."""

    last_used_at: datetime
    unused_seconds: int

    freed_bytes: int
    """Free space the deletion gave back as measured (data shared with other boxes stays)."""

    deleted_at: datetime
    reason: str

    _wire_ = (
        ("vm_id", "vmId", "str", True),
        ("name", "name", "str", True),
        ("box", "box", "str", True),
        ("kind", "kind", "str", True),
        ("last_used_at", "lastUsedAt", "datetime", True),
        ("unused_seconds", "unusedSeconds", "int", True),
        ("freed_bytes", "freedBytes", "int", True),
        ("deleted_at", "deletedAt", "datetime", True),
        ("reason", "reason", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class CreateVMsResult(Model):
    """The ``CreateVMsResult`` wire object."""

    items: list[BatchItem]

    _wire_ = (("items", "items", "list[BatchItem]", True),)


@dataclass(frozen=True, kw_only=True)
class BatchItem(Model):
    """One item of a batch, accepted or refused. An item is idempotent on its own (the batch's
    Idempotency-Key plus its index), so a replayed batch returns the same operations.
    """

    index: int
    """1-based position in the batch."""

    name: str | None = None
    accepted: OperationAccepted | None = None
    error: Error | None = None

    _wire_ = (
        ("index", "index", "int", True),
        ("name", "name", "str", False),
        ("accepted", "accepted", "OperationAccepted", False),
        ("error", "error", "Error", False),
    )


@dataclass(frozen=True, kw_only=True)
class SnapshotVMRequest(Model):
    """The ``SnapshotVMRequest`` wire object."""

    name: str

    _wire_ = (("name", "name", "str", True),)


@dataclass(frozen=True, kw_only=True)
class OpResult(Model):
    """Terminal success payload."""

    vm_id: str | None = None
    snapshot_id: str | None = None
    boot_mode: BootMode | None = None

    noop: bool | None = None
    """The request was a safe no-op (e.g. stop on a stopped VM)."""

    duration_ms: int
    """Measured wall time from start to terminal state."""

    _wire_ = (
        ("vm_id", "vmId", "str", False),
        ("snapshot_id", "snapshotId", "str", False),
        ("boot_mode", "bootMode", "BootMode", False),
        ("noop", "noop", "bool", False),
        ("duration_ms", "durationMs", "int", True),
    )


@dataclass(frozen=True, kw_only=True)
class Operation(Model):
    """Durable asynchronous intent; survives client disconnect and daemon restart."""

    id: str
    kind: OperationKind
    vm_id: str
    owner: str
    state: OperationState
    phase: str
    progress: float
    result: OpResult | None = None
    error: ErrorInfo | None = None

    reason: str | None = None
    """Why the host stopped the VM on its own: `idle` (its idle timeout), `ttl` (auto-stop),
    `reclaim` (to make room for another VM) or `gc` (reclaimVMs); absent for a stop a client
    asked for. Open set.
    """

    created_at: datetime
    updated_at: datetime
    completed_at: datetime | None = None

    _wire_ = (
        ("id", "id", "str", True),
        ("kind", "kind", "OperationKind", True),
        ("vm_id", "vmId", "str", True),
        ("owner", "owner", "str", True),
        ("state", "state", "OperationState", True),
        ("phase", "phase", "str", True),
        ("progress", "progress", "float", True),
        ("result", "result", "OpResult", False),
        ("error", "error", "ErrorInfo", False),
        ("reason", "reason", "str", False),
        ("created_at", "createdAt", "datetime", True),
        ("updated_at", "updatedAt", "datetime", True),
        ("completed_at", "completedAt", "datetime", False),
    )


@dataclass(frozen=True, kw_only=True)
class OperationAccepted(Model):
    """202 body of every lifecycle mutation."""

    operation: Operation
    vm: VM | None = None

    _wire_ = (
        ("operation", "operation", "Operation", True),
        ("vm", "vm", "VM", False),
    )


@dataclass(frozen=True, kw_only=True)
class OperationEvent(Model):
    """One ordered progress record; `(operationId, seq)` is the cursor."""

    operation_id: str
    seq: int
    time: datetime
    state: OperationState
    phase: str
    progress: float
    message: str | None = None

    _wire_ = (
        ("operation_id", "operationId", "str", True),
        ("seq", "seq", "int", True),
        ("time", "time", "datetime", True),
        ("state", "state", "OperationState", True),
        ("phase", "phase", "str", True),
        ("progress", "progress", "float", True),
        ("message", "message", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class OperationEventList(Model):
    """The ``OperationEventList`` wire object."""

    operation: Operation
    items: list[OperationEvent]

    next_cursor: str | None = None
    """Last returned `seq` as a decimal string; pass it as `after`."""

    _wire_ = (
        ("operation", "operation", "Operation", True),
        ("items", "items", "list[OperationEvent]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class Snapshot(Model):
    """Immutable committed disk generation. An empty `name` marks an automatic stop commit owned
    by its VM: it is kept while it is the VM's last committed state or something depends on
    it, and is collected with the VM. Named snapshots survive VM deletion. Several snapshots
    may name the same committed data; the host-internal storage manifest is not exposed.
    """

    id: str
    name: str | None = None
    owner: str
    source_vm_id: str

    generation: int
    """The source VM's instanceGeneration whose disk was committed."""

    image_id: str
    arch: Arch
    kind: SnapshotKind
    consistency: Consistency

    size_bytes: int
    """Logical disk size; a fork's disk cannot be smaller."""

    digest: str | None = None
    """`sha256:` of the immutable storage manifest (identifies the commit, not a hash of the disk
    bytes).
    """

    created_at: datetime
    """When the disk data was committed (shared by snapshots naming the same commit)."""

    _wire_ = (
        ("id", "id", "str", True),
        ("name", "name", "str", False),
        ("owner", "owner", "str", True),
        ("source_vm_id", "sourceVmId", "str", True),
        ("generation", "generation", "int", True),
        ("image_id", "imageId", "str", True),
        ("arch", "arch", "Arch", True),
        ("kind", "kind", "SnapshotKind", True),
        ("consistency", "consistency", "Consistency", True),
        ("size_bytes", "sizeBytes", "int", True),
        ("digest", "digest", "str", False),
        ("created_at", "createdAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class SnapshotList(Model):
    """The ``SnapshotList`` wire object."""

    items: list[Snapshot]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[Snapshot]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class ExecRequest(Model):
    """`argv` is executed directly; a shell is never inferred from the argument count (use
    `["sh","-c","..."]` explicitly).
    """

    argv: list[str]
    cwd: str | None = None
    env: dict[str, str] | None = None

    user: str | None = None
    """Guest user to run as (default the image's user)."""

    deadline_seconds: int | None = None
    """Kill the process group after this many seconds."""

    detached: bool | None = None

    stdin: bool | None = None
    """Keep stdin open for the first attached stream."""

    tty: bool | None = None

    _wire_ = (
        ("argv", "argv", "list[str]", True),
        ("cwd", "cwd", "str", False),
        ("env", "env", "map[str]", False),
        ("user", "user", "str", False),
        ("deadline_seconds", "deadlineSeconds", "int", False),
        ("detached", "detached", "bool", False),
        ("stdin", "stdin", "bool", False),
        ("tty", "tty", "bool", False),
    )


class ProcessStatus(OpenEnum):
    """`unknown`/`lost` mean the outcome could not be determined (e.g. the instance restarted).
    Clients MUST tolerate unknown values.
    """

    STARTING = "starting"
    RUNNING = "running"
    EXITED = "exited"
    KILLED = "killed"
    UNKNOWN = "unknown"
    LOST = "lost"


@dataclass(frozen=True, kw_only=True)
class Process(Model):
    """The ``Process`` wire object."""

    id: str
    vm_id: str

    generation: int
    """VM instance generation the process belongs to."""

    status: ProcessStatus
    argv: list[str]
    detached: bool
    exit_code: int | None = None
    signal: str | None = None

    timed_out: bool | None = None
    """The process was killed by its `deadlineSeconds`."""

    output_cursor: int
    """Seq of the last output chunk produced; attach after any retained seq."""

    created_at: datetime
    exited_at: datetime | None = None

    _wire_ = (
        ("id", "id", "str", True),
        ("vm_id", "vmId", "str", True),
        ("generation", "generation", "int", True),
        ("status", "status", "ProcessStatus", True),
        ("argv", "argv", "list[str]", True),
        ("detached", "detached", "bool", True),
        ("exit_code", "exitCode", "int", False),
        ("signal", "signal", "str", False),
        ("timed_out", "timedOut", "bool", False),
        ("output_cursor", "outputCursor", "int", True),
        ("created_at", "createdAt", "datetime", True),
        ("exited_at", "exitedAt", "datetime", False),
    )


@dataclass(frozen=True, kw_only=True)
class ProcessList(Model):
    """The ``ProcessList`` wire object."""

    items: list[Process]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[Process]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class ProcessStreamMessage(Model):
    """One attachProcess WebSocket message."""

    type: str
    """Server: `stdout`, `stderr`, `exit`, `error`. Client: `stdin`, `stdin.close`, `signal`.
    Unknown types must be ignored.
    """

    seq: int | None = None

    data: str | None = None
    """Base64 (standard) bytes of a stdout/stderr/stdin chunk."""

    signal: str | None = None
    process: Process | None = None
    error: Error | None = None

    _wire_ = (
        ("type", "type", "str", True),
        ("seq", "seq", "int", False),
        ("data", "data", "str", False),
        ("signal", "signal", "str", False),
        ("process", "process", "Process", False),
        ("error", "error", "Error", False),
    )


@dataclass(frozen=True, kw_only=True)
class FileInfo(Model):
    """The ``FileInfo`` wire object."""

    path: str

    size_bytes: int
    """Bytes uploaded (the tar stream for archives)."""

    mode: str
    """Octal permission bits now on the file, e.g. `0644`; for archives, the mode requested for a
    newly created directory.
    """

    sha256: str | None = None
    """Hex SHA-256 of the uploaded bytes, verified by the agent."""

    generation: int
    archive: bool | None = None

    _wire_ = (
        ("path", "path", "str", True),
        ("size_bytes", "sizeBytes", "int", True),
        ("mode", "mode", "str", True),
        ("sha256", "sha256", "str", False),
        ("generation", "generation", "int", True),
        ("archive", "archive", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class PromptRequest(Model):
    """The ``PromptRequest`` wire object."""

    text: str

    harness: str | None = None
    """`claude` (default; alias `claude-code`), `codex`, `opencode` or `pi`
    (`capabilities.harnesses`).
    """

    conversation_id: str | None = None
    """Continue this conversation."""

    new: bool | None = None
    """Start a new conversation."""

    model: str | None = None

    cwd: str | None = None
    """Guest working directory of a new conversation (default the user's home)."""

    env: dict[str, str] | None = None
    """Environment for the harness process; values enter the guest and are not stored."""

    secrets: list[str] | None = None
    """Names of daemon-configured secrets to pass as same-named environment variables."""

    attachments: list[PromptAttachment] | None = None
    """Files for the turn (feature promptAttachments; types in `harnesses[].attachmentTypes`).
    Inline data is placed in a per-turn guest directory; images go to the harness natively and
    every attachment is named in the prompt by guest path.
    """

    options: PromptOptions | None = None

    turn_timeout_seconds: int | None = None
    """Bounds the turn (default: the daemon's harness.defaultTurnTimeout, 1h unless configured;
    at most 86400); the guest kills the harness at `turnDeadlineAt` and the turn fails with
    `turn_timeout`. While it runs, the turn holds the VM's auto-stop (feature
    promptTurnLease).
    """

    _wire_ = (
        ("text", "text", "str", True),
        ("harness", "harness", "str", False),
        ("conversation_id", "conversationId", "str", False),
        ("new", "new", "bool", False),
        ("model", "model", "str", False),
        ("cwd", "cwd", "str", False),
        ("env", "env", "map[str]", False),
        ("secrets", "secrets", "list[str]", False),
        ("attachments", "attachments", "list[PromptAttachment]", False),
        ("options", "options", "PromptOptions", False),
        ("turn_timeout_seconds", "turnTimeoutSeconds", "int", False),
    )


class PromptState(OpenEnum):
    """Open enum of PromptState values."""

    STARTING = "starting"
    RUNNING = "running"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    CANCELLED = "cancelled"


@dataclass(frozen=True, kw_only=True)
class PromptUsage(Model):
    """The ``PromptUsage`` wire object."""

    input_tokens: int
    output_tokens: int
    cached_input_tokens: int | None = None
    cost_usd: float | None = None

    _wire_ = (
        ("input_tokens", "inputTokens", "int", True),
        ("output_tokens", "outputTokens", "int", True),
        ("cached_input_tokens", "cachedInputTokens", "int", False),
        ("cost_usd", "costUsd", "float", False),
    )


@dataclass(frozen=True, kw_only=True)
class PromptError(Model):
    """The ``PromptError`` wire object."""

    code: str
    """harness_missing, harness_failed, harness_exit, cancelled, lost, turn_timeout or
    attachment; tolerate others.
    """

    message: str

    _wire_ = (
        ("code", "code", "str", True),
        ("message", "message", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class Prompt(Model):
    """The ``Prompt`` wire object."""

    id: str
    conversation_id: str
    vm_id: str
    harness: str
    model: str | None = None
    state: PromptState
    process_id: str | None = None

    event_cursor: int
    """Seq of the last event recorded."""

    usage: PromptUsage | None = None
    error: PromptError | None = None
    created_at: datetime
    finished_at: datetime | None = None

    turn_deadline_at: datetime | None = None
    """When the active-turn lease ends (promptTurnLease)."""

    _wire_ = (
        ("id", "id", "str", True),
        ("conversation_id", "conversationId", "str", True),
        ("vm_id", "vmId", "str", True),
        ("harness", "harness", "str", True),
        ("model", "model", "str", False),
        ("state", "state", "PromptState", True),
        ("process_id", "processId", "str", False),
        ("event_cursor", "eventCursor", "int", True),
        ("usage", "usage", "PromptUsage", False),
        ("error", "error", "PromptError", False),
        ("created_at", "createdAt", "datetime", True),
        ("finished_at", "finishedAt", "datetime", False),
        ("turn_deadline_at", "turnDeadlineAt", "datetime", False),
    )


@dataclass(frozen=True, kw_only=True)
class Conversation(Model):
    """The ``Conversation`` wire object."""

    id: str
    vm_id: str
    harness: str
    model: str | None = None
    cwd: str | None = None

    session_id: str | None = None
    """Harness-native session that later turns resume."""

    active_prompt_id: str | None = None
    turns: int
    created_at: datetime
    updated_at: datetime

    _wire_ = (
        ("id", "id", "str", True),
        ("vm_id", "vmId", "str", True),
        ("harness", "harness", "str", True),
        ("model", "model", "str", False),
        ("cwd", "cwd", "str", False),
        ("session_id", "sessionId", "str", False),
        ("active_prompt_id", "activePromptId", "str", False),
        ("turns", "turns", "int", True),
        ("created_at", "createdAt", "datetime", True),
        ("updated_at", "updatedAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class ConversationList(Model):
    """The ``ConversationList`` wire object."""

    items: list[Conversation]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[Conversation]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class PromptEvent(Model):
    """A normalized harness event. Types: `conversation.started` (sessionId), `message.delta`
    (role, text), `tool.call` (toolCallId, toolName, input), `tool.result` (toolCallId,
    output, isError), `turn.completed` (status succeeded|failed|cancelled, usage, error,
    always last), and `raw` for harness lines without a mapping. `raw` carries the harness's
    own line on every event. Tolerate unknown types.
    """

    seq: int
    time: datetime
    prompt_id: str
    conversation_id: str
    type: str
    session_id: str | None = None
    role: str | None = None
    text: str | None = None
    tool_call_id: str | None = None
    tool_name: str | None = None
    input: Any | None = None
    output: str | None = None
    is_error: bool | None = None
    status: str | None = None
    usage: PromptUsage | None = None
    error: PromptError | None = None
    raw: Any | None = None

    _wire_ = (
        ("seq", "seq", "int", True),
        ("time", "time", "datetime", True),
        ("prompt_id", "promptId", "str", True),
        ("conversation_id", "conversationId", "str", True),
        ("type", "type", "str", True),
        ("session_id", "sessionId", "str", False),
        ("role", "role", "str", False),
        ("text", "text", "str", False),
        ("tool_call_id", "toolCallId", "str", False),
        ("tool_name", "toolName", "str", False),
        ("input", "input", "any", False),
        ("output", "output", "str", False),
        ("is_error", "isError", "bool", False),
        ("status", "status", "str", False),
        ("usage", "usage", "PromptUsage", False),
        ("error", "error", "PromptError", False),
        ("raw", "raw", "any", False),
    )


@dataclass(frozen=True, kw_only=True)
class PromptEventList(Model):
    """The ``PromptEventList`` wire object."""

    prompt: Prompt
    items: list[PromptEvent]
    next_cursor: str | None = None

    _wire_ = (
        ("prompt", "prompt", "Prompt", True),
        ("items", "items", "list[PromptEvent]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


class MemberRole(OpenEnum):
    """Open enum of MemberRole values."""

    VIEWER = "viewer"
    EXEC = "exec"
    ADMIN = "admin"


class ShareKind(OpenEnum):
    """Open enum of ShareKind values."""

    VM = "vm"
    SNAPSHOT = "snapshot"
    ENVIRONMENT = "environment"


@dataclass(frozen=True, kw_only=True)
class Member(Model):
    """The ``Member`` wire object."""

    id: str
    login: str

    user_id: int | None = None
    """Stable tailnet user ID, pinned at the first connection (absent until then)."""

    role: MemberRole
    added_at: datetime

    _wire_ = (
        ("id", "id", "str", True),
        ("login", "login", "str", True),
        ("user_id", "userId", "int", False),
        ("role", "role", "MemberRole", True),
        ("added_at", "addedAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class AddMemberRequest(Model):
    """The ``AddMemberRequest`` wire object."""

    login: str
    role: MemberRole

    _wire_ = (
        ("login", "login", "str", True),
        ("role", "role", "MemberRole", True),
    )


@dataclass(frozen=True, kw_only=True)
class UpdateMemberRequest(Model):
    """The ``UpdateMemberRequest`` wire object."""

    role: MemberRole

    _wire_ = (("role", "role", "MemberRole", True),)


@dataclass(frozen=True, kw_only=True)
class MemberList(Model):
    """The ``MemberList`` wire object."""

    items: list[Member]

    _wire_ = (("items", "items", "list[Member]", True),)


@dataclass(frozen=True, kw_only=True)
class Share(Model):
    """The ``Share`` wire object."""

    id: str
    kind: ShareKind
    resource_id: str

    member: str
    """Member ID, or "*" for every member (role absent then means each member's own role)."""

    role: MemberRole | None = None

    credentials: bool
    """The member(s) may use the VM's credential grants; off, an exec or admin share pauses them."""

    created_at: datetime

    _wire_ = (
        ("id", "id", "str", True),
        ("kind", "kind", "ShareKind", True),
        ("resource_id", "resourceId", "str", True),
        ("member", "member", "str", True),
        ("role", "role", "MemberRole", False),
        ("credentials", "credentials", "bool", True),
        ("created_at", "createdAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class CreateShareRequest(Model):
    """The ``CreateShareRequest`` wire object."""

    kind: ShareKind

    resource: str
    """VM, snapshot or environment ID or name."""

    member: str
    """Member ID, login, or "*"."""

    role: MemberRole | None = None
    credentials: bool | None = None

    _wire_ = (
        ("kind", "kind", "ShareKind", True),
        ("resource", "resource", "str", True),
        ("member", "member", "str", True),
        ("role", "role", "MemberRole", False),
        ("credentials", "credentials", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class ShareList(Model):
    """The ``ShareList`` wire object."""

    items: list[Share]

    _wire_ = (("items", "items", "list[Share]", True),)


class AppEndpointKind(OpenEnum):
    """Open enum of AppEndpointKind values."""

    HTTP = "http"
    DESKTOP = "desktop"


@dataclass(frozen=True, kw_only=True)
class OpenDesktopRequest(Model):
    """The ``OpenDesktopRequest`` wire object."""

    port: int | None = None
    """Guest VNC server port (default 5900)."""

    _wire_ = (("port", "port", "int", False),)


class AppEndpointState(OpenEnum):
    """Open enum of AppEndpointState values."""

    ACTIVE = "active"
    HIDDEN = "hidden"
    VM_NOT_RUNNING = "vm_not_running"
    GATEWAY_DOWN = "gateway_down"


@dataclass(frozen=True, kw_only=True)
class AppEndpoint(Model):
    """The ``AppEndpoint`` wire object."""

    id: str
    vm_id: str

    name: str
    """Unique within the VM; a DNS label without "--"."""

    port: int
    """Guest loopback port of the app."""

    kind: AppEndpointKind

    url: str | None = None
    """Stable tailnet URL; empty until the gateway knows its name."""

    gateway_port: int
    """Gateway port allocated to the endpoint for life."""

    hidden: bool

    forward_identity: bool
    """Send the caller's tailnet login to the app as `Tailscale-User-Login`. Off by default: the
    guest is untrusted and learns nothing about who is on the tailnet unless the owner asks. A
    client-sent value is always dropped.
    """

    state: AppEndpointState
    created_at: datetime

    _wire_ = (
        ("id", "id", "str", True),
        ("vm_id", "vmId", "str", True),
        ("name", "name", "str", True),
        ("port", "port", "int", True),
        ("kind", "kind", "AppEndpointKind", True),
        ("url", "url", "str", False),
        ("gateway_port", "gatewayPort", "int", True),
        ("hidden", "hidden", "bool", True),
        ("forward_identity", "forwardIdentity", "bool", True),
        ("state", "state", "AppEndpointState", True),
        ("created_at", "createdAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class CreateAppEndpointRequest(Model):
    """The ``CreateAppEndpointRequest`` wire object."""

    port: int

    name: str | None = None
    """Defaults to `port-<port>`."""

    forward_identity: bool | None = None
    """See `AppEndpoint.forwardIdentity`; default false."""

    _wire_ = (
        ("port", "port", "int", True),
        ("name", "name", "str", False),
        ("forward_identity", "forwardIdentity", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class UpdateAppEndpointRequest(Model):
    """The ``UpdateAppEndpointRequest`` wire object."""

    name: str | None = None
    hidden: bool | None = None
    forward_identity: bool | None = None

    _wire_ = (
        ("name", "name", "str", False),
        ("hidden", "hidden", "bool", False),
        ("forward_identity", "forwardIdentity", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class AppEndpointList(Model):
    """The ``AppEndpointList`` wire object."""

    items: list[AppEndpoint]

    _wire_ = (("items", "items", "list[AppEndpoint]", True),)


class ReverseTargetKind(OpenEnum):
    """Open enum of ReverseTargetKind values."""

    SESSION = "session"
    HOST = "host"


class ReverseForwardState(OpenEnum):
    """Open enum of ReverseForwardState values."""

    AWAITING_SESSION = "awaiting_session"
    ACTIVE = "active"


class ReverseSessionEventType(OpenEnum):
    """Open enum of ReverseSessionEventType values."""

    CONNECTION = "connection"
    PING = "ping"
    REVOKED = "revoked"


@dataclass(frozen=True, kw_only=True)
class ReverseTarget(Model):
    """The ``ReverseTarget`` wire object."""

    kind: ReverseTargetKind

    address: str | None = None
    """`host`: the host service as `ip:port`, listed exactly in network.reverseHostServices.
    `session`: an optional label of where the client forwards (shown in lists, never dialed by
    the daemon).
    """

    _wire_ = (
        ("kind", "kind", "ReverseTargetKind", True),
        ("address", "address", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class CreateReverseForwardRequest(Model):
    """The ``CreateReverseForwardRequest`` wire object."""

    guest_port: int
    target: ReverseTarget

    _wire_ = (
        ("guest_port", "guestPort", "int", True),
        ("target", "target", "ReverseTarget", True),
    )


@dataclass(frozen=True, kw_only=True)
class ReverseForward(Model):
    """The ``ReverseForward`` wire object."""

    id: str
    vm_id: str
    generation: int
    guest_port: int
    target: ReverseTarget
    state: ReverseForwardState

    active: int
    """Guest connections bridged right now."""

    created_at: datetime

    session_expires_at: datetime | None = None
    """When an unattached session forward is revoked."""

    _wire_ = (
        ("id", "id", "str", True),
        ("vm_id", "vmId", "str", True),
        ("generation", "generation", "int", True),
        ("guest_port", "guestPort", "int", True),
        ("target", "target", "ReverseTarget", True),
        ("state", "state", "ReverseForwardState", True),
        ("active", "active", "int", True),
        ("created_at", "createdAt", "datetime", True),
        ("session_expires_at", "sessionExpiresAt", "datetime", False),
    )


@dataclass(frozen=True, kw_only=True)
class ReverseForwardList(Model):
    """The ``ReverseForwardList`` wire object."""

    items: list[ReverseForward]

    _wire_ = (("items", "items", "list[ReverseForward]", True),)


@dataclass(frozen=True, kw_only=True)
class ReverseSessionEvent(Model):
    """The ``ReverseSessionEvent`` wire object."""

    type: ReverseSessionEventType
    connection_id: str | None = None
    reason: str | None = None

    _wire_ = (
        ("type", "type", "ReverseSessionEventType", True),
        ("connection_id", "connectionId", "str", False),
        ("reason", "reason", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class OpenTunnelRequest(Model):
    """The ``OpenTunnelRequest`` wire object."""

    port: int

    _wire_ = (("port", "port", "int", True),)


@dataclass(frozen=True, kw_only=True)
class Tunnel(Model):
    """The ``Tunnel`` wire object."""

    id: str
    vm_id: str
    generation: int
    port: int
    created_at: datetime
    expires_at: datetime

    _wire_ = (
        ("id", "id", "str", True),
        ("vm_id", "vmId", "str", True),
        ("generation", "generation", "int", True),
        ("port", "port", "int", True),
        ("created_at", "createdAt", "datetime", True),
        ("expires_at", "expiresAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class IssueSSHCredentialRequest(Model):
    """The ``IssueSSHCredentialRequest`` wire object."""

    public_key: str
    """Temporary client public key (OpenSSH authorized_keys format)."""

    guest_user: str | None = None
    """Guest login user; default `user`."""

    ttl_seconds: int | None = None
    """Certificate lifetime; default 300, maximum 600."""

    _wire_ = (
        ("public_key", "publicKey", "str", True),
        ("guest_user", "guestUser", "str", False),
        ("ttl_seconds", "ttlSeconds", "int", False),
    )


@dataclass(frozen=True, kw_only=True)
class SSHCredential(Model):
    """The ``SSHCredential`` wire object."""

    certificate: str
    """Short-lived OpenSSH certificate signed by the host CA."""

    guest_user: str
    generation: int

    host_keys: list[str]
    """Guest host public keys to pin; never disable host-key checking."""

    ssh_target: str
    expires_at: datetime

    _wire_ = (
        ("certificate", "certificate", "str", True),
        ("guest_user", "guestUser", "str", True),
        ("generation", "generation", "int", True),
        ("host_keys", "hostKeys", "list[str]", True),
        ("ssh_target", "sshTarget", "str", True),
        ("expires_at", "expiresAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class TokenScope(Model):
    """The ``TokenScope`` wire object."""

    vm_id: str | None = None
    """Restrict to one VM; omitted means all VMs the issuer can access."""

    actions: list[str]

    _wire_ = (
        ("vm_id", "vmId", "str", False),
        ("actions", "actions", "list[str]", True),
    )


@dataclass(frozen=True, kw_only=True)
class CreateTokenRequest(Model):
    """The ``CreateTokenRequest`` wire object."""

    name: str
    scopes: list[TokenScope]

    ttl_seconds: int
    """Lifetime; at most one year."""

    _wire_ = (
        ("name", "name", "str", True),
        ("scopes", "scopes", "list[TokenScope]", True),
        ("ttl_seconds", "ttlSeconds", "int", True),
    )


@dataclass(frozen=True, kw_only=True)
class Token(Model):
    """The ``Token`` wire object."""

    id: str
    name: str
    scopes: list[TokenScope]
    created_at: datetime
    expires_at: datetime
    last_used_at: datetime | None = None

    use_count: int
    """Requests that presented the token (feature tokenUsage); written in batches with
    lastUsedAt, so a daemon crash may lose the last minute of uses.
    """

    _wire_ = (
        ("id", "id", "str", True),
        ("name", "name", "str", True),
        ("scopes", "scopes", "list[TokenScope]", True),
        ("created_at", "createdAt", "datetime", True),
        ("expires_at", "expiresAt", "datetime", True),
        ("last_used_at", "lastUsedAt", "datetime", False),
        ("use_count", "useCount", "int", True),
    )


@dataclass(frozen=True, kw_only=True)
class RotateTokenRequest(Model):
    """The ``RotateTokenRequest`` wire object."""

    overlap_seconds: int | None = None
    """How long the old secret keeps working (0, the default, revokes it at once)."""

    ttl_seconds: int | None = None
    """Lifetime of the new token; omitted keeps the old token's lifetime."""

    _wire_ = (
        ("overlap_seconds", "overlapSeconds", "int", False),
        ("ttl_seconds", "ttlSeconds", "int", False),
    )


@dataclass(frozen=True, kw_only=True)
class Me(Model):
    """Who the caller is to this daemon. Never carries a secret."""

    principal: str

    auth: str
    """`local`: the owner over the Unix socket; `tailnet`: an owner identity on the gateway;
    `token`: a service token; `member`: a tailnet member using what is shared with it.
    """

    identity: str | None = None
    """The transport identity, e.g. `uid 1000` or a tailnet login and node."""

    token: Token | None = None
    member: MeMember | None = None
    host: str
    version: str

    _wire_ = (
        ("principal", "principal", "str", True),
        ("auth", "auth", "str", True),
        ("identity", "identity", "str", False),
        ("token", "token", "Token", False),
        ("member", "member", "MeMember", False),
        ("host", "host", "str", True),
        ("version", "version", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class MeMember(Model):
    """A member's access: shared VMs as scopes (the member role as a token action: viewer=view,
    exec, admin), shared snapshots and environments (ID to action).
    """

    id: str
    login: str
    scopes: list[TokenScope]
    snapshots: dict[str, str] | None = None
    environments: dict[str, str] | None = None

    _wire_ = (
        ("id", "id", "str", True),
        ("login", "login", "str", True),
        ("scopes", "scopes", "list[TokenScope]", True),
        ("snapshots", "snapshots", "map[str]", False),
        ("environments", "environments", "map[str]", False),
    )


@dataclass(frozen=True, kw_only=True)
class CreatedToken(Model):
    """A token plus its secret, returned only once at creation."""

    id: str
    name: str
    scopes: list[TokenScope]
    created_at: datetime
    expires_at: datetime
    last_used_at: datetime | None = None

    secret: str = field(repr=False)
    """The bearer token: `Authorization: Bearer <secret>`."""

    _wire_ = (
        ("id", "id", "str", True),
        ("name", "name", "str", True),
        ("scopes", "scopes", "list[TokenScope]", True),
        ("created_at", "createdAt", "datetime", True),
        ("expires_at", "expiresAt", "datetime", True),
        ("last_used_at", "lastUsedAt", "datetime", False),
        ("secret", "secret", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class TokenList(Model):
    """The ``TokenList`` wire object."""

    items: list[Token]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[Token]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class Event(Model):
    """One entry of the host event stream, in seq order. Types: vm.created, vm.state,
    operation.created, operation.state, operation.progress, run.created, run.state,
    prompt.created, prompt.state, golden.evicted, vm.autodeleted (data: AutoDeletion);
    tolerate unknown types. data is an allowlisted summary (IDs, states, error codes): never
    environment values, prompt text or secrets.
    """

    seq: int
    type: str
    time: datetime
    vm_id: str | None = None
    subject: str
    data: dict[str, Any]

    _wire_ = (
        ("seq", "seq", "int", True),
        ("type", "type", "str", True),
        ("time", "time", "datetime", True),
        ("vm_id", "vmId", "str", False),
        ("subject", "subject", "str", True),
        ("data", "data", "map[any]", True),
    )


@dataclass(frozen=True, kw_only=True)
class EventList(Model):
    """The ``EventList`` wire object."""

    items: list[Event]
    next_cursor: str

    _wire_ = (
        ("items", "items", "list[Event]", True),
        ("next_cursor", "nextCursor", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class Webhook(Model):
    """The ``Webhook`` wire object."""

    id: str
    url: str
    description: str | None = None
    events: list[str]
    enabled: bool
    created_at: datetime
    updated_at: datetime
    previous_secret_expires_at: datetime | None = None
    pending: int
    dead: int

    _wire_ = (
        ("id", "id", "str", True),
        ("url", "url", "str", True),
        ("description", "description", "str", False),
        ("events", "events", "list[str]", True),
        ("enabled", "enabled", "bool", True),
        ("created_at", "createdAt", "datetime", True),
        ("updated_at", "updatedAt", "datetime", True),
        ("previous_secret_expires_at", "previousSecretExpiresAt", "datetime", False),
        ("pending", "pending", "int", True),
        ("dead", "dead", "int", True),
    )


@dataclass(frozen=True, kw_only=True)
class WebhookList(Model):
    """The ``WebhookList`` wire object."""

    items: list[Webhook]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[Webhook]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class CreateWebhookRequest(Model):
    """The ``CreateWebhookRequest`` wire object."""

    url: str
    """http or https URL that receives POSTed deliveries."""

    events: list[str] | None = None
    description: str | None = None
    enabled: bool | None = None

    _wire_ = (
        ("url", "url", "str", True),
        ("events", "events", "list[str]", False),
        ("description", "description", "str", False),
        ("enabled", "enabled", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class UpdateWebhookRequest(Model):
    """The ``UpdateWebhookRequest`` wire object."""

    url: str | None = None
    events: list[str] | None = None
    description: str | None = None
    enabled: bool | None = None

    _wire_ = (
        ("url", "url", "str", False),
        ("events", "events", "list[str]", False),
        ("description", "description", "str", False),
        ("enabled", "enabled", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class RotateWebhookSecretRequest(Model):
    """The ``RotateWebhookSecretRequest`` wire object."""

    overlap_seconds: int | None = None

    _wire_ = (("overlap_seconds", "overlapSeconds", "int", False),)


@dataclass(frozen=True, kw_only=True)
class WebhookSecret(Model):
    """The ``WebhookSecret`` wire object."""

    webhook: Webhook
    secret: str = field(repr=False)

    _wire_ = (
        ("webhook", "webhook", "Webhook", True),
        ("secret", "secret", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class WebhookDelivery(Model):
    """The ``WebhookDelivery`` wire object."""

    id: str
    webhook_id: str
    event_seq: int
    event_type: str
    state: str
    attempts: int
    next_attempt_at: datetime | None = None
    last_status: int | None = None
    last_error: str | None = None
    created_at: datetime
    delivered_at: datetime | None = None

    _wire_ = (
        ("id", "id", "str", True),
        ("webhook_id", "webhookId", "str", True),
        ("event_seq", "eventSeq", "int", True),
        ("event_type", "eventType", "str", True),
        ("state", "state", "str", True),
        ("attempts", "attempts", "int", True),
        ("next_attempt_at", "nextAttemptAt", "datetime", False),
        ("last_status", "lastStatus", "int", False),
        ("last_error", "lastError", "str", False),
        ("created_at", "createdAt", "datetime", True),
        ("delivered_at", "deliveredAt", "datetime", False),
    )


@dataclass(frozen=True, kw_only=True)
class WebhookDeliveryList(Model):
    """The ``WebhookDeliveryList`` wire object."""

    items: list[WebhookDelivery]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[WebhookDelivery]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class WebhookPayload(Model):
    """The ``WebhookPayload`` wire object."""

    delivery_id: str
    webhook_id: str
    event: Event

    _wire_ = (
        ("delivery_id", "deliveryId", "str", True),
        ("webhook_id", "webhookId", "str", True),
        ("event", "event", "Event", True),
    )


class FeatureStatus(OpenEnum):
    """`not_implemented`: specified, not built; its operations answer `not_implemented`.
    `unsupported`: built, but this host or backend cannot provide it
    (`unsupported_capability`).
    """

    AVAILABLE = "available"
    NOT_IMPLEMENTED = "not_implemented"
    UNSUPPORTED = "unsupported"


@dataclass(frozen=True, kw_only=True)
class Feature(Model):
    """The ``Feature`` wire object."""

    name: str
    status: FeatureStatus

    gap: str
    """PARITY.md gap ID, e.g. G3."""

    operations: list[str]
    """operationIds (and request fields as `operationId.field`) the feature covers."""

    detail: str | None = None

    _wire_ = (
        ("name", "name", "str", True),
        ("status", "status", "FeatureStatus", True),
        ("gap", "gap", "str", True),
        ("operations", "operations", "list[str]", True),
        ("detail", "detail", "str", False),
    )


class SteerSupport(OpenEnum):
    """`native`: the running turn takes the message. `restart`: the turn is cancelled and a new
    turn carries the message (reported as such). `unsupported`: steerPrompt answers
    `unsupported_capability`.
    """

    NATIVE = "native"
    RESTART = "restart"
    UNSUPPORTED = "unsupported"


@dataclass(frozen=True, kw_only=True)
class HarnessCapabilities(Model):
    """The ``HarnessCapabilities`` wire object."""

    name: str
    steer: SteerSupport

    attachment_types: list[str]
    """Accepted attachment media types; empty means none."""

    reasoning_efforts: list[str]
    fast: bool

    experimental: bool | None = None
    """The adapter follows the harness CLI's documentation but has not yet been accepted against
    a real harness run; its flags and event mapping may change. Absent means false.
    """

    _wire_ = (
        ("name", "name", "str", True),
        ("steer", "steer", "SteerSupport", True),
        ("attachment_types", "attachmentTypes", "list[str]", True),
        ("reasoning_efforts", "reasoningEfforts", "list[str]", True),
        ("fast", "fast", "bool", True),
        ("experimental", "experimental", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class Environment(Model):
    """The ``Environment`` wire object."""

    id: str
    name: str
    latest_revision: int
    created_at: datetime
    updated_at: datetime

    _wire_ = (
        ("id", "id", "str", True),
        ("name", "name", "str", True),
        ("latest_revision", "latestRevision", "int", True),
        ("created_at", "createdAt", "datetime", True),
        ("updated_at", "updatedAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class EnvironmentRevision(Model):
    """The ``EnvironmentRevision`` wire object."""

    environment_id: str
    revision: int
    manifest: EnvManifest
    created_at: datetime

    _wire_ = (
        ("environment_id", "environmentId", "str", True),
        ("revision", "revision", "int", True),
        ("manifest", "manifest", "EnvManifest", True),
        ("created_at", "createdAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class EnvManifest(Model):
    """How to prepare a workspace. Holds no secret values: secret files name host secrets, which
    the host delivers over the authenticated guest session at preparation time.
    """

    repos: list[RepoSpec]

    variables: dict[str, str]
    """Non-secret environment variables of guest processes."""

    secret_files: list[SecretFileRef]

    setup: list[SetupAction]
    """Run in order after repos and files; the first failure fails the workspace."""

    credential_profile: str | None = None
    """Profile whose grants new VMs receive (G4)."""

    _wire_ = (
        ("repos", "repos", "list[RepoSpec]", True),
        ("variables", "variables", "map[str]", True),
        ("secret_files", "secretFiles", "list[SecretFileRef]", True),
        ("setup", "setup", "list[SetupAction]", True),
        ("credential_profile", "credentialProfile", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class RepoSpec(Model):
    """The ``RepoSpec`` wire object."""

    url: str
    """Any Git URL, without credentials (private access comes from a credential grant)."""

    ref: str | None = None
    """Branch, tag or commit; empty means the default branch."""

    path: str
    """Absolute guest path."""

    _wire_ = (
        ("url", "url", "str", True),
        ("ref", "ref", "str", False),
        ("path", "path", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class SecretFileRef(Model):
    """The ``SecretFileRef`` wire object."""

    path: str

    secret: str
    """Name of a host secret."""

    mode: str | None = None
    """Octal permission bits, default 0600."""

    _wire_ = (
        ("path", "path", "str", True),
        ("secret", "secret", "str", True),
        ("mode", "mode", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class SetupAction(Model):
    """The ``SetupAction`` wire object."""

    name: str
    argv: list[str]
    cwd: str | None = None
    user: str | None = None
    timeout_seconds: int | None = None

    _wire_ = (
        ("name", "name", "str", True),
        ("argv", "argv", "list[str]", True),
        ("cwd", "cwd", "str", False),
        ("user", "user", "str", False),
        ("timeout_seconds", "timeoutSeconds", "int", False),
    )


@dataclass(frozen=True, kw_only=True)
class EnvironmentRef(Model):
    """The ``EnvironmentRef`` wire object."""

    environment: str
    """Environment ID or name."""

    revision: int | None = None
    """0 or absent means the latest at request time; the VM pins that number."""

    _wire_ = (
        ("environment", "environment", "str", True),
        ("revision", "revision", "int", False),
    )


@dataclass(frozen=True, kw_only=True)
class EnvironmentPin(Model):
    """The ``EnvironmentPin`` wire object."""

    environment_id: str
    name: str
    revision: int

    _wire_ = (
        ("environment_id", "environmentId", "str", True),
        ("name", "name", "str", True),
        ("revision", "revision", "int", True),
    )


class WorkspaceState(OpenEnum):
    """Workspace preparation state; distinct from VM readiness. A failed workspace leaves the VM
    usable.
    """

    PENDING = "pending"
    PREPARING = "preparing"
    READY = "ready"
    FAILED = "failed"


@dataclass(frozen=True, kw_only=True)
class Workspace(Model):
    """The ``Workspace`` wire object."""

    state: WorkspaceState
    started_at: datetime | None = None
    ready_at: datetime | None = None
    error: SetupError | None = None

    _wire_ = (
        ("state", "state", "WorkspaceState", True),
        ("started_at", "startedAt", "datetime", False),
        ("ready_at", "readyAt", "datetime", False),
        ("error", "error", "SetupError", False),
    )


@dataclass(frozen=True, kw_only=True)
class SetupError(Model):
    """Which preparation step failed; step and phase are absent when it failed outside a step."""

    step: str | None = None
    phase: str | None = None
    exit_code: int | None = None
    message: str

    _wire_ = (
        ("step", "step", "str", False),
        ("phase", "phase", "str", False),
        ("exit_code", "exitCode", "int", False),
        ("message", "message", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class CreateEnvironmentRequest(Model):
    """The ``CreateEnvironmentRequest`` wire object."""

    name: str
    manifest: EnvManifest

    _wire_ = (
        ("name", "name", "str", True),
        ("manifest", "manifest", "EnvManifest", True),
    )


@dataclass(frozen=True, kw_only=True)
class CreateEnvironmentRevisionRequest(Model):
    """The ``CreateEnvironmentRevisionRequest`` wire object."""

    manifest: EnvManifest

    _wire_ = (("manifest", "manifest", "EnvManifest", True),)


@dataclass(frozen=True, kw_only=True)
class EnvironmentList(Model):
    """The ``EnvironmentList`` wire object."""

    items: list[Environment]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[Environment]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class EnvironmentRevisionList(Model):
    """The ``EnvironmentRevisionList`` wire object."""

    items: list[EnvironmentRevision]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[EnvironmentRevision]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


class CredentialKind(OpenEnum):
    """Open enum of CredentialKind values."""

    API_KEY = "api_key"
    CUSTOM_API = "custom_api"


@dataclass(frozen=True, kw_only=True)
class Credential(Model):
    """Credential metadata; the secret is write-only and never returned."""

    name: str
    kind: CredentialKind
    provider: str | None = None
    base_url: str | None = None

    env_var: str
    """Guest variable that carries a grant's placeholder."""

    has_secret: bool
    updated_at: datetime

    _wire_ = (
        ("name", "name", "str", True),
        ("kind", "kind", "CredentialKind", True),
        ("provider", "provider", "str", False),
        ("base_url", "baseUrl", "str", False),
        ("env_var", "envVar", "str", True),
        ("has_secret", "hasSecret", "bool", True),
        ("updated_at", "updatedAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class CredentialProfile(Model):
    """The ``CredentialProfile`` wire object."""

    id: str
    name: str
    default: bool
    credentials: list[Credential]
    created_at: datetime
    updated_at: datetime

    _wire_ = (
        ("id", "id", "str", True),
        ("name", "name", "str", True),
        ("default", "default", "bool", True),
        ("credentials", "credentials", "list[Credential]", True),
        ("created_at", "createdAt", "datetime", True),
        ("updated_at", "updatedAt", "datetime", True),
    )


@dataclass(frozen=True, kw_only=True)
class CreateCredentialProfileRequest(Model):
    """The ``CreateCredentialProfileRequest`` wire object."""

    name: str
    default: bool | None = None

    _wire_ = (
        ("name", "name", "str", True),
        ("default", "default", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class PutCredentialRequest(Model):
    """The ``PutCredentialRequest`` wire object."""

    kind: CredentialKind
    provider: str | None = None
    base_url: str | None = None
    env_var: str

    secret: str = field(repr=False)
    """Write-only. Stored on the host only; never returned, never sent to a guest."""

    _wire_ = (
        ("kind", "kind", "CredentialKind", True),
        ("provider", "provider", "str", False),
        ("base_url", "baseUrl", "str", False),
        ("env_var", "envVar", "str", True),
        ("secret", "secret", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class CredentialProfileList(Model):
    """The ``CredentialProfileList`` wire object."""

    items: list[CredentialProfile]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[CredentialProfile]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


class GrantState(OpenEnum):
    """Open enum of GrantState values."""

    ACTIVE = "active"
    REVOKED = "revoked"


class RevokeReason(OpenEnum):
    """Open enum of RevokeReason values."""

    REVOKED = "revoked"
    VM_DELETED = "vm_deleted"
    PROFILE_DELETED = "profile_deleted"
    CREDENTIAL_REMOVED = "credential_removed"


@dataclass(frozen=True, kw_only=True)
class CredentialGrant(Model):
    """Proxied use of one credential by one VM. The guest sees only `placeholder` (in `envVar`);
    the host proxy swaps it for the real key on requests to `upstream`. Grants bind to the VM
    ID: stop keeps them (dormant), resume reuses them, a fork child inherits none (the copied
    placeholder is refused from the child), delete revokes them (`vm_deleted`).
    """

    id: str
    vm_id: str
    profile_id: str
    credential: str
    state: GrantState
    placeholder: str
    env_var: str
    upstream: str
    revoke_reason: RevokeReason | None = None
    created_at: datetime
    revoked_at: datetime | None = None

    _wire_ = (
        ("id", "id", "str", True),
        ("vm_id", "vmId", "str", True),
        ("profile_id", "profileId", "str", True),
        ("credential", "credential", "str", True),
        ("state", "state", "GrantState", True),
        ("placeholder", "placeholder", "str", True),
        ("env_var", "envVar", "str", True),
        ("upstream", "upstream", "str", True),
        ("revoke_reason", "revokeReason", "RevokeReason", False),
        ("created_at", "createdAt", "datetime", True),
        ("revoked_at", "revokedAt", "datetime", False),
    )


@dataclass(frozen=True, kw_only=True)
class CreateCredentialGrantRequest(Model):
    """The ``CreateCredentialGrantRequest`` wire object."""

    profile: str
    """Profile ID or name."""

    credential: str

    _wire_ = (
        ("profile", "profile", "str", True),
        ("credential", "credential", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class CredentialGrantList(Model):
    """The ``CredentialGrantList`` wire object."""

    items: list[CredentialGrant]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[CredentialGrant]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class PromptAttachment(Model):
    """A file for the turn. Exactly one of `path` (a guest file) and `data` (inline base64)."""

    name: str
    media_type: str
    path: str | None = None

    data: str | None = None
    """Inline file bytes, base64 (standard encoding)."""

    _wire_ = (
        ("name", "name", "str", True),
        ("media_type", "mediaType", "str", True),
        ("path", "path", "str", False),
        ("data", "data", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class PromptOptions(Model):
    """Harness controls; `harnesses[]` in capabilities says which a harness accepts."""

    reasoning_effort: str | None = None
    fast: bool | None = None

    _wire_ = (
        ("reasoning_effort", "reasoningEffort", "str", False),
        ("fast", "fast", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class PromptFeedItem(Model):
    """The ``PromptFeedItem`` wire object."""

    cursor: str
    """Orders the VM-wide feed across prompts; pass the last one as `after`."""

    event: PromptEvent

    _wire_ = (
        ("cursor", "cursor", "str", True),
        ("event", "event", "PromptEvent", True),
    )


@dataclass(frozen=True, kw_only=True)
class PromptFeed(Model):
    """The ``PromptFeed`` wire object."""

    items: list[PromptFeedItem]
    next_cursor: str

    _wire_ = (
        ("items", "items", "list[PromptFeedItem]", True),
        ("next_cursor", "nextCursor", "str", True),
    )


@dataclass(frozen=True, kw_only=True)
class InterruptRequest(Model):
    """One conversation's active turn, or every active turn with `all`."""

    conversation_id: str | None = None
    all: bool | None = None

    _wire_ = (
        ("conversation_id", "conversationId", "str", False),
        ("all", "all", "bool", False),
    )


@dataclass(frozen=True, kw_only=True)
class InterruptResult(Model):
    """The ``InterruptResult`` wire object."""

    cancelled: list[Prompt]

    _wire_ = (("cancelled", "cancelled", "list[Prompt]", True),)


@dataclass(frozen=True, kw_only=True)
class SteerRequest(Model):
    """The ``SteerRequest`` wire object."""

    text: str

    _wire_ = (("text", "text", "str", True),)


@dataclass(frozen=True, kw_only=True)
class SteerResult(Model):
    """The ``SteerResult`` wire object."""

    delivery: SteerSupport
    prompt: Prompt

    _wire_ = (
        ("delivery", "delivery", "SteerSupport", True),
        ("prompt", "prompt", "Prompt", True),
    )


class RunState(OpenEnum):
    """Open enum of RunState values."""

    CREATING = "creating"
    WAITING = "waiting"
    RUNNING = "running"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    CANCELLED = "cancelled"


@dataclass(frozen=True, kw_only=True)
class RunRequest(Model):
    """Create a VM, run `argv` in it, keep the exit code and delete the VM unless `keep`. `image`
    and `snapshotId` are exclusive. `hold` stops in `waiting` so the client can pushTree its
    files before startRun.
    """

    image: str | None = None
    snapshot_id: str | None = None
    type: str | None = None
    shape: ShapeOverride | None = None
    environment: EnvironmentRef | None = None
    argv: list[str]
    cwd: str | None = None
    env: dict[str, str] | None = None
    timeout_seconds: int | None = None
    keep: bool | None = None
    hold: bool | None = None

    output_grace_seconds: int | None = None
    """How long the VM outlives the command so a client can still read its output (0: none).
    Omitted: the host default (daemon config runs.outputGrace, 5s unless configured).
    """

    _wire_ = (
        ("image", "image", "str", False),
        ("snapshot_id", "snapshotId", "str", False),
        ("type", "type", "str", False),
        ("shape", "shape", "ShapeOverride", False),
        ("environment", "environment", "EnvironmentRef", False),
        ("argv", "argv", "list[str]", True),
        ("cwd", "cwd", "str", False),
        ("env", "env", "map[str]", False),
        ("timeout_seconds", "timeoutSeconds", "int", False),
        ("keep", "keep", "bool", False),
        ("hold", "hold", "bool", False),
        ("output_grace_seconds", "outputGraceSeconds", "int", False),
    )


@dataclass(frozen=True, kw_only=True)
class Run(Model):
    """`outcome` is the run's result as soon as it is known; `state` becomes it once cleanup ends
    (output grace, VM delete). Calling cancelRun after `outcome` is set only ends the grace.
    """

    id: str
    state: RunState
    vm_id: str | None = None
    process_id: str | None = None
    exit_code: int | None = None

    signal: str | None = None
    """The signal that killed the command (outcome failed, no error)."""

    outcome: RunState | None = None

    decided_at: datetime | None = None
    """When `outcome` was decided."""

    keep: bool
    error: Error | None = None
    created_at: datetime
    ended_at: datetime | None = None
    labels: dict[str, str] | None = None

    _wire_ = (
        ("id", "id", "str", True),
        ("state", "state", "RunState", True),
        ("vm_id", "vmId", "str", False),
        ("process_id", "processId", "str", False),
        ("exit_code", "exitCode", "int", False),
        ("signal", "signal", "str", False),
        ("outcome", "outcome", "RunState", False),
        ("decided_at", "decidedAt", "datetime", False),
        ("keep", "keep", "bool", True),
        ("error", "error", "Error", False),
        ("created_at", "createdAt", "datetime", True),
        ("ended_at", "endedAt", "datetime", False),
        ("labels", "labels", "map[str]", False),
    )


@dataclass(frozen=True, kw_only=True)
class RunList(Model):
    """The ``RunList`` wire object."""

    items: list[Run]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[Run]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class PushResult(Model):
    """The ``PushResult`` wire object."""

    path: str

    files: int
    """Regular files and symlinks in the tree."""

    bytes: int
    """Size of the tar as received."""

    removed: int
    """Files deleted by mode=mirror."""

    sha256: str
    generation: int

    _wire_ = (
        ("path", "path", "str", True),
        ("files", "files", "int", True),
        ("bytes", "bytes", "int", True),
        ("removed", "removed", "int", True),
        ("sha256", "sha256", "str", True),
        ("generation", "generation", "int", True),
    )


@dataclass(frozen=True, kw_only=True)
class HistoryEntry(Model):
    """The ``HistoryEntry`` wire object."""

    process: Process

    handle: str
    """Durable job handle known to the guest agent (guest protocol feature "jobs")."""

    stdout_bytes: int
    stderr_bytes: int

    output_dropped: bool
    """The retention limit cut the oldest output."""

    output_not_retained: OutputNotRetainedReason | None = None

    _wire_ = (
        ("process", "process", "Process", True),
        ("handle", "handle", "str", True),
        ("stdout_bytes", "stdoutBytes", "int", True),
        ("stderr_bytes", "stderrBytes", "int", True),
        ("output_dropped", "outputDropped", "bool", True),
        ("output_not_retained", "outputNotRetained", "OutputNotRetainedReason", False),
    )


class OutputNotRetainedReason(OpenEnum):
    """Why the host kept none (or not all) of a job's output, apart from the retention limit
    (`outputDropped`). `policy`: the VM's privacy policy `noOutputRetention` was on while the
    job ran, or was turned on later and purged its output. Absent when the output was kept.
    """

    POLICY = "policy"


@dataclass(frozen=True, kw_only=True)
class HistoryList(Model):
    """The ``HistoryList`` wire object."""

    items: list[HistoryEntry]
    next_cursor: str | None = None

    _wire_ = (
        ("items", "items", "list[HistoryEntry]", True),
        ("next_cursor", "nextCursor", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class ProcessOutput(Model):
    """The ``ProcessOutput`` wire object."""

    items: list[ProcessStreamMessage]
    next_cursor: str | None = None

    complete: bool
    """The process ended and every retained chunk was returned."""

    _wire_ = (
        ("items", "items", "list[ProcessStreamMessage]", True),
        ("next_cursor", "nextCursor", "str", False),
        ("complete", "complete", "bool", True),
    )


@dataclass(frozen=True, kw_only=True)
class GuestProcess(Model):
    """The ``GuestProcess`` wire object."""

    pid: int
    ppid: int
    user: str
    args: list[str]
    state: str
    cpu_time_millis: int
    rss_bytes: int
    started_at: datetime

    process_id: str | None = None
    """Set for davit-managed processes."""

    _wire_ = (
        ("pid", "pid", "int", True),
        ("ppid", "ppid", "int", True),
        ("user", "user", "str", True),
        ("args", "args", "list[str]", True),
        ("state", "state", "str", True),
        ("cpu_time_millis", "cpuTimeMillis", "int", True),
        ("rss_bytes", "rssBytes", "int", True),
        ("started_at", "startedAt", "datetime", True),
        ("process_id", "processId", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class GuestProcessList(Model):
    """The ``GuestProcessList`` wire object."""

    sampled_at: datetime

    stale: bool
    """The VM is not running; this is its last sample."""

    truncated: bool
    items: list[GuestProcess]

    _wire_ = (
        ("sampled_at", "sampledAt", "datetime", True),
        ("stale", "stale", "bool", True),
        ("truncated", "truncated", "bool", True),
        ("items", "items", "list[GuestProcess]", True),
    )


@dataclass(frozen=True, kw_only=True)
class SnapshotEntry(Model):
    """The ``SnapshotEntry`` wire object."""

    path: str
    type: str
    size_bytes: int
    mode: str
    modified_at: datetime
    link_target: str | None = None

    _wire_ = (
        ("path", "path", "str", True),
        ("type", "type", "str", True),
        ("size_bytes", "sizeBytes", "int", True),
        ("mode", "mode", "str", True),
        ("modified_at", "modifiedAt", "datetime", True),
        ("link_target", "linkTarget", "str", False),
    )


@dataclass(frozen=True, kw_only=True)
class SnapshotTree(Model):
    """The ``SnapshotTree`` wire object."""

    path: str
    items: list[SnapshotEntry]
    next_cursor: str | None = None

    _wire_ = (
        ("path", "path", "str", True),
        ("items", "items", "list[SnapshotEntry]", True),
        ("next_cursor", "nextCursor", "str", False),
    )
