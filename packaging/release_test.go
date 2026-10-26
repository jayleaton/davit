//go:build linux

package packaging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"davit/internal/config"
	"davit/internal/runtime/firecracker"
)

// TestReleaseInstallAndBoot installs a real local release (`make release`) into a throwaway
// HOME: the binary with the release's own install.sh (--client, which needs no systemd), the
// image bundle unpacked as install.sh --host does. With DAVIT_TEST_FIRECRACKER=1 it then boots
// one VM from the bundled image using only what was installed: the installed davit daemon and
// CLI, the installed image and its baked guest agent. (scripts/fresh-host-e2e.sh runs the
// full --host install with its service.) Opt-in (needs a release, GiBs of disk, KVM):
//
//	DAVIT_TEST_RELEASE=<repo>/.build/release   the release root (latest + <version>/)
//	DAVIT_TEST_RELEASE_IMAGE                   image to install (default ubuntu-24.04-base)
//	DAVIT_TEST_SCRATCH                         reflink-capable work dir, never tmpfs
//	                                             (default ~/.cache/davit-test)
func TestReleaseInstallAndBoot(t *testing.T) {
	rel := os.Getenv("DAVIT_TEST_RELEASE")
	if rel == "" {
		t.Skip("set DAVIT_TEST_RELEASE=<repo>/.build/release (run `make release` first)")
	}
	image := envOr("DAVIT_TEST_RELEASE_IMAGE", "ubuntu-24.04-base")
	home, _ := os.UserHomeDir()
	scratch := envOr("DAVIT_TEST_SCRATCH", filepath.Join(home, ".cache", "davit-test"))
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	hdir, err := os.MkdirTemp(scratch, "release-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(hdir) })
	latest, err := os.ReadFile(filepath.Join(rel, "latest"))
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(latest))

	// Install exactly as a user would: the release's own install.sh, a plain directory URL.
	tmp := filepath.Join(hdir, "tmp")
	os.MkdirAll(tmp, 0o700)
	env := []string{"HOME=" + hdir, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + tmp}
	dir := filepath.Join(rel, version)
	inst := exec.Command("sh", filepath.Join(dir, "install.sh"), "--client", "--from", dir)
	inst.Env = env
	if out, err := inst.CombinedOutput(); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	bin := filepath.Join(hdir, ".local", "bin", "davit")
	data := filepath.Join(hdir, ".local", "share", "davit")
	if err := os.MkdirAll(filepath.Join(data, "images"), 0o700); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "davit-image-"+image+"-x86_64.tar.zst")
	unpack := exec.Command("sh", "-c", `zstd -dcq "$1" | tar -xf - -C "$2" --no-same-owner`, "sh", bundle, filepath.Join(data, "images"))
	if out, err := unpack.CombinedOutput(); err != nil {
		t.Fatalf("unpack %s: %v\n%s", bundle, err, out)
	}
	manifest := filepath.Join(data, "images", "manifests", image+".json")
	var m struct {
		Kernel struct {
			Release string `json:"release"`
		} `json:"kernel"`
		Guest struct {
			Agent struct {
				Version string `json:"version"`
			} `json:"agent"`
		} `json:"guest"`
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("installed manifest: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), version) {
		t.Fatalf("installed binary version: %v %q, want %s", err, out, version)
	}
	t.Logf("installed davit %s and %s (agent %s)", version, image, m.Guest.Agent.Version)

	if os.Getenv("DAVIT_TEST_FIRECRACKER") != "1" {
		t.Log("install verified; set DAVIT_TEST_FIRECRACKER=1 to also boot a VM from the bundled image")
		return
	}
	ctx := context.Background()
	fc, err := firecracker.EnsureBinary(ctx, filepath.Join(scratch, "bin"), firecracker.FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "slwrel") // short: sun_path is 108 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	cfg := config.Defaults()
	cfg.DataDir, cfg.Socket, cfg.Runtime, cfg.LogLevel = data, filepath.Join(sockDir, "s.sock"), "firecracker", "debug"
	cfg.Firecracker.Binary = fc.Path // Firecracker is a separately versioned dependency, not in the release
	cfgPath := filepath.Join(hdir, "config.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var log syncLog
	daemon := exec.Command(bin, "daemon", "--config", cfgPath)
	daemon.Env, daemon.Stdout, daemon.Stderr = env, &log, &log
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		daemon.Process.Signal(syscall.SIGTERM)
		daemon.Wait()
		killVMMs(data)
		if t.Failed() {
			t.Logf("daemon log:\n%s", log.String())
		}
	})
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if c, err := net.Dial("unix", cfg.Socket); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not start:\n%s", log.String())
		}
	}
	cli := func(args ...string) string {
		t.Helper()
		c := exec.Command(bin, args...)
		c.Env = append(env, "DAVIT_API_URL=unix://"+cfg.Socket, "DAVIT_CURRENT_ID=")
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("davit %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	start := time.Now()
	cli("new", "rel", "--image", image, "--cpus=1", "--memory=512MiB")
	t.Logf("new (golden capture + barrier) took %v", time.Since(start).Round(time.Millisecond))
	if got := cli("exec", "rel", "--", "uname", "-r"); got != m.Kernel.Release {
		t.Fatalf("guest kernel %q, manifest says %q", got, m.Kernel.Release)
	}
	if got := cli("exec", "rel", "--", "id", "-un"); got != "davit" {
		t.Fatalf("default guest user %q, want davit", got)
	}
	cli("delete", "rel", "--yes")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// killVMMs kills any process whose working directory is under dir (the daemon's
// Firecracker processes), so a failed test never leaks VMs.
func killVMMs(dir string) {
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil || !strings.HasPrefix(cwd, dir+string(os.PathSeparator)) {
			continue
		}
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// syncLog collects a subprocess's output while the test may read it.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
