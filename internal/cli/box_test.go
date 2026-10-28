package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"davit/api"
	"davit/internal/domain"
)

func TestBoxName(t *testing.T) {
	for in, want := range map[string]string{
		"davit": "davit", "My_Project": "my-project", "a.b c--d": "a-b-c-d", "__x__": "x", "web2": "web2",
	} {
		if got, err := boxName(in); err != nil || got != want {
			t.Errorf("boxName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "---", "a/b", "ünïcode", string(make([]byte, 64)) + "x"} {
		if got, err := boxName(bad); err == nil {
			t.Errorf("boxName(%q) = %q, want an error", bad, got)
		}
	}
}

func TestSSHRemoteQuote(t *testing.T) {
	for in, want := range map[string]string{
		"davit": "davit", "/run/user/1000/davit/davit.sock": "/run/user/1000/davit/davit.sock",
		"a b": "'a b'", "it's": `'it'\''s'`, "": "''", "$(x)": "'$(x)'",
	} {
		if got := sshRemoteQuote(in); got != want {
			t.Errorf("sshRemoteQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBoxRef(t *testing.T) {
	for in, want := range map[string]string{
		"davit/main": "davit--main", "My_Proj/feature/Login": "my-proj--feature-login",
	} {
		r, err := parseBoxRef(in)
		if err != nil || r.vm() != want {
			t.Errorf("parseBoxRef(%q).vm() = %q, %v; want %q", in, r.vm(), err, want)
		}
	}
	long := boxRef{project: "p", instance: "a-very-long-branch-name-that-goes-on-and-on-and-on-past-the-limit-of-names"}
	if n := long.vm(); len(n) > 63 || n[:3] != "p--" {
		t.Errorf("long instance: %q (%d)", n, len(n))
	}
	for _, bad := range []string{"p/image", "p/---", "bad|name/x"} {
		if r, err := parseBoxRef(bad); err == nil {
			t.Errorf("parseBoxRef(%q) = %+v, want an error", bad, r)
		}
	}
}

func TestDetectSetup(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, found := detectSetup(dir); !slices.Equal(found, []string{"nothing to install"}) {
		t.Fatalf("empty tree: %v", found)
	}
	write("mise.toml", "[tools]\ngo = '1.26'\n")
	write("go.mod", "module x\n")
	write("package.json", "{}")
	write("pnpm-lock.yaml", "")
	write("uv.lock", "")
	write("Makefile", "VAR := 1\nbuild:\n\techo\nsetup deps: build\n\techo setup\n")
	script, found := detectSetup(dir)
	want := []string{"mise install", "go mod download", "pnpm install", "uv sync", "make setup"}
	if !slices.Equal(found, want) {
		t.Fatalf("found %v, want %v", found, want)
	}
	for _, s := range []string{"mise exec -- go mod download", "mise exec -- pnpm install --frozen-lockfile", "mise exec -- uv sync --frozen", "set -eu"} {
		if !strings.Contains(script, s) {
			t.Errorf("script lacks %q:\n%s", s, script)
		}
	}
	os.Remove(filepath.Join(dir, "pnpm-lock.yaml"))
	write("package-lock.json", "{}")
	os.Remove(filepath.Join(dir, "mise.toml"))
	if script, found := detectSetup(dir); !slices.Contains(found, "npm ci") || strings.Contains(script, "mise exec") {
		t.Fatalf("npm without mise: %v\n%s", found, script)
	}
	if makeTarget("setup-deps:\n", "setup") || makeTarget("\tsetup: x\n", "setup") || makeTarget("setup := 1\n", "setup") {
		t.Fatal("makeTarget matched a non-target")
	}
}

// TestDetectVitePlus: a Vite+ project (vite-plus in package.json, or a vite
// config importing it) installs through vp, with the lockfile frozen; other
// JS projects keep their package manager's step.
func TestDetectVitePlus(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"devDependency": {"package.json": `{"devDependencies":{"vite-plus":"catalog:"},"packageManager":"pnpm@11.10.0"}`, "pnpm-lock.yaml": ""},
		"dependency":    {"package.json": `{"dependencies":{"vite-plus":"1.0.0"}}`},
		"config":        {"package.json": `{}`, "vite.config.ts": `import { defineConfig } from "vite-plus";`, "yarn.lock": ""},
	} {
		dir := t.TempDir()
		for f, data := range files {
			os.WriteFile(filepath.Join(dir, f), []byte(data), 0o644)
		}
		script, found := detectSetup(dir)
		if !slices.Equal(found, []string{"vp install"}) || !jsProject(dir) {
			t.Fatalf("%s: found %v", name, found)
		}
		_, locked := files["pnpm-lock.yaml"]
		_, yarn := files["yarn.lock"]
		if frozen := strings.Contains(script, "vp install --frozen-lockfile"); frozen != (locked || yarn) {
			t.Errorf("%s: frozen %v:\n%s", name, frozen, script)
		}
		if !strings.Contains(script, "VP_NODE_MANAGER=$vp_node") || !strings.Contains(script, "if ! vp --version") {
			t.Errorf("%s: no guarded vp install:\n%s", name, script)
		}
		if out, err := exec.Command("bash", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: script does not parse: %v %s", name, err, out)
		}
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"devDependencies":{"vite":"7"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "vite.config.ts"), []byte(`import { defineConfig } from "vite";`), 0o644)
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), nil, 0o644)
	if _, found := detectSetup(dir); !slices.Equal(found, []string{"pnpm install"}) {
		t.Fatalf("plain vite: %v", found)
	}
	if jsProject(t.TempDir()) || jsProject("") {
		t.Fatal("a tree without package.json is a JS project")
	}
}

// TestNodeBinPath: the login profile snippet and box run's prelude put
// node_modules/.bin first on PATH under a POSIX sh (dash where present),
// nearest directory first, and leave PATH alone without one.
func TestNodeBinPath(t *testing.T) {
	shells := []string{"sh"}
	if _, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, "dash")
	}
	tree := filepath.Join(t.TempDir(), "my work")
	sub := filepath.Join(tree, "packages", "web")
	for _, d := range []string{filepath.Join(tree, "node_modules", ".bin"), filepath.Join(sub, "node_modules", ".bin")} {
		os.MkdirAll(d, 0o755)
	}
	// Past the mise activation (a bash-only step).
	prelude := boxRunPrelude[strings.Index(boxRunPrelude, "fi; ")+4:]
	for _, sh := range shells {
		profile := nodeBinProfile(tree)
		cmd := exec.Command(sh, "-c", profile+profile+`printf %s "$PATH"`)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		out, err := cmd.Output()
		want := filepath.Join(tree, "node_modules/.bin") + ":/usr/bin:/bin"
		if err != nil || string(out) != want {
			t.Errorf("%s profile: %q %v, want %q (once)", sh, out, err, want)
		}
		cmd = exec.Command(sh, "-c", prelude, "box-run", sh, "-c", `printf %s "$PATH"`)
		cmd.Dir = sub
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		out, err = cmd.Output()
		want = filepath.Join(sub, "node_modules/.bin") + ":" + filepath.Join(tree, "node_modules/.bin") + ":/usr/bin:/bin"
		if err != nil || string(out) != want {
			t.Errorf("%s prelude: %q %v, want %q", sh, out, err, want)
		}
		cmd = exec.Command(sh, "-c", prelude, "box-run", sh, "-c", `printf %s "$PATH"`)
		cmd.Dir, cmd.Env = t.TempDir(), []string{"PATH=/usr/bin:/bin"}
		if out, err := cmd.Output(); err != nil || string(out) != "/usr/bin:/bin" {
			t.Errorf("%s prelude without node_modules: %q %v", sh, out, err)
		}
	}
}

// TestJSBoxMemory: a new box of a JS project defaults to DefaultJSBoxMemory
// unless --memory or --type says otherwise or the host cannot hold it;
// other projects keep the host default.
func TestJSBoxMemory(t *testing.T) {
	var maxMem, budget int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caps := api.HostCapabilities{Capacity: &domain.HostCapacity{MemoryBudgetBytes: budget}}
		caps.Runtime.Shape.MaxMemoryBytes = maxMem
		json.NewEncoder(w).Encode(caps)
	}))
	defer srv.Close()
	cl, err := newClient(srv.URL, "req_test")
	if err != nil {
		t.Fatal(err)
	}
	var diag bytes.Buffer
	s := &session{cl: cl, out: &printer{stdout: io.Discard, stderr: &diag}}
	js, plain := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(js, "package.json"), []byte("{}"), 0o644)
	ctx := context.Background()
	mem := func(o *opts, dir string) int64 {
		if sh := s.newBoxShape(ctx, o, dir); sh != nil {
			return sh.MemoryBytes
		}
		return 0
	}
	if got := mem(&opts{}, js); got != DefaultJSBoxMemory {
		t.Fatalf("JS project: %d", got)
	}
	if got := mem(&opts{cpus: 4}, js); got != DefaultJSBoxMemory {
		t.Fatalf("JS project with --cpus: %d", got)
	}
	if got := mem(&opts{}, plain); got != 0 {
		t.Fatalf("plain project: %d", got)
	}
	o := &opts{}
	o.memory.v = 2 * domain.GiB
	if got := mem(o, js); got != 2*domain.GiB {
		t.Fatalf("--memory: %d", got)
	}
	if got := mem(&opts{typ: "small"}, js); got != 0 {
		t.Fatalf("--type: %d", got)
	}
	maxMem = 8 * domain.GiB
	budget = 3 * domain.GiB
	if got := mem(&opts{}, js); got != 0 || !strings.Contains(diag.String(), "keeping the host default") {
		t.Fatalf("budget below 4 GiB: %d %q", got, diag.String())
	}
}

func TestRemoteName(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:example/go-service.git": "go-service", "https://github.com/example/app": "app",
		"https://host/x/app.git/": "app", "/srv/git/proj.git": "proj",
	} {
		if m := remoteNameRE.FindStringSubmatch(in); m == nil || m[1] != want {
			t.Errorf("remote %q -> %v, want %q", in, m, want)
		}
	}
}

// TestSetupFileFallback: .davit/setup.sh wins; a tree from before the rename
// with only .slipway/setup.sh still has its setup run.
func TestSetupFileFallback(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("echo "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	o := &opts{}
	write(legacySetupFile)
	if _, name, err := o.setupArgv(dir); err != nil || name != filepath.Join(dir, legacySetupFile) {
		t.Fatalf("legacy only: %q %v", name, err)
	}
	write(defaultSetupFile)
	if _, name, err := o.setupArgv(dir); err != nil || name != filepath.Join(dir, defaultSetupFile) {
		t.Fatalf("both: %q %v", name, err)
	}
}

// TestClientConfigMigrates: a client.json written before the rename is
// moved to the davit config dir on first read.
func TestClientConfigMigrates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	p, err := clientConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(filepath.Dir(filepath.Dir(p)), "slipway", "client.json")
	os.MkdirAll(filepath.Join(filepath.Dir(old), "ssh"), 0o700)
	os.WriteFile(old, []byte(`{"host":"ssh://u@h"}`), 0o600)
	os.WriteFile(filepath.Join(filepath.Dir(old), "ssh", "id_ed25519"), []byte("key"), 0o600)
	c, _, err := loadClientConfig()
	if err != nil || c.Host != "ssh://u@h" {
		t.Fatalf("legacy config: %+v %v", c, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("not moved: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(old)); !os.IsNotExist(err) {
		t.Fatalf("old dir kept: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "ssh", "id_ed25519")); string(b) != "key" {
		t.Fatalf("ssh key not moved: %q", b)
	}
}

// TestBoxDiskFullHint: a full box disk exits 8 with its own hint, not the
// host-is-full one.
func TestBoxDiskFullHint(t *testing.T) {
	de := domain.BoxDiskFull("guest: /home/davit/work: no space left on device")
	err := boxHint(&cliError{ErrorBody{Code: de.Code, Message: de.Message, Details: de.Details, Origin: "server"}})
	ce := err.(*cliError)
	if ExitCode(ce.Code) != ExitExhaustion || strings.Contains(ce.Message, "the host is full") ||
		!strings.Contains(ce.Message, "box disk full: `davit box up --disk 40GiB` (resize) or clean the workspace") {
		t.Fatalf("exit %d: %s", ExitCode(ce.Code), ce.Message)
	}
}
