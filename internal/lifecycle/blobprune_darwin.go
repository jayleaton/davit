//go:build darwin

package lifecycle

import (
	"io/fs"
	"syscall"
	"time"
)

// changedAt is the later of fi's modification and inode change times: a
// rename into the blob store (install.sh) updates the change time even when
// the file keeps its archive's modification time.
func changedAt(fi fs.FileInfo) time.Time {
	t := fi.ModTime()
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if c := time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec); c.After(t) {
			t = c
		}
	}
	return t
}
