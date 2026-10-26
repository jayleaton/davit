package guestclient

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"

	"davit/internal/guestproto"
)

// ReadFile copies a guest file, read as user, into w and returns its size
// and mode as reported by the agent. limit 0 reads to end of file.
func (c *Client) ReadFile(ctx context.Context, path, user string, offset, limit int64, w io.Writer) (guestproto.FileReadResult, error) {
	return c.read(ctx, guestproto.ReadFile{Path: path, User: user, Offset: offset, Limit: limit}, w)
}

// ReadArchive writes a tar of path (a directory's contents, or one file)
// read as user into w. It needs FeatureFilesTar.
func (c *Client) ReadArchive(ctx context.Context, path, user string, w io.Writer) (guestproto.FileReadResult, error) {
	if !c.Has(guestproto.FeatureFilesTar) {
		return guestproto.FileReadResult{}, guestproto.Errorf(guestproto.CodeUnsupported, "guest agent does not support archives")
	}
	return c.read(ctx, guestproto.ReadFile{Path: path, User: user, Archive: guestproto.ArchiveTar}, w)
}

func (c *Client) read(ctx context.Context, req guestproto.ReadFile, w io.Writer) (guestproto.FileReadResult, error) {
	return c.ReadTo(ctx, req, func(guestproto.FileReadResult) (io.Writer, error) { return w, nil })
}

// ReadTo runs req and calls open with the agent's FileReadResult before
// any data arrives (e.g. to set response headers), then copies the data
// into the writer it returns.
func (c *Client) ReadTo(ctx context.Context, req guestproto.ReadFile, open func(guestproto.FileReadResult) (io.Writer, error)) (guestproto.FileReadResult, error) {
	if req.Archive != "" && !c.Has(guestproto.FeatureFilesTar) {
		return guestproto.FileReadResult{}, guestproto.Errorf(guestproto.CodeUnsupported, "guest agent does not support archives")
	}
	cl, err := c.start(req, true)
	if err != nil {
		return guestproto.FileReadResult{}, err
	}
	defer cl.release()
	f, err := cl.wait(ctx)
	if err != nil {
		return guestproto.FileReadResult{}, err
	}
	res, err := guestproto.Decode[guestproto.FileReadResult](f)
	if err != nil {
		return res, err
	}
	w, err := open(res)
	if err != nil {
		return res, err
	}
	var seqs guestproto.SeqTracker
	for {
		f, err := cl.next(ctx)
		if err != nil {
			return res, err
		}
		if dup, err := seqs.Accept(f); err != nil {
			return res, err
		} else if dup {
			continue
		}
		ch, err := guestproto.Decode[guestproto.FileChunk](f)
		if err != nil {
			return res, err
		}
		if err := write(w, ch.Data); err != nil {
			return res, err
		}
		if ch.EOF {
			return res, nil
		}
	}
}

// WriteFile atomically replaces a guest file with r's contents, written as
// user with permission bits mode (0 keeps an existing file's mode, else
// 0644). The agent renames into place only if size and SHA-256 match.
func (c *Client) WriteFile(ctx context.Context, path, user string, mode uint32, r io.Reader) (int64, error) {
	return c.write(ctx, guestproto.WriteFile{Path: path, User: user, Mode: mode}, r)
}

// WriteArchive extracts the tar stream r into directory path as user
// (created with mode, 0 meaning 0755). It needs FeatureFilesTar.
func (c *Client) WriteArchive(ctx context.Context, path, user string, mode uint32, r io.Reader) (int64, error) {
	if !c.Has(guestproto.FeatureFilesTar) {
		return 0, guestproto.Errorf(guestproto.CodeUnsupported, "guest agent does not support archives")
	}
	return c.write(ctx, guestproto.WriteFile{Path: path, User: user, Mode: mode, Archive: guestproto.ArchiveTar}, r)
}

func (c *Client) write(ctx context.Context, req guestproto.WriteFile, r io.Reader) (int64, error) {
	cl, err := c.start(req, false)
	if err != nil {
		return 0, err
	}
	f, err := cl.wait(ctx)
	cl.release()
	if err != nil {
		return 0, err
	}
	if _, err := guestproto.Decode[guestproto.Ack](f); err != nil {
		return 0, err
	}

	h := sha256.New()
	buf := make([]byte, 256<<10)
	var size int64
	var seq uint64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			size += int64(n)
			seq++
			fr, err := guestproto.EncodeStream(cl.id, seq, guestproto.FileChunk{Data: buf[:n]})
			if err != nil {
				return 0, err
			}
			fr.V = c.ack.Version
			if err := c.pc.WriteFrame(fr); err != nil {
				return 0, err
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			// Abandon: the agent discards the temp file when the session
			// closes or a commit never arrives; close to abort promptly.
			c.fail(ErrClosed)
			return 0, rerr
		}
		if err := ctx.Err(); err != nil {
			c.fail(ErrClosed)
			return 0, err
		}
	}

	commit, err := c.startOn(guestproto.FileCommit{Size: size, SHA256: h.Sum(nil)}, cl.id, false)
	if err != nil {
		return 0, err
	}
	defer commit.release()
	f, err = commit.wait(ctx)
	if err != nil {
		return 0, err
	}
	done, err := guestproto.Decode[guestproto.FileWriteResult](f)
	return done.Size, err
}
