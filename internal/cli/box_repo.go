package cli

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"davit/api"
	"davit/internal/buildinfo"
)

// Zero-config defaults from the git repository the CLI runs in: the
// project (the origin remote's repository name, else the main worktree's
// directory name), the instance (the branch), the tree to sync (the
// worktree's top level) and where in the box a command runs (the same
// subdirectory of the synced tree). git always runs without an inherited
// repository-locating variable (AGENTS.md), on the current directory.

var gitRepoLocatingEnv = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_CEILING_DIRECTORIES"}

// gitCmd is git in dir ("" for the current directory) without inherited
// repository-locating variables.
func gitCmd(dir string, args ...string) *exec.Cmd {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(gitRepoLocatingEnv, k)
	})
	return cmd
}

// gitIn runs git in dir and returns its trimmed output ("" on failure).
func gitIn(dir string, args ...string) string {
	out, err := gitCmd(dir, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitOut is gitIn in the current directory.
func gitOut(args ...string) string { return gitIn("", args...) }

// repoRoot is the current worktree's top level ("" outside a work tree).
func repoRoot() string { return gitOut("rev-parse", "--show-toplevel") }

var remoteNameRE = regexp.MustCompile(`([^/:]+?)(\.git)?/?$`)

// defaultProject names the project of the current directory: the origin
// remote's repository name, else the main worktree's directory (the same
// for every worktree of a repository), else the current directory's name.
func defaultProject() string {
	if u := gitOut("remote", "get-url", "origin"); u != "" {
		if m := remoteNameRE.FindStringSubmatch(u); m != nil && m[1] != "" {
			return m[1]
		}
	}
	if common := gitOut("rev-parse", "--path-format=absolute", "--git-common-dir"); common != "" {
		if filepath.Base(common) == ".git" {
			return filepath.Base(filepath.Dir(common))
		}
		return strings.TrimSuffix(filepath.Base(common), ".git") // a bare repository's worktree
	}
	wd, _ := os.Getwd()
	return filepath.Base(wd)
}

// defaultInstance is the current git worktree's branch, or its directory
// name on a detached HEAD, or "default" outside a work tree.
func defaultInstance() string {
	if b := gitOut("symbolic-ref", "-q", "--short", "HEAD"); b != "" {
		return b
	}
	if top := repoRoot(); top != "" {
		return filepath.Base(top)
	}
	return "default"
}

// defaultTree is what a box sync or image build pushes by default: the
// worktree's top level (to the box's work dir); "" outside a git work tree
// (a sync selects files with git, so there is nothing to push by default).
func defaultTree() string { return repoRoot() }

// BoxWorkdirEnv overrides where in a box the tree is synced (an image with
// another layout; tests on the fake runtime, whose "guest" is the host).
var BoxWorkdirEnv = buildinfo.Env("BOX_WORKDIR")

// boxWorkdir is the synced tree's place in box VM vm: $DAVIT_BOX_WORKDIR,
// else the guest's work dir (vmWorkdir).
func (s *session) boxWorkdir(ctx context.Context, vm string) string {
	if d := os.Getenv(BoxWorkdirEnv); path.IsAbs(d) {
		return path.Clean(d)
	}
	return s.vmWorkdir(ctx, vm)
}

// vmWorkdir is the default work dir in VM vm: work/ in the home of its
// image's default user, so a VM keeps the layout of the image it was made
// from (boxes made before the davit rename have /home/slipway). It is
// DefaultPushPath when the image does not say.
func (s *session) vmWorkdir(ctx context.Context, vm string) string {
	if d, ok := s.workdirs[vm]; ok {
		return d
	}
	d := DefaultPushPath
	if v, err := s.getVM(ctx, vm); err == nil && v.ImageID != "" {
		d = s.imageWorkdir(ctx, string(v.ImageID))
	}
	if s.workdirs == nil {
		s.workdirs = map[string]string{}
	}
	s.workdirs[vm] = d
	return d
}

// imageWorkdir is work/ in the home of image ref's default user
// (DefaultPushPath for "", the host's default image, or when unknown).
func (s *session) imageWorkdir(ctx context.Context, ref string) string {
	if ref == "" {
		return DefaultPushPath
	}
	var img api.Image
	if err := s.cl.get(ctx, "/images/"+url.PathEscape(ref), nil, &img); err != nil || !validUser(img.DefaultUser) {
		return DefaultPushPath
	}
	if img.DefaultUser == "root" {
		return "/root/work"
	}
	return "/home/" + img.DefaultUser + "/work"
}

func validUser(u string) bool {
	if u == "" || len(u) > 32 {
		return false
	}
	for _, r := range u {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// boxCwd is where `box run` runs by default: the current directory's place
// in the synced tree (the work dir at the worktree's top level).
func (s *session) boxCwd(ctx context.Context, vm string) string {
	top := repoRoot()
	wd, err := os.Getwd()
	if top == "" || err != nil {
		return s.boxWorkdir(ctx, vm)
	}
	if r, err := filepath.EvalSymlinks(wd); err == nil {
		wd = r
	}
	if r, err := filepath.EvalSymlinks(top); err == nil {
		top = r
	}
	rel, err := filepath.Rel(top, wd)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return s.boxWorkdir(ctx, vm)
	}
	return path.Join(s.boxWorkdir(ctx, vm), filepath.ToSlash(rel))
}
