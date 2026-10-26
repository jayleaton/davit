// Package push is the client-side seam of git-aware upload (G6, parity v1
// contract): a Selector picks the files of a local tree, and the CLI tars
// them to pushTree. A git-aware selector returns tracked files plus
// untracked files that are not ignored, recursing into submodules, and
// never .git itself; that keeps build output and ignored secrets (.env)
// out of the VM. Owner: CLI/API.
package push

import (
	"context"
	"io/fs"
	"path"
	"strings"

	"davit/internal/domain"
)

// Entry is one file of a selection, relative to the tree root.
type Entry struct {
	Path       string // slash-separated, relative, clean
	Mode       fs.FileMode
	Size       int64
	LinkTarget string // symlinks only
}

// Selector picks the entries of the tree at root to push.
type Selector interface {
	Select(ctx context.Context, root string) ([]Entry, error)
}

// Static is a fake Selector returning fixed entries.
type Static []Entry

// Select implements Selector.
func (s Static) Select(context.Context, string) ([]Entry, error) { return Validate(s) }

// Validate checks a selection: relative clean unique paths, no path
// through a .git directory. Every Selector's output goes through it.
func Validate(es []Entry) ([]Entry, error) {
	seen := map[string]bool{}
	for _, e := range es {
		p := e.Path
		if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || seen[p] {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "push: bad or duplicate path %q", p)
		}
		for _, part := range strings.Split(p, "/") {
			if part == ".git" {
				return nil, domain.Errorf(domain.CodeInvalidArgument, "push: %q is inside .git", p)
			}
		}
		if e.LinkTarget != "" && e.Mode&fs.ModeSymlink == 0 {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "push: %q has a link target but is not a symlink", p)
		}
		seen[p] = true
	}
	return es, nil
}
