// Package domain holds the shared, transport-independent contract: VM,
// snapshot, operation and image types, the VM lifecycle state machine, typed
// errors and the runtime/storage capability contract (SPEC §2, §4, §5).
//
// Only workstream A changes this package, in a dedicated PR.
package domain

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"strings"
	"time"
)

type (
	VMID        string
	SnapshotID  string
	OperationID string
	ImageID     string
	// PrincipalID is an authorization principal (owner, service token), never
	// an upstream harness account.
	PrincipalID string
)

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewID returns an opaque, prefixed, URL-safe random ID such as "vm_k3...".
func NewID(prefix string) string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + idEncoding.EncodeToString(b[:])
}

func NewVMID() VMID               { return VMID(NewID("vm")) }
func NewSnapshotID() SnapshotID   { return SnapshotID(NewID("snap")) }
func NewOperationID() OperationID { return OperationID(NewID("op")) }

// ValidName reports whether name is an acceptable VM/snapshot name: 1-63
// lowercase letters, digits and '-', not starting or ending with '-'. Names
// are labels, never identities.
func ValidName(name string) bool {
	if len(name) == 0 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// Label limits: at most MaxLabels per VM; keys 1-63 bytes of lowercase
// letters, digits, '.', '_' and '-', starting with a letter or digit;
// values at most 255 bytes of printable ASCII.
const (
	MaxLabels        = 32
	maxLabelKey      = 63
	maxLabelValue    = 255
	LabelBox         = "davit.box" // the project of a project box (davit box)
	labelKeyAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789._-"
)

// ValidateLabels checks a VM's labels.
func ValidateLabels(l map[string]string) error {
	if len(l) > MaxLabels {
		return Errorf(CodeInvalidArgument, "at most %d labels", MaxLabels).WithDetail("field", "labels")
	}
	for k, v := range l {
		if len(k) == 0 || len(k) > maxLabelKey || strings.Trim(k, labelKeyAlphabet) != "" || strings.ContainsAny(k[:1], "._-") {
			return Errorf(CodeInvalidArgument, "invalid label key %q: 1-%d of a-z 0-9 . _ -, starting with a letter or digit", k, maxLabelKey).WithDetail("field", "labels")
		}
		if len(v) > maxLabelValue {
			return Errorf(CodeInvalidArgument, "label %s: value longer than %d bytes", k, maxLabelValue).WithDetail("field", "labels")
		}
		for _, r := range v {
			if r < 0x20 || r > 0x7e {
				return Errorf(CodeInvalidArgument, "label %s: value must be printable ASCII", k).WithDetail("field", "labels")
			}
		}
	}
	return nil
}

// MaxIdleTimeout bounds IdleTimeoutSeconds (30 days).
const MaxIdleTimeout = 30 * 24 * time.Hour

// Arch is a guest/host CPU architecture.
type Arch string

const (
	ArchAMD64 Arch = "amd64"
	ArchARM64 Arch = "arm64"
)

// Shape is a VM's resources. APIs use integer bytes.
type Shape struct {
	CPUs        int   `json:"cpus"`
	MemoryBytes int64 `json:"memoryBytes"`
	DiskBytes   int64 `json:"diskBytes"`
}

const (
	MiB = int64(1) << 20
	GiB = int64(1) << 30
)

// DefaultShape is used when a request supplies no shape.
var DefaultShape = Shape{CPUs: 2, MemoryBytes: 2 * GiB, DiskBytes: 8 * GiB}

// Merge returns s with any zero field filled from base.
func (s Shape) Merge(base Shape) Shape {
	if s.CPUs == 0 {
		s.CPUs = base.CPUs
	}
	if s.MemoryBytes == 0 {
		s.MemoryBytes = base.MemoryBytes
	}
	if s.DiskBytes == 0 {
		s.DiskBytes = base.DiskBytes
	}
	return s
}

// Validate checks s against the runtime's constraints.
func (s Shape) Validate(c ShapeConstraints) error {
	switch {
	case s.CPUs < 1 || (c.MaxCPUs > 0 && s.CPUs > c.MaxCPUs):
		return Errorf(CodeUnsupportedCapability, "cpus %d outside supported range 1..%d", s.CPUs, c.MaxCPUs)
	case s.MemoryBytes < c.MinMemoryBytes || (c.MaxMemoryBytes > 0 && s.MemoryBytes > c.MaxMemoryBytes):
		return Errorf(CodeUnsupportedCapability, "memoryBytes %d outside supported range", s.MemoryBytes)
	case c.MemoryAlignBytes > 0 && s.MemoryBytes%c.MemoryAlignBytes != 0:
		return Errorf(CodeUnsupportedCapability, "memoryBytes must be a multiple of %d", c.MemoryAlignBytes)
	case s.DiskBytes < c.MinDiskBytes:
		return Errorf(CodeUnsupportedCapability, "diskBytes %d below minimum %d", s.DiskBytes, c.MinDiskBytes)
	}
	return nil
}

// Image is an immutable, architecture-specific kernel/rootfs identified by
// digest.
type Image struct {
	ID       ImageID `json:"id"`
	Ref      string  `json:"ref"`
	Digest   string  `json:"digest"`
	Arch     Arch    `json:"arch"`
	Kernel   string  `json:"-"` // host path; never exposed
	Rootfs   string  `json:"-"` // host path; never exposed
	BootArgs string  `json:"-"` // kernel command line from the image manifest
	// DefaultUser is the image's default login account (manifest
	// guest.user.name), e.g. for `ssh <vm>@gateway` without a user.
	DefaultUser string    `json:"defaultUser,omitempty"`
	RootfsSize  int64     `json:"rootfsBytes"`
	CreatedAt   time.Time `json:"createdAt"`
}

// BootMode is how an instance actually started; never misreported (SPEC §6).
type BootMode string

const (
	BootModeNone             BootMode = ""
	BootModeColdBoot         BootMode = "cold_boot"
	BootModeGoldenRestore    BootMode = "golden_restore"
	BootModeBootstrapRestore BootMode = "bootstrap_restore"
	BootModeRAMRestore       BootMode = "ram_restore"
)

// Readiness is reported separately from VM state: ready never implies
// services are healthy.
type Readiness struct {
	Agent    bool            `json:"agent"`
	SSH      bool            `json:"ssh"`
	Services map[string]bool `json:"services"`
}

// MarshalJSON always emits services as an object, never null.
func (r Readiness) MarshalJSON() ([]byte, error) {
	type plain Readiness
	if r.Services == nil {
		r.Services = map[string]bool{}
	}
	return json.Marshal(plain(r))
}

// Endpoint describes how the VM is reached. M0 has only the local socket.
type Endpoint struct {
	Mode        string `json:"mode"`  // "gateway" | "node"
	State       string `json:"state"` // "pending" | "ready" | "error" | "none"
	GatewayFQDN string `json:"gatewayFQDN,omitempty"`
	VMFQDN      string `json:"vmFQDN,omitempty"`
	SSHTarget   string `json:"sshTarget,omitempty"`
	// SSHHostKeyFingerprints are the SHA256 fingerprints of the guest's SSH
	// host keys, as reported to the authenticated restore barrier of the
	// current generation. Clients pin these; they never trust on first use.
	SSHHostKeyFingerprints []string `json:"sshHostKeyFingerprints,omitempty"`
}

// StorageInfo exposes the storage mode actually used for this VM.
type StorageInfo struct {
	Mode      string `json:"mode"` // "reflink" | "copy"
	FastClone bool   `json:"fastClone"`
}

// VM is the durable record of a machine.
type VM struct {
	ID                 VMID        `json:"id"`
	Owner              PrincipalID `json:"owner"`
	Name               string      `json:"name"`
	State              VMState     `json:"state"`
	StateRevision      int64       `json:"stateRevision"`
	InstanceGeneration int64       `json:"instanceGeneration"`
	Shape              Shape       `json:"shape"`
	ImageID            ImageID     `json:"imageId"`
	BootMode           BootMode    `json:"bootMode"`
	Env                []string    `json:"-"` // KEY=VALUE; never echoed (may hold secrets)
	TTLSeconds         int64       `json:"ttlSeconds"`
	AutoStop           bool        `json:"autoStop"`
	ExpiresAt          *time.Time  `json:"expiresAt,omitempty"`
	// AutoStopWarningAt is when the host starts warning about the coming
	// auto-stop (ExpiresAt minus the host's TTL warning); computed on read.
	AutoStopWarningAt *time.Time  `json:"autoStopWarningAt,omitempty"`
	ActiveOperationID OperationID `json:"activeOperationId,omitempty"`
	LastError         *Error      `json:"lastError,omitempty"`
	LastSnapshotID    SnapshotID  `json:"lastSnapshotId,omitempty"`
	SourceSnapshotID  SnapshotID  `json:"sourceSnapshotId,omitempty"`
	Readiness         Readiness   `json:"readiness"`
	Endpoint          Endpoint    `json:"endpoint"`
	Storage           StorageInfo `json:"storage"`
	// Privacy is the VM's ephemeral/privacy policy (G17; zero: usual retention).
	Privacy PrivacyPolicy `json:"privacy"`
	// Egress is the VM's internet egress policy (zero value: filtered).
	Egress Egress `json:"egress"`
	// Environment is the environment revision the VM was prepared from and
	// Workspace its preparation state; both absent without an environment.
	Environment *EnvironmentPin `json:"environment,omitempty"`
	Workspace   *Workspace      `json:"workspace,omitempty"`
	// Labels are the owner's key/value tags (ValidateLabels); listVMs
	// filters on them. Forks do not inherit them.
	Labels map[string]string `json:"labels,omitempty"`
	// IdleTimeoutSeconds stops a ready VM after this long with no exec,
	// attached process stream or tunnel (ssh) activity (0: never). It is
	// independent of the TTL.
	IdleTimeoutSeconds int64 `json:"idleTimeoutSeconds,omitempty"`
	// LastActivityAt is the last exec, stream or tunnel activity this daemon
	// run saw (or when the VM last became ready); computed on read for VMs
	// with an idle timeout.
	LastActivityAt *time.Time `json:"lastActivityAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	DeletedAt      *time.Time `json:"deletedAt,omitempty"`
}

// SnapshotKind distinguishes disk snapshots from later RAM checkpoints.
type SnapshotKind string

const (
	SnapshotKindDisk SnapshotKind = "disk"
	SnapshotKindRAM  SnapshotKind = "ram"
)

// Consistency records whether a disk snapshot came from a clean shutdown.
type Consistency string

const (
	ConsistencyClean Consistency = "clean"
	ConsistencyCrash Consistency = "crash"
)

// Snapshot is an immutable committed disk generation. A snapshot with an
// empty Name is an automatic stop commit owned by its VM and is collected
// with it; named snapshots survive VM deletion.
type Snapshot struct {
	ID          SnapshotID   `json:"id"`
	Name        string       `json:"name,omitempty"`
	Owner       PrincipalID  `json:"owner"`
	SourceVMID  VMID         `json:"sourceVmId"`
	Generation  int64        `json:"generation"`
	ImageID     ImageID      `json:"imageId"`
	Arch        Arch         `json:"arch"`
	Kind        SnapshotKind `json:"kind"`
	Consistency Consistency  `json:"consistency"`
	SizeBytes   int64        `json:"sizeBytes"`
	Digest      string       `json:"digest,omitempty"`
	CommitID    string       `json:"-"` // storage commit; host-internal
	CreatedAt   time.Time    `json:"createdAt"`
}
