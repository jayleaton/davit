//go:build linux

package packaging_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeHostBinary stands in for davit in host-mode tests: it logs every call to
// $FAKE_LOG, `init --data-dir D` writes a config naming D, `doctor --fixes`
// prints $FAKE_FIXES, and `doctor` fails when $FAKE_DOCTOR_FAIL is set.
const fakeHostBinary = `#!/bin/sh
echo "davit $*" >>"$FAKE_LOG"
case "$1" in
version) echo "davit v0.0.1-test" ;;
capabilities) exit 0 ;;
init)
	cfg= data=
	while [ $# -gt 0 ]; do
		case "$1" in
		--config) cfg=$2; shift ;;
		--data-dir) data=$2; shift ;;
		esac
		shift
	done
	if [ -n "$data" ]; then
		mkdir -p "$(dirname "$cfg")" "$data"
		printf '{\n  "dataDir": "%s",\n  "runtime": "firecracker"\n}\n' "$data" >"$cfg"
	fi
	;;
doctor)
	case " $* " in
	*" --fixes "*) [ -z "$FAKE_FIXES" ] || cat "$FAKE_FIXES" ;;
	*) [ -z "$FAKE_DOCTOR_FAIL" ] || { echo "kvm FAIL"; exit 6; } ; echo "davit doctor: ready" ;;
	esac
	;;
esac
`

// hostFixture is a fixture plus stub sudo, systemctl, loginctl and sg on PATH
// that log their calls; systemctl keeps the service's active state in a file.
type hostFixture struct {
	*fixture
	stubs, log, fixes, data string
	env                     map[string]string
}

func newHostFixture(t *testing.T) *hostFixture {
	t.Helper()
	for _, tool := range []string{"zstd", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	bin := binaryTarball(t, fakeHostBinary)
	imgArch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	kernel, rootfs := []byte("kernel bytes"), []byte("rootfs bytes")
	bundle := imageBundle(t, "ubuntu-24.04-dev", map[string][]byte{sha256Hex(kernel): kernel, sha256Hex(rootfs): rootfs})
	bname := "davit-image-ubuntu-24.04-dev-" + imgArch + ".tar.zst"
	f := newFixture(t, bin, sha256Hex(bin)+"  "+assetName()+"\n"+sha256Hex(bundle)+"  "+bname+"\n")
	f.publish(t, bname, bundle)
	h := &hostFixture{fixture: f, stubs: filepath.Join(f.home, "stubs"), log: filepath.Join(f.home, "calls.log"),
		fixes: filepath.Join(f.home, "fixes"), data: filepath.Join(f.home, "data dir")}
	state := filepath.Join(f.home, "service-active")
	linger := filepath.Join(f.home, "linger")
	stubs := map[string]string{
		"sudo": `echo "sudo $*" >>"$FAKE_LOG"`,
		"sg":   `echo "sg $1" >>"$FAKE_LOG"; exec sh -c "$3"`,
		"systemctl": `echo "systemctl $*" >>"$FAKE_LOG"
case "$*" in
*is-active*) [ -e "` + state + `" ] ;;
*restart*) touch "` + state + `" ;;
esac`,
		"loginctl": `echo "loginctl $*" >>"$FAKE_LOG"
case "$*" in
show-user*) [ -e "` + linger + `" ] && echo yes || echo no ;;
enable-linger*) touch "` + linger + `" ;;
esac`,
		"journalctl": `exit 0`,
	}
	if err := os.MkdirAll(h.stubs, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(h.stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h.env = map[string]string{
		"PATH":     h.stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"FAKE_LOG": h.log, "XDG_CONFIG_HOME": filepath.Join(f.home, ".config"),
		"XDG_DATA_HOME": "", "DAVIT_DATA_DIR": "", "DAVIT_IMAGES": "",
	}
	return h
}

func (h *hostFixture) runHost(t *testing.T, extraEnv map[string]string, args ...string) (string, error) {
	t.Helper()
	env := map[string]string{}
	for k, v := range h.env {
		env[k] = v
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	os.Remove(h.log)
	return h.run(t, "sh", env, append([]string{"--host", "--data-dir", h.data}, args...)...)
}

func (h *hostFixture) calls(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(h.log)
	return string(b)
}

func (h *hostFixture) manifest() string {
	return filepath.Join(h.data, "images", "manifests", "ubuntu-24.04-dev.json")
}

// A fresh host: root fixes come before any image download; without --yes-sudo
// they are printed and the install stops; with it they run, then the image,
// service, lingering and doctor follow; a rerun is an idempotent upgrade.
func TestInstallHost(t *testing.T) {
	requireTools(t)
	h := newHostFixture(t)
	fix := "# kvm: /dev/kvm is not writable by this user (group kvm)\nsudo usermod -aG kvm tester\n"
	if err := os.WriteFile(h.fixes, []byte(fix), 0o644); err != nil {
		t.Fatal(err)
	}
	withFixes := map[string]string{"FAKE_FIXES": h.fixes}

	out, err := h.runHost(t, withFixes)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("want exit 3 (root fixes needed), got %v\n%s", err, out)
	}
	if !strings.Contains(out, "sudo usermod -aG kvm tester") || !strings.Contains(out, "--yes-sudo") {
		t.Fatalf("fix not shown:\n%s", out)
	}
	first := h.calls(t)
	if strings.Contains(first, "sudo ") {
		t.Fatalf("ran sudo without --yes-sudo:\n%s", first)
	}
	cfg := filepath.Join(h.home, ".config", "davit", "config.json")
	if !strings.Contains(first, "davit init --config "+cfg+" --data-dir "+h.data+" --runtime firecracker") {
		t.Fatalf("a new install must write a firecracker config:\n%s", first)
	}
	if _, err := os.Stat(h.manifest()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image downloaded before the prerequisites were fixed (stat err=%v)", err)
	}

	out, err = h.runHost(t, withFixes, "--yes-sudo")
	if err != nil {
		t.Fatalf("host install: %v\n%s", err, out)
	}
	calls := h.calls(t)
	wants := []string{"sudo usermod -aG kvm tester", "davit init --config " + cfg + " --service",
		"systemctl --user restart davit.service", "davit doctor --config " + cfg}
	if u, err := user.Current(); err == nil {
		if _, err := os.Stat("/var/lib/systemd/linger/" + u.Username); err != nil {
			wants = append(wants, "loginctl enable-linger")
		}
	}
	for _, want := range wants {
		if !strings.Contains(calls, want) {
			t.Fatalf("missing call %q:\n%s\noutput:\n%s", want, calls, out)
		}
	}
	if strings.Index(calls, "sudo usermod") > strings.Index(calls, "--service") {
		t.Fatalf("service installed before the root fixes:\n%s", calls)
	}
	if _, err := os.Stat(h.manifest()); err != nil {
		t.Fatalf("default image not installed: %v\n%s", err, out)
	}
	for _, want := range []string{"this host is ready", "sh -s -- --client", "davit box run -- make test"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	h.assertTmpClean(t)

	// Rerun: nothing to fix, image unchanged, binary unchanged: no restart, no new download.
	out, err = h.runHost(t, nil)
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	calls = h.calls(t)
	if !strings.Contains(out, "image ubuntu-24.04-dev: up to date") || strings.Contains(calls, "restart") ||
		strings.Contains(calls, "--runtime") {
		t.Fatalf("rerun was not a no-op:\n%s\ncalls:\n%s", out, calls)
	}
}

// A failing doctor at the end fails the install.
func TestInstallHostDoctorFails(t *testing.T) {
	requireTools(t)
	h := newHostFixture(t)
	out, err := h.runHost(t, map[string]string{"FAKE_DOCTOR_FAIL": "1"})
	if err == nil || !strings.Contains(out, "doctor reports a problem") {
		t.Fatalf("want a doctor failure, got %v\n%s", err, out)
	}
}

func TestInstallHostImages(t *testing.T) {
	requireTools(t)
	imgArch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	kernel := []byte("kernel bytes")
	for _, tc := range []struct {
		name   string
		bundle func() []byte
		want   string
	}{
		{"blob not matching its digest", func() []byte {
			return imageBundle(t, "ubuntu-24.04-dev", map[string][]byte{sha256Hex(kernel): []byte("tampered")})
		}, "does not match its digest"},
		{"path outside the bundle layout", func() []byte {
			return imageBundle(t, "ubuntu-24.04-dev", map[string][]byte{sha256Hex(kernel): kernel}, tarEntry{"../escape", []byte("x"), 0o644})
		}, "unexpected path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostFixture(t)
			bundle := tc.bundle()
			bname := "davit-image-ubuntu-24.04-dev-" + imgArch + ".tar.zst"
			bin, _ := os.ReadFile(filepath.Join(h.root, assetName()))
			h.publish(t, bname, bundle)
			h.publish(t, "SHA256SUMS", []byte(sha256Hex(bin)+"  "+assetName()+"\n"+sha256Hex(bundle)+"  "+bname+"\n"))
			out, err := h.runHost(t, nil)
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("want failure %q, got err=%v\n%s", tc.want, err, out)
			}
			if _, err := os.Stat(h.manifest()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("manifest must not be installed on failure (stat err=%v)", err)
			}
			if ents, _ := filepath.Glob(filepath.Join(h.data, "images", ".install.*")); len(ents) != 0 {
				t.Fatalf("staging left behind: %v", ents)
			}
			if strings.Contains(h.calls(t), "--service") {
				t.Fatalf("service installed after a failed image install")
			}
		})
	}
}

// imageBundle builds a zstd-compressed image bundle (release.sh format) with
// manifests/<name>.json and the given blobs (name -> body; name should be the
// body's sha256 unless the test wants a mismatch).
func imageBundle(t *testing.T, name string, blobs map[string][]byte, extra ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(e tarEntry) {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	add(tarEntry{"manifests/" + name + ".json", []byte(`{"name":"` + name + `"}`), 0o644})
	for h, body := range blobs {
		add(tarEntry{"blobs/sha256/" + h, body, 0o444})
	}
	for _, e := range extra {
		add(e)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("zstd", "-q", "-c")
	cmd.Stdin = &buf
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	return out
}

// A release without an image for this architecture (arm64 hosts today) is
// refused before anything is installed.
func TestInstallHostNoImageForArch(t *testing.T) {
	requireTools(t)
	h := newHostFixture(t)
	bin, _ := os.ReadFile(filepath.Join(h.root, assetName()))
	h.publish(t, "SHA256SUMS", []byte(sha256Hex(bin)+"  "+assetName()+"\n"))
	out, err := h.runHost(t, nil)
	h.assertFailedClean(t, out, err, "hosts are x86_64 only")
}
