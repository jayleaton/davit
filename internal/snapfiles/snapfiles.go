// Package snapfiles reads files out of a snapshot's committed disk on the
// host, without booting it, mounting it or root (CONTRACT-v1 G12): listing a
// directory, reading a file or a directory as tar, and exporting the whole
// filesystem as tar or the raw disk.
//
// The disk is an immutable storage commit, read through internal/ext4 as an
// untrusted image. Paths resolve inside the snapshot as if it were the root
// directory, so neither ".." nor any symlink reaches outside it. A read holds
// the commit open (storage.OpenCommit) from start to end, so it completes
// with the same bytes even if the snapshot is deleted and collected
// meanwhile. Memory is bounded: everything streams through fixed buffers.
package snapfiles

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/ext4"
	"davit/internal/storage"
)

// Snapshots resolves a snapshot reference visible to a principal
// (lifecycle.Service).
type Snapshots interface {
	GetSnapshot(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.Snapshot, error)
}

// Service serves snapshot file reads.
type Service struct {
	snaps Snapshots
	store storage.Store
}

// New returns a service over snaps and the store holding their data.
func New(snaps Snapshots, store storage.Store) *Service {
	return &Service{snaps: snaps, store: store}
}

const (
	defaultLimit = 50
	maxLimit     = 200
	maxPath      = 4096
	maxDepth     = 1024 // directory nesting in one archive
	dirPage      = 256  // entries read at a time while archiving
	copyBuf      = 256 << 10
)

// disk is one open snapshot disk; Close releases the commit.
type disk struct {
	snap *domain.Snapshot
	cf   *storage.CommitFile
	fs   *ext4.FS
}

func (d *disk) Close() error { return d.cf.Close() }

func (s *Service) open(ctx context.Context, p domain.PrincipalID, ref string) (*disk, error) {
	snap, err := s.snaps.GetSnapshot(ctx, p, ref)
	if err != nil {
		return nil, err
	}
	if snap.Kind != domain.SnapshotKindDisk || snap.CommitID == "" {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "snapshot %s has no disk to read", snap.ID).
			WithDetail("capability", domain.FeatureSnapshotFiles)
	}
	cf, err := s.store.OpenCommit(ctx, storage.CommitID(snap.CommitID))
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			return nil, domain.Errorf(domain.CodeNotFound, "snapshot %s data not found", snap.ID)
		}
		return nil, err
	}
	fsys, err := ext4.Open(cf, cf.Commit.SizeBytes)
	if err != nil {
		cf.Close()
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "snapshot %s: the disk is not a readable ext2/3/4 filesystem: %v", snap.ID, err).
			WithDetail("capability", domain.FeatureSnapshotFiles)
	}
	return &disk{snap: snap, cf: cf, fs: fsys}, nil
}

// fsError maps reader errors to API errors for path p.
func fsError(err error, p string) error {
	var de *domain.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &de):
		return err
	case errors.Is(err, fs.ErrNotExist):
		return domain.Errorf(domain.CodeNotFound, "%s: no such file or directory in the snapshot", p).WithDetail("path", p)
	case errors.Is(err, ext4.ErrNotDir):
		return domain.Errorf(domain.CodeInvalidArgument, "%s: a path component is not a directory", p).WithDetail("field", "path")
	case errors.Is(err, ext4.ErrLoop):
		return domain.Errorf(domain.CodeInvalidArgument, "%s: too many levels of symbolic links", p).WithDetail("field", "path")
	case errors.Is(err, ext4.ErrUnsupported):
		return domain.Errorf(domain.CodeUnsupportedCapability, "%s: %v", p, err).WithDetail("capability", domain.FeatureSnapshotFiles)
	case errors.Is(err, ext4.ErrCorrupt):
		return domain.Errorf(domain.CodeInternal, "%s: %v", p, err)
	}
	return err
}

func checkPath(p string) error {
	if !strings.HasPrefix(p, "/") || len(p) > maxPath || strings.ContainsRune(p, 0) {
		return domain.Errorf(domain.CodeInvalidArgument, "path must be absolute (a path inside the snapshot), at most %d bytes", maxPath).WithDetail("field", "path")
	}
	return nil
}

// FormatMode renders permission bits as four octal digits ("0644").
func FormatMode(m uint16) string {
	s := strconv.FormatUint(uint64(m&0o7777), 8)
	return strings.Repeat("0", max(0, 4-len(s))) + s
}

type cursor struct {
	Snapshot string `json:"s"`
	Path     string `json:"p"`
	Offset   int64  `json:"o"`
}

// Tree lists one page of the directory at p (symlinks to it followed).
func (s *Service) Tree(ctx context.Context, principal domain.PrincipalID, ref, p string, limit int, cur string) (*api.SnapshotTree, error) {
	if p == "" {
		p = "/"
	}
	if err := checkPath(p); err != nil {
		return nil, err
	}
	switch {
	case limit == 0:
		limit = defaultLimit
	case limit < 1 || limit > maxLimit:
		return nil, domain.Errorf(domain.CodeInvalidArgument, "limit must be 1..%d", maxLimit).WithDetail("field", "limit")
	}
	d, err := s.open(ctx, principal, ref)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	dir, canon, err := d.fs.Resolve(p, true)
	if err != nil {
		return nil, fsError(err, p)
	}
	if dir.Type() != ext4.TypeDir {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "%s is not a directory", p).WithDetail("field", "path")
	}
	var off int64
	if cur != "" {
		c, err := decodeCursor(cur)
		if err != nil || c.Snapshot != string(d.snap.ID) || c.Path != canon || c.Offset <= 0 {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "cursor does not belong to this listing").WithDetail("field", "cursor")
		}
		off = c.Offset
	}
	entries, next, err := d.fs.ReadDir(dir, off, limit)
	if errors.Is(err, ext4.ErrBadOffset) {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "cursor does not belong to this listing").WithDetail("field", "cursor")
	}
	if err != nil {
		return nil, fsError(err, p)
	}
	out := &api.SnapshotTree{Path: canon, Items: make([]api.SnapshotEntry, 0, len(entries))}
	for _, e := range entries {
		in, err := d.fs.Inode(e.Ino)
		if err != nil {
			return nil, fsError(err, path.Join(canon, e.Name))
		}
		item := api.SnapshotEntry{
			Path: path.Join(canon, e.Name), Type: in.Type().String(), SizeBytes: in.Size,
			Mode: FormatMode(in.Mode), ModifiedAt: in.ModTime,
		}
		if in.Type() == ext4.TypeSymlink {
			if item.LinkTarget, err = d.fs.Readlink(in); err != nil {
				return nil, fsError(err, item.Path)
			}
		}
		out.Items = append(out.Items, item)
	}
	if next >= 0 {
		b, _ := json.Marshal(cursor{Snapshot: string(d.snap.ID), Path: canon, Offset: next})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, nil
}

func decodeCursor(s string) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	return c, err
}

// Head describes a response before its body is written.
type Head struct {
	ContentType string
	Size        int64  // -1 for a stream of unknown length
	Mode        string // permission bits of the file or archive root ("" for raw)
	SHA256      string // hex digest of the body, when known before it is sent
}

// Start is called once with the head; it returns where the body goes.
type Start func(Head) (io.Writer, error)

// ReadFile writes the file at p (symlinks followed), or with archive the
// directory or file at p as tar. It returns the hex SHA-256 of what it
// wrote. Errors before start are API errors; after it, the body is cut.
func (s *Service) ReadFile(ctx context.Context, principal domain.PrincipalID, ref, p string, archive bool, start Start) (string, error) {
	if err := checkPath(p); err != nil {
		return "", err
	}
	d, err := s.open(ctx, principal, ref)
	if err != nil {
		return "", err
	}
	defer d.Close()
	in, canon, err := d.fs.Resolve(p, true)
	if err != nil {
		return "", fsError(err, p)
	}
	switch {
	case archive && (in.Type() == ext4.TypeDir || in.Type() == ext4.TypeFile):
		return d.writeTar(ctx, in, canon, start)
	case in.Type() == ext4.TypeDir:
		return "", domain.Errorf(domain.CodeInvalidArgument, "%s is a directory: read it with archive=tar", p).WithDetail("field", "archive")
	case in.Type() != ext4.TypeFile:
		return "", domain.Errorf(domain.CodeInvalidArgument, "%s is not a regular file", p).WithDetail("field", "path")
	}
	// The digest goes in the head: hash first. The commit is immutable, so
	// the second pass reads the same bytes.
	h := sha256.New()
	if _, err := io.CopyBuffer(h, ctxReader{ctx, d.fs.NewReader(in)}, make([]byte, copyBuf)); err != nil {
		return "", fsError(err, p)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	w, err := start(Head{ContentType: "application/octet-stream", Size: in.Size, Mode: FormatMode(in.Mode), SHA256: sum})
	if err != nil {
		return "", err
	}
	_, err = io.CopyBuffer(w, ctxReader{ctx, d.fs.NewReader(in)}, make([]byte, copyBuf))
	return sum, err
}

// Export writes the whole filesystem as tar, or the raw disk image, and
// returns the hex SHA-256 of what it wrote.
func (s *Service) Export(ctx context.Context, principal domain.PrincipalID, ref, format string, start Start) (string, error) {
	switch format {
	case "", api.ExportTar:
		return s.ReadFile(ctx, principal, ref, "/", true, start)
	case api.ExportRaw:
	default:
		return "", domain.Errorf(domain.CodeInvalidArgument, "format must be %q or %q", api.ExportTar, api.ExportRaw).WithDetail("field", "format")
	}
	snap, err := s.snaps.GetSnapshot(ctx, principal, ref)
	if err != nil {
		return "", err
	}
	if snap.Kind != domain.SnapshotKindDisk || snap.CommitID == "" {
		return "", domain.Errorf(domain.CodeUnsupportedCapability, "snapshot %s has no disk to export", snap.ID).
			WithDetail("capability", domain.FeatureSnapshotFiles)
	}
	cf, err := s.store.OpenCommit(ctx, storage.CommitID(snap.CommitID))
	if err != nil {
		return "", err
	}
	defer cf.Close()
	w, err := start(Head{ContentType: "application/octet-stream", Size: cf.Commit.SizeBytes})
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.CopyBuffer(io.MultiWriter(w, h), ctxReader{ctx, io.NewSectionReader(cf, 0, cf.Commit.SizeBytes)}, make([]byte, copyBuf))
	return hex.EncodeToString(h.Sum(nil)), err
}

// writeTar archives in (a directory's contents, or one file under its base
// name) in tarx's layout: names relative to the root, directories,
// regular files and symlinks (stored, never followed), permission bits and
// mtimes, no owners; other types are skipped.
func (d *disk) writeTar(ctx context.Context, in *ext4.Inode, canon string, start Start) (string, error) {
	w, err := start(Head{ContentType: api.ContentTypeTar, Size: -1, Mode: FormatMode(in.Mode)})
	if err != nil {
		return "", err
	}
	h := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(w, h))
	buf := make([]byte, copyBuf)
	if in.Type() == ext4.TypeFile {
		err = d.addEntry(ctx, tw, buf, path.Base(canon), in)
	} else {
		err = d.addDir(ctx, tw, buf, "", in, []uint32{in.Ino})
	}
	if err == nil {
		err = tw.Close()
	}
	return hex.EncodeToString(h.Sum(nil)), err
}

func (d *disk) addDir(ctx context.Context, tw *tar.Writer, buf []byte, prefix string, dir *ext4.Inode, onPath []uint32) error {
	if len(onPath) > maxDepth {
		return fmt.Errorf("snapshot directory nesting deeper than %d", maxDepth)
	}
	for off := int64(0); off >= 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, next, err := d.fs.ReadDir(dir, off, dirPage)
		if err != nil {
			return err
		}
		off = next
		for _, e := range entries {
			in, err := d.fs.Inode(e.Ino)
			if err != nil {
				return err
			}
			name := prefix + e.Name
			if err := d.addEntry(ctx, tw, buf, name, in); err != nil {
				return err
			}
			if in.Type() != ext4.TypeDir {
				continue
			}
			for _, ino := range onPath { // a directory cycle: a corrupt image
				if ino == in.Ino {
					return fmt.Errorf("%w: directory cycle at %s", ext4.ErrCorrupt, name)
				}
			}
			if err := d.addDir(ctx, tw, buf, name+"/", in, append(onPath, in.Ino)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *disk) addEntry(ctx context.Context, tw *tar.Writer, buf []byte, name string, in *ext4.Inode) error {
	hdr := &tar.Header{Name: name, Mode: int64(in.Mode & 0o7777), ModTime: in.ModTime, Format: tar.FormatPAX}
	switch in.Type() {
	case ext4.TypeDir:
		hdr.Typeflag, hdr.Name = tar.TypeDir, name+"/"
	case ext4.TypeFile:
		hdr.Typeflag, hdr.Size = tar.TypeReg, in.Size
	case ext4.TypeSymlink:
		target, err := d.fs.Readlink(in)
		if err != nil {
			return err
		}
		hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, target
	default:
		return nil // devices, FIFOs, sockets: as tarx does
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if hdr.Typeflag != tar.TypeReg {
		return nil
	}
	_, err := io.CopyBuffer(tw, ctxReader{ctx, d.fs.NewReader(in)}, buf)
	return err
}

// ctxReader stops a long copy when its request is gone.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
