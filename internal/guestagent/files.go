//go:build linux

package guestagent

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"

	"davit/internal/guestproto"
	"davit/internal/tarx"
)

// File RPCs run their filesystem calls as the requested guest user: the
// kernel checks permissions (including symlink traversal) with that user's
// fsuid/fsgid and supplementary groups. The credentials are switched on a
// dedicated, locked OS thread that is never unlocked, so Go discards it
// afterwards and no other goroutine ever runs with them. Opened descriptors
// are process-wide and are used from ordinary goroutines.

const fileChunk = 256 << 10

// fileUser resolves a request's user: empty means DefaultUser, and an
// empty DefaultUser means the agent's own credentials.
func (a *Agent) fileUser(u string) string {
	if u == "" {
		return a.cfg.DefaultUser
	}
	return u
}

// asUser runs fn on a fresh OS thread with u's filesystem credentials
// (the agent's own when name is empty).
func asUser(name string, fn func() error) error {
	if name == "" {
		done := make(chan error, 1)
		go func() { done <- fn() }()
		return <-done
	}
	u, err := user.Lookup(name)
	if err != nil {
		return guestproto.Errorf(guestproto.CodeNotFound, "unknown user %q", name)
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return guestproto.Errorf(guestproto.CodeInternal, "user %q has non-numeric ids", name)
	}
	var groups []int
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			if n, err := strconv.Atoi(g); err == nil {
				groups = append(groups, n)
			}
		}
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // never unlocked: the thread exits with us
		if os.Geteuid() != 0 {
			if uid != os.Geteuid() {
				done <- guestproto.Errorf(guestproto.CodeInvalidArgument, "agent is unprivileged and cannot act as %q", name)
				return
			}
			done <- fn()
			return
		}
		// Per-thread raw syscalls (x/sys/unix, not syscall.Set*, which apply
		// to every thread).
		if err := unix.Setgroups(groups); err != nil {
			done <- internal("setgroups", err)
			return
		}
		unix.Setfsgid(gid)
		unix.Setfsuid(uid)
		if cur, _ := unix.SetfsuidRetUid(-1); cur != uid {
			done <- guestproto.Errorf(guestproto.CodeInternal, "setfsuid did not take effect")
			return
		}
		done <- fn()
	}()
	return <-done
}

// fsError maps filesystem errors to protocol errors without leaking more
// than the guest path the host already knows.
func fsError(path string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return guestproto.Errorf(guestproto.CodeNotFound, "%s: no such file", path)
	case errors.Is(err, fs.ErrPermission):
		return guestproto.Errorf(guestproto.CodePermissionDenied, "%s: permission denied", path)
	}
	var pe *guestproto.Error
	if errors.As(err, &pe) {
		return pe
	}
	var ee *tarx.EntryError
	if errors.As(err, &ee) {
		return guestproto.Errorf(guestproto.CodeInvalidArgument, "%s", ee.Msg)
	}
	return guestproto.Errorf(guestproto.CodeInternal, "%s: %v", path, unwrapExecErr(err))
}

// readFile streams a file: FileReadResult, then FileChunk frames, the last
// with EOF.
func (s *session) readFile(id uint64, req guestproto.ReadFile) {
	req.User = s.a.fileUser(req.User)
	if req.Archive != "" {
		s.readArchive(id, req)
		return
	}
	var f *os.File
	err := asUser(req.User, func() (err error) {
		f, err = os.Open(req.Path)
		return err
	})
	if err != nil {
		s.fail(id, fsError(req.Path, err))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.fail(id, fsError(req.Path, err))
		return
	}
	if !fi.Mode().IsRegular() {
		s.fail(id, guestproto.Errorf(guestproto.CodeInvalidArgument, "%s is not a regular file", req.Path))
		return
	}
	s.send(id, guestproto.FileReadResult{Size: fi.Size(), Mode: uint32(fi.Mode().Perm())})

	var r io.Reader = io.NewSectionReader(f, req.Offset, 1<<62)
	if req.Limit > 0 {
		r = io.LimitReader(r, req.Limit)
	}
	buf := make([]byte, fileChunk)
	var seq uint64
	for {
		n, err := io.ReadFull(r, buf)
		eof := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if err != nil && !eof {
			// Mid-stream failure: end the stream; the host sees a short
			// read without EOF and the session error.
			s.a.cfg.Log.Warn("read file", "err", err)
			s.close()
			return
		}
		seq++
		fr, _ := guestproto.EncodeStream(id, seq, guestproto.FileChunk{Data: buf[:n], EOF: eof})
		if !s.sendStream(fr) {
			return
		}
		if eof {
			return
		}
	}
}

// fileWrite is an open WriteFile: data goes to a temp file in the target's
// directory until FileCommit renames it into place.
type fileWrite struct {
	req  guestproto.WriteFile
	tmp  *os.File
	size int64
	sum  hash.Hash
	err  error
}

func (w *fileWrite) abort() {
	if w.tmp != nil {
		name := w.tmp.Name()
		w.tmp.Close()
		os.Remove(name)
		w.tmp = nil
	}
}

func (s *session) writeFile(id uint64, req guestproto.WriteFile) {
	req.User = s.a.fileUser(req.User)
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmpPath := filepath.Join(filepath.Dir(req.Path), ".davit-"+hex.EncodeToString(rnd[:])+".tmp")
	var tmp *os.File
	err := asUser(req.User, func() (err error) {
		tmp, err = os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) // archives are read back at commit
		return err
	})
	if err != nil {
		s.fail(id, fsError(req.Path, err))
		return
	}
	s.mu.Lock()
	s.writes[id] = &fileWrite{req: req, tmp: tmp, sum: sha256.New()}
	s.mu.Unlock()
	s.send(id, guestproto.Ack{})
}

// chunk appends data to the write open on stream (reader goroutine).
func (s *session) chunk(stream uint64, c *guestproto.FileChunk) {
	// Hold mu across the write so commit (another goroutine) observes it.
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.writes[stream]
	if w == nil || w.err != nil || len(c.Data) == 0 {
		return
	}
	if _, err := w.tmp.Write(c.Data); err != nil {
		w.err = err
		return
	}
	w.sum.Write(c.Data)
	w.size += int64(len(c.Data))
}

// commit verifies and publishes the write on stream. It runs after every
// earlier chunk because the reader goroutine applied them before
// dispatching this request.
func (s *session) commit(id, stream uint64, req guestproto.FileCommit) {
	s.mu.Lock()
	w := s.writes[stream]
	delete(s.writes, stream)
	s.mu.Unlock()
	if w == nil {
		s.fail(id, guestproto.Errorf(guestproto.CodeNotFound, "no open write on stream %d", stream))
		return
	}
	defer w.abort()
	switch {
	case w.err != nil:
		s.fail(id, fsError(w.req.Path, w.err))
		return
	case w.size != req.Size:
		s.fail(id, guestproto.Errorf(guestproto.CodeInvalidArgument, "size %d does not match %d received", req.Size, w.size))
		return
	case req.SHA256 != nil && !bytes.Equal(req.SHA256, w.sum.Sum(nil)):
		s.fail(id, guestproto.Errorf(guestproto.CodeInvalidArgument, "sha256 mismatch"))
		return
	}
	if w.req.Archive != "" {
		s.commitArchive(id, w)
		return
	}
	mode := fs.FileMode(w.req.Mode)
	if mode == 0 {
		mode = 0o644
		if fi, err := os.Stat(w.req.Path); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	if err := w.tmp.Chmod(mode); err != nil {
		s.fail(id, fsError(w.req.Path, err))
		return
	}
	if err := w.tmp.Sync(); err != nil {
		s.fail(id, fsError(w.req.Path, err))
		return
	}
	tmpName := w.tmp.Name()
	if err := asUser(w.req.User, func() error { return os.Rename(tmpName, w.req.Path) }); err != nil {
		s.fail(id, fsError(w.req.Path, err))
		return
	}
	_ = syncDir(filepath.Dir(w.req.Path))
	w.tmp.Close()
	w.tmp = nil
	s.send(id, guestproto.FileWriteResult{Size: w.size})
}
