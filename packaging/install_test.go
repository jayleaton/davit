package packaging_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"davit/internal/buildinfo"
)

const fakeBinary = "#!/bin/sh\necho davit v0.0.1-test\n"

// installScript returns the absolute path of install.sh (go test runs in the
// package directory, which is where the script lives).
func installScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func requireTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("installer supports linux and darwin only, not %s", runtime.GOOS)
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("installer supports amd64 and arm64 only, not %s", runtime.GOARCH)
	}
	for _, tool := range []string{"sh", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found: %v", tool, err)
		}
	}
	_, e1 := exec.LookPath("sha256sum")
	_, e2 := exec.LookPath("shasum")
	if e1 != nil && e2 != nil {
		t.Skip("neither sha256sum nor shasum found")
	}
}

func assetName() string {
	return "davit-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
}

type tarEntry struct {
	name string
	body []byte
	mode int64
}

// tarball builds a gzip'd tar of entries (the release binary archive format).
func tarball(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// binaryTarball is a release archive holding only the binary body.
func binaryTarball(t *testing.T, body string) []byte {
	return tarball(t, tarEntry{"davit", []byte(body), 0o755})
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type fixture struct {
	root       string // one flat release directory, like a GitHub release
	home       string
	installDir string
	tmpDir     string // TMPDIR for the installer; must be empty afterwards
}

// newFixture writes <root>/<asset> with content and a SHA256SUMS whose body is
// sums (use defaultSums for the honest one).
func newFixture(t *testing.T, content []byte, sums string) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{
		root:       filepath.Join(base, "release"),
		home:       filepath.Join(base, "home"),
		installDir: filepath.Join(base, "home", "bin with space"),
		tmpDir:     filepath.Join(base, "tmp"),
	}
	for _, d := range []string{f.root, f.home, f.tmpDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.publish(t, assetName(), content)
	f.publish(t, "SHA256SUMS", []byte(sums))
	return f
}

func (f *fixture) publish(t *testing.T, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, name), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func defaultSums(content []byte) string {
	return sha256Hex(content) + "  " + assetName() + "\n" +
		strings.Repeat("0", 64) + "  davit-other-arch\n"
}

func (f *fixture) env(overrides map[string]string) []string {
	vars := map[string]string{
		"PATH":              os.Getenv("PATH"),
		"HOME":              f.home,
		"TMPDIR":            f.tmpDir,
		"DAVIT_RELEASE_URL": "file://" + f.root,
		"DAVIT_INSTALL_DIR": f.installDir,
	}
	for k, v := range overrides {
		if v == "" {
			delete(vars, k)
		} else {
			vars[k] = v
		}
	}
	env := make([]string, 0, len(vars))
	for k, v := range vars {
		env = append(env, k+"="+v)
	}
	return env
}

func (f *fixture) run(t *testing.T, shell string, overrides map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(shell, append([]string{installScript(t)}, args...)...)
	cmd.Env = f.env(overrides)
	cmd.Dir = f.home
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *fixture) assertFailedClean(t *testing.T, out string, err error, wantMsg string) {
	t.Helper()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("want nonzero exit, got err=%v\n%s", err, out)
	}
	if !strings.Contains(out, wantMsg) {
		t.Fatalf("output missing %q:\n%s", wantMsg, out)
	}
	if _, err := os.Stat(filepath.Join(f.installDir, "davit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("binary must not be installed on failure (stat err=%v)", err)
	}
	if ents, err := os.ReadDir(f.installDir); err == nil && len(ents) > 0 {
		t.Fatalf("install dir must stay empty on failure, has %d entries", len(ents))
	}
	f.assertTmpClean(t)
}

func (f *fixture) assertTmpClean(t *testing.T) {
	t.Helper()
	ents, err := os.ReadDir(f.tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("installer left %d entries in TMPDIR (first: %s)", len(ents), ents[0].Name())
	}
}

// assertClient checks a client install: only the binary, the PATH note and
// the connect step.
func (f *fixture) assertClient(t *testing.T, out string, want []byte) {
	t.Helper()
	dst := filepath.Join(f.installDir, "davit")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("binary not installed: %v\n%s", err, out)
	}
	if string(got) != string(want) {
		t.Fatalf("installed bytes differ: got %q want %q", got, want)
	}
	if st, err := os.Stat(dst); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary: %v %v", err, st)
	}
	if ents, err := os.ReadDir(f.installDir); err != nil || len(ents) != 1 {
		t.Fatalf("install dir should hold only the binary: %v %d entries", err, len(ents))
	}
	for _, s := range []string{"davit connect user@host", "is not on your PATH"} {
		if !strings.Contains(out, s) {
			t.Fatalf("output missing %q:\n%s", s, out)
		}
	}
	f.assertTmpClean(t)
}

func TestInstallSyntax(t *testing.T) {
	requireTools(t)
	for _, sh := range []string{"sh", "dash", "bash"} {
		if _, err := exec.LookPath(sh); err != nil {
			continue
		}
		if out, err := exec.Command(sh, "-n", installScript(t)).CombinedOutput(); err != nil {
			t.Fatalf("%s -n: %v\n%s", sh, err, out)
		}
	}
}

func TestInstallClient(t *testing.T) {
	requireTools(t)
	shells := []string{"sh"}
	for _, s := range []string{"bash", "dash"} {
		if _, err := exec.LookPath(s); err == nil {
			shells = append(shells, s)
		}
	}
	content := binaryTarball(t, fakeBinary)
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			f := newFixture(t, content, defaultSums(content))
			out, err := f.run(t, shell, nil, "--client")
			if err != nil {
				t.Fatalf("install failed: %v\n%s", err, out)
			}
			f.assertClient(t, out, []byte(fakeBinary))
			// Rerun over the existing binary (the upgrade path), from a plain directory.
			out, err = f.run(t, shell, map[string]string{"DAVIT_RELEASE_URL": ""}, "--client", "--from", f.root)
			if err != nil {
				t.Fatalf("reinstall failed: %v\n%s", err, out)
			}
			f.assertClient(t, out, []byte(fakeBinary))
		})
	}
}

// The default install dir on PATH: no PATH note.
func TestInstallDefaultDirOnPath(t *testing.T) {
	requireTools(t)
	content := binaryTarball(t, fakeBinary)
	f := newFixture(t, content, defaultSums(content))
	def := filepath.Join(f.home, ".local", "bin")
	out, err := f.run(t, "sh", map[string]string{
		"DAVIT_INSTALL_DIR": "",
		"PATH":              def + string(os.PathListSeparator) + os.Getenv("PATH"),
	}, "--client")
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(def, "davit")); err != nil {
		t.Fatalf("not installed into $HOME/.local/bin: %v\n%s", err, out)
	}
	if strings.Contains(out, "not on your PATH") {
		t.Fatalf("unexpected PATH note:\n%s", out)
	}
}

func TestInstallFailures(t *testing.T) {
	requireTools(t)
	content := binaryTarball(t, fakeBinary)
	other := sha256Hex([]byte("something else"))

	type failCase struct {
		name      string
		published []byte // bytes actually served (content is what SHA256SUMS describes)
		sums      string // empty: defaultSums(content)
		env       map[string]string
		args      []string // nil: --client
		want      string
	}
	extra := tarball(t, tarEntry{"davit", []byte(fakeBinary), 0o755}, tarEntry{"../evil", []byte("x"), 0o644})
	cases := []failCase{
		{name: "tampered binary", published: binaryTarball(t, fakeBinary+"echo pwned\n"), want: "checksum mismatch"},
		{name: "archive with extra entries", sums: sha256Hex(extra) + "  " + assetName() + "\n", published: extra, want: "unexpected contents"},
		{name: "missing SHA256SUMS entry", sums: other + "  davit-plan9-mips\n", want: "no entry for " + assetName()},
		{name: "duplicate SHA256SUMS entry", sums: defaultSums(content) + sha256Hex(content) + "  " + assetName() + "\n", want: "expected one"},
		{name: "http release URL refused", env: map[string]string{"DAVIT_RELEASE_URL": "http://127.0.0.1:9/releases"}, want: "refusing plain http://"},
		{name: "path traversal version", args: []string{"--client", "--version", "../x"}, want: "invalid version"},
		{name: "missing release", env: map[string]string{"DAVIT_RELEASE_URL": "file:///nonexistent/davit-release"}, want: "download failed"},
		{name: "unknown argument", args: []string{"--client", "--frobnicate"}, want: "unknown argument"},
	}
	if runtime.GOOS == "linux" {
		cases = append(cases, failCase{name: "no role on Linux", args: []string{}, want: "choose a role"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sums := tc.sums
			if sums == "" {
				sums = defaultSums(content)
			}
			f := newFixture(t, content, sums)
			if tc.published != nil {
				f.publish(t, assetName(), tc.published)
			}
			args := tc.args
			if args == nil {
				args = []string{"--client"}
			}
			out, err := f.run(t, "sh", tc.env, args...)
			f.assertFailedClean(t, out, err, tc.want)
		})
	}
}

// install.sh's REPO default matches buildinfo.Repo (release.sh stamps it from
// there; the checked-in copy must agree so it works unstamped too).
func TestInstallRepoMatchesBuildinfo(t *testing.T) {
	b, err := os.ReadFile(installScript(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\nREPO="+buildinfo.Repo+"\n") {
		t.Fatalf("install.sh REPO differs from buildinfo.Repo %q", buildinfo.Repo)
	}
}
