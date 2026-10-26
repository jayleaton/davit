package harness

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestexec"
	"davit/internal/lifecycle"
)

// Attachment limits. Inline data is also bounded by the request size
// limit.
const (
	MaxAttachments = 32
	// MaxImageBytes bounds a guest image read to pass inline to a harness
	// that takes images as bytes (Claude Code).
	MaxImageBytes = 20 << 20
)

// attachment is a checked PromptAttachment: Path is a guest file, or Data
// the decoded inline bytes.
type attachment struct {
	Name      string
	MediaType string
	Path      string
	Data      []byte
}

// parseAttachments checks a request's attachments (before anything is
// recorded).
func parseAttachments(in []api.PromptAttachment) ([]attachment, error) {
	if len(in) > MaxAttachments {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "at most %d attachments", MaxAttachments).WithDetail("field", "attachments")
	}
	out := make([]attachment, 0, len(in))
	names := map[string]bool{}
	for i, a := range in {
		mt, _, _ := strings.Cut(a.MediaType, ";")
		at := attachment{Name: a.Name, MediaType: strings.ToLower(strings.TrimSpace(mt)), Path: a.Path}
		switch {
		case a.Name == "" || a.Name == "." || a.Name == ".." || strings.ContainsAny(a.Name, "/\\\x00"):
			return nil, fieldErr(i, "name must be a plain file name")
		case at.MediaType == "":
			return nil, fieldErr(i, "mediaType is required")
		case (a.Path == "") == (a.Data == ""):
			return nil, fieldErr(i, "set exactly one of path (a guest file) and data (inline base64)")
		case a.Path != "" && (!path.IsAbs(a.Path) || path.Clean(a.Path) != a.Path):
			return nil, fieldErr(i, "path must be a clean absolute guest path")
		case a.Data != "":
			b, err := base64.StdEncoding.DecodeString(a.Data)
			if err != nil || len(b) == 0 {
				return nil, fieldErr(i, "data must be non-empty standard base64")
			}
			if names[a.Name] {
				return nil, fieldErr(i, "inline attachment name %q is used twice", a.Name)
			}
			names[a.Name] = true
			at.Data = b
		}
		out = append(out, at)
	}
	return out, nil
}

// place puts a turn's attachments where its harness reads them. Inline
// data is written with the guest files API into a directory of its own
// (AttachmentDir/davit-<prompt>, one tar upload); path attachments stay
// where they are and must exist. Images go to the harness natively: as
// bytes in the message, or as image paths on its command line. Every
// attachment is also listed in the message text by guest path, so the
// harness can open the others with its own file tools.
func (s *Service) place(ctx context.Context, principal domain.PrincipalID, t lifecycle.GuestTarget, prompt string, ad Adapter,
	text string, atts []attachment) (Message, []string, error) {
	fr := guestexec.FileRequest{Principal: principal, VM: string(t.Key.VMID), Generation: t.Key.Generation}
	dir := path.Join(s.cfg.AttachmentDir, "davit-"+prompt)
	var upload bytes.Buffer
	tw := tar.NewWriter(&upload)
	paths := make([]string, len(atts))
	inline := false
	now := s.cfg.Now()
	for i, a := range atts {
		if a.Data == nil {
			paths[i] = a.Path
			continue
		}
		inline = true
		paths[i] = path.Join(dir, a.Name)
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: a.Name, Mode: 0o644, Size: int64(len(a.Data)), ModTime: now}); err != nil {
			return Message{}, nil, err
		}
		if _, err := tw.Write(a.Data); err != nil {
			return Message{}, nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return Message{}, nil, err
	}
	if inline {
		up := fr
		up.Path, up.Archive = dir, true
		if _, err := s.cfg.Procs.WriteFile(ctx, up, 0o755, &upload); err != nil {
			return Message{}, nil, fmt.Errorf("placing inline attachments in %s: %v", dir, err)
		}
	}
	msg := Message{Text: text + "\n\nAttached files (paths in this VM):"}
	var images []string
	for i, a := range atts {
		msg.Text += fmt.Sprintf("\n- %s (%s)", paths[i], a.MediaType)
		img := isImage(a.MediaType)
		switch {
		case img && ad.InlineImages():
			data := a.Data
			if data == nil {
				var err error
				if data, err = s.readGuest(ctx, fr, a.Path, MaxImageBytes); err != nil {
					return Message{}, nil, err
				}
			}
			msg.Images = append(msg.Images, Image{MediaType: a.MediaType, Data: data})
			continue
		case a.Data == nil:
			if _, err := s.readGuest(ctx, fr, a.Path, -1); err != nil {
				return Message{}, nil, err
			}
		}
		if img {
			images = append(images, paths[i])
		}
	}
	return msg, images, nil
}

var errSkip = errors.New("not read")

// readGuest reads guest file p (up to limit bytes; a negative limit only
// checks that it exists and is readable).
func (s *Service) readGuest(ctx context.Context, fr guestexec.FileRequest, p string, limit int64) ([]byte, error) {
	fr.Path = p
	var buf bytes.Buffer
	var size int64
	seen := false
	err := s.cfg.Procs.ReadFile(ctx, fr, func(h guestexec.FileHead) (io.Writer, error) {
		seen, size = true, h.Size
		if limit < 0 || h.Size > limit {
			return nil, errSkip
		}
		return &buf, nil
	})
	switch {
	case !seen:
		return nil, fmt.Errorf("attachment %s: %v", p, err)
	case limit >= 0 && size > limit:
		return nil, fmt.Errorf("attachment %s is %d bytes; at most %d can be passed inline to this harness", p, size, limit)
	case limit < 0:
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("attachment %s: %v", p, err)
	}
	return buf.Bytes(), nil
}
