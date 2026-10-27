//go:build linux

package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"runtime"
	"slices"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// idleIO puts the calling goroutine's thread in the idle I/O class until
// the returned func runs (the thread stays locked to the goroutine
// meanwhile). Without permission or support it changes nothing.
func idleIO() func() {
	const whoProcess, classIdle = 1, 3 // IOPRIO_WHO_PROCESS (0 = this thread), IOPRIO_CLASS_IDLE
	runtime.LockOSThread()
	prev, _, e := unix.Syscall(unix.SYS_IOPRIO_GET, whoProcess, 0, 0)
	if e == 0 {
		_, _, e = unix.Syscall(unix.SYS_IOPRIO_SET, whoProcess, 0, classIdle<<13)
	}
	if e != 0 {
		runtime.UnlockOSThread()
		return func() {}
	}
	return func() {
		unix.Syscall(unix.SYS_IOPRIO_SET, whoProcess, 0, prev)
		runtime.UnlockOSThread()
	}
}

// dropCache evicts f's clean pages from the page cache, so a tier copy
// neither crowds out the guests' cache nor reads its own writes back from
// memory.
func dropCache(f *os.File) { _ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED) }

// punchHole deallocates [off, off+n) of f, keeping its size; it writes
// zeros where the filesystem cannot punch holes.
func punchHole(f *os.File, off, n int64) error {
	err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		_, err = f.WriteAt(make([]byte, n), off)
	}
	return err
}

// networkFS reports whether path is on a network or FUSE filesystem
// (NFS, SMB/CIFS, 9p, Ceph, FUSE), which the cold tier refuses: their
// fsync and rename durability depend on the server.
func networkFS(path string) bool {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return false
	}
	switch uint64(st.Type) {
	case unix.NFS_SUPER_MAGIC, unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC, unix.SMB_SUPER_MAGIC,
		unix.V9FS_MAGIC, unix.CEPH_SUPER_MAGIC, unix.FUSE_SUPER_MAGIC, unix.AFS_SUPER_MAGIC:
		return true
	}
	return false
}

func allocatedOf(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

// extent is a physical byte range of a file's data.
type extent struct{ start, end uint64 }

// deltaBytes is the length of data's extents that are not physically
// base's (FIEMAP; reflinked blocks have the same physical address). With
// no base, or without FIEMAP, it is data's allocated size.
func deltaBytes(ctx context.Context, data, base *os.File) (int64, error) {
	fi, err := data.Stat()
	if err != nil {
		return 0, err
	}
	if base == nil {
		return allocatedOf(fi), nil
	}
	mine, err := fiemap(ctx, data)
	if err != nil {
		return allocatedOf(fi), nil
	}
	theirs, err := fiemap(ctx, base)
	if err != nil {
		return allocatedOf(fi), nil
	}
	slices.SortFunc(theirs, func(a, b extent) int {
		switch {
		case a.start < b.start:
			return -1
		case a.start > b.start:
			return 1
		}
		return 0
	})
	var delta uint64
	for _, e := range mine {
		covered := uint64(0)
		// The first base extent that could overlap e.
		i, _ := slices.BinarySearchFunc(theirs, e.start, func(x extent, t uint64) int {
			if x.end <= t {
				return -1
			}
			return 1
		})
		for ; i < len(theirs) && theirs[i].start < e.end; i++ {
			lo, hi := max(e.start, theirs[i].start), min(e.end, theirs[i].end)
			if hi > lo {
				covered += hi - lo
			}
		}
		delta += (e.end - e.start) - min(covered, e.end-e.start)
	}
	return int64(delta), nil
}

// fiemap lists f's data extents by physical address (FS_IOC_FIEMAP).
// Extents without a stable address (delayed allocation, inline data) fail
// it: the caller then falls back to the allocated size.
func fiemap(ctx context.Context, f *os.File) ([]extent, error) {
	const (
		ioc      = 0xC020660B // FS_IOC_FIEMAP
		flagSync = 0x1        // FIEMAP_FLAG_SYNC
		hdr      = 32         // struct fiemap
		size     = 56         // struct fiemap_extent
		batch    = 512
		last     = 0x1   // FIEMAP_EXTENT_LAST
		unknown  = 0x2   // FIEMAP_EXTENT_UNKNOWN
		delalloc = 0x4   // FIEMAP_EXTENT_DELALLOC
		inline   = 0x200 // FIEMAP_EXTENT_DATA_INLINE
	)
	ne := binary.NativeEndian
	buf := make([]byte, hdr+batch*size)
	var out []extent
	for start := uint64(0); ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		clear(buf)
		ne.PutUint64(buf[0:], start)
		ne.PutUint64(buf[8:], ^uint64(0)-start)
		ne.PutUint32(buf[16:], flagSync)
		ne.PutUint32(buf[24:], batch)
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, f.Fd(), ioc, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
			return nil, e
		}
		n := int(ne.Uint32(buf[20:]))
		if n == 0 {
			return out, nil
		}
		for i := 0; i < n; i++ {
			e := buf[hdr+i*size:]
			logical, physical, length := ne.Uint64(e[0:]), ne.Uint64(e[8:]), ne.Uint64(e[16:])
			flags := ne.Uint32(e[40:])
			if flags&(unknown|delalloc|inline) != 0 {
				return nil, errors.ErrUnsupported
			}
			out = append(out, extent{physical, physical + length})
			start = logical + length
			if flags&last != 0 {
				return out, nil
			}
		}
	}
}
