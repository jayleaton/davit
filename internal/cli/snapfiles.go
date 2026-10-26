package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/tarx"
)

// Snapshot files (G12): read a snapshot's filesystem without booting it.
//
//	snapshot tree SNAPSHOT [PATH] [--all] [--limit N] [--cursor C]
//	snapshot pull SNAPSHOT PATH [DEST]
//	snapshot export SNAPSHOT --output FILE [--format tar|raw]

type snapFileOpts struct {
	all    bool
	output string
	format string
}

// snapshotFileFlags are the snapshot command's flags for tree/pull/export.
func snapshotFileFlags(fs *flag.FlagSet, o *opts) {
	fs.IntVar(&o.limit, "limit", 0, "tree: page size (max 200)")
	fs.StringVar(&o.cursor, "cursor", "", "tree: page cursor")
	fs.BoolVar(&o.snapFiles.all, "all", false, "tree: every page")
	fs.StringVar(&o.snapFiles.output, "output", "", "export: file to write")
	fs.StringVar(&o.snapFiles.output, "o", "", "same as --output")
	fs.StringVar(&o.snapFiles.format, "format", api.ExportTar, "export: tar (the filesystem) or raw (the disk image)")
}

func snapFilesPath(ref, op string) string { return "/snapshots/" + url.PathEscape(ref) + "/" + op }

// snapPath makes a snapshot path absolute (paths are inside the snapshot).
func snapPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

func runSnapshotTree(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return usageErr("usage: %s snapshot tree SNAPSHOT [PATH] [--all] [--limit N] [--cursor C]", buildinfo.Name)
	}
	p := "/"
	if len(args) == 2 {
		p = snapPath(args[1])
	}
	q := url.Values{"path": {p}}
	if o.limit != 0 {
		q.Set("limit", strconv.Itoa(o.limit))
	}
	cursor, count := o.cursor, 0
	var tree api.SnapshotTree
	for {
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if err := s.cl.get(ctx, snapFilesPath(args[0], "tree"), q, &tree); err != nil {
			return err
		}
		for _, e := range tree.Items {
			s.out.emit("item", "", 0, e, nil)
			name := e.Path
			if e.LinkTarget != "" {
				name += " -> " + e.LinkTarget
			}
			s.out.human("%s %-7s %12s %s %s", e.Mode, e.Type, humanBytes(e.SizeBytes), e.ModifiedAt.Local().Format("2006-01-02 15:04"), name)
		}
		count += len(tree.Items)
		if cursor = tree.NextCursor; cursor == "" || !o.snapFiles.all {
			break
		}
	}
	s.out.result("", map[string]any{"path": tree.Path, "count": count, "nextCursor": tree.NextCursor})
	return nil
}

type pullResult struct {
	Snapshot    string `json:"snapshot"`
	Path        string `json:"path"`
	Destination string `json:"destination"`
	Bytes       int64  `json:"bytes"`
	Archive     bool   `json:"archive"`
	SHA256      string `json:"sha256,omitempty"`
}

// runSnapshotPull copies a file, or a directory (as cp -r does), out of a
// snapshot, verifying its SHA-256.
func runSnapshotPull(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return usageErr("usage: %s snapshot pull SNAPSHOT PATH [DEST]", buildinfo.Name)
	}
	ref, remote, dst := args[0], snapPath(args[1]), "."
	if len(args) == 3 {
		dst = args[2]
	}
	q := url.Values{"path": {remote}}
	resp, err := s.cl.stream(ctx, http.MethodGet, snapFilesPath(ref, "files"), q, nil, "")
	archive := false
	if e, ok := err.(*cliError); ok && e.Code == domain.CodeInvalidArgument && e.Details["field"] == "archive" {
		// A directory: pull it as an archive.
		archive = true
		q.Set("archive", api.ArchiveTar)
		resp, err = s.cl.stream(ctx, http.MethodGet, snapFilesPath(ref, "files"), q, nil, "")
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	mode := fs.FileMode(0o644)
	if m, err := strconv.ParseUint(resp.Header.Get(api.HeaderFileMode), 8, 32); err == nil {
		mode = fs.FileMode(m).Perm()
	}
	name := path.Base(remote)
	if name == "/" {
		name = "root"
	}
	target := localTarget(dst, name)
	p := &progress{s: s, start: time.Now()}
	stop := p.run()
	h := sha256.New()
	body := io.TeeReader(p.reader(resp.Body), h)
	if archive {
		err = tarx.Extract(body, target, mode)
		if err != nil {
			err = clientErr(domain.CodeUnavailable, "extracting into %s: %v", target, err)
		} else {
			_, err = io.Copy(io.Discard, body) // to the end, where the trailer is
		}
	} else {
		err = writeLocal(target, mode, body, resp.ContentLength)
	}
	stop()
	if err != nil {
		return err
	}
	sum, err := checkDigest(resp, h, target)
	if err != nil {
		if !archive {
			os.Remove(target)
		}
		return err
	}
	if sum == "" {
		s.out.diag("warning: no digest came with the transfer; it was not verified")
	}
	res := pullResult{Snapshot: ref, Path: remote, Destination: target, Bytes: p.n.Load(), Archive: archive, SHA256: sum}
	s.out.result("", res)
	s.out.diag("pulled %s:%s to %s (%s in %s)", ref, remote, target, humanBytes(res.Bytes), time.Since(p.start).Round(time.Millisecond))
	return nil
}

// checkDigest compares what was received with the server's SHA-256 (a
// header for a file, a trailer for a stream). It returns the digest, or ""
// if the server sent none.
func checkDigest(resp *http.Response, h hash.Hash, what string) (string, error) {
	want := resp.Header.Get(api.HeaderContentSHA256)
	if want == "" {
		want = resp.Trailer.Get(api.HeaderContentSHA256)
	}
	if want == "" {
		return "", nil
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", clientErr(domain.CodeUnavailable, "%s: SHA-256 %s does not match the snapshot's %s; the transfer is corrupt", what, got, want)
	}
	return want, nil
}

type exportResult struct {
	Snapshot string `json:"snapshot"`
	Format   string `json:"format"`
	File     string `json:"file"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256,omitempty"`
}

// runSnapshotExport writes a snapshot's filesystem (tar) or disk image
// (raw, written sparse) to a file, atomically.
func runSnapshotExport(ctx context.Context, s *session, o *opts, args []string) error {
	f := o.snapFiles
	if len(args) != 1 || f.output == "" {
		return usageErr("usage: %s snapshot export SNAPSHOT --output FILE [--format tar|raw]", buildinfo.Name)
	}
	if f.format != api.ExportTar && f.format != api.ExportRaw {
		return usageErr("--format must be %s or %s", api.ExportTar, api.ExportRaw)
	}
	resp, err := s.cl.stream(ctx, http.MethodGet, snapFilesPath(args[0], "export"), url.Values{"format": {f.format}}, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tmp, err := os.CreateTemp(filepath.Dir(f.output), ".davit-export-*")
	if err != nil {
		return clientErr(domain.CodeInvalidArgument, "cannot write %s: %v", f.output, err)
	}
	defer os.Remove(tmp.Name())
	p := &progress{s: s, start: time.Now()}
	stop := p.run()
	h := sha256.New()
	body := io.TeeReader(p.reader(resp.Body), h)
	if f.format == api.ExportRaw {
		err = copySparse(tmp, body)
	} else {
		_, err = io.Copy(tmp, body)
	}
	stop()
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return clientErr(domain.CodeUnavailable, "export of %s failed: %v", args[0], err)
	}
	sum, err := checkDigest(resp, h, f.output)
	if err != nil {
		return err
	}
	if sum == "" {
		s.out.diag("warning: no digest came with the export; it was not verified")
	}
	if err := os.Rename(tmp.Name(), f.output); err != nil {
		return clientErr(domain.CodeInvalidArgument, "cannot write %s: %v", f.output, err)
	}
	res := exportResult{Snapshot: args[0], Format: f.format, File: f.output, Bytes: p.n.Load(), SHA256: sum}
	s.out.result("", res)
	s.out.diag("exported %s (%s) to %s (%s in %s)", args[0], f.format, f.output, humanBytes(res.Bytes), time.Since(p.start).Round(time.Millisecond))
	return nil
}

// copySparse writes r to dst, seeking over all-zero blocks so a disk image
// stays sparse, and sets the final length.
func copySparse(dst *os.File, r io.Reader) error {
	buf := make([]byte, 64<<10)
	var off int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if !allZero(buf[:n]) {
				if _, werr := dst.WriteAt(buf[:n], off); werr != nil {
					return werr
				}
			}
			off += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return dst.Truncate(off)
}

func allZero(b []byte) bool {
	var zero [4096]byte
	for len(b) > 0 {
		n := min(len(b), len(zero))
		if !bytes.Equal(b[:n], zero[:n]) {
			return false
		}
		b = b[n:]
	}
	return true
}

// snapshotFilesSub runs the tree/pull/export subcommands of snapshot; ok is
// false for any other subcommand.
func snapshotFilesSub(ctx context.Context, s *session, o *opts, sub string, rest []string) (handled bool, err error) {
	switch sub {
	case "tree":
		return true, runSnapshotTree(ctx, s, o, rest)
	case "pull":
		return true, runSnapshotPull(ctx, s, o, rest)
	case "export":
		return true, runSnapshotExport(ctx, s, o, rest)
	}
	return false, nil
}
