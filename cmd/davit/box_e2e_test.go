package main_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
)

// boxUp is `box up`'s JSON result.
type boxUp struct {
	Box        string `json:"box"`
	Instance   string `json:"instance"`
	Action     string `json:"action"`
	Generation int    `json:"generation"`
	SSHHost    string `json:"sshHost"`
	VM         api.VM `json:"vm"`
}

func (h *host) boxUp(args ...string) boxUp {
	h.t.Helper()
	r := h.cli(append([]string{"box", "up"}, args...)...)
	if r.code != 0 || r.last().Event != "result" {
		h.t.Fatalf("box up %v: exit %d %+v\n%s", args, r.code, r.last(), r.stderr)
	}
	var b boxUp
	json.Unmarshal(r.raw[len(r.raw)-1], &b)
	return b
}

type boxLsRow struct {
	Instance, Kind, State string
	Generation            int
	Stale                 bool
}

func (h *host) boxLs(args ...string) []boxLsRow {
	h.t.Helper()
	r := h.cli(append([]string{"box", "ls"}, args...)...)
	var l struct {
		Items []boxLsRow `json:"items"`
	}
	if r.code != 0 || json.Unmarshal(r.raw[len(r.raw)-1], &l) != nil {
		h.t.Fatalf("box ls: exit %d %s", r.code, r.stderr)
	}
	return l.Items
}

// human runs the CLI without --json, with env, and returns its streams.
func human(t *testing.T, env []string, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env, cmd.Dir = env, dir
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	fatalIfSignaled(t, args, err)
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return so.String(), se.String(), code
}

// gitTree makes a throwaway git work tree on branch.
func gitTree(t *testing.T, dir, branch string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	git := exec.Command("git", "-C", dir, "-c", "init.defaultBranch="+branch, "init", "-q")
	git.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
}

// TestProjectBoxes: from inside a repository with no arguments, an image
// build runs its .davit/setup.sh and makes generation 1 (the project is
// the repo's name); instances (one per worktree, the name derived from the
// branch) start from the latest generation in parallel, persist across
// down and up, and keep their generation after an image update until up
// --refresh; run syncs the worktree first, runs in the same subdirectory
// of the synced tree with git ls-files working there, and keeps stdout,
// stderr and the exit code apart; --pull brings files back; ls shows the
// image and its instances; a plain VM of a box's name is refused; rm
// deletes.
func TestProjectBoxes(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(0, 0)
	// The fake guest runs commands on this host, so the box work dir is a
	// test path.
	work := filepath.Join(h.dir, "work")
	h.env = append(h.env, "DAVIT_BOX_WORKDIR="+work)
	env := append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=", "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"),
		"DAVIT_BOX_WORKDIR="+work)

	tree := filepath.Join(h.dir, "proj")
	gitTree(t, tree, "feature/login")
	os.MkdirAll(filepath.Join(tree, ".davit"), 0o755)
	os.MkdirAll(filepath.Join(tree, "sub"), 0o755)
	os.WriteFile(filepath.Join(tree, ".davit", "setup.sh"), []byte("#!/bin/sh\nset -e\necho setup-ran >> setup.log\necho installing deps\n"), 0o755)
	os.WriteFile(filepath.Join(tree, "Makefile"), []byte("check:\n\t@echo checked\n"), 0o644)
	os.WriteFile(filepath.Join(tree, "sub", "x.txt"), []byte("x\n"), 0o644)
	os.WriteFile(filepath.Join(tree, ".gitignore"), []byte("secret.env\nsetup.log\n"), 0o644)
	os.WriteFile(filepath.Join(tree, "secret.env"), []byte("TOKEN=x\n"), 0o644)

	h.expectError(h.cli("box", "image", "update", "proj"), 5, domain.CodeNotFound)
	so, se, code := human(t, env, filepath.Join(tree, "sub"), "box", "image", "build", "--idle", "20m")
	if code != 0 || !strings.Contains(so, "installing deps") || !strings.Contains(so, "box proj image generation 1 ready") {
		t.Fatalf("image build: exit %d %q %q", code, so, se)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "setup.log")); string(b) != "setup-ran\n" {
		t.Fatalf("setup ran %q", b)
	}
	if _, err := os.Stat(filepath.Join(work, "secret.env")); err == nil {
		t.Fatal("the image sync pushed an ignored file")
	}
	h.expectError(h.cli("box", "image", "build", "proj"), 6, domain.CodeInvalidState)
	if builder := h.info("proj--image"); builder.State != domain.StateStopped || builder.Labels["davit.box.role"] != "image" {
		t.Fatalf("builder after build: %s %v", builder.State, builder.Labels)
	}

	// Two instances in parallel, from generation 1.
	var wg sync.WaitGroup
	ups := make([]boxUp, 2)
	for i, inst := range []string{"proj/alpha", "proj/beta"} {
		wg.Add(1)
		go func() { defer wg.Done(); ups[i] = h.boxUp(inst) }()
	}
	wg.Wait()
	for i, want := range []string{"alpha", "beta"} {
		u := ups[i]
		if u.Action != "created" || u.Instance != want || u.Generation != 1 || u.VM.Name != "proj--"+want ||
			u.VM.SourceSnapshotID == "" || u.VM.AutoStop || u.VM.IdleTimeoutSeconds != 900 || u.VM.Labels["davit.box"] != "proj" {
			t.Fatalf("instance %s: %+v", want, u)
		}
	}
	if again := h.boxUp("proj/alpha"); again.Action != "running" || again.VM.ID != ups[0].VM.ID {
		t.Fatalf("second up: %+v", again)
	}

	// Zero arguments: the repo's project, the branch's instance.
	so, se, code = human(t, env, tree, "box", "up")
	if code != 0 || !strings.Contains(so, "box proj/feature-login created") {
		t.Fatalf("up from the worktree: exit %d %q %q", code, so, se)
	}
	os.WriteFile(filepath.Join(tree, "sub", "new.txt"), []byte("new\n"), 0o644) // run syncs it
	so, se, code = human(t, env, filepath.Join(tree, "sub"), "box", "run", "--", "sh", "-c",
		"pwd; cat new.txt; git -C .. ls-files | LC_ALL=C sort | tr '\\n' ' '; echo; sed -n 1p ../Makefile; echo to-stderr >&2; echo made > artifact.txt; exit 3")
	want := work + "/sub\nnew\n.davit/setup.sh .gitignore Makefile sub/new.txt sub/x.txt \ncheck:\n"
	if code != 3 || so != want || !strings.Contains(se, "to-stderr") || !strings.Contains(se, "synced") {
		t.Fatalf("box run: exit %d stdout %q (want %q) stderr %q", code, so, want, se)
	}
	out := filepath.Join(h.dir, "pulled")
	os.MkdirAll(out, 0o755)
	if _, se, code := human(t, env, tree, "box", "sync", "--pull", "sub/artifact.txt", out); code != 0 {
		t.Fatalf("pull file: exit %d %s", code, se)
	}
	if b, err := os.ReadFile(filepath.Join(out, "artifact.txt")); err != nil || string(b) != "made\n" {
		t.Fatalf("pulled artifact: %q %v", b, err)
	}
	if _, se, code := human(t, env, tree, "box", "sync", "proj/alpha", "--pull", "sub", out); code != 0 {
		t.Fatalf("pull dir: exit %d %s", code, se)
	}
	if b, err := os.ReadFile(filepath.Join(out, "sub", "x.txt")); err != nil || string(b) != "x\n" {
		t.Fatalf("pulled directory: %q %v", b, err)
	}

	// Down keeps the disk; up resumes it; run brings a stopped box up.
	if r := h.cli("box", "down", "proj/alpha"); r.code != 0 || h.info("proj--alpha").State != domain.StateStopped {
		t.Fatalf("box down: exit %d %s", r.code, r.stderr)
	}
	if again := h.boxUp("proj/alpha"); again.Action != "resumed" || again.VM.ID != ups[0].VM.ID {
		t.Fatalf("up after down: %+v", again)
	}
	h.cli("box", "down", "proj/beta")
	if _, se, code := human(t, env, tree, "box", "run", "proj/beta", "--no-sync", "--", "true"); code != 0 || h.info("proj--beta").State != domain.StateReady {
		t.Fatalf("run on a stopped instance: exit %d %s", code, se)
	}

	// An image update makes generation 2; instances keep generation 1
	// until --refresh, which recreates one.
	if _, se, code := human(t, env, tree, "box", "image", "update", "--setup-cmd", "echo updated >> setup.log"); code != 0 {
		t.Fatalf("image update: exit %d %s", code, se)
	}
	rows := h.boxLs("proj")
	if len(rows) != 4 || rows[0].Kind != "image" || rows[0].Generation != 2 {
		t.Fatalf("box ls after update: %+v", rows)
	}
	for _, r := range rows[1:] {
		if r.Kind != "instance" || r.Generation != 1 || !r.Stale {
			t.Fatalf("instance row after update: %+v", r)
		}
	}
	if b := h.boxUp("proj/beta"); b.Generation != 1 || b.Action != "running" {
		t.Fatalf("an old-generation instance: %+v", b)
	}
	so, se, code = human(t, env, tree, "box", "up", "/alpha", "--refresh")
	if code != 0 || !strings.Contains(so, "proj/alpha refreshed") || !strings.Contains(so, "image generation 2") || !strings.Contains(se, "discards its disk") {
		t.Fatalf("refresh: exit %d %q %q", code, so, se)
	}
	if vm := h.info("proj--alpha"); vm.ID == ups[0].VM.ID || vm.Labels["davit.box.generation"] != "2" {
		t.Fatalf("refreshed instance: %s %v", vm.ID, vm.Labels)
	}
	if b := h.boxUp("proj/alpha", "--refresh"); b.Action != "running" {
		t.Fatalf("refresh of a current instance: %+v", b)
	}

	// Names: a plain VM is not taken over; reserved and invalid names fail.
	h.waited(h.cli("new", "plain--x"), domain.OpCreate)
	h.expectError(h.cli("box", "up", "plain/x"), 6, domain.CodeInvalidState)
	h.expectError(h.cli("box", "up", "proj/image"), 2, domain.CodeInvalidArgument)
	h.expectError(h.cli("box", "up", "bad|name"), 2, domain.CodeInvalidArgument)
	if rows := h.boxLs(); len(rows) != 4 {
		t.Fatalf("box ls lists %d rows, want the image and 3 instances: %+v", len(rows), rows)
	}

	// box ls heads the list with the host's memory and reservation.
	r := h.cli("box", "ls")
	var ls struct {
		Host struct {
			FreeMemoryBytes, ReservedMemoryBytes int64
			LiveVMs, RunningBoxes                int
		} `json:"host"`
	}
	if r.code != 0 || json.Unmarshal(r.raw[len(r.raw)-1], &ls) != nil || ls.Host.LiveVMs < 3 || ls.Host.RunningBoxes < 2 || ls.Host.ReservedMemoryBytes <= 0 {
		t.Fatalf("box ls host summary: exit %d %+v %s", r.code, ls, r.stderr)
	}
	if so, _, code := human(t, env, tree, "box", "ls"); code != 0 || !strings.HasPrefix(so, "host: memory ") || !strings.Contains(so, "boxes running") {
		t.Fatalf("box ls header: %q", so)
	}

	// box gc stops idle boxes (dry run first), then removes old stopped ones.
	so, _, code = human(t, env, tree, "box", "gc", "proj", "--idle", "0s", "--dry-run")
	if code != 0 || !strings.Contains(so, "would stop proj/alpha") || h.info("proj--alpha").State != domain.StateReady {
		t.Fatalf("gc dry run: exit %d %q", code, so)
	}
	if so, se, code := human(t, env, tree, "box", "gc", "proj", "--idle", "0s"); code != 0 || !strings.Contains(so, "stopped proj/alpha") {
		t.Fatalf("gc: exit %d %q %q", code, so, se)
	}
	for _, n := range []string{"proj--alpha", "proj--beta"} {
		if vm := h.info(n); vm.State != domain.StateStopped {
			t.Fatalf("%s after gc: %s", n, vm.State)
		}
	}
	if so, _, code := human(t, env, tree, "box", "gc", "proj", "--rm-older-than", "0d", "--dry-run"); code != 0 ||
		!strings.Contains(so, "would remove proj/alpha") || strings.Contains(so, "proj/image") {
		t.Fatalf("gc --rm-older-than dry run: exit %d %q", code, so)
	}

	h.expectError(h.cli("box", "rm", "proj/beta"), 2, domain.CodeInvalidArgument) // needs --yes
	if r := h.cli("box", "rm", "proj/beta", "--yes"); r.code != 0 {
		t.Fatalf("box rm: %d %s", r.code, r.stderr)
	}
	h.expectError(h.cli("box", "down", "proj/beta"), 5, domain.CodeNotFound)

	// image rm refuses while instances exist; --force deletes them, the
	// builder and every generation snapshot.
	h.expectError(h.cli("box", "image", "rm", "proj", "--yes"), 6, domain.CodeInvalidState)
	if r := h.cli("box", "image", "rm", "proj", "--force", "--yes"); r.code != 0 {
		t.Fatalf("image rm --force: %d %s", r.code, r.stderr)
	}
	if rows := h.boxLs("proj"); len(rows) != 0 {
		t.Fatalf("box ls after image rm: %+v", rows)
	}
	if r := h.cli("snapshots"); strings.Contains(string(bytesJoin(r.raw)), "proj-image-g") {
		t.Fatalf("generation snapshots left: %s", bytesJoin(r.raw))
	}
	h.expectError(h.cli("box", "image", "rm", "proj", "--yes"), 5, domain.CodeNotFound)
}

func bytesJoin(raw []json.RawMessage) []byte {
	var b []byte
	for _, r := range raw {
		b = append(append(b, r...), '\n')
	}
	return b
}

// fakeSSH writes an "ssh" that runs the remote command here: the ssh://
// transport without a network (OpenSSH itself is not under test).
func fakeSSH(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "fake-ssh")
	script := "#!/bin/sh\nwhile [ \"$#\" -gt 0 ] && [ \"$1\" != \"--\" ]; do shift; done\nshift\neval \"$*\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSSHTransport: --host ssh://USER@HOST/SOCKET reaches the daemon
// through `davit dial-stdio` on the far side: lifecycle calls, a
// streamed box run and real OpenSSH into the box (ProxyCommand through
// the same transport).
func TestSSHTransport(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(0, 0)
	home := filepath.Join(h.dir, "home")
	os.MkdirAll(home, 0o700)
	env := append(os.Environ(), "HOME="+home, "DAVIT_API_URL=", "DAVIT_CURRENT_ID=", "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"),
		"XDG_CACHE_HOME="+filepath.Join(h.dir, "cache"),
		"DAVIT_HOST=ssh://davit@host.invalid"+h.sock, "DAVIT_SSH="+fakeSSH(t, h.dir), "DAVIT_REMOTE_BIN="+bin,
		"DAVIT_BOX_WORKDIR="+filepath.Join(h.dir, "work"))
	start := time.Now()
	if so, se, code := human(t, env, h.dir, "box", "up", "remote/dev"); code != 0 || !strings.Contains(so, "box remote/dev created") {
		t.Fatalf("box up over ssh: exit %d %q %q", code, so, se)
	}
	if so, se, code := human(t, env, h.dir, "box", "run", "remote/dev", "--cwd", "/", "--", "sh", "-c", "echo via-ssh; exit 5"); code != 5 || so != "via-ssh\n" {
		t.Fatalf("box run over ssh: exit %d %q %q", code, so, se)
	}
	if _, err := exec.LookPath("ssh"); err == nil {
		if so, se, code := human(t, env, h.dir, "box", "ssh", "remote/dev", "--", "echo", "hello-box"); code != 0 || so != "hello-box\n" {
			t.Fatalf("box ssh over the ssh transport: exit %d %q %q", code, so, se)
		}
	}
	t.Logf("box up + run + ssh over the ssh:// transport took %s", time.Since(start).Round(time.Millisecond))

	// A host the transport cannot reach says why.
	bad := append(env[:len(env):len(env)], "DAVIT_REMOTE_BIN=/nonexistent/davit")
	if _, se, code := human(t, bad, h.dir, "list"); code == 0 || !strings.Contains(se, "davit is not installed on davit@host.invalid") {
		t.Fatalf("unreachable remote: exit %d %q", code, se)
	}
}
