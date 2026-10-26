package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
