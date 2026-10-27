// Package storage owns VM disk data on the host (SPEC §5): private writable
// disks, immutable committed generations, runtime leases and garbage
// collection. Bulk data lives in directories, never in SQLite.
//
// Contract every implementation must keep:
//   - IDs are chosen by the caller (lifecycle) so every call is idempotent and
//     can be safely re-run after a crash.
//   - Commit publishes a commit only after data and directories are durably
//     synced; partial work stays unreferenced and invisible.
//   - A published commit is immutable and is never replaced.
//   - Probe reports the mode that is actually used; FastClone is true only if
//     a real clone succeeded on this filesystem. Never label a copy fast.
//   - Paths returned are host-internal and never cross the API boundary.
package storage

import (
	"context"
	"os"
	"time"

	"davit/internal/domain"
)

// Mode is the clone strategy in use.
type Mode string

const (
	ModeReflink Mode = "reflink"
	ModeCopy    Mode = "copy"
)

// DiskID names a private writable disk. CommitID names an immutable commit.
type (
	DiskID   string
	CommitID string
)

// ProbeResult is the measured clone capability of the store's filesystem.
type ProbeResult struct {
	Mode      Mode
	FastClone bool
	Detail    string // e.g. "FICLONE ok on btrfs" or "FICLONE: operation not supported; using copy"
}

// Capabilities converts a probe to the domain report.
func (p ProbeResult) Capabilities() domain.StorageCapabilities {
	return domain.StorageCapabilities{Mode: string(p.Mode), FastClone: p.FastClone, Detail: p.Detail}
}

// Disk is a private writable disk image.
type Disk struct {
	ID        DiskID
	Path      string
	SizeBytes int64
}

// Commit is an immutable, durably published disk generation.
type Commit struct {
	ID        CommitID
	Path      string
	SizeBytes int64
	Digest    string // "sha256:<hex>" of the manifest, not the full data
	CreatedAt time.Time
	// Tier is where the data is: hot (the store root) or cold (tier.go).
	Tier domain.StorageTier
}

// CommitFile is an open, read-only commit (Store.OpenCommit). Its file never
// changes; Close releases it.
type CommitFile struct {
	*os.File
	Commit Commit
}

// Lease pins a disk while a runtime has it open.
type Lease struct {
	ID   string
	Disk DiskID
	Path string
}

// Store is the storage interface. All methods are safe for concurrent use.
type Store interface {
	// Probe measures clone support on the store's filesystem.
	Probe(ctx context.Context) (ProbeResult, error)
	// CreateWritable makes disk id from base (an image rootfs path), grown
	// (sparse) to sizeBytes. If id already exists it is returned unchanged.
	CreateWritable(ctx context.Context, id DiskID, base string, sizeBytes int64) (Disk, error)
	// CloneCommitted makes writable disk id from commit from. If id already
	// exists it is returned unchanged. sizeBytes may grow, never shrink.
	CloneCommitted(ctx context.Context, from CommitID, id DiskID, sizeBytes int64) (Disk, error)
	// Commit publishes the current contents of disk as immutable commit id.
	// If id is already published it is returned unchanged.
	Commit(ctx context.Context, disk DiskID, id CommitID) (Commit, error)
	// GetCommit returns a published commit, or a not_found *domain.Error.
	GetCommit(ctx context.Context, id CommitID) (Commit, error)
	// OpenCommit opens a published commit's data read-only. The handle keeps
	// that data readable until Close, even if Collect removes the commit
	// meanwhile: readers never see it vanish or change mid-read.
	OpenCommit(ctx context.Context, id CommitID) (*CommitFile, error)
	// OpenForRuntime leases disk for a runtime.
	OpenForRuntime(ctx context.Context, disk DiskID) (Lease, error)
	// Release ends a lease. Releasing an unknown lease is not an error.
	Release(ctx context.Context, lease Lease) error
	// Collect removes every disk and commit not in keep and not leased, plus
	// any unpublished partial work. It returns what it removed.
	Collect(ctx context.Context, keep KeepSet) (Collected, error)
}

// KeepSet lists the objects that must survive Collect.
type KeepSet struct {
	Disks   []DiskID
	Commits []CommitID
	// Cold are the kept commits the caller recorded on the cold tier:
	// their hot copy goes once the cold one is published. Any other kept
	// commit keeps its hot copy and loses a cold duplicate (tier.go).
	Cold []CommitID
}

// Collected reports what Collect removed.
type Collected struct {
	Disks   []DiskID
	Commits []CommitID
	Partial int
	// Moved counts the duplicate copies of kept commits removed (tier.go).
	Moved int
}
