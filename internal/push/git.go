package push

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"davit/internal/domain"
)

// Git selects what a commit of the work tree could hold: tracked files plus
// untracked files that are not ignored (`git ls-files --cached --others
// --exclude-standard`), recursing into checked-out submodules with their
// paths prefixed, and never .git. Modes come from the filesystem; symlinks
// stay symlinks; tracked files deleted from the work tree are left out.
type Git struct {
	// Bin is the git executable (default "git" from PATH).
	Bin string
}

// Select implements Selector. root may be a subdirectory of a work tree:
// paths are relative to it.
func (g Git) Select(ctx context.Context, root string) ([]Entry, error) {
	if _, err := g.git(ctx, root, "rev-parse", "--is-inside-work-tree"); err != nil {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "push: %s is not in a git work tree: %v", root, err)
	}
	es, err := g.selectTree(ctx, root, "")
	if err != nil {
		return nil, err
	}
	return Validate(es)
}

func (g Git) selectTree(ctx context.Context, dir, prefix string) ([]Entry, error) {
	// Submodules are gitlinks (stage mode 160000) in the index.
	staged, err := g.git(ctx, dir, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	subs := map[string]bool{}
	for _, rec := range splitZ(staged) {
		meta, p, ok := strings.Cut(rec, "\t")
		if ok && strings.HasPrefix(meta, "160000 ") {
			subs[p] = true
		}
	}
	files, err := g.git(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	var out []Entry
	seen := map[string]bool{}
	for _, p := range splitZ(files) {
		// Unmerged paths are listed once per stage; an untracked nested
		// repository is listed as "dir/" and is not part of this tree.
		if seen[p] || strings.HasSuffix(p, "/") {
			continue
		}
		seen[p] = true
		full := filepath.Join(dir, filepath.FromSlash(p))
		if subs[p] {
			if _, err := os.Lstat(filepath.Join(full, ".git")); err != nil {
				continue // not checked out: nothing to push
			}
			es, err := g.selectTree(ctx, full, prefix+p+"/")
			if err != nil {
				return nil, fmt.Errorf("submodule %s: %w", prefix+p, err)
			}
			out = append(out, es...)
			continue
		}
		e, ok, err := entryOf(full, prefix+p)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// entryOf describes the file at full as name; ok is false for paths that
// are gone or are not regular files or symlinks.
func entryOf(full, name string) (Entry, bool, error) {
	fi, err := os.Lstat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	e := Entry{Path: name, Mode: fi.Mode(), Size: fi.Size()}
	switch {
	case fi.Mode().IsRegular():
	case fi.Mode()&fs.ModeSymlink != 0:
		if e.LinkTarget, err = os.Readlink(full); err != nil {
			return Entry{}, false, err
		}
		e.Size = 0
	default:
		return Entry{}, false, nil
	}
	return e, true, nil
}

func (g Git) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"-C", dir}, args...)...)
	cmd.Env = childEnv(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("git %s: %s", args[0], msg)
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return out, nil
}

// repoLocatingEnv are the variables that tell git which repository, index
// or object store to use, or where discovery stops. Inherited from a caller
// (a git hook, an editor, a script inside another checkout) they would point
// every command at that repository instead of the tree being pushed.
var repoLocatingEnv = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_CEILING_DIRECTORIES"}

// childEnv is env without repoLocatingEnv: git finds the repository from
// the -C directory alone.
func childEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(repoLocatingEnv, k) {
			out = append(out, kv)
		}
	}
	return out
}

func splitZ(b []byte) []string {
	var out []string
	for _, s := range strings.Split(string(b), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// WriteTar writes the selected entries of root as the tar stream pushTree
// takes: regular files and symlinks under their relative names, permission
// bits and mtimes, no owners (directories are created by the receiver). A
// file whose type changed since selection is an error. It returns the
// number of entries written.
func WriteTar(w io.Writer, root string, es []Entry) (int, error) {
	if _, err := Validate(es); err != nil {
		return 0, err
	}
	tw := tar.NewWriter(w)
	n := 0
	for _, e := range es {
		full := filepath.Join(root, filepath.FromSlash(e.Path))
		fi, err := os.Lstat(full)
		if err != nil {
			return n, err
		}
		var link string
		switch {
		case e.Mode&fs.ModeSymlink != 0 && fi.Mode()&fs.ModeSymlink != 0:
			if link, err = os.Readlink(full); err != nil {
				return n, err
			}
		case e.Mode.IsRegular() && fi.Mode().IsRegular():
		default:
			return n, fmt.Errorf("push: %s changed type during the push", e.Path)
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return n, err
		}
		hdr.Name, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = path.Clean(e.Path), 0, 0, "", ""
		hdr.Format = tar.FormatPAX
		if err := tw.WriteHeader(hdr); err != nil {
			return n, err
		}
		if fi.Mode().IsRegular() {
			if err := copyFile(tw, full, hdr.Size); err != nil {
				return n, fmt.Errorf("push: %s: %w", e.Path, err)
			}
		}
		n++
	}
	return n, tw.Close()
}

func copyFile(w io.Writer, full string, size int64) error {
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.CopyN(w, f, size); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("file shrank during the push")
		}
		return err
	}
	return nil
}
