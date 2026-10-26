package cli

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"davit/internal/domain"
)

// boxRunPrelude runs `box run`'s command in a login shell with mise's
// shims on PATH, so the image's toolchains and a project's mise pins apply,
// and in front of them every node_modules/.bin from the directory up, the
// nearest first (as npm run does), so a JS project's own tools (vp, tsc)
// resolve. POSIX sh apart from mise's bash activation.
const boxRunPrelude = `if command -v mise >/dev/null 2>&1; then eval "$(mise activate bash --shims 2>/dev/null)"; fi; ` +
	`_davit_d=$PWD; _davit_p=; while [ -n "$_davit_d" ]; do if [ -d "$_davit_d/node_modules/.bin" ]; then ` +
	`_davit_p=$_davit_p${_davit_p:+:}$_davit_d/node_modules/.bin; fi; _davit_d=${_davit_d%/*}; done; ` +
	`if [ -n "$_davit_p" ]; then PATH=$_davit_p:$PATH; export PATH; fi; unset _davit_d _davit_p; exec "$@"`

// looksLikePath reports whether a `box sync` argument is a directory
// (., .., ./x, ../x, ~/x, an absolute path, or DIR:/GUEST) rather than
// PROJECT[/INSTANCE]; a bare name is always a project.
func looksLikePath(s string) bool {
	return s == "." || s == ".." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") ||
		strings.HasPrefix(s, "~") || filepath.IsAbs(s) || strings.Contains(s, ":/")
}

// boxSync pushes spec (DIR[:GUEST_PATH]) into the box, then sets up git
// metadata there (--git): index runs `git init` once and `git add -A`, so
// git ls-files and status work on the synced files; full also brings the
// current branch's history (a bundle of HEAD) and points the index at it,
// so diffs against HEAD are this worktree's; none leaves git alone. quiet
// prints only a diagnostic line (box run).
func (s *session) boxSync(ctx context.Context, o *opts, ref boxRef, spec string, quiet bool) error {
	mode := o.box.git
	switch mode {
	case "", "index", "full", "none":
	default:
		return usageErr("--git must be index, full or none, got %q", mode)
	}
	ps, err := parsePushSpec(spec)
	if err != nil {
		return err
	}
	if ps.guest == "" {
		ps.guest = s.boxWorkdir(ctx, ref.vm())
	}
	sel, err := selectTree(ctx, ps)
	if err != nil {
		return err
	}
	start := time.Now()
	res, err := s.pushTree(ctx, ref.vm(), ps, sel)
	if err != nil {
		return err
	}
	if mode != "none" {
		if err := s.boxGit(ctx, ref.vm(), ps, mode); err != nil {
			s.out.diag("box %s/%s: git metadata not set up: %v", ref.project, ref.instance, err)
		}
	}
	if quiet {
		s.out.diag("synced %d files (%s) to %s/%s:%s in %s", res.Files, humanBytes(res.Bytes), ref.project, ref.instance, res.Path,
			time.Since(start).Round(time.Millisecond))
		return nil
	}
	s.out.result("", res)
	s.out.human("synced %d files (%s) to %s/%s:%s in %s", res.Files, humanBytes(res.Bytes), ref.project, ref.instance, res.Path,
		time.Since(start).Round(time.Millisecond))
	return nil
}

// boxGit sets up git metadata for the synced tree at ps.guest.
func (s *session) boxGit(ctx context.Context, vm string, ps pushSpec, mode string) error {
	script := `set -e; cd "$1"; [ -e .git ] || git init -q; git add -A >/dev/null 2>&1 || true`
	if mode == "full" {
		bundle, branch, err := gitBundle(ps.dir)
		if err != nil {
			return err
		}
		defer os.Remove(bundle)
		v, err := s.getVM(ctx, vm)
		if err != nil {
			return err
		}
		remote := path.Join(ps.guest, ".git-davit.bundle")
		if _, err := s.upload(ctx, cpOpts{}, v, bundle, remote, &progress{s: s, start: time.Now()}); err != nil {
			return err
		}
		script = `set -e; cd "$1"; b="$2"; [ -e .git ] || git init -q
git fetch -q --force .git-davit.bundle "+HEAD:refs/heads/$b"; git symbolic-ref HEAD "refs/heads/$b"; git reset -q
rm -f .git-davit.bundle`
		p, err := s.boxExecQuiet(ctx, vm, []string{"sh", "-c", script, "box-git", ps.guest, branch}, ps.guest, 5*time.Minute)
		return gitExit(p.ExitCode, err)
	}
	p, err := s.boxExecQuiet(ctx, vm, []string{"sh", "-c", script, "box-git", ps.guest}, ps.guest, 5*time.Minute)
	return gitExit(p.ExitCode, err)
}

func gitExit(code *int, err error) error {
	if err != nil {
		return err
	}
	if code != nil && *code != 0 {
		return clientErr(domain.CodeInternal, "git in the box exited %d (see `davit history`)", *code)
	}
	return nil
}

// gitBundle bundles dir's HEAD into a temporary file and returns it and
// the branch name (HEAD's, else "main").
func gitBundle(dir string) (file, branch string, err error) {
	f, err := os.CreateTemp("", "davit-*.bundle")
	if err != nil {
		return "", "", err
	}
	f.Close()
	cmd := gitCmd(dir, "bundle", "create", "-q", f.Name(), "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(f.Name())
		return "", "", clientErr(domain.CodeInvalidArgument, "git bundle of %s: %v %s", dir, err, strings.TrimSpace(string(out)))
	}
	branch = strings.TrimSpace(gitIn(dir, "symbolic-ref", "-q", "--short", "HEAD"))
	if branch == "" {
		branch = "main"
	}
	return f.Name(), branch, nil
}
