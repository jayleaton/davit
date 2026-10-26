package guestexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestproto"
	"davit/internal/lifecycle"
)

// FileRequest addresses one transfer.
type FileRequest struct {
	Principal  domain.PrincipalID
	VM         string // ID or name
	Path       string
	User       string // "" = image default
	Generation int64  // must match the VM's current instance
	Archive    bool   // tar of a directory
}

// FileHead is what a read reports before its data.
type FileHead struct {
	Size int64 // -1 when unknown (archives)
	Mode uint32
}

// FormatMode renders permission bits as e.g. "0644".
func FormatMode(m uint32) string {
	s := strconv.FormatUint(uint64(m&0o7777), 8)
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

func (m *Manager) fileTarget(ctx context.Context, r FileRequest) (lifecycle.GuestTarget, error) {
	if err := guestproto.ValidGuestPath(r.Path); err != nil {
		return lifecycle.GuestTarget{}, domain.Errorf(domain.CodeInvalidArgument, "path: %s", protoMessage(err)).WithDetail("field", "path")
	}
	t, err := m.cfg.Targets.GuestTarget(ctx, r.Principal, r.VM)
	if err != nil {
		return t, err
	}
	if r.Generation != t.Key.Generation {
		return t, domain.Errorf(domain.CodeInvalidState, "vm %s is at generation %d, not %d", t.Key.VMID, t.Key.Generation, r.Generation).
			WithDetail("generation", t.Key.Generation)
	}
	return t, nil
}

func protoMessage(err error) string {
	if pe, ok := err.(*guestproto.Error); ok {
		return pe.Message
	}
	return err.Error()
}

// ReadFile streams r's file (or archive) into the writer open returns;
// open runs once, after the guest accepted the read and before any data.
func (m *Manager) ReadFile(ctx context.Context, r FileRequest, open func(FileHead) (io.Writer, error)) error {
	t, err := m.fileTarget(ctx, r)
	if err != nil {
		return err
	}
	c, err := m.session(ctx, t)
	if err != nil {
		return err
	}
	req := guestproto.ReadFile{Path: r.Path, User: r.User}
	if r.Archive {
		req.Archive = guestproto.ArchiveTar
	}
	_, err = c.ReadTo(ctx, req, func(res guestproto.FileReadResult) (io.Writer, error) {
		return open(FileHead{Size: res.Size, Mode: res.Mode})
	})
	if err != nil {
		return protoErr(err)
	}
	return nil
}

// WriteFile streams body into r's path (or extracts it, for archives).
// mode 0 keeps an existing file's mode (else 0644), or creates an archive
// directory 0755.
func (m *Manager) WriteFile(ctx context.Context, r FileRequest, mode uint32, body io.Reader) (api.FileInfo, error) {
	t, err := m.fileTarget(ctx, r)
	if err != nil {
		return api.FileInfo{}, err
	}
	c, err := m.session(ctx, t)
	if err != nil {
		return api.FileInfo{}, err
	}
	h := sha256.New()
	tee := io.TeeReader(body, h)
	var n int64
	if r.Archive {
		n, err = c.WriteArchive(ctx, r.Path, r.User, mode, tee)
	} else {
		n, err = c.WriteFile(ctx, r.Path, r.User, mode, tee)
	}
	if err != nil {
		return api.FileInfo{}, protoErr(err)
	}
	info := api.FileInfo{Path: r.Path, SizeBytes: n, SHA256: hex.EncodeToString(h.Sum(nil)), Generation: t.Key.Generation, Archive: r.Archive}
	switch {
	case r.Archive && mode == 0:
		info.Mode = FormatMode(0o755)
	case r.Archive:
		info.Mode = FormatMode(mode)
	default:
		// Reading zero bytes at the end reports the mode actually on disk.
		res, err := c.ReadFile(ctx, r.Path, r.User, n, 0, io.Discard)
		if err != nil {
			return api.FileInfo{}, protoErr(err)
		}
		info.Mode = FormatMode(res.Mode)
	}
	return info, nil
}
