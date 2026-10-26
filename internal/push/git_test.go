package push

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"davit/internal/domain"
)

// gitRun runs git in dir (the test sets a hermetic configuration).
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, path, data string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // umask-proof
		t.Fatal(err)
	}
}

// hermeticGit isolates every git the test runs from the caller: no
// user/system config or personal excludes (HOME and XDG_CONFIG_HOME are in
// tmp), no inherited repository variables, a fixed default branch and
// locale. The test's own gits never discover a repository above tmp (the
// test's TMPDIR may be inside a work tree); the selector ignores that
// ceiling, like every repository-locating variable.
func hermeticGit(t *testing.T, tmp string) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			t.Setenv(k, "") // registers the restore
			os.Unsetenv(k)
		}
	}
	home := filepath.Join(tmp, "home")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "LC_ALL": "C",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_TERMINAL_PROMPT": "0",
		"GIT_CEILING_DIRECTORIES": tmp,
		"GIT_AUTHOR_NAME":         "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		t.Setenv(k, v)
	}
	// Environment config reaches child gits too (the submodule clone). No
	// automatic maintenance: git detaches it after a commit, and it can
	// still be writing into .git when the test's TempDir is removed.
	cfg := [][2]string{{"init.defaultBranch", "main"}, {"protocol.file.allow", "always"},
		{"commit.gpgSign", "false"}, {"core.fsmonitor", "false"}, {"core.hooksPath", os.DevNull},
		{"maintenance.auto", "false"}, {"gc.auto", "0"}}
	t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(cfg)))
	for i, kv := range cfg {
		t.Setenv(fmt.Sprintf("GIT_CONFIG_KEY_%d", i), kv[0])
		t.Setenv(fmt.Sprintf("GIT_CONFIG_VALUE_%d", i), kv[1])
	}
}

func TestGitSelection(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	tmp := t.TempDir()
	hermeticGit(t, tmp)

	// A submodule source repository.
	sub := filepath.Join(tmp, "sub-src")
	gitRun(t, tmp, "init", "-q", "-b", "main", sub)
	write(t, filepath.Join(sub, "lib.go"), "package lib\n", 0o644)
	write(t, filepath.Join(sub, ".gitignore"), "*.o\n", 0o644)
	write(t, filepath.Join(sub, "lib.o"), "object", 0o644)
	gitRun(t, sub, "add", "lib.go", ".gitignore")
	gitRun(t, sub, "commit", "-q", "-m", "lib")

	repo := filepath.Join(tmp, "repo")
	gitRun(t, tmp, "init", "-q", "-b", "main", repo)
	write(t, filepath.Join(repo, "a.txt"), "tracked\n", 0o644)
	write(t, filepath.Join(repo, "bin", "run.sh"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(repo, "gone.txt"), "deleted later\n", 0o644)
	write(t, filepath.Join(repo, ".gitignore"), "*.env\nbuild/\n", 0o644)
	if err := os.Symlink("a.txt", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "submodule", "add", "-q", sub, "vendor/sub")
	gitRun(t, repo, "commit", "-q", "-m", "init")
	os.Remove(filepath.Join(repo, "gone.txt"))
	write(t, filepath.Join(repo, "new.txt"), "untracked\n", 0o600)
	write(t, filepath.Join(repo, "secret.env"), "TOKEN=x\n", 0o600)
	write(t, filepath.Join(repo, "build", "out.bin"), "build output", 0o644)
	write(t, filepath.Join(repo, "vendor", "sub", "lib.o"), "object", 0o644)
	gitRun(t, tmp, "init", "-q", "-b", "main", filepath.Join(repo, "nested")) // untracked nested repo

	es, err := Git{}.Select(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]Entry{}
	for _, e := range es {
		names = append(names, e.Path)
		byName[e.Path] = e
	}
	slices.Sort(names)
	want := []string{".gitignore", ".gitmodules", "a.txt", "bin/run.sh", "link", "new.txt", "vendor/sub/.gitignore", "vendor/sub/lib.go"}
	if !slices.Equal(names, want) {
		t.Fatalf("selection %q, want %q", names, want)
	}
	if m := byName["bin/run.sh"].Mode; m.Perm() != 0o755 || !m.IsRegular() {
		t.Errorf("bin/run.sh mode %v", m)
	}
	if m := byName["new.txt"].Mode; m.Perm() != 0o600 {
		t.Errorf("new.txt mode %v", m)
	}
	if l := byName["link"]; l.Mode&fs.ModeSymlink == 0 || l.LinkTarget != "a.txt" {
		t.Errorf("link %+v", l)
	}

	// The tar holds exactly the selection, contents and modes included.
	var buf bytes.Buffer
	n, err := WriteTar(&buf, repo, es)
	if err != nil || n != len(want) {
		t.Fatalf("WriteTar: %d %v", n, err)
	}
	tr := tar.NewReader(&buf)
	var got []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, h.Name)
		data, _ := io.ReadAll(tr)
		switch h.Name {
		case "a.txt":
			if string(data) != "tracked\n" || h.Typeflag != tar.TypeReg {
				t.Errorf("a.txt in tar: %q %c", data, h.Typeflag)
			}
		case "bin/run.sh":
			if fs.FileMode(h.Mode).Perm() != 0o755 {
				t.Errorf("bin/run.sh tar mode %o", h.Mode)
			}
		case "link":
			if h.Typeflag != tar.TypeSymlink || h.Linkname != "a.txt" {
				t.Errorf("link in tar: %c %q", h.Typeflag, h.Linkname)
			}
		}
		if h.Uname != "" || h.Uid != 0 {
			t.Errorf("%s records an owner", h.Name)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("tar entries %q", got)
	}

	// A subdirectory selects relative to itself.
	es, err = Git{}.Select(context.Background(), filepath.Join(repo, "bin"))
	if err != nil || len(es) != 1 || es[0].Path != "run.sh" {
		t.Fatalf("subdirectory selection: %+v %v", es, err)
	}
	// A plain directory is refused, not pushed wholesale. Under a TMPDIR
	// inside a work tree there is no plain directory to try: git rightly
	// finds the enclosing tree.
	plain := filepath.Join(tmp, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := exec.Command("git", "-C", plain, "rev-parse", "--show-toplevel")
	probe.Env = childEnv(os.Environ())
	if top, err := probe.Output(); err == nil {
		t.Logf("skipping the non-git check: TMPDIR is inside the work tree %s", bytes.TrimSpace(top))
	} else if _, err := (Git{}).Select(context.Background(), plain); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("non-git directory: %v", err)
	}
	// A file that changed type after selection fails the tar.
	os.Remove(filepath.Join(repo, "new.txt"))
	os.Mkdir(filepath.Join(repo, "new.txt"), 0o755)
	if _, err := WriteTar(io.Discard, repo, []Entry{{Path: "new.txt", Mode: 0o600}}); err == nil {
		t.Fatal("WriteTar of a file that became a directory succeeded")
	}
}

// TestGitSelectionIgnoresRepoEnv: repository-locating variables inherited
// from the caller (here pointing at another throwaway repository) do not
// redirect the selection, and a ceiling does not hide the tree.
func TestGitSelectionIgnoresRepoEnv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	tmp := t.TempDir()
	hermeticGit(t, tmp)
	mine, other := filepath.Join(tmp, "mine"), filepath.Join(tmp, "other")
	for _, r := range []struct{ dir, file string }{{mine, "src/mine.txt"}, {other, "other.txt"}} {
		gitRun(t, tmp, "init", "-q", "-b", "main", r.dir)
		write(t, filepath.Join(r.dir, r.file), r.file+"\n", 0o644)
		gitRun(t, r.dir, "add", ".")
		gitRun(t, r.dir, "commit", "-q", "-m", "init")
	}
	otherGit := filepath.Join(other, ".git")
	for k, v := range map[string]string{
		"GIT_DIR": otherGit, "GIT_WORK_TREE": other, "GIT_INDEX_FILE": filepath.Join(otherGit, "index"),
		"GIT_OBJECT_DIRECTORY": filepath.Join(otherGit, "objects"), "GIT_COMMON_DIR": otherGit,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(otherGit, "objects"), "GIT_CEILING_DIRECTORIES": mine,
	} {
		t.Setenv(k, v)
	}
	for dir, want := range map[string]string{mine: "src/mine.txt", filepath.Join(mine, "src"): "mine.txt"} {
		es, err := Git{}.Select(context.Background(), dir)
		if err != nil || len(es) != 1 || es[0].Path != want {
			t.Fatalf("Select(%s) under another repository's env: %+v %v, want [%s]", dir, es, err, want)
		}
	}
}
