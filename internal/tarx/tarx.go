// Package tarx writes and safely extracts the tar streams davit uses for
// directory transfer, on both the guest agent and the CLI. Archives hold
// regular files, directories and symlinks (stored, never followed), names
// relative to the root, permission bits and mtimes; owners are not
// recorded. Extraction rejects entries that are absolute, contain "..",
// pass through a symlink or non-directory, or have other types, and drops
// setuid/setgid/sticky bits.
package tarx

import (
	"archive/tar"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// EntryError is a rejected archive or entry.
type EntryError struct{ Msg string }

func (e *EntryError) Error() string { return "archive: " + e.Msg }

func bad(format string, args ...any) error { return &EntryError{fmt.Sprintf(format, args...)} }

// Write writes root (a directory's contents, or one file under its base
// name) as tar. Symlinks are stored, not followed; sockets, devices and
// FIFOs are skipped. Owner fields are left empty.
func Write(w io.Writer, root string, rootInfo fs.FileInfo) error {
	tw := tar.NewWriter(w)
	add := func(name, full string, fi fs.FileInfo) error {
		var link string
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			l, err := os.Readlink(full)
			if err != nil {
				return err
			}
			link = l
		case fi.IsDir(), fi.Mode().IsRegular():
		default:
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = name, 0, 0, "", ""
		if fi.IsDir() {
			hdr.Name += "/"
		}
		hdr.Format = tar.FormatPAX
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.CopyN(tw, f, hdr.Size)
		return err
	}
	if !rootInfo.IsDir() {
		if err := add(filepath.Base(root), root, rootInfo); err != nil {
			return err
		}
		return tw.Close()
	}
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if full == root {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		return add(filepath.ToSlash(rel), full, fi)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Extract unpacks r into dest (created with mode if missing). Files are
// replaced atomically one by one; directory modes are applied last so
// read-only directories can still be filled.
func Extract(r io.Reader, dest string, mode fs.FileMode) error {
	if err := os.MkdirAll(dest, mode); err != nil {
		return err
	}
	if fi, err := os.Lstat(dest); err != nil || !fi.IsDir() {
		return bad("destination %s is not a directory", dest)
	}
	type dirFix struct {
		path string
		hdr  *tar.Header
	}
	var dirs []dirFix
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return bad("%v", err)
		}
		name := path.Clean(strings.TrimSuffix(hdr.Name, "/"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return bad("entry %q escapes the destination", hdr.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		if err := checkParents(dest, target); err != nil {
			return err
		}
		perm := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			fi, err := os.Lstat(target)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				if err := os.Mkdir(target, 0o700); err != nil {
					return err
				}
			case err != nil:
				return err
			case !fi.IsDir():
				return bad("%s exists and is not a directory", name)
			}
			dirs = append(dirs, dirFix{target, hdr})
		case tar.TypeReg:
			if err := writeEntry(tr, target, perm, hdr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if fi, err := os.Lstat(target); err == nil {
				if fi.IsDir() {
					return bad("%s exists and is a directory", name)
				}
				if err := os.Remove(target); err != nil {
					return err
				}
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			return bad("entry %q has unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}
	for _, d := range slices.Backward(dirs) {
		if err := os.Chmod(d.path, fs.FileMode(d.hdr.Mode).Perm()); err != nil {
			return err
		}
		_ = os.Chtimes(d.path, d.hdr.ModTime, d.hdr.ModTime)
	}
	return nil
}

// checkParents rejects a target whose existing parents (below dest) are
// not real directories, so an archive cannot write through a symlink, and
// creates missing ones.
func checkParents(dest, target string) error {
	rel, err := filepath.Rel(dest, filepath.Dir(target))
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	cur := dest
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			// Archives need not list every directory; create it plainly.
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return bad("%s goes through a non-directory", target)
		}
	}
	return nil
}

func writeEntry(r io.Reader, target string, perm fs.FileMode, hdr *tar.Header) error {
	if fi, err := os.Lstat(target); err == nil && fi.IsDir() {
		return bad("%s exists and is a directory", target)
	}
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(filepath.Dir(target), ".davit-"+hex.EncodeToString(rnd[:])+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	_ = os.Chtimes(tmp, hdr.ModTime, hdr.ModTime)
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(target), err)
	}
	return nil
}
