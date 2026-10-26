package ext4

import (
	"encoding/binary"
	"io"
)

const (
	extentMagic    = 0xF30A
	maxExtentDepth = 5 // the kernel's limit
	uninitLen      = 32768
)

// run is a contiguous stretch of a file's logical blocks: mapped to physical
// blocks starting at phys, or a hole (zeros).
type run struct {
	phys  uint64
	count uint64
	hole  bool
}

// mapBlock maps logical block lblk of in to its run, which starts at lblk
// and is at least one block long. A block past every mapping is a hole that
// extends to limit (the block count of the file).
func (f *FS) mapBlock(in *Inode, lblk, limit uint64) (run, error) {
	if in.flags&flagExtents != 0 {
		return f.mapExtent(in.block[:], lblk, limit, 0)
	}
	return f.mapIndirect(in, lblk)
}

// mapExtent walks an extent tree node (the inode's block area or a tree
// block) down to the leaf covering lblk.
func (f *FS) mapExtent(node []byte, lblk, limit uint64, level int) (run, error) {
	le := binary.LittleEndian
	if len(node) < 12 || le.Uint16(node[0:]) != extentMagic {
		return run{}, corrupt("bad extent header")
	}
	entries := int(le.Uint16(node[2:]))
	depth := int(le.Uint16(node[6:]))
	if 12+12*entries > len(node) || depth > maxExtentDepth || level > maxExtentDepth {
		return run{}, corrupt("bad extent node (%d entries, depth %d)", entries, depth)
	}
	if depth > 0 {
		// The last index whose first block is <= lblk.
		pick := -1
		for i := 0; i < entries; i++ {
			if uint64(le.Uint32(node[12+12*i:])) > lblk {
				break
			}
			pick = i
		}
		if pick < 0 { // before the first index: a hole up to it
			return holeTo(lblk, min(limit, uint64(le.Uint32(node[12:])))), nil
		}
		e := node[12+12*pick:]
		child := uint64(le.Uint32(e[4:])) | uint64(le.Uint16(e[8:]))<<32
		blk := make([]byte, f.blockSize)
		if err := f.readAt(blk, child, 0); err != nil {
			return run{}, err
		}
		if d := int(le.Uint16(blk[6:])); d != depth-1 {
			return run{}, corrupt("extent depth %d under depth %d", d, depth)
		}
		r, err := f.mapExtent(blk, lblk, limit, level+1)
		if err != nil {
			return r, err
		}
		// A hole found in this subtree ends where the next index begins.
		if r.hole && pick+1 < entries {
			if next := uint64(le.Uint32(node[12+12*(pick+1):])); next > lblk && lblk+r.count > next {
				r.count = next - lblk
			}
		}
		return r, nil
	}
	for i := 0; i < entries; i++ {
		e := node[12+12*i:]
		start := uint64(le.Uint32(e[0:]))
		n := uint64(le.Uint16(e[4:]))
		uninit := n > uninitLen
		if uninit {
			n -= uninitLen
		}
		if lblk < start {
			return run{hole: true, count: start - lblk}, nil
		}
		if lblk < start+n {
			phys := uint64(le.Uint32(e[8:])) | uint64(le.Uint16(e[6:]))<<32
			off := lblk - start
			if uninit {
				return run{hole: true, count: n - off}, nil
			}
			return run{phys: phys + off, count: n - off}, nil
		}
	}
	return holeTo(lblk, limit), nil
}

// holeTo is the hole from lblk to limit (at least one block).
func holeTo(lblk, limit uint64) run {
	if limit > lblk {
		return run{hole: true, count: limit - lblk}
	}
	return run{hole: true, count: 1}
}

// mapIndirect maps a block-mapped (ext2/3 style) file: 12 direct pointers,
// then single, double and triple indirect blocks. A zero pointer is a hole.
func (f *FS) mapIndirect(in *Inode, lblk uint64) (run, error) {
	le := binary.LittleEndian
	per := uint64(f.blockSize / 4)
	ptr := func(i int) uint64 { return uint64(le.Uint32(in.block[4*i:])) }
	var root uint64
	var depth int
	switch {
	case lblk < 12:
		return physOrHole(ptr(int(lblk))), nil
	case lblk < 12+per:
		root, depth, lblk = ptr(12), 1, lblk-12
	case lblk < 12+per+per*per:
		root, depth, lblk = ptr(13), 2, lblk-12-per
	case lblk < 12+per+per*per+per*per*per:
		root, depth, lblk = ptr(14), 3, lblk-12-per-per*per
	default:
		return run{}, corrupt("block %d beyond the block map", lblk)
	}
	blk := root
	var b [4]byte
	for d := depth; d > 0; d-- {
		if blk == 0 {
			return run{hole: true, count: 1}, nil
		}
		span := uint64(1)
		for i := 1; i < d; i++ {
			span *= per
		}
		idx := lblk / span
		lblk %= span
		if err := f.readAt(b[:], blk, int64(idx*4)); err != nil {
			return run{}, err
		}
		blk = uint64(le.Uint32(b[:]))
	}
	return physOrHole(blk), nil
}

func physOrHole(p uint64) run {
	if p == 0 {
		return run{hole: true, count: 1}
	}
	return run{phys: p, count: 1}
}

// Reader reads a file's data sequentially. Its memory is one fixed buffer
// no matter how large or sparse the file is.
type Reader struct {
	f    *FS
	in   *Inode
	off  int64
	err  error
	cur  run    // the run containing block cur.lblk
	lblk uint64 // first logical block of cur
}

// NewReader returns a reader of in's data (a file, a directory or a slow
// symlink).
func (f *FS) NewReader(in *Inode) *Reader {
	return &Reader{f: f, in: in, err: in.checkData()}
}

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.off >= r.in.Size {
		return 0, io.EOF
	}
	if rest := r.in.Size - r.off; int64(len(p)) > rest {
		p = p[:rest]
	}
	bs := r.f.blockSize
	lblk := uint64(r.off / bs)
	if r.cur.count == 0 || lblk < r.lblk || lblk >= r.lblk+r.cur.count {
		limit := uint64((r.in.Size + bs - 1) / bs)
		cur, err := r.f.mapBlock(r.in, lblk, limit)
		if err != nil {
			r.err = err
			return 0, err
		}
		r.cur, r.lblk = cur, lblk
	}
	// Within the current run, up to its end.
	end := int64(r.lblk+r.cur.count) * bs
	if n := end - r.off; int64(len(p)) > n {
		p = p[:n]
	}
	if r.cur.hole {
		clear(p)
	} else {
		phys := r.cur.phys + (lblk - r.lblk)
		if err := r.f.readAt(p, phys, r.off%bs); err != nil {
			r.err = err
			return 0, err
		}
	}
	r.off += int64(len(p))
	return len(p), nil
}
