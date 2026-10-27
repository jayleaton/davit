package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"davit/internal/domain"
)

// Directory layout under the store root. Everything is created inside root so
// reflinks and renames stay on one filesystem.
//
//	disks/<id>.img             private writable disks
//	commits/<id>/disk.img      published, read-only commit data
//	commits/<id>/manifest.json commit manifest; its presence and validity is
//	                           what makes a commit published
//	tmp/                       partial work; never referenced, removed by Collect
const (
	disksDir     = "disks"
	commitsDir   = "commits"
	tmpDir       = "tmp"
	diskExt      = ".img"
	commitData   = "disk.img"
	manifestName = "manifest.json"
	maxIDLen     = 128
	copyBufSize  = 1 << 16
)

// FS is a directory-backed Store. Disks are raw image files; clones use the
// host's copy-on-write strategy (Linux FICLONE, macOS APFS clonefile) when the
// probe proved it works, otherwise a sparse-aware full copy. A single mutex
// serialises all mutations, including copies; that is deliberately simple
// for M0.
type FS struct {
	root  string
	probe ProbeResult
	clone cloneStrategy

	mu     sync.Mutex
	leases map[string]DiskID // lease ID -> disk
	// cold is the cold tier's root ("" without one, tier.go) and
	// coldProbe its filesystem's clone probe.
	cold      string
	coldProbe ProbeResult
	// work are the tmp/ directories of moves in flight and moving the
	// commits they move (tier.go); Collect leaves both alone.
	work   map[string]bool
	moving map[CommitID]int
}

var _ Store = (*FS)(nil)

// manifest is the on-disk commit manifest. Commit.Digest is the sha256 of the
// manifest.json bytes as stored; it identifies the manifest, not the data.
type manifest struct {
	Version   int       `json:"version"`
	ID        CommitID  `json:"id"`
	SizeBytes int64     `json:"sizeBytes"`
	CreatedAt time.Time `json:"createdAt"`
	Mode      Mode      `json:"mode"`
	Data      string    `json:"data"`
	// DataDigest is set on a cold copy (tier.go): the digest of its data,
	// checked when it is copied and again when it is restored.
	DataDigest string `json:"dataDigest,omitempty"`
}

// Open opens (creating if needed) a store rooted at root. mode "" probes the
// filesystem and picks reflink when a real clone (FICLONE on Linux, APFS
// clonefile on macOS) succeeds, else copy. ModeReflink fails if the probe
// cannot clone; it never silently downgrades. ModeCopy always copies.
func Open(root string, mode Mode) (*FS, error) {
	return openWith(root, mode, platformClone)
}

// openWith is Open with an explicit clone strategy (tests use it to run the
// absent-destination path on any host).
func openWith(root string, mode Mode, cs cloneStrategy) (*FS, error) {
	switch mode {
	case "", ModeReflink, ModeCopy:
	default:
		return nil, domain.Errorf(domain.CodeInvalidArgument, "unknown storage mode %q", mode)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage root: %w", err)
	}
	for _, d := range []string{abs, filepath.Join(abs, disksDir), filepath.Join(abs, commitsDir), filepath.Join(abs, tmpDir)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("storage root: %w", err)
		}
	}
	f := &FS{root: abs, clone: cs, leases: map[string]DiskID{}, work: map[string]bool{}, moving: map[CommitID]int{}}

	if mode == ModeCopy {
		f.probe = ProbeResult{Mode: ModeCopy, FastClone: false, Detail: "copy mode forced by configuration; clones are full copies"}
		return f, nil
	}
	p, err := probeDir(filepath.Join(abs, tmpDir), mode, cs)
	if err != nil {
		return nil, err
	}
	f.probe = p
	return f, nil
}

// ProbeDir measures clone support for files in the existing directory dir
// with a real clone of a temp file (removed afterwards), without creating a
// store. Mode semantics match Open: "" reports copy on failure, ModeReflink
// returns an unsupported_capability error, ModeCopy skips the probe.
func ProbeDir(dir string, mode Mode) (ProbeResult, error) {
	return probeDir(dir, mode, platformClone)
}

// probeDir is ProbeDir for strategy cs. A successful clone reports the wire
// mode reflink whatever the strategy; Detail names the strategy.
func probeDir(dir string, mode Mode, cs cloneStrategy) (ProbeResult, error) {
	if mode == ModeCopy {
		return ProbeResult{Mode: ModeCopy, FastClone: false, Detail: "copy mode forced by configuration; clones are full copies"}, nil
	}
	fsn := fsName(dir)
	if perr := probeClone(dir, cs); perr != nil {
		if mode == ModeReflink {
			return ProbeResult{}, domain.Errorf(domain.CodeUnsupportedCapability,
				"reflink mode requested but %s failed on %s: %v", cs.name, fsn, perr)
		}
		return ProbeResult{Mode: ModeCopy, FastClone: false,
			Detail: fmt.Sprintf("%s failed on %s: %v; using copy", cs.name, fsn, perr)}, nil
	}
	return ProbeResult{Mode: ModeReflink, FastClone: true, Detail: cs.name + " ok on " + fsn}, nil
}

// Probe returns the result measured when the store was opened.
func (f *FS) Probe(ctx context.Context) (ProbeResult, error) {
	return f.probe, ctx.Err()
}

// CreateWritable implements Store. base == "" creates an empty sparse disk.
func (f *FS) CreateWritable(ctx context.Context, id DiskID, base string, sizeBytes int64) (Disk, error) {
	if err := validID("disk", string(id)); err != nil {
		return Disk{}, err
	}
	if sizeBytes < 0 || (base == "" && sizeBytes == 0) {
		return Disk{}, domain.Errorf(domain.CodeInvalidArgument, "invalid disk size %d", sizeBytes)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok, err := f.existingDisk(id); ok || err != nil {
		return d, err
	}
	var src *os.File
	if base != "" {
		var err error
		if src, err = os.Open(base); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return Disk{}, domain.Errorf(domain.CodeNotFound, "base image not found")
			}
			return Disk{}, fmt.Errorf("open base: %w", err)
		}
		defer src.Close()
	}
	// A base image may live outside the store filesystem; a clone cannot
	// cross filesystems, so only that case falls back to copying (once).
	return f.publishDisk(ctx, id, src, sizeBytes, true)
}

// CloneCommitted implements Store.
func (f *FS) CloneCommitted(ctx context.Context, from CommitID, id DiskID, sizeBytes int64) (Disk, error) {
	if err := validID("commit", string(from)); err != nil {
		return Disk{}, err
	}
	if err := validID("disk", string(id)); err != nil {
		return Disk{}, err
	}
	if sizeBytes < 0 {
		return Disk{}, domain.Errorf(domain.CodeInvalidArgument, "invalid disk size %d", sizeBytes)
	}
	// A cold commit is copied back outside the lock (Restore).
	return f.Restore(ctx, from, id, sizeBytes, RestoreOptions{})
}

// cloneLocked is CloneCommitted of a hot commit. Caller holds f.mu.
func (f *FS) cloneLocked(ctx context.Context, from CommitID, id DiskID, sizeBytes int64) (Disk, error) {
	c, _, err := f.readCommit(from)
	if err != nil {
		return Disk{}, err
	}
	src, err := os.Open(c.Path)
	if err != nil {
		return Disk{}, fmt.Errorf("open commit: %w", err)
	}
	defer src.Close()
	return f.publishDisk(ctx, id, src, sizeBytes, false)
}

// publishDisk clones src (nil = empty) into a new file in a private tmp
// directory, grows it to sizeBytes, syncs it and renames it into disks/.
// Partial work is removed on failure. Caller holds f.mu.
func (f *FS) publishDisk(ctx context.Context, id DiskID, src *os.File, sizeBytes int64, crossFSOK bool) (Disk, error) {
	work, err := os.MkdirTemp(filepath.Join(f.root, tmpDir), "disk-")
	if err != nil {
		return Disk{}, err
	}
	// After publication the directory is empty; on failure it holds the
	// partial clone. Either way it goes.
	defer os.RemoveAll(work)
	tmp, err := f.workFile(ctx, src, work, "disk"+diskExt, crossFSOK)
	if err != nil {
		return Disk{}, err
	}
	defer tmp.Close() // no-op error after the explicit Close below
	st, err := tmp.Stat()
	if err != nil {
		return Disk{}, err
	}
	size := st.Size()
	if sizeBytes > size { // grow sparse, never shrink
		if err := tmp.Truncate(sizeBytes); err != nil {
			return Disk{}, err
		}
		size = sizeBytes
	}
	if err := fullSync(tmp); err != nil {
		return Disk{}, err
	}
	if err := tmp.Close(); err != nil {
		return Disk{}, err
	}
	dst := f.diskPath(id)
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return Disk{}, err
	}
	if err := syncDir(filepath.Join(f.root, disksDir)); err != nil {
		return Disk{}, err
	}
	return Disk{ID: id, Path: dst, SizeBytes: size}, nil
}

// sparseCopy copies src to dst, visiting only src's data extents (holes are
// skipped without reading them) and seeking over all-zero blocks inside
// extents, so holes and zeroed regions stay unallocated. dst ends with src's
// length.
func sparseCopy(ctx context.Context, dst, src *os.File) error {
	st, err := src.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	buf := make([]byte, copyBufSize)
	for off := int64(0); off < size; {
		start, end, ok, err := nextData(src, off, size)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		for pos := start; pos < end; {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, err := src.ReadAt(buf[:min(int64(len(buf)), end-pos)], pos)
			if n > 0 && !allZero(buf[:n]) {
				if _, err := dst.WriteAt(buf[:n], pos); err != nil {
					return err
				}
			}
			pos += int64(n)
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
		off = end
	}
	return dst.Truncate(size)
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// Commit implements Store.
func (f *FS) Commit(ctx context.Context, disk DiskID, id CommitID) (Commit, error) {
	if err := validID("disk", string(disk)); err != nil {
		return Commit{}, err
	}
	if err := validID("commit", string(id)); err != nil {
		return Commit{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, _, err := f.readCommit(id); err == nil {
		return c, nil
	} else if !domain.IsCode(err, domain.CodeNotFound) {
		return Commit{}, err
	}
	src, err := os.Open(f.diskPath(disk))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Commit{}, domain.Errorf(domain.CodeNotFound, "disk %q not found", disk)
		}
		return Commit{}, err
	}
	defer src.Close()

	work, err := os.MkdirTemp(filepath.Join(f.root, tmpDir), "commit-")
	if err != nil {
		return Commit{}, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(work)
		}
	}()
	data, err := f.workFile(ctx, src, work, commitData, false)
	if err != nil {
		return Commit{}, err
	}
	defer data.Close()
	st, err := data.Stat()
	if err != nil {
		return Commit{}, err
	}
	if err := data.Chmod(0o444); err != nil { // a commit ends read-only
		return Commit{}, err
	}
	if err := fullSync(data); err != nil {
		return Commit{}, err
	}
	m := manifest{Version: 1, ID: id, SizeBytes: st.Size(), CreatedAt: time.Now().UTC(), Mode: f.probe.Mode, Data: commitData}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Commit{}, err
	}
	if err := writeFileSync(filepath.Join(work, manifestName), append(mb, '\n'), 0o444); err != nil {
		return Commit{}, err
	}
	if err := syncDir(work); err != nil {
		return Commit{}, err
	}
	// A leftover directory under commits/<id> without a valid manifest is
	// unpublished partial work; replace it.
	dst := f.commitDir(id)
	if err := os.RemoveAll(dst); err != nil {
		return Commit{}, err
	}
	if err := os.Rename(work, dst); err != nil {
		return Commit{}, err
	}
	ok = true
	if err := syncDir(filepath.Join(f.root, commitsDir)); err != nil {
		return Commit{}, err
	}
	c, _, err := f.readCommit(id)
	return c, err
}

// GetCommit implements Store.
func (f *FS) GetCommit(ctx context.Context, id CommitID) (Commit, error) {
	if err := validID("commit", string(id)); err != nil {
		return Commit{}, err
	}
	if err := ctx.Err(); err != nil {
		return Commit{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, _, err := f.readCommit(id)
	return c, err
}

// OpenCommit implements Store. The data is opened under the store lock, so
// Collect cannot remove it between the check and the open; afterwards the
// open file keeps it readable whatever happens to the directory.
func (f *FS) OpenCommit(ctx context.Context, id CommitID) (*CommitFile, error) {
	if err := validID("commit", string(id)); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, _, err := f.readCommit(id)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(c.Path)
	if err != nil {
		return nil, err
	}
	return &CommitFile{File: file, Commit: c}, nil
}

// readCommit loads a published commit, hot or else cold. Caller holds f.mu.
func (f *FS) readCommit(id CommitID) (Commit, manifest, error) {
	c, m, err := f.readCommitIn(f.root, id)
	if err != nil && f.cold != "" && domain.IsCode(err, domain.CodeNotFound) {
		if cc, cm, cerr := f.readCommitIn(f.cold, id); cerr == nil {
			return cc, cm, nil
		}
	}
	return c, m, err
}

// readCommitAt loads the published commit id in dir. A missing or invalid
// manifest, or data whose size disagrees with it, means not published
// (not_found).
func readCommitAt(dir string, id CommitID) (Commit, manifest, error) {
	notFound := domain.Errorf(domain.CodeNotFound, "commit %q not found", id)
	mb, err := os.ReadFile(filepath.Join(dir, manifestName))
	if errors.Is(err, os.ErrNotExist) {
		return Commit{}, manifest{}, notFound
	} else if err != nil {
		return Commit{}, manifest{}, err
	}
	var m manifest
	if err := json.Unmarshal(mb, &m); err != nil || m.ID != id || m.Data != commitData {
		return Commit{}, manifest{}, notFound
	}
	path := filepath.Join(dir, commitData)
	st, err := os.Stat(path)
	if err != nil || st.Size() != m.SizeBytes {
		return Commit{}, manifest{}, notFound
	}
	sum := sha256.Sum256(mb)
	return Commit{ID: id, Path: path, SizeBytes: m.SizeBytes, Digest: "sha256:" + hex.EncodeToString(sum[:]), CreatedAt: m.CreatedAt, Tier: domain.TierHot}, m, nil
}

// OpenForRuntime implements Store.
func (f *FS) OpenForRuntime(ctx context.Context, disk DiskID) (Lease, error) {
	if err := validID("disk", string(disk)); err != nil {
		return Lease{}, err
	}
	if err := ctx.Err(); err != nil {
		return Lease{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok, err := f.existingDisk(disk)
	if err != nil {
		return Lease{}, err
	}
	if !ok {
		return Lease{}, domain.Errorf(domain.CodeNotFound, "disk %q not found", disk)
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Lease{}, err
	}
	l := Lease{ID: "lease_" + hex.EncodeToString(b[:]), Disk: disk, Path: d.Path}
	f.leases[l.ID] = disk
	return l, nil
}

// Release implements Store.
func (f *FS) Release(ctx context.Context, lease Lease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.leases, lease.ID)
	return nil
}

// Collect implements Store. Leases are in-memory, so after a restart only
// keep protects disks.
func (f *FS) Collect(ctx context.Context, keep KeepSet) (Collected, error) {
	if err := ctx.Err(); err != nil {
		return Collected{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out Collected

	tmpEntries, err := os.ReadDir(filepath.Join(f.root, tmpDir))
	if err != nil {
		return out, err
	}
	for _, e := range tmpEntries {
		p := filepath.Join(f.root, tmpDir, e.Name())
		if f.work[p] {
			continue // a restore in flight (tier.go)
		}
		if err := os.RemoveAll(p); err != nil {
			return out, err
		}
		out.Partial++
	}

	leased := map[DiskID]bool{}
	for _, d := range f.leases {
		leased[d] = true
	}
	diskEntries, err := os.ReadDir(filepath.Join(f.root, disksDir))
	if err != nil {
		return out, err
	}
	for _, e := range diskEntries {
		name := e.Name()
		id := DiskID(strings.TrimSuffix(name, diskExt))
		if !strings.HasSuffix(name, diskExt) || validID("disk", string(id)) != nil {
			continue // not ours; leave it alone
		}
		if leased[id] || slices.Contains(keep.Disks, id) {
			continue
		}
		if err := os.Remove(filepath.Join(f.root, disksDir, name)); err != nil {
			return out, err
		}
		out.Disks = append(out.Disks, id)
	}

	commitEntries, err := os.ReadDir(filepath.Join(f.root, commitsDir))
	if err != nil {
		return out, err
	}
	for _, e := range commitEntries {
		id := CommitID(e.Name())
		if validID("commit", string(id)) != nil || f.moving[id] > 0 {
			continue
		}
		_, _, rerr := f.readCommitIn(f.root, id)
		published := rerr == nil
		if published && slices.Contains(keep.Commits, id) {
			continue
		}
		if err := os.RemoveAll(f.commitDir(id)); err != nil {
			return out, err
		}
		if published {
			out.Commits = append(out.Commits, id)
		} else {
			out.Partial++
		}
	}
	if err := syncDir(filepath.Join(f.root, disksDir)); err != nil {
		return out, err
	}
	if f.cold != "" {
		return out, f.collectCold(keep, &out)
	}
	return out, syncDir(filepath.Join(f.root, commitsDir))
}

// existingDisk returns disk id if present. Caller holds f.mu.
func (f *FS) existingDisk(id DiskID) (Disk, bool, error) {
	p := f.diskPath(id)
	st, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return Disk{}, false, nil
	} else if err != nil {
		return Disk{}, false, err
	}
	return Disk{ID: id, Path: p, SizeBytes: st.Size()}, true, nil
}

func (f *FS) diskPath(id DiskID) string {
	return filepath.Join(f.root, disksDir, string(id)+diskExt)
}

func (f *FS) commitDir(id CommitID) string { return filepath.Join(f.root, commitsDir, string(id)) }

// validID accepts 1-128 characters of [A-Za-z0-9_.-], excluding "." and
// "..", so an ID is always a single safe path element.
func validID(kind, id string) error {
	bad := id == "" || len(id) > maxIDLen || id == "." || id == ".." || strings.Contains(id, "..")
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			bad = true
			break
		}
	}
	if bad {
		return domain.Errorf(domain.CodeInvalidArgument, "invalid %s id %q", kind, id)
	}
	return nil
}

func writeFileSync(path string, b []byte, perm os.FileMode) error {
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := fh.Write(b); err != nil {
		fh.Close()
		return err
	}
	if err := fullSync(fh); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return fullSync(d)
}
