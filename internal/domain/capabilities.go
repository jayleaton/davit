package domain

import "time"

// ShapeConstraints bound acceptable shapes for a runtime.
type ShapeConstraints struct {
	MaxCPUs          int   `json:"maxCpus"`
	MinMemoryBytes   int64 `json:"minMemoryBytes"`
	MaxMemoryBytes   int64 `json:"maxMemoryBytes"`
	MemoryAlignBytes int64 `json:"memoryAlignBytes"`
	MinDiskBytes     int64 `json:"minDiskBytes"`
}

// RuntimeCapabilities is what an engine adapter honestly supports (SPEC §5).
// Feature negotiation happens in lifecycle planning against this struct; an
// adapter that does not support a feature returns CodeUnsupportedCapability.
type RuntimeCapabilities struct {
	Engine string `json:"engine"`
	// EngineVersion is the exact engine release in use, when known.
	EngineVersion string `json:"engineVersion,omitempty"`
	Arch          []Arch `json:"arch"`
	ColdBoot      bool   `json:"coldBoot"`
	GoldenRestore bool   `json:"goldenRestore"`
	DiskFork      bool   `json:"diskFork"`
	RAMSnapshot   bool   `json:"ramSnapshot"`
	// RAMCheckpointFormats names the checkpoint formats the engine can write
	// and restore (e.g. "firecracker-1.x"); empty when RAMSnapshot is false.
	// Checkpoints never convert across formats.
	RAMCheckpointFormats []string         `json:"ramCheckpointFormats"`
	LiveFork             bool             `json:"liveFork"`
	GuestTransport       string           `json:"guestTransport"` // e.g. "vsock", "hybrid-vsock-uds", "none"
	Shape                ShapeConstraints `json:"shapeConstraints"`
	// FingerprintRules names the inputs that decide RAM checkpoint
	// compatibility (engine version, CPU model, kernel, device layout, shape).
	FingerprintRules []string `json:"fingerprintRules"`
	// HostChecks are host preflight results the adapter measured (KVM
	// access, engine binary). Storage clone support is reported separately.
	HostChecks []HostCheck `json:"hostChecks,omitempty"`
}

// HostCheck is one measured host preflight result.
type HostCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// BootModes derives the boot modes the runtime can actually provide.
func (c RuntimeCapabilities) BootModes() []BootMode {
	var m []BootMode
	if c.ColdBoot {
		m = append(m, BootModeColdBoot)
	}
	if c.GoldenRestore {
		m = append(m, BootModeGoldenRestore)
	}
	if c.RAMSnapshot {
		m = append(m, BootModeRAMRestore)
	}
	return m
}

// StorageCapabilities is the measured result of a storage probe.
type StorageCapabilities struct {
	Mode      string `json:"mode"`      // "reflink" | "copy"
	FastClone bool   `json:"fastClone"` // true only when a real reflink probe succeeded
	Detail    string `json:"detail,omitempty"`
}

// HostCapabilities is the full host capability report returned by the API.
type HostCapabilities struct {
	Version   string              `json:"version"`
	Runtime   RuntimeCapabilities `json:"runtime"`
	BootModes []BootMode          `json:"bootModes"`
	Storage   StorageCapabilities `json:"storage"`
	Network   NetworkCapabilities `json:"network"`
	States    []VMState           `json:"states"`
	Services  []string            `json:"services"`
	// Profiles are the administrator-defined shapes `new/fork --type` accept.
	Profiles []Profile `json:"profiles"`
	// TTLWarningSeconds is how long before an auto-stop the host warns.
	TTLWarningSeconds int64 `json:"ttlWarningSeconds"`
	// Features lists contract surfaces and whether this host provides them
	// (ParityFeatures); an operation of a not_implemented feature answers
	// not_implemented.
	Features []Feature `json:"features"`
	// Harnesses are the prompt harnesses and their controls.
	Harnesses []HarnessCapabilities `json:"harnesses"`
	// Capacity is the host's room for more VMs, measured when capabilities
	// are read (feature hostCapacity); absent where not reported.
	Capacity *HostCapacity `json:"capacity,omitempty"`
}

// HostCapacity is what admission checks new, fork, resume and recover
// against (G9).
type HostCapacity struct {
	// FreeMemoryBytes is the host's available memory (Linux MemAvailable).
	FreeMemoryBytes int64 `json:"freeMemoryBytes"`
	// FreeDiskBytes is the data dir's free space.
	FreeDiskBytes int64 `json:"freeDiskBytes"`
	// Allocated is the shape sum of VMs holding resources (see Limits).
	Allocated Shape `json:"allocated"`
	// Limits are the administrator bounds (zero: unlimited).
	Limits Limits `json:"limits"`
	// LiveVMs is how many VMs hold a slot (provisioning to stopping).
	LiveVMs int `json:"liveVms"`
	// Slots is how many more VMs of DefaultShape admission accepts now.
	Slots int `json:"slots"`
	// TotalMemoryBytes is the host's memory (Linux MemTotal); 0 where not
	// measured.
	TotalMemoryBytes int64 `json:"totalMemoryBytes,omitempty"`
	// MemoryBudgetBytes is what the live VMs' declared memory
	// (allocated.memoryBytes) may sum to: Limits.MemoryBudget(); 0 is
	// unlimited.
	MemoryBudgetBytes int64 `json:"memoryBudgetBytes,omitempty"`
	// Reclaimable is how many idle VMs admission would stop now to make
	// room (Limits.reclaimIdleAfterSeconds).
	Reclaimable int `json:"reclaimable"`
	// Waiting are the createVM/resumeVM requests queued for capacity
	// (waitSeconds), first in line first.
	Waiting []CapacityWaiter `json:"waiting"`
}

// CapacityWaiter is one request queued for host capacity.
type CapacityWaiter struct {
	// Ticket is CapacityTicket of the request's Idempotency-Key, so a client
	// can find its own place without the queue revealing anyone's key.
	Ticket string `json:"ticket"`
	// MemoryBytes is the memory the request needs.
	MemoryBytes int64     `json:"memoryBytes"`
	Since       time.Time `json:"since"`
}

// Profile is a named VM shape configured by the host administrator.
type Profile struct {
	Name  string `json:"name"`
	Shape Shape  `json:"shape"`
}

// NetworkCapabilities describes reachability features. M0: local socket only.
type NetworkCapabilities struct {
	Listeners    []string `json:"listeners"` // "unix", "tailnet"
	EndpointMode []string `json:"endpointModes"`
	Forwarding   bool     `json:"forwarding"`
}

// Limits are administrator-configured host allocation bounds. Zero means
// unlimited, except MinFreeDiskBytes.
type Limits struct {
	CPUs        int   `json:"cpus"`
	MemoryBytes int64 `json:"memoryBytes"`
	DiskBytes   int64 `json:"diskBytes"`
	// MinFreeDiskBytes is the free-space floor of the data dir: below it,
	// new, fork and resume are refused (stop and delete never are). Zero
	// means DefaultMinFreeDiskBytes; negative disables the floor.
	MinFreeDiskBytes int64 `json:"minFreeDiskBytes,omitempty"`
	// VMs bounds how many VMs may be live (provisioning to stopping) at
	// once; zero is unlimited (G9).
	VMs int `json:"vms,omitempty"`
	// MinFreeMemoryBytes is the host memory that must stay available after
	// admitting a VM's memory (new, fork, resume, recover). Zero means
	// DefaultMinFreeMemoryBytes; negative disables the check (G9).
	MinFreeMemoryBytes int64 `json:"minFreeMemoryBytes,omitempty"`
	// ConcurrentBoots bounds create/fork/resume/recover boots running at
	// once (a batch queues the rest). Zero means DefaultConcurrentBoots;
	// negative is unlimited.
	ConcurrentBoots int `json:"concurrentBoots,omitempty"`
	// MemoryOvercommit scales MemoryBytes into the memory reservation
	// budget: the declared memory of live VMs may sum to at most
	// MemoryBytes × MemoryOvercommit, because VMs restored from one golden
	// share pages. Zero means DefaultMemoryOvercommit; otherwise at least 1.
	MemoryOvercommit float64 `json:"memoryOvercommit,omitempty"`
	// ReclaimIdleAfterSeconds: when admission has no room, VMs with an idle
	// timeout that are ready, run no process and have had no exec or tunnel
	// activity for this long are stopped first, oldest activity first.
	// Zero means DefaultReclaimIdleAfterSeconds; negative disables it.
	ReclaimIdleAfterSeconds int64 `json:"reclaimIdleAfterSeconds,omitempty"`
}

// DefaultConcurrentBoots bounds boots in flight when Limits.ConcurrentBoots
// is zero.
const DefaultConcurrentBoots = 8

// BootSlots returns how many boots may run at once (0 = unlimited).
func (l Limits) BootSlots() int {
	switch {
	case l.ConcurrentBoots < 0:
		return 0
	case l.ConcurrentBoots == 0:
		return DefaultConcurrentBoots
	}
	return l.ConcurrentBoots
}

// DefaultMinFreeMemoryBytes is the memory floor when none is configured.
const DefaultMinFreeMemoryBytes = 256 * MiB

// DefaultMinFreeDiskBytes is the floor when none is configured.
const DefaultMinFreeDiskBytes = GiB

// FreeFloor returns the effective free-space floor (0 = disabled).
func (l Limits) FreeFloor() int64 {
	switch {
	case l.MinFreeDiskBytes < 0:
		return 0
	case l.MinFreeDiskBytes == 0:
		return DefaultMinFreeDiskBytes
	}
	return l.MinFreeDiskBytes
}

// AdmitFreeSpace refuses work that would start below the free-space floor.
func (l Limits) AdmitFreeSpace(free int64) error {
	floor := l.FreeFloor()
	if floor == 0 || free >= floor {
		return nil
	}
	e := Errorf(CodeResourceExhausted, "host data dir has %d bytes free, below the %d-byte floor; free space or delete VMs/snapshots", free, floor)
	e.Retryable = true
	return e.WithDetail("resource", "disk").WithDetail("freeBytes", free).WithDetail("floorBytes", floor)
}

// Admit checks whether adding want to used stays within l.
func (l Limits) Admit(used, want Shape) error {
	exceeded := func(what string) error {
		e := Errorf(CodeResourceExhausted, "host %s limit exceeded", what)
		e.Retryable = true
		return e.WithDetail("resource", what)
	}
	switch {
	case l.CPUs > 0 && used.CPUs+want.CPUs > l.CPUs:
		return exceeded("cpu")
	case l.MemoryBytes > 0 && used.MemoryBytes+want.MemoryBytes > l.MemoryBudget():
		e := Errorf(CodeResourceExhausted,
			"live VMs reserve %d MiB of the %d MiB memory budget (limits.memoryBytes × memoryOvercommit); this VM needs %d MiB",
			used.MemoryBytes/MiB, l.MemoryBudget()/MiB, want.MemoryBytes/MiB)
		e.Retryable = true
		return e.WithDetail("resource", "memory").WithDetail("reservedBytes", used.MemoryBytes).
			WithDetail("budgetBytes", l.MemoryBudget()).WithDetail("needBytes", want.MemoryBytes)
	case l.DiskBytes > 0 && used.DiskBytes+want.DiskBytes > l.DiskBytes:
		return exceeded("disk")
	}
	return nil
}
