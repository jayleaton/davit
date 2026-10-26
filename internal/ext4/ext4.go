// Package ext4 reads an ext2/3/4 filesystem image in userspace, read-only:
// no mount, no root, no cgo. It is the minimal reader snapshot file access
// needs (directories, regular files, symlinks, inode metadata) and nothing
// more: no journal replay, no writes, no extended attributes.
//
// The image is untrusted. Every on-disk number is bounds-checked against the
// image before use, nothing is sized by it, and every structure is read a
// block at a time, so memory stays bounded whatever the image claims.
//
// Supported: extent-mapped and block-mapped (indirect) files, holes and
// uninitialized extents (read as zeros), linear and hashed (dir_index)
// directories, fast and slow symlinks, 64bit and flex_bg layouts, checksummed
// metadata (checksums are not verified). Refused with ErrUnsupported:
// meta_bg layouts, inline-data and encrypted inodes.
package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"
)

// RootIno is the root directory's inode number.
const RootIno = 2

var (
	// ErrCorrupt reports an image that is not a valid ext filesystem or
	// contradicts itself.
	ErrCorrupt = errors.New("ext4: corrupt filesystem")
	// ErrUnsupported reports a valid feature this reader does not implement.
	ErrUnsupported = fmt.Errorf("ext4: %w", errors.ErrUnsupported)
)

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

const (
	sbOffset = 1024
	sbMagic  = 0xEF53

	incompatFiletype   = 0x2
	incompatRecover    = 0x4
	incompatJournalDev = 0x8
	incompatMetaBG     = 0x10
	incompat64bit      = 0x80
	incompatDirdata    = 0x1000

	flagIndex      = 0x1000 // hashed directory: readable linearly
	flagExtents    = 0x80000
	flagInlineData = 0x10000000
	flagEncrypt    = 0x800
)

// FS is an open filesystem image. It is safe for concurrent use: it holds
// no mutable state beyond the immutable superblock facts.
type FS struct {
	r          io.ReaderAt
	size       int64
	blockSize  int64
	blocks     uint64
	inodes     uint32
	perGroup   uint32 // inodes per group
	inodeSize  int64
	descSize   int64
	gdtBlock   uint64
	groups     uint32
	filetype   bool
	needsRecov bool
}

// Open reads the superblock of the image in r (size bytes long).
func Open(r io.ReaderAt, size int64) (*FS, error) {
	var sb [1024]byte
	if size < sbOffset+int64(len(sb)) {
		return nil, corrupt("image of %d bytes is too small", size)
	}
	if _, err := r.ReadAt(sb[:], sbOffset); err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	if le.Uint16(sb[0x38:]) != sbMagic {
		return nil, fmt.Errorf("%w: no ext2/3/4 superblock", ErrCorrupt)
	}
	f := &FS{r: r, size: size}
	logBS := le.Uint32(sb[0x18:])
	if logBS > 6 { // 1 KiB << 6 = 64 KiB, the ext4 maximum
		return nil, corrupt("block size 2^%d KiB", logBS)
	}
	f.blockSize = 1024 << logBS
	incompat := le.Uint32(sb[0x60:])
	if incompat&(incompatJournalDev|incompatDirdata) != 0 {
		return nil, fmt.Errorf("%w: incompatible features %#x", ErrUnsupported, incompat)
	}
	if incompat&incompatMetaBG != 0 {
		return nil, fmt.Errorf("%w: meta_bg layout", ErrUnsupported)
	}
	f.filetype = incompat&incompatFiletype != 0
	f.needsRecov = incompat&incompatRecover != 0
	f.blocks = uint64(le.Uint32(sb[0x04:]))
	f.descSize = 32
	if incompat&incompat64bit != 0 {
		f.blocks |= uint64(le.Uint32(sb[0x150:])) << 32
		if ds := int64(le.Uint16(sb[0xFE:])); ds != 0 {
			f.descSize = ds
		}
	}
	if f.descSize < 32 || f.descSize > f.blockSize || f.descSize&(f.descSize-1) != 0 {
		return nil, corrupt("group descriptor size %d", f.descSize)
	}
	f.inodes = le.Uint32(sb[0x00:])
	f.perGroup = le.Uint32(sb[0x28:])
	perGroupBlocks := le.Uint32(sb[0x20:])
	f.inodeSize = 128
	if le.Uint32(sb[0x4C:]) >= 1 { // dynamic revision
		f.inodeSize = int64(le.Uint16(sb[0x58:]))
	}
	if f.inodeSize < 128 || f.inodeSize > f.blockSize || f.inodeSize&(f.inodeSize-1) != 0 {
		return nil, corrupt("inode size %d", f.inodeSize)
	}
	if f.perGroup == 0 || perGroupBlocks == 0 || f.inodes == 0 {
		return nil, corrupt("empty geometry")
	}
	if uint64(f.blocks)*uint64(f.blockSize) > uint64(size) {
		// A grown image may be truncated by a sparse copy; reads past
		// the end fail individually instead.
		f.blocks = uint64(size / f.blockSize)
	}
	f.groups = (f.inodes + f.perGroup - 1) / f.perGroup
	f.gdtBlock = uint64(le.Uint32(sb[0x14:])) + 1 // after the superblock's block
	return f, nil
}

// NeedsRecovery reports an image whose journal was not replayed (a crash
// copy): it is read as it is on disk, so recent changes may be missing.
func (f *FS) NeedsRecovery() bool { return f.needsRecov }

// BlockSize is the filesystem block size in bytes.
func (f *FS) BlockSize() int64 { return f.blockSize }

// readBlocks reads n bytes at block blk, refusing blocks outside the image.
func (f *FS) readAt(p []byte, blk uint64, off int64) error {
	if blk >= f.blocks || off < 0 || off+int64(len(p)) > f.blockSize*int64(f.blocks-blk) {
		return corrupt("block %d outside the filesystem", blk)
	}
	_, err := f.r.ReadAt(p, int64(blk)*f.blockSize+off)
	if errors.Is(err, io.EOF) {
		return corrupt("block %d past the end of the image", blk)
	}
	return err
}

// FileType is an inode's type.
type FileType uint8

const (
	TypeOther FileType = iota
	TypeFile
	TypeDir
	TypeSymlink
)

func (t FileType) String() string {
	return [...]string{"other", "file", "dir", "symlink"}[t]
}

// Inode is one inode's metadata.
type Inode struct {
	Ino     uint32
	Mode    uint16 // type and permission bits, as st_mode
	UID     uint32
	GID     uint32
	Size    int64
	Links   uint16
	ModTime time.Time
	flags   uint32
	block   [60]byte
}

// Type reports the inode's file type.
func (in *Inode) Type() FileType {
	switch in.Mode & 0xF000 {
	case 0x8000:
		return TypeFile
	case 0x4000:
		return TypeDir
	case 0xA000:
		return TypeSymlink
	}
	return TypeOther
}

// FileMode converts Mode to an fs.FileMode.
func (in *Inode) FileMode() fs.FileMode {
	m := fs.FileMode(in.Mode & 0o777)
	if in.Mode&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if in.Mode&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if in.Mode&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	switch in.Mode & 0xF000 {
	case 0x4000:
		m |= fs.ModeDir
	case 0xA000:
		m |= fs.ModeSymlink
	case 0x2000:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case 0x6000:
		m |= fs.ModeDevice
	case 0x1000:
		m |= fs.ModeNamedPipe
	case 0xC000:
		m |= fs.ModeSocket
	}
	return m
}

// Inode reads inode ino.
func (f *FS) Inode(ino uint32) (*Inode, error) {
	if ino == 0 || ino > f.inodes {
		return nil, corrupt("inode %d out of range", ino)
	}
	group := (ino - 1) / f.perGroup
	index := int64((ino - 1) % f.perGroup)
	// The group descriptor, read where it lies (a block at a time).
	descOff := int64(group) * f.descSize
	desc := make([]byte, f.descSize)
	if err := f.readAt(desc, f.gdtBlock+uint64(descOff/f.blockSize), descOff%f.blockSize); err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	table := uint64(le.Uint32(desc[0x08:]))
	if f.descSize >= 64 {
		table |= uint64(le.Uint32(desc[0x28:])) << 32
	}
	raw := make([]byte, f.inodeSize)
	off := index * f.inodeSize
	if err := f.readAt(raw, table+uint64(off/f.blockSize), off%f.blockSize); err != nil {
		return nil, err
	}
	in := &Inode{
		Ino:   ino,
		Mode:  le.Uint16(raw[0x00:]),
		UID:   uint32(le.Uint16(raw[0x02:])) | uint32(le.Uint16(raw[0x78:]))<<16,
		GID:   uint32(le.Uint16(raw[0x18:])) | uint32(le.Uint16(raw[0x7A:]))<<16,
		Size:  int64(uint64(le.Uint32(raw[0x04:])) | uint64(le.Uint32(raw[0x6C:]))<<32),
		Links: le.Uint16(raw[0x1A:]),
		flags: le.Uint32(raw[0x20:]),
	}
	if in.Size < 0 {
		return nil, corrupt("inode %d size", ino)
	}
	copy(in.block[:], raw[0x28:0x28+60])
	sec, nsec := int64(int32(le.Uint32(raw[0x10:]))), int64(0)
	if f.inodeSize > 128 {
		if extra := int64(le.Uint16(raw[0x80:])); extra >= 0x0C && 0x88+4 <= f.inodeSize {
			x := le.Uint32(raw[0x88:])
			sec += int64(x&3) << 32
			nsec = int64(x >> 2)
		}
	}
	in.ModTime = time.Unix(sec, nsec).UTC()
	if in.Mode == 0 {
		return nil, fmt.Errorf("inode %d: %w", ino, fs.ErrNotExist)
	}
	return in, nil
}

// checkData refuses inodes whose data this reader cannot interpret.
func (in *Inode) checkData() error {
	switch {
	case in.flags&flagInlineData != 0:
		return fmt.Errorf("%w: inline data (inode %d)", ErrUnsupported, in.Ino)
	case in.flags&flagEncrypt != 0:
		return fmt.Errorf("%w: encrypted inode %d", ErrUnsupported, in.Ino)
	}
	return nil
}

// Readlink returns a symlink's target.
func (f *FS) Readlink(in *Inode) (string, error) {
	if in.Type() != TypeSymlink {
		return "", fmt.Errorf("inode %d is not a symlink", in.Ino)
	}
	if in.Size > 4096 {
		return "", corrupt("symlink %d of %d bytes", in.Ino, in.Size)
	}
	if err := in.checkData(); err != nil {
		return "", err
	}
	// A fast symlink keeps its target in the block map area.
	if in.flags&flagExtents == 0 && in.Size < 60 {
		return string(in.block[:in.Size]), nil
	}
	b, err := io.ReadAll(f.NewReader(in))
	return string(b), err
}
