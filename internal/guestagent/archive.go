//go:build linux

package guestagent

import (
	"errors"
	"io"
	"io/fs"
	"os"

	"davit/internal/guestproto"
	"davit/internal/tarx"
)

// Directory transfer (ReadFile/WriteFile with Archive "tar"). Both sides
// run entirely as the requested user on one credential-switched thread, so
// the kernel scopes every walk, open and create to that user.

// readArchive streams a tar of req.Path: FileReadResult (Size -1), then
// FileChunk frames, the last with EOF.
func (s *session) readArchive(id uint64, req guestproto.ReadFile) {
	pr, pw := io.Pipe()
	ready := make(chan error, 1)
	var rootMode fs.FileMode
	go func() {
		err := asUser(req.User, func() error {
			fi, err := os.Lstat(req.Path)
			if err == nil && !fi.IsDir() && !fi.Mode().IsRegular() {
				err = guestproto.Errorf(guestproto.CodeInvalidArgument, "%s is not a directory or regular file", req.Path)
			}
			if err != nil {
				ready <- err
				return nil
			}
			rootMode = fi.Mode().Perm()
			ready <- nil
			return tarx.Write(pw, req.Path, fi)
		})
		pw.CloseWithError(err)
	}()
	if err := <-ready; err != nil {
		s.fail(id, fsError(req.Path, err))
		return
	}
	s.send(id, guestproto.FileReadResult{Size: -1, Mode: uint32(rootMode)})
	buf := make([]byte, fileChunk)
	var seq uint64
	for {
		n, err := io.ReadFull(pr, buf)
		eof := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
		if err != nil && !eof {
			s.a.cfg.Log.Warn("read archive", "err", err)
			pr.CloseWithError(err)
			s.close() // the host sees a stream without EOF
			return
		}
		seq++
		fr, _ := guestproto.EncodeStream(id, seq, guestproto.FileChunk{Data: buf[:n], EOF: eof})
		if !s.sendStream(fr) {
			pr.CloseWithError(errors.New("session closed"))
			return
		}
		if eof {
			return
		}
	}
}

// commitArchive extracts a verified tar upload into w.req.Path.
func (s *session) commitArchive(id uint64, w *fileWrite) {
	if _, err := w.tmp.Seek(0, io.SeekStart); err != nil {
		s.fail(id, fsError(w.req.Path, err))
		return
	}
	mode := fs.FileMode(w.req.Mode).Perm()
	if mode == 0 {
		mode = 0o755
	}
	src := w.tmp
	if err := asUser(w.req.User, func() error { return tarx.Extract(src, w.req.Path, mode) }); err != nil {
		s.fail(id, fsError(w.req.Path, err))
		return
	}
	w.abort() // remove the upload before replying
	s.send(id, guestproto.FileWriteResult{Size: w.size})
}
