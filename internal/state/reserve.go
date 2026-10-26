package state

import (
	"errors"
	"math/rand/v2"
	"os"
	"strings"
	"sync"

	"modernc.org/sqlite"

	"davit/internal/domain"
)

// ReserveBytes is the size of the space reserve kept next to the database:
// enough for many journal transactions (terminal results, failure records)
// after the host disk fills up.
const ReserveBytes = 8 << 20

// reserve is a file of incompressible bytes (so compressing or
// deduplicating filesystems really set the space aside). Releasing it frees
// space the journal can use to finish recording operations.
type reserve struct {
	path string
	size int64
	mu   sync.Mutex
}

func (r *reserve) arm() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fi, err := os.Stat(r.path); err == nil && fi.Size() == r.size {
		return nil
	}
	tmp := r.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	rng := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	for written := int64(0); written < r.size && err == nil; written += int64(len(buf)) {
		for i := 0; i < len(buf); i += 8 {
			v := rng.Uint64()
			for j := 0; j < 8; j++ {
				buf[i+j] = byte(v >> (8 * j))
			}
		}
		_, err = f.Write(buf[:min(int64(len(buf)), r.size-written)])
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, r.path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// release deletes the reserve, reporting whether it existed.
func (r *reserve) release() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return os.Remove(r.path) == nil
}

func (r *reserve) armed() bool {
	fi, err := os.Stat(r.path)
	return err == nil && fi.Size() == r.size
}

// ReserveArmed reports whether the space reserve is in place. It is false
// after a disk-full event released it, until Rearm succeeds.
func (d *DB) ReserveArmed() bool { return d.reserve.armed() }

// Rearm recreates the space reserve if it was released; call it once free
// space is back above the admission floor.
func (d *DB) Rearm() error {
	if d.reserve.armed() {
		return nil
	}
	return d.reserve.arm()
}

// InjectWriteFault makes every journal write call f first and fail with
// its error. It exists for fault-injection tests (for example a disk that
// fills up); production code never sets it. Pass nil to clear.
func (d *DB) InjectWriteFault(f func() error) {
	if f == nil {
		d.fault.Store(nil)
		return
	}
	d.fault.Store(&f)
}

// sqliteFull is SQLITE_FULL ("database or disk is full").
const sqliteFull = 13

func isDiskFull(err error) bool {
	if err == nil {
		return false
	}
	if domain.IsDiskFull(err) {
		return true
	}
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code()&0xff == sqliteFull {
		return true
	}
	return strings.Contains(err.Error(), "database or disk is full")
}
