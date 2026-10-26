package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

var (
	// ErrNotDir reports a path component that is not a directory.
	ErrNotDir = errors.New("not a directory")
	// ErrLoop reports too many symlinks while resolving a path.
	ErrLoop = errors.New("too many levels of symbolic links")
	// ErrBadOffset reports a directory offset that is not an entry boundary.
	ErrBadOffset = errors.New("ext4: not a directory entry offset")
)

// maxSymlinks bounds symlink hops in one resolution, as Linux does.
const maxSymlinks = 40

// DirEntry is one directory entry.
type DirEntry struct {
	Name string
	Ino  uint32
}

// ReadDir returns up to max entries of dir from byte offset off (0, or a
// next value returned earlier), skipping "." and "..". next is the offset of
// the following entry, or -1 at the end. Entries come in on-disk order.
func (f *FS) ReadDir(dir *Inode, off int64, max int) (out []DirEntry, next int64, err error) {
	next = -1
	err = f.walkDir(dir, off, func(at int64, e DirEntry) bool {
		if len(out) == max {
			next = at
			return false
		}
		out = append(out, e)
		return true
	})
	return out, next, err
}

// walkDir calls fn for each entry from offset from until fn returns false.
func (f *FS) walkDir(dir *Inode, from int64, fn func(at int64, e DirEntry) bool) error {
	if dir.Type() != TypeDir {
		return ErrNotDir
	}
	if err := dir.checkData(); err != nil {
		return err
	}
	if from < 0 || from > dir.Size {
		return ErrBadOffset
	}
	le := binary.LittleEndian
	bs := f.blockSize
	blk := make([]byte, bs)
	nblocks := uint64((dir.Size + bs - 1) / bs)
	for b := uint64(from / bs); b < nblocks; b++ {
		r, err := f.mapBlock(dir, b, nblocks)
		if err != nil {
			return err
		}
		if r.hole { // an unallocated directory block holds no entries
			if int64(b)*bs < from && from%bs != 0 {
				return ErrBadOffset
			}
			continue
		}
		if err := f.readAt(blk, r.phys, 0); err != nil {
			return err
		}
		base := int64(b) * bs
		matched := from <= base
		for pos := int64(0); pos < bs; {
			if pos+8 > bs {
				return corrupt("directory %d: truncated entry", dir.Ino)
			}
			ino := le.Uint32(blk[pos:])
			recLen := int64(le.Uint16(blk[pos+4:]))
			if bs == 65536 && recLen == 0 { // 64 KiB blocks encode 65536 as 0
				recLen = 65536
			}
			nameLen := int64(blk[pos+6])
			if !f.filetype {
				nameLen |= int64(blk[pos+7]) << 8
			}
			if recLen < 8 || recLen%4 != 0 || pos+recLen > bs || 8+nameLen > recLen {
				return corrupt("directory %d: bad entry at %d", dir.Ino, base+pos)
			}
			at := base + pos
			if at == from {
				matched = true
			}
			if matched && at >= from && ino != 0 {
				name := string(blk[pos+8 : pos+8+nameLen])
				switch {
				case name == "." || name == "..":
				case name == "" || strings.ContainsAny(name, "/\x00"):
					return corrupt("directory %d: invalid name %q", dir.Ino, name)
				case ino > f.inodes:
					return corrupt("directory %d: entry %q names inode %d", dir.Ino, name, ino)
				default:
					if !fn(at, DirEntry{Name: name, Ino: ino}) {
						return nil
					}
				}
			}
			pos += recLen
		}
		if !matched {
			return ErrBadOffset
		}
	}
	return nil
}

// Lookup finds name in dir.
func (f *FS) Lookup(dir *Inode, name string) (uint32, error) {
	var found uint32
	err := f.walkDir(dir, 0, func(_ int64, e DirEntry) bool {
		if e.Name == name {
			found = e.Ino
			return false
		}
		return true
	})
	if err != nil {
		return 0, err
	}
	if found == 0 {
		return 0, fs.ErrNotExist
	}
	return found, nil
}

// Resolve resolves p as if this filesystem were the root directory: ".." at
// the root stays there, and symlinks, relative or absolute, resolve inside
// the filesystem, never anywhere else. With follow, a symlink as the last
// component is followed too. It returns the inode and its canonical path
// (absolute, without symlinks, "." or "..").
func (f *FS) Resolve(p string, follow bool) (*Inode, string, error) {
	type comp struct {
		ino  uint32
		name string
	}
	var stack []comp // the directories from the root to the current one
	cur := func() uint32 {
		if len(stack) == 0 {
			return RootIno
		}
		return stack[len(stack)-1].ino
	}
	pathOf := func() string {
		names := make([]string, len(stack))
		for i, c := range stack {
			names[i] = c.name
		}
		return "/" + path.Join(names...)
	}
	todo := strings.Split(p, "/")
	hops := 0
	for len(todo) > 0 {
		name := todo[0]
		todo = todo[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		dir, err := f.Inode(cur())
		if err != nil {
			return nil, "", err
		}
		if dir.Type() != TypeDir {
			return nil, "", fmt.Errorf("%s: %w", pathOf(), ErrNotDir)
		}
		ino, err := f.Lookup(dir, name)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", path.Join(pathOf(), name), err)
		}
		in, err := f.Inode(ino)
		if err != nil {
			return nil, "", err
		}
		if in.Type() == TypeSymlink && (follow || hasMore(todo)) {
			if hops++; hops > maxSymlinks {
				return nil, "", fmt.Errorf("%s: %w", path.Join(pathOf(), name), ErrLoop)
			}
			target, err := f.Readlink(in)
			if err != nil {
				return nil, "", err
			}
			if strings.HasPrefix(target, "/") {
				stack = stack[:0]
			}
			todo = append(strings.Split(target, "/"), todo...)
			continue
		}
		stack = append(stack, comp{ino: ino, name: name})
	}
	in, err := f.Inode(cur())
	return in, pathOf(), err
}

// hasMore reports whether a path remainder names anything further.
func hasMore(rest []string) bool {
	for _, c := range rest {
		if c != "" && c != "." {
			return true
		}
	}
	return false
}
