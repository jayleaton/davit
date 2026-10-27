package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// Host capacity admission (G9): how many VMs may be live at once and how
// much host memory must stay available. The CPU/memory/disk allocation
// limits and the disk floor are in capabilities.go.

// MaxSlots caps HostCapacity.Slots when nothing else bounds it.
const MaxSlots = 1000

// DefaultMemoryOvercommit is Limits.MemoryOvercommit when it is zero.
const DefaultMemoryOvercommit = 1.5

// DefaultReclaimIdleAfterSeconds is Limits.ReclaimIdleAfterSeconds when it
// is zero.
const DefaultReclaimIdleAfterSeconds = 120

// MaxCapacityWaitSeconds bounds createVM/resumeVM waitSeconds.
const MaxCapacityWaitSeconds = 3600

// MemoryBudget returns what the declared memory of live VMs may sum to:
// MemoryBytes × the overcommit (0: no budget).
func (l Limits) MemoryBudget() int64 {
	if l.MemoryBytes <= 0 {
		return 0
	}
	f := l.MemoryOvercommit
	if f == 0 {
		f = DefaultMemoryOvercommit
	}
	return int64(float64(l.MemoryBytes) * f)
}

// ReclaimIdleAfter returns how long a VM must be idle before admission may
// stop it to make room (0: never).
func (l Limits) ReclaimIdleAfter() time.Duration {
	switch {
	case l.ReclaimIdleAfterSeconds < 0:
		return 0
	case l.ReclaimIdleAfterSeconds == 0:
		return DefaultReclaimIdleAfterSeconds * time.Second
	}
	return time.Duration(l.ReclaimIdleAfterSeconds) * time.Second
}

// CapacityTicket is the CapacityWaiter.Ticket of a request with
// Idempotency-Key key.
func CapacityTicket(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:8])
}

// IsRoomError reports whether err is an admission refusal that may lift
// while the request waits: memory, live VM count or CPU (stopping idle VMs
// frees them) or free disk space (IsDiskSpaceError; the low-disk cleanup
// frees it). The disk allocation limit (limits.diskBytes) is not.
func IsRoomError(err error) bool {
	if IsDiskSpaceError(err) {
		return true
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeResourceExhausted {
		return false
	}
	switch e.Details["resource"] {
	case "memory", "vms", "cpu":
		return true
	}
	return false
}

// MemoryFloor returns the effective free-memory floor (0 = check disabled).
func (l Limits) MemoryFloor() int64 {
	switch {
	case l.MinFreeMemoryBytes < 0:
		return 0
	case l.MinFreeMemoryBytes == 0:
		return DefaultMinFreeMemoryBytes
	}
	return l.MinFreeMemoryBytes
}

// AdmitMemory refuses a VM needing want bytes of memory when the host's
// available memory (free) less want would fall below the floor.
func (l Limits) AdmitMemory(free, want int64) error {
	floor := l.MemoryFloor()
	if floor == 0 || free-want >= floor {
		return nil
	}
	e := Errorf(CodeResourceExhausted,
		"host has %d MiB of memory available; this VM needs %d MiB and %d MiB must stay free (limits.minFreeMemoryBytes); stop or delete VMs, or retry later",
		free/MiB, want/MiB, floor/MiB)
	e.Retryable = true
	return e.WithDetail("resource", "memory").WithDetail("freeBytes", free).
		WithDetail("needBytes", want+floor).WithDetail("floorBytes", floor)
}

// AdmitVMs refuses one more live VM when live VMs already reach l.VMs.
func (l Limits) AdmitVMs(live int) error {
	if l.VMs <= 0 || live < l.VMs {
		return nil
	}
	e := Errorf(CodeResourceExhausted, "host has %d live VMs, the limit (limits.vms) is %d; stop or delete a VM, or retry later", live, l.VMs)
	e.Retryable = true
	return e.WithDetail("resource", "vms").WithDetail("live", live).WithDetail("limit", l.VMs)
}

// Slots returns how many more VMs of shape admission would accept, given
// the allocation used, the live VM count, the host's available memory
// (negative: not measured, no bound), the data dir's filesystem (nil: not
// measured) and the disk headroom reserved for VMs still provisioning. It
// is at most MaxSlots.
func (l Limits) Slots(used Shape, live int, freeMemory int64, disk *DiskUsage, reservedDisk int64, shape Shape) int {
	n := MaxSlots
	bound := func(room, each int64) {
		if each > 0 {
			n = min(n, int(max(0, room/each)))
		}
	}
	if l.VMs > 0 {
		n = min(n, max(0, l.VMs-live))
	}
	if l.CPUs > 0 {
		bound(int64(l.CPUs-used.CPUs), int64(shape.CPUs))
	}
	if l.MemoryBytes > 0 {
		bound(l.MemoryBudget()-used.MemoryBytes, shape.MemoryBytes)
	}
	if l.DiskBytes > 0 {
		bound(l.DiskBytes-used.DiskBytes, shape.DiskBytes)
	}
	if floor := l.DiskFloor(0); disk != nil && floor > 0 {
		room := disk.FreeBytes - reservedDisk - l.DiskFloor(disk.TotalBytes)
		if each := l.DiskHeadroom(shape); each > 0 {
			bound(room, each)
		} else if room < 0 {
			n = 0
		}
	}
	if floor := l.MemoryFloor(); freeMemory >= 0 && floor > 0 {
		bound(freeMemory-floor, shape.MemoryBytes)
	}
	return n
}
