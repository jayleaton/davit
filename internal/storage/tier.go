package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"time"

	"davit/internal/domain"
)

// The cold tier (docs/storage-tiers.md): a second directory, on a
// slower and larger filesystem, that published commits move to while no
// VM boots from them. It has the hot root's commits/ and tmp/ layout:
//
//	<cold>/commits/<id>/disk.img       a commit's data, a full copy: blocks it
//	                                   shared with other commits on the hot
//	                                   filesystem cannot be shared across
//	<cold>/commits/<id>/manifest.json  the hot manifest plus DataDigest; its
//	                                   presence publishes the cold copy
//	<cold>/tmp/                        partial copies; removed by Collect
//
// A commit is looked up hot first, then cold; both copies are identical.
// Moving one is crash-safe at every point because the hot copy is never
// removed by the move itself:
//
//  1. Demote copies the hot data into <cold>/tmp, fsyncs it, drops it
//     from the page cache, reads it back and checks its digest against the
//     source's, writes the manifest and renames the directory into
//     <cold>/commits (fsyncing the directory). Both copies now exist.
//  2. The caller journals the commit as cold (KeepSet.Cold) and releases
//     the Demotion.
//  3. Collect removes the hot copy of a commit in KeepSet.Cold whose cold
//     copy is published, and the cold copy of any other kept commit whose
//     hot copy is published (a demotion the journal never recorded). It
//     never removes the only copy of a kept commit, and never touches a
//     commit while a Demote or Restore of it is in flight.
//
// Restore copies a cold commit back into a new hot disk, optionally as a
// reflink clone of a hot base (the image or snapshot the VM started from)
// with only the blocks that differ written, so the restored disk shares
// with that base again. It verifies the digest while it reads.

// blockSize is the unit of tier copies and of the data digest.
const blockSize = copyBufSize

// MoveOptions paces a Demote.
type MoveOptions struct {
	// BytesPerSecond caps the read rate (0: unlimited).
	BytesPerSecond int64
	// IdleIO runs the copy in the idle I/O class where the OS has one
	// (Linux ioprio; effective with the BFQ scheduler).
	IdleIO bool
}

// RestoreOptions shapes a Restore.
type RestoreOptions struct {
	// Base, when set, is a file on the hot filesystem holding the data the
	// commit most likely shares (its image root filesystem or the snapshot
	// it was created from): the new disk starts as a clone of it and only
	// the blocks that differ are written. A base that cannot be cloned is
	// ignored.
	Base string
	// Progress, when set, is called as data is copied with the bytes done
	// and the estimated total (the commit's allocated bytes).
	Progress func(done, total int64)
}

// Move is a published copy of a commit on its new tier (Demote, Promote).
// Until Release, Collect keeps both copies whatever the KeepSet says.
type Move struct {
	Commit Commit
	// Bytes is the data the new copy holds (what the move wrote).
	Bytes   int64
	release func()
}

// Release ends the move's protection from Collect.
func (d *Move) Release() {
	if d != nil && d.release != nil {
		d.release()
		d.release = nil
	}
}

// Tiered is a Store with a cold tier.
type Tiered interface {
	Store
	// ColdProbe is the clone probe of the cold tier's filesystem; ok is
	// false without a cold tier.
	ColdProbe() (p ProbeResult, ok bool)
	// Demote publishes a verified copy of commit id on the cold tier. If
	// the cold copy is already published it is returned unchanged.
	Demote(ctx context.Context, id CommitID, o MoveOptions) (*Move, error)
	// Promote publishes a hot copy of cold commit id (onto o.Base, as
	// Restore does), the reverse of Demote. If the hot copy is already
	// published it is returned unchanged.
	Promote(ctx context.Context, id CommitID, o RestoreOptions) (*Move, error)
	// Restore makes hot writable disk id from commit from, wherever it is
	// (CloneCommitted, plus a base and progress for a cold commit). If id
	// already exists it is returned unchanged.
	Restore(ctx context.Context, from CommitID, id DiskID, sizeBytes int64, o RestoreOptions) (Disk, error)
	// DeltaBytes estimates the hot space commit id holds beyond base (a
	// file on the hot filesystem, or ""): the bytes of its data that are
	// not the very blocks of base. It is what demoting the commit frees and
	// what restoring it onto base writes. Without extent reporting it is
	// the commit's allocated size.
	DeltaBytes(ctx context.Context, id CommitID, base string) (int64, error)
}

var _ Tiered = (*FS)(nil)

// AttachCold adds the cold tier rooted at dir, which must exist (a mount
// point, or a directory on the mounted cold filesystem), and probes its
// filesystem. The first attach writes a marker (coldMarker) into dir and
// records its ID in the hot root (coldRecord); later attaches refuse a
// dir without the same marker, so an unmounted cold disk (dir is then an
// empty directory on another filesystem) is never mistaken for an empty
// tier. Network filesystems are refused. Call it before the store is used.
func (f *FS) AttachCold(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("cold storage: %w", err)
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("cold storage %s is not a directory (is the cold disk mounted?): %v", abs, err)
	}
	if fs := fsName(abs); networkFS(abs) {
		return fmt.Errorf("cold storage %s is on a network filesystem (%s): not supported, use a local disk (docs/storage-tiers.md#network-storage)", abs, fs)
	}
	if err := f.checkMarker(abs); err != nil {
		return err
	}
	for _, d := range []string{filepath.Join(abs, commitsDir), filepath.Join(abs, tmpDir)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("cold storage: %w", err)
		}
	}
	p, err := probeDir(filepath.Join(abs, tmpDir), "", f.clone)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cold, f.coldProbe = abs, p
	return nil
}

// The cold tier's identity: coldMarker in the cold dir and coldRecord in
// the hot root hold the same random ID.
const (
	coldMarker = "davit-cold.json"
	coldRecord = "cold.json"
)

type coldID struct {
	ID  string `json:"id"`
	Dir string `json:"dir,omitempty"` // where it was last attached (coldRecord)
}

// checkMarker checks (or on first use, writes) the cold tier's identity.
func (f *FS) checkMarker(dir string) error {
	var have, want coldID
	mb, merr := os.ReadFile(filepath.Join(dir, coldMarker))
	if merr == nil {
		merr = json.Unmarshal(mb, &have)
	}
	rb, rerr := os.ReadFile(filepath.Join(f.root, coldRecord))
	if rerr == nil {
		rerr = json.Unmarshal(rb, &want)
	}
	switch {
	case rerr == nil && merr != nil:
		return fmt.Errorf("cold storage %s has no %s of this host's cold tier (last at %s): is the cold disk mounted?", dir, coldMarker, want.Dir)
	case rerr == nil && have.ID != want.ID:
		return fmt.Errorf("cold storage %s belongs to another davit store (%s does not match %s)", dir, coldMarker, filepath.Join(f.root, coldRecord))
	case rerr != nil && !errors.Is(rerr, os.ErrNotExist):
		return fmt.Errorf("cold storage record: %w", rerr)
	case rerr != nil && merr == nil:
		return fmt.Errorf("cold storage %s already belongs to another davit store (it has %s); use an empty directory", dir, coldMarker)
	case rerr != nil:
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		have = coldID{ID: hex.EncodeToString(b[:])}
		mb, _ := json.Marshal(have)
		if err := writeFileSync(filepath.Join(dir, coldMarker), append(mb, '\n'), 0o444); err != nil {
			return fmt.Errorf("cold storage: %w", err)
		}
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	if want.Dir == dir {
		return nil
	}
	rb, _ = json.Marshal(coldID{ID: have.ID, Dir: dir})
	tmp := filepath.Join(f.root, coldRecord+".tmp")
	os.Remove(tmp)
	if err := writeFileSync(tmp, append(rb, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(f.root, coldRecord)); err != nil {
		return err
	}
	return syncDir(f.root)
}

// ForgetCold removes the hot root's record of its cold tier, once no VM
// is on it, so that a new, empty tier can be attached.
func ForgetCold(root string) error {
	if err := os.Remove(filepath.Join(root, coldRecord)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(root)
}

// ColdRecorded reports the directory of the cold tier this store last
// attached (ok false if it never had one), so that a missing cold tier is
// noticed.
func ColdRecorded(root string) (dir string, ok bool) {
	b, err := os.ReadFile(filepath.Join(root, coldRecord))
	var r coldID
	if err != nil || json.Unmarshal(b, &r) != nil {
		return "", false
	}
	return r.Dir, true
}

// ColdProbe implements Tiered.
func (f *FS) ColdProbe() (ProbeResult, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.coldProbe, f.cold != ""
}

// Demote implements Tiered.
func (f *FS) Demote(ctx context.Context, id CommitID, o MoveOptions) (*Move, error) {
	if err := validID("commit", string(id)); err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.cold == "" {
		f.mu.Unlock()
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "no cold storage tier")
	}
	if c, _, err := f.readCommitIn(f.cold, id); err == nil {
		f.pin(id)
		f.mu.Unlock()
		return &Move{Commit: c, Bytes: allocated(c.Path), release: f.unpinFunc(id)}, nil
	}
	hot, m, err := f.readCommitIn(f.root, id)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	src, err := os.Open(hot.Path)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	defer src.Close()
	work, err := f.workDir(f.cold, "demote-")
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	f.pin(id)
	f.mu.Unlock()
	published := false
	defer func() {
		f.mu.Lock()
		delete(f.work, work)
		if !published {
			f.unpin(id)
		}
		f.mu.Unlock()
		os.RemoveAll(work) // empty after publication
	}()

	if o.IdleIO {
		defer idleIO()()
	}
	data, err := os.OpenFile(filepath.Join(work, commitData), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer data.Close()
	pace := newPacer(o.BytesPerSecond)
	sum := newDigest()
	var written int64
	err = scanBlocks(ctx, src, m.SizeBytes, func(off int64, b []byte) error {
		if err := pace.wait(ctx, int64(len(b))); err != nil {
			return err
		}
		if allZero(b) {
			return nil
		}
		sum.add(off, b)
		written += int64(len(b))
		_, err := data.WriteAt(b, off)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := data.Truncate(m.SizeBytes); err != nil {
		return nil, err
	}
	if err := fullSync(data); err != nil {
		return nil, err
	}
	want := sum.sum(m.SizeBytes)
	// Read back what reached the disk, not the page cache.
	dropCache(data)
	dropCache(src)
	got, err := fileDigest(ctx, data, m.SizeBytes, pace)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("cold copy of commit %s does not match: digest %s, source %s", id, got, want)
	}
	dropCache(data)
	if err := data.Chmod(0o444); err != nil {
		return nil, err
	}
	if err := data.Close(); err != nil {
		return nil, err
	}
	cm := m
	cm.DataDigest = want
	mb, err := json.MarshalIndent(cm, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFileSync(filepath.Join(work, manifestName), append(mb, '\n'), 0o444); err != nil {
		return nil, err
	}
	if err := syncDir(work); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	dst := filepath.Join(f.cold, commitsDir, string(id))
	if err := os.RemoveAll(dst); err != nil { // unpublished leftovers
		return nil, err
	}
	if err := os.Rename(work, dst); err != nil {
		return nil, err
	}
	published = true
	if err := syncDir(filepath.Join(f.cold, commitsDir)); err != nil {
		f.unpin(id)
		return nil, err
	}
	c, _, err := f.readCommitIn(f.cold, id)
	if err != nil {
		f.unpin(id)
		return nil, err
	}
	return &Move{Commit: c, Bytes: written, release: f.unpinFunc(id)}, nil
}

// Restore implements Tiered.
func (f *FS) Restore(ctx context.Context, from CommitID, id DiskID, sizeBytes int64, o RestoreOptions) (Disk, error) {
	if err := validID("commit", string(from)); err != nil {
		return Disk{}, err
	}
	if err := validID("disk", string(id)); err != nil {
		return Disk{}, err
	}
	if sizeBytes < 0 {
		return Disk{}, domain.Errorf(domain.CodeInvalidArgument, "invalid disk size %d", sizeBytes)
	}
	f.mu.Lock()
	if d, ok, err := f.existingDisk(id); ok || err != nil {
		f.mu.Unlock()
		return d, err
	}
	if _, _, err := f.readCommitIn(f.root, from); err == nil || f.cold == "" {
		defer f.mu.Unlock()
		return f.cloneLocked(ctx, from, id, sizeBytes)
	}
	c, m, err := f.readCommitIn(f.cold, from)
	if err != nil {
		f.mu.Unlock()
		return Disk{}, err
	}
	src, err := os.Open(c.Path)
	if err != nil {
		f.mu.Unlock()
		return Disk{}, err
	}
	defer src.Close()
	work, err := f.workDir(f.root, "restore-")
	if err != nil {
		f.mu.Unlock()
		return Disk{}, err
	}
	f.pin(from)
	reflink := f.probe.Mode == ModeReflink
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.work, work)
		f.unpin(from)
		f.mu.Unlock()
		os.RemoveAll(work)
	}()

	tmp, err := f.restoreInto(ctx, src, c, m, work, "disk"+diskExt, o, reflink)
	if err != nil {
		return Disk{}, err
	}
	defer tmp.Close()
	size := m.SizeBytes
	if sizeBytes > size {
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
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok, err := f.existingDisk(id); ok || err != nil {
		return d, err
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

// Promote implements Tiered.
func (f *FS) Promote(ctx context.Context, id CommitID, o RestoreOptions) (*Move, error) {
	if err := validID("commit", string(id)); err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.cold == "" {
		f.mu.Unlock()
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "no cold storage tier")
	}
	if c, _, err := f.readCommitIn(f.root, id); err == nil {
		f.pin(id)
		f.mu.Unlock()
		return &Move{Commit: c, Bytes: allocated(c.Path), release: f.unpinFunc(id)}, nil
	}
	c, m, err := f.readCommitIn(f.cold, id)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	src, err := os.Open(c.Path)
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	defer src.Close()
	work, err := f.workDir(f.root, "promote-")
	if err != nil {
		f.mu.Unlock()
		return nil, err
	}
	f.pin(id)
	reflink := f.probe.Mode == ModeReflink
	f.mu.Unlock()
	published := false
	defer func() {
		f.mu.Lock()
		delete(f.work, work)
		if !published {
			f.unpin(id)
		}
		f.mu.Unlock()
		os.RemoveAll(work)
	}()
	data, err := f.restoreInto(ctx, src, c, m, work, commitData, o, reflink)
	if err != nil {
		return nil, err
	}
	defer data.Close()
	if err := data.Chmod(0o444); err != nil {
		return nil, err
	}
	if err := fullSync(data); err != nil {
		return nil, err
	}
	if err := data.Close(); err != nil {
		return nil, err
	}
	hm := m
	hm.DataDigest = ""
	mb, err := json.MarshalIndent(hm, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFileSync(filepath.Join(work, manifestName), append(mb, '\n'), 0o444); err != nil {
		return nil, err
	}
	if err := syncDir(work); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.RemoveAll(f.commitDir(id)); err != nil { // unpublished leftovers
		return nil, err
	}
	if err := os.Rename(work, f.commitDir(id)); err != nil {
		return nil, err
	}
	published = true
	if err := syncDir(filepath.Join(f.root, commitsDir)); err != nil {
		f.unpin(id)
		return nil, err
	}
	hc, _, err := f.readCommitIn(f.root, id)
	if err != nil {
		f.unpin(id)
		return nil, err
	}
	return &Move{Commit: hc, Bytes: allocated(hc.Path), release: f.unpinFunc(id)}, nil
}

// restoreInto copies cold commit c (data src, manifest m) into a new file
// name in work, as a clone of o.Base with only the differing blocks
// written when one is given and clones, checking the data digest; it
// returns the file open, at the commit's size.
func (f *FS) restoreInto(ctx context.Context, src *os.File, c Commit, m manifest, work, name string, o RestoreOptions, reflink bool) (*os.File, error) {
	tmp, based, err := f.restoreTarget(work, name, o.Base, reflink)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
		}
	}()
	if err := tmp.Truncate(m.SizeBytes); err != nil { // cuts a longer base
		return nil, err
	}
	total, done := max(allocated(c.Path), 1), int64(0)
	var reported int64
	sum := newDigest()
	var have []bool // cold blocks holding data, for the base pass
	if based {
		have = make([]bool, (m.SizeBytes+blockSize-1)/blockSize)
	}
	cur := make([]byte, blockSize)
	err = scanBlocks(ctx, src, m.SizeBytes, func(off int64, b []byte) error {
		done += int64(len(b))
		if o.Progress != nil && (done-reported >= 64*domain.MiB) {
			reported = done
			o.Progress(min(done, total), total)
		}
		if allZero(b) {
			return nil
		}
		sum.add(off, b)
		if based {
			have[off/blockSize] = true
			n, err := tmp.ReadAt(cur[:len(b)], off)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if n == len(b) && bytes.Equal(cur[:n], b) {
				return nil // still shared with the base
			}
		}
		_, err := tmp.WriteAt(b, off)
		return err
	})
	if err != nil {
		return nil, err
	}
	if got := sum.sum(m.SizeBytes); m.DataDigest != "" && got != m.DataDigest {
		return nil, domain.Errorf(domain.CodeInternal, "cold copy of commit %s is damaged: digest %s, expected %s", c.ID, got, m.DataDigest)
	}
	if based {
		// Base data where the commit has none (a hole or zeros) goes.
		err = scanBlocks(ctx, tmp, m.SizeBytes, func(off int64, b []byte) error {
			if have[off/blockSize] || allZero(b) {
				return nil
			}
			return punchHole(tmp, off, int64(len(b)))
		})
		if err != nil {
			return nil, err
		}
	}
	if o.Progress != nil {
		o.Progress(total, total)
	}
	dropCache(src)
	ok = true
	return tmp, nil
}

// restoreTarget creates the restore's new file work/name: a clone of base
// when one is given and clones, else an empty file.
func (f *FS) restoreTarget(work, name, base string, reflink bool) (*os.File, bool, error) {
	path := filepath.Join(work, name)
	if base != "" && reflink {
		if b, err := os.Open(base); err == nil {
			err = f.clone.clone(b, work, name)
			b.Close()
			if err == nil {
				t, err := openPrivate(path)
				return t, err == nil, err
			}
			os.Remove(path)
		}
	}
	t, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	return t, false, err
}

// DeltaBytes implements Tiered.
func (f *FS) DeltaBytes(ctx context.Context, id CommitID, base string) (int64, error) {
	if err := validID("commit", string(id)); err != nil {
		return 0, err
	}
	f.mu.Lock()
	c, _, err := f.readCommitIn(f.root, id)
	var data *os.File
	if err == nil {
		data, err = os.Open(c.Path)
	}
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	defer data.Close()
	var b *os.File
	if base != "" {
		if b, err = os.Open(base); err == nil {
			defer b.Close()
		}
	}
	return deltaBytes(ctx, data, b)
}

// readCommitIn loads a published commit from the store rooted at root
// (the hot root or the cold one). A cold copy needs its digest.
func (f *FS) readCommitIn(root string, id CommitID) (Commit, manifest, error) {
	c, m, err := readCommitAt(filepath.Join(root, commitsDir, string(id)), id)
	if err == nil && root != f.root {
		if m.DataDigest == "" {
			return Commit{}, manifest{}, domain.Errorf(domain.CodeNotFound, "commit %q not found", id)
		}
		c.Tier = domain.TierCold
	}
	return c, m, err
}

// workDir makes a private temp directory under root's tmp/ that Collect
// leaves alone until it is deleted from f.work. Caller holds f.mu.
func (f *FS) workDir(root, prefix string) (string, error) {
	w, err := os.MkdirTemp(filepath.Join(root, tmpDir), prefix)
	if err != nil {
		return "", err
	}
	f.work[w] = true
	return w, nil
}

// pin and unpin count the moves of commit id in flight. Caller holds f.mu.
func (f *FS) pin(id CommitID) { f.moving[id]++ }

func (f *FS) unpin(id CommitID) {
	if f.moving[id]--; f.moving[id] <= 0 {
		delete(f.moving, id)
	}
}

func (f *FS) unpinFunc(id CommitID) func() {
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unpin(id)
	}
}

// collectCold is Collect's cold-tier half and its duplicate resolution:
// with both copies of a kept commit published, the journal's tier
// (keep.Cold) decides which one goes. Caller holds f.mu.
func (f *FS) collectCold(keep KeepSet, out *Collected) error {
	kept := map[CommitID]bool{}
	for _, id := range keep.Commits {
		kept[id] = true
	}
	cold := map[CommitID]bool{}
	for _, id := range keep.Cold {
		cold[id] = true
	}
	tmpEntries, err := os.ReadDir(filepath.Join(f.cold, tmpDir))
	if err != nil {
		return err
	}
	for _, e := range tmpEntries {
		p := filepath.Join(f.cold, tmpDir, e.Name())
		if f.work[p] {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return err
		}
		out.Partial++
	}
	entries, err := os.ReadDir(filepath.Join(f.cold, commitsDir))
	if err != nil {
		return err
	}
	for _, e := range entries {
		id := CommitID(e.Name())
		if validID("commit", string(id)) != nil || f.moving[id] > 0 {
			continue
		}
		_, _, cerr := f.readCommitIn(f.cold, id)
		_, _, herr := f.readCommitIn(f.root, id)
		coldOK, hotOK := cerr == nil, herr == nil
		switch {
		case coldOK && kept[id] && hotOK && cold[id]:
			if err := os.RemoveAll(f.commitDir(id)); err != nil { // the move finished
				return err
			}
			out.Moved++
			continue
		case coldOK && kept[id] && !(hotOK && !cold[id]):
			continue
		}
		if err := os.RemoveAll(filepath.Join(f.cold, commitsDir, string(id))); err != nil {
			return err
		}
		switch {
		case coldOK && kept[id]:
			out.Moved++ // a move the journal never recorded: the hot copy stays
		case coldOK:
			out.Commits = append(out.Commits, id)
		default:
			out.Partial++
		}
	}
	if err := syncDir(filepath.Join(f.root, commitsDir)); err != nil {
		return err
	}
	return syncDir(filepath.Join(f.cold, commitsDir))
}

// scanBlocks calls fn, in ascending order, for every blockSize-aligned
// block of f's first size bytes that holds a data extent (SEEK_DATA), with
// its content (a short last block; holes read as zeros). Blocks with no
// data are never read.
func scanBlocks(ctx context.Context, f *os.File, size int64, fn func(off int64, b []byte) error) error {
	buf := make([]byte, blockSize)
	last := int64(-1)
	for off := int64(0); off < size; {
		start, end, ok, err := nextData(f, off, size)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		w := start &^ (blockSize - 1)
		if w <= last {
			w = last + blockSize
		}
		for ; w < end; w += blockSize {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := min(blockSize, size-w)
			got, err := f.ReadAt(buf[:n], w)
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			clear(buf[got:n])
			if err := fn(w, buf[:n]); err != nil {
				return err
			}
			last = w
		}
		off = max(end, last+blockSize)
	}
	return nil
}

// digest is a commit's data digest: sha256 over every non-zero block's
// offset and bytes, then the size. It depends only on the content, not on
// where the file has holes, so a sparse copy has the source's digest.
type digest struct{ h hash.Hash }

func newDigest() *digest { return &digest{h: sha256.New()} }

func (d *digest) add(off int64, b []byte) {
	var hdr [12]byte
	binary.BigEndian.PutUint64(hdr[:8], uint64(off))
	binary.BigEndian.PutUint32(hdr[8:], uint32(len(b)))
	d.h.Write(hdr[:])
	d.h.Write(b)
}

func (d *digest) sum(size int64) string {
	var tail [8]byte
	binary.BigEndian.PutUint64(tail[:], uint64(size))
	d.h.Write(tail[:])
	return "blocks-sha256:" + hex.EncodeToString(d.h.Sum(nil))
}

// fileDigest computes the data digest of f's first size bytes.
func fileDigest(ctx context.Context, f *os.File, size int64, pace *pacer) (string, error) {
	d := newDigest()
	err := scanBlocks(ctx, f, size, func(off int64, b []byte) error {
		if err := pace.wait(ctx, int64(len(b))); err != nil {
			return err
		}
		if !allZero(b) {
			d.add(off, b)
		}
		return nil
	})
	return d.sum(size), err
}

// allocated is the space path's blocks take (its size where unknown).
func allocated(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return allocatedOf(fi)
}

// pacer keeps a copy at or under a rate.
type pacer struct {
	rate  int64
	start time.Time
	done  int64
}

func newPacer(rate int64) *pacer { return &pacer{rate: rate, start: time.Now()} }

// wait accounts n bytes and sleeps until the rate allows them.
func (p *pacer) wait(ctx context.Context, n int64) error {
	if p == nil || p.rate <= 0 {
		return nil
	}
	p.done += n
	due := p.start.Add(time.Duration(float64(p.done) / float64(p.rate) * float64(time.Second)))
	d := time.Until(due)
	if d < 10*time.Millisecond {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
