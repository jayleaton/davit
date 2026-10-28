package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"davit/internal/buildinfo"
)

// Disk admission (docs/vm-host.md#disk-space). The data dir's filesystem
// holds every VM's disk, mostly reflinks of an image or a snapshot commit:
// a new VM costs almost nothing when it starts and grows as its guest
// writes. Admission therefore reserves a share of each VM's disk size for
// that private growth (DiskHeadroom) and refuses a create, fork or resume
// whose reservation would take free space below the floor (DiskFloor).
// VMs accepted but still provisioning have not written yet, so their
// headroom is counted as reserved until they are up, like their memory.
// Below the floor stop and delete still work, and the save reserve keeps
// room for a stop's commit (SaveReserve).

// DefaultMinFreeDiskBytes and DefaultMinFreeDiskPercent make the floor
// when none is configured: 10% of the filesystem, at least 5 GiB.
const (
	DefaultMinFreeDiskBytes   = 5 * GiB
	DefaultMinFreeDiskPercent = 10
)

// DefaultDiskHeadroomPercent is the share of a VM's disk size reserved for
// its growth when Limits.DiskHeadroomPercent is zero.
const DefaultDiskHeadroomPercent = 25

// DefaultSaveReserveBytes is the save reserve when Limits.SaveReserveBytes
// is zero.
const DefaultSaveReserveBytes = 2 * GiB

// DiskUsage is a measurement of the data dir's filesystem.
type DiskUsage struct {
	// FreeBytes is the space available to the daemon (statfs f_bavail).
	FreeBytes int64 `json:"freeBytes"`
	// TotalBytes is the filesystem's size; 0 when not known.
	TotalBytes int64 `json:"totalBytes"`
}

// DiskFloor returns the free-space floor for a filesystem of total bytes:
// the larger of MinFreeDiskBytes and MinFreeDiskPercent of total (0 =
// disabled).
func (l Limits) DiskFloor(total int64) int64 {
	if l.MinFreeDiskBytes < 0 {
		return 0
	}
	floor := l.MinFreeDiskBytes
	if floor == 0 {
		floor = DefaultMinFreeDiskBytes
	}
	pct := l.MinFreeDiskPercent
	if pct == 0 {
		pct = DefaultMinFreeDiskPercent
	}
	if pct > 0 && total > 0 {
		floor = max(floor, total*int64(pct)/100)
	}
	return floor
}

// DiskHeadroom returns the free space admission reserves for a VM of shape
// to grow into: DiskHeadroomPercent of its disk size. A VM's disk shares
// the image or snapshot it starts from (reflink), so only what the guest
// writes is its own; the full disk size would be the true worst case but
// would refuse most hosts long before they are full.
func (l Limits) DiskHeadroom(shape Shape) int64 {
	pct := l.DiskHeadroomPercent
	switch {
	case pct < 0:
		return 0
	case pct == 0:
		pct = DefaultDiskHeadroomPercent
	}
	return shape.DiskBytes * int64(pct) / 100
}

// SaveReserve returns the size of the save reserve (0 = disabled).
func (l Limits) SaveReserve() int64 {
	switch {
	case l.SaveReserveBytes < 0:
		return 0
	case l.SaveReserveBytes == 0:
		return DefaultSaveReserveBytes
	}
	return l.SaveReserveBytes
}

// AdmitDisk refuses a VM whose growth headroom want, on top of the headroom
// reserved for VMs still provisioning, would leave less than the floor of
// d free. The refusal is retryable and queues (IsRoomError): the host's
// low-disk cleanup or the user can free space.
func (l Limits) AdmitDisk(d DiskUsage, reserved, want int64) error {
	floor := l.DiskFloor(d.TotalBytes)
	if floor == 0 || d.FreeBytes-reserved-want >= floor {
		return nil
	}
	need := reserved + want + floor
	n := buildinfo.Name
	e := Errorf(CodeResourceExhausted,
		"host disk low: %s free, need %s (%s growth headroom for this VM, %s for VMs still starting, %s floor); "+
			"free space with `%s box gc` (stops idle boxes) or `%s box rm`, or retry with a wait to queue",
		HumanBytes(d.FreeBytes), HumanBytes(need), HumanBytes(want), HumanBytes(reserved), HumanBytes(floor), n, n)
	e.Retryable = true
	return e.WithDetail("resource", "disk").WithDetail("freeBytes", d.FreeBytes).WithDetail("needBytes", need).
		WithDetail("floorBytes", floor).WithDetail("headroomBytes", want).WithDetail("reservedBytes", reserved)
}

// ResourceBoxDisk is the details "resource" of BoxDiskFull.
const ResourceBoxDisk = "boxDisk"

// BoxDiskFull is a write inside a box that failed because the box's own
// disk is full (the guest's ENOSPC or EDQUOT; msg is the guest's message).
// It is the user's to fix, by resizing the disk or cleaning the
// workspace, not the host's: resource_exhausted (exit code 8), not
// retryable, details resource "boxDisk".
func BoxDiskFull(msg string) *Error {
	n := buildinfo.Name
	return Errorf(CodeResourceExhausted, "%s; box disk full: `%s box up --disk 40GiB` (resize) or clean the workspace", msg, n).
		WithDetail("resource", ResourceBoxDisk)
}

// IsBoxDiskFull reports whether err is a BoxDiskFull.
func IsBoxDiskFull(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == CodeResourceExhausted && e.Details["resource"] == ResourceBoxDisk
}

// NoSpaceMessage reports whether an error message from inside a guest
// (where the host has no errno) says its filesystem is out of space.
func NoSpaceMessage(msg string) bool {
	return strings.Contains(msg, "no space left on device") || strings.Contains(msg, "disk quota exceeded")
}

// IsDiskSpaceError reports whether err is an AdmitDisk refusal.
func IsDiskSpaceError(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == CodeResourceExhausted && e.Details["resource"] == "disk" && e.Details["floorBytes"] != nil
}

// HumanBytes formats n in binary units ("5.0 GiB").
func HumanBytes(n int64) string {
	switch {
	case n >= GiB || n <= -GiB:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(GiB))
	case n >= MiB || n <= -MiB:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(MiB))
	case n >= 1<<10 || n <= -1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// AutoDeletion is one unused box the low-disk cleanup deleted: the data of
// a vm.autodeleted event.
type AutoDeletion struct {
	VMID VMID `json:"vmId"`
	// Name is the VM's name; Box is PROJECT/INSTANCE, or PROJECT for a
	// project image (Kind "image" or "instance").
	Name       string    `json:"name"`
	Box        string    `json:"box"`
	Kind       string    `json:"kind"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	// UnusedSeconds is how long it had been unused when it was deleted.
	UnusedSeconds int64 `json:"unusedSeconds"`
	// FreedBytes is the free space the deletion gave back as measured
	// (data shared with other boxes stays, so it may be 0).
	FreedBytes int64     `json:"freedBytes"`
	DeletedAt  time.Time `json:"deletedAt"`
	Reason     string    `json:"reason"`
}
