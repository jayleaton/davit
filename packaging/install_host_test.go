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
		// Free space is the fixture's, not the test machine's (CI runners
		// have little): FAKE_DF_AVAIL_KIB, default 100 GiB.
		"df": `echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
echo "fake 209715200 0 ${FAKE_DF_AVAIL_KIB:-104857600} 0% /"`,
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

// TestInstallHostUpgradesService: an installed davit.service whose binary and
// config are not the defaults is the install a rerun upgrades: the binary is
// replaced where the unit runs it, its config and data dir are kept (no new
// init), the daemon is awaited on the socket that config names, the replaced
// image's manifest is kept by ID (images/retired) while its rootfs blob goes
// and its kernel stays; an
// --install-dir that disagrees with the unit is refused before anything changes.
func TestInstallHostUpgradesService(t *testing.T) {
	requireTools(t)
	h := newHostFixture(t)
	xdg := h.env["XDG_CONFIG_HOME"]
	bin := filepath.Join(h.home, ".svm", "davit", "bin", "davit")
	cfg := filepath.Join(xdg, "davit-host", "config.json")
	data := filepath.Join(h.home, ".svm", "old data")
	oldRootfs, kernel, oldID := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("d", 64)
	manifest := filepath.Join(data, "images", "manifests", "ubuntu-24.04-dev.json")
	for p, body := range map[string]string{
		bin: "#!/bin/sh\necho old\n",
		cfg: "{\n  \"dataDir\": \"" + data + "\",\n  \"socket\": \"" + data + "/davit.sock\",\n  \"images\": [\n    {\n      \"manifest\": \"" + manifest + "\"\n    }\n  ]\n}\n",
		filepath.Join(xdg, "systemd", "user", "davit.service"): "[Service]\nExecStart=\"" + bin + "\" daemon --config \"" + cfg + "\"\n",
		manifest: "{\n  \"id\": \"sha256:" + oldID + "\",\n  \"kernel\": {\n    \"digest\": \"sha256:" + kernel + "\"\n  },\n  \"rootfs\": {\n    \"digest\": \"sha256:" + oldRootfs + "\",\n" +
			"    \"packageList\": {\n      \"digest\": \"sha256:" + strings.Repeat("c", 64) + "\"\n    }\n  }\n}\n",
		filepath.Join(data, "images", "blobs", "sha256", oldRootfs): "old rootfs",
		filepath.Join(data, "images", "blobs", "sha256", kernel):    "old kernel",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	keepCfg, _ := os.ReadFile(cfg)

	out, err := h.runHost(t, nil, "--install-dir", filepath.Join(h.home, ".local", "bin"))
	if err == nil || !strings.Contains(out, "rerun without --install-dir") {
		t.Fatalf("want a refusal of the other --install-dir, got %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(bin); string(b) != "#!/bin/sh\necho old\n" {
		t.Fatal("the refused install replaced the binary")
	}
	out, err = h.runHost(t, map[string]string{"DAVIT_INSTALL_DIR": "", "FAKE_DF_AVAIL_KIB": "3145728"})
	if err == nil || !strings.Contains(out, "this upgrade (nothing was changed) stops here") {
		t.Fatalf("want a low-disk refusal of the upgrade, got %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(bin); string(b) != "#!/bin/sh\necho old\n" {
		t.Fatal("a low-disk refusal replaced the binary")
	}

	out, err = h.runHost(t, map[string]string{"DAVIT_INSTALL_DIR": ""})
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(bin); !strings.Contains(string(b), "FAKE_LOG") {
		t.Fatalf("binary not upgraded in place at %s\n%s", bin, out)
	}
	if b, _ := os.ReadFile(cfg); string(b) != string(keepCfg) {
		t.Fatalf("config changed:\n%s", b)
	}
	calls := h.calls(t)
	for _, want := range []string{"davit init --config " + cfg + " --service", "davit doctor --config " + cfg,
		"davit --api-url unix://" + data + "/davit.sock capabilities"} {
		if !strings.Contains(calls, want) {
			t.Fatalf("missing call %q:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "--data-dir") {
		t.Fatalf("an upgrade ran a new init:\n%s", calls)
	}
	if _, err := os.Stat(filepath.Join(xdg, "davit", "config.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a second config was written (stat err=%v)", err)
	}
	if _, err := os.Stat(h.manifest()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the image went to --data-dir, not the existing config's data dir")
	}
	if b, _ := os.ReadFile(manifest); !strings.Contains(string(b), `"name":"ubuntu-24.04-dev"`) {
		t.Fatalf("image not replaced in the existing data dir:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(data, "images", "blobs", "sha256", oldRootfs)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the replaced rootfs blob was kept")
	}
	if b, _ := os.ReadFile(filepath.Join(data, "images", "retired", oldID+".json")); !strings.Contains(string(b), oldRootfs) {
		t.Fatalf("the replaced manifest was not kept in images/retired/<id>.json: %q", b)
	}
	if _, err := os.Stat(filepath.Join(data, "images", "blobs", "sha256", kernel)); err != nil {
		t.Fatal("the old kernel blob was removed")
	}
	for _, want := range []string{"keeping the data dir of the existing config", "doctor --config '" + cfg + "'"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
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

// TestInstallHostLowDisk: too little room for the image stops the install
// before anything is installed or downloaded, naming the data dir and --data-dir.
func TestInstallHostLowDisk(t *testing.T) {
	requireTools(t)
	h := newHostFixture(t)
	out, err := h.runHost(t, map[string]string{"FAKE_DF_AVAIL_KIB": "3145728"})
	if err == nil || !strings.Contains(out, "has 3 GiB free") || !strings.Contains(out, "--data-dir") {
		t.Fatalf("want a low-disk refusal, got %v\n%s", err, out)
	}
	if strings.Contains(out, "image ubuntu-24.04-dev") {
		t.Fatalf("an image was installed despite the refusal:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(h.installDir, "davit")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused install left a binary (stat err=%v)", err)
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
