// Package fctest is the real-VM fixture shared by Firecracker integration
// tests (adapter conformance, CLI end to end, benchmarks). Those tests run
// only with DAVIT_TEST_FIRECRACKER=1 on a separate KVM host, through
// scripts/vm-tier.sh (docs/vm-tier.md), so `make check` stays fast and
// hermetic. Inputs:
//
//	DAVIT_TEST_IMAGE_MANIFEST  images/manifests/<image>.json (required)
//	DAVIT_TEST_IMAGE_BLOBS     its blob dir (default <manifest>/../../out/blobs)
//	DAVIT_TEST_SCRATCH         work dir on the store's reflink filesystem,
//	                             never tmpfs (default ~/.cache/davit-test)
//
// The base image ships without the guest agent. The fixture builds the
// agent from this checkout (the static davit binary, which runs as the
// agent when invoked as davit-guest-agent) and derives a test image with
// it installed, in the scratch dir only. Commands then run the way the
// product runs them: restore barrier, then an authenticated exec.
package fctest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"davit/internal/domain"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
	"davit/internal/lifecycle"
	"davit/internal/runtime"
	"davit/internal/runtime/firecracker"
)

// deriveVersion changes whenever derive's recipe does; it keys the image.
const deriveVersion = "agent-2" // 2: records the injected agent's protocol

// Fixture is a verified Firecracker binary plus the derived test image.
type Fixture struct {
	Scratch  string
	Binary   firecracker.Binary
	Image    domain.Image
	Manifest string // derived manifest (for daemon configs)
	BlobDir  string
	// SourceManifest and SourceBlobDir are the image as built, with the
	// guest agent baked into it (not this checkout's).
	SourceManifest, SourceBlobDir string
}

var (
	once    sync.Once
	fixture Fixture
	fixErr  error
)

// Setup skips t unless real-VM tests are enabled, else returns the fixture
// (built once per process).
func Setup(t testing.TB) Fixture {
	t.Helper()
	if os.Getenv("DAVIT_TEST_FIRECRACKER") != "1" {
		t.Skip("real Firecracker tests disabled; set DAVIT_TEST_FIRECRACKER=1 (see fctest docs)")
	}
	once.Do(func() { fixture, fixErr = build(context.Background()) })
	if fixErr != nil {
		t.Fatalf("fctest: %v", fixErr)
	}
	return fixture
}

func build(ctx context.Context) (Fixture, error) {
	man := os.Getenv("DAVIT_TEST_IMAGE_MANIFEST")
	if man == "" {
		return Fixture{}, fmt.Errorf("DAVIT_TEST_IMAGE_MANIFEST is required")
	}
	blobs := os.Getenv("DAVIT_TEST_IMAGE_BLOBS")
	if blobs == "" {
		blobs = filepath.Join(filepath.Dir(man), "..", "out", "blobs")
	}
	scratch := os.Getenv("DAVIT_TEST_SCRATCH")
	if scratch == "" {
		home, _ := os.UserHomeDir()
		scratch = filepath.Join(home, ".cache", "davit-test")
	}
	f := Fixture{Scratch: scratch, BlobDir: filepath.Join(scratch, "images", "blobs"), SourceManifest: man, SourceBlobDir: blobs}
	var err error
	if f.Binary, err = firecracker.EnsureBinary(ctx, filepath.Join(scratch, "bin"), firecracker.FetchOptions{}); err != nil {
		return f, err
	}
	agent, err := buildDavit(ctx, filepath.Join(scratch, "agent"))
	if err != nil {
		return f, err
	}
	f.Manifest, err = derive(man, blobs, filepath.Join(scratch, "images"), agent)
	os.Remove(agent) // installed in the image (or already there)
	if err != nil {
		return f, fmt.Errorf("derive test image: %w", err)
	}
	cat, err := lifecycle.LoadManifestCatalog(ctx, filepath.Join(scratch, "images", "cache"),
		lifecycle.ManifestSource{Manifest: f.Manifest, BlobDir: f.BlobDir})
	if err != nil {
		return f, err
	}
	f.Image, err = cat.Resolve(ctx, "")
	return f, err
}

// Derive derives a test image, the way Setup does, from another image
// built beside the fixture's (its blobs in the same blob dir), e.g.
// DAVIT_TEST_CI_IMAGE_MANIFEST. The returned manifest, named after the
// image plus "-test", has its blobs in f.BlobDir.
func (f Fixture) Derive(t testing.TB, manifest string) string {
	t.Helper()
	agent, err := buildDavit(context.Background(), filepath.Join(f.Scratch, "agent"))
	if err != nil {
		t.Fatalf("fctest: %v", err)
	}
	defer os.Remove(agent)
	m, err := derive(manifest, f.SourceBlobDir, filepath.Join(f.Scratch, "images"), agent)
	if err != nil {
		t.Fatalf("fctest: derive %s: %v", manifest, err)
	}
	return m
}

// buildDavit builds a static linux/amd64 davit from this checkout.
func buildDavit(ctx context.Context, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	mod, err := exec.CommandContext(ctx, "go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	out := filepath.Join(dir, fmt.Sprintf("davit-%d", os.Getpid()))
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/davit")
	cmd.Dir = filepath.Dir(strings.TrimSpace(string(mod)))
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build davit: %v: %s", err, b)
	}
	return out, nil
}

// derive writes <out>/manifests/<name>-test.json whose rootfs is the base
// rootfs plus agent, reusing a previous derivation of the same inputs and
// deleting a superseded one.
func derive(manifest, blobs, out, agent string) (string, error) {
	// Test packages run in parallel processes: one derivation at a time.
	if err := os.MkdirAll(out, 0o700); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(filepath.Join(out, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return "", err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	name, _ := m["name"].(string)
	kernel, _ := m["kernel"].(map[string]any)
	rootfs, _ := m["rootfs"].(map[string]any)
	if kernel == nil || rootfs == nil {
		return "", fmt.Errorf("%s: no kernel/rootfs", manifest)
	}
	agentSum, err := hashFile(agent)
	if err != nil {
		return "", err
	}
	// Keyed by content, not manifest ID, so metadata-only rebuilds upstream
	// reuse the derivation.
	from := fmt.Sprintf("%v+%v+%v+agent:%s+%s", rootfs["digest"], kernel["digest"], kernel["bootArgs"], agentSum, deriveVersion)
	dst := filepath.Join(out, "manifests", name+"-test.json")
	var stale string
	if b, err := os.ReadFile(dst); err == nil {
		var prev map[string]any
		if json.Unmarshal(b, &prev) == nil {
			if prev["derivedFrom"] == from {
				return dst, nil
			}
			if r, ok := prev["rootfs"].(map[string]any); ok {
				stale, _ = r["digest"].(string)
			}
		}
	}
	blobDir := filepath.Join(out, "blobs", "sha256")
	for _, d := range []string{blobDir, filepath.Dir(dst)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
	}
	khex := strings.TrimPrefix(kernel["digest"].(string), "sha256:")
	if err := reflink(filepath.Join(blobs, "sha256", khex), filepath.Join(blobDir, khex)); err != nil && !os.IsExist(err) {
		return "", err
	}
	tmp := filepath.Join(blobDir, fmt.Sprintf(".rootfs-%d", os.Getpid()))
	os.Remove(tmp)
	if err := reflink(filepath.Join(blobs, "sha256", strings.TrimPrefix(rootfs["digest"].(string), "sha256:")), tmp); err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", err
	}
	if err := inject(tmp, agent); err != nil {
		return "", err
	}
	sum, err := hashFile(tmp)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(blobDir, sum)); err != nil {
		return "", err
	}
	rootfs["digest"] = "sha256:" + sum
	if old := strings.TrimPrefix(stale, "sha256:"); old != "" && old != sum {
		p := filepath.Join(blobDir, old)
		os.Chmod(p, 0o600)
		os.Remove(p) // superseded test rootfs: reclaim the space
	}
	// The injected agent is this checkout's: record its binary and protocol so
	// the catalog's version-skew check grades what actually runs.
	guest, _ := m["guest"].(map[string]any)
	if guest == nil {
		guest = map[string]any{}
		m["guest"] = guest
	}
	agentRec, _ := guest["agent"].(map[string]any)
	if agentRec == nil {
		agentRec = map[string]any{}
		guest["agent"] = agentRec
	}
	agentRec["binary"] = map[string]any{"digest": "sha256:" + agentSum}
	agentRec["version"] = "test-checkout"
	agentRec["protocol"] = guestclient.AgentProtocol{Min: guestproto.MinProtocolVersion, Max: guestproto.ProtocolVersion, Features: guestclient.Features}
	m["name"] = name + "-test"
	m["derivedFrom"] = from
	delete(m, "id")
	b, _ := json.Marshal(m)
	id := sha256.Sum256(b)
	m["id"] = "sha256:" + hex.EncodeToString(id[:])
	b, _ = json.MarshalIndent(m, "", "  ")
	return dst, os.WriteFile(dst, b, 0o600)
}

// inject installs agent as the image's guest agent with debugfs: at
// /usr/local/bin/davit-guest-agent, or slipway-guest-agent in an image
// built before the davit rename (its unit runs that name; the binary
// answers to both).
func inject(img, agent string) error {
	name := "davit-guest-agent"
	if out, err := exec.Command("debugfs", "-R", "stat /usr/local/bin/"+name, img).CombinedOutput(); err != nil || !strings.Contains(string(out), "Mode:") {
		if out, err := exec.Command("debugfs", "-R", "stat /usr/local/bin/"+legacyAgent, img).CombinedOutput(); err == nil && strings.Contains(string(out), "Mode:") {
			name = legacyAgent
		}
	}
	cmds := strings.Join([]string{
		"cd /usr/local/bin",
		"rm " + name,
		"write " + agent + " " + name,
		"sif " + name + " mode 0100755",
		"sif " + name + " uid 0",
		"sif " + name + " gid 0",
	}, "\n") + "\n"
	cmd := exec.Command("debugfs", "-w", "-f", "-", img)
	cmd.Stdin = strings.NewReader(cmds)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("debugfs: %v: %s", err, out)
	}
	check := exec.Command("debugfs", "-R", "stat /usr/local/bin/"+name, img)
	if out, err := check.CombinedOutput(); err != nil || !strings.Contains(string(out), "Mode:  0755") {
		return fmt.Errorf("agent not installed: %v: %s", err, out)
	}
	return nil
}

// legacyAgent is the guest agent's name in images built before the rename.
const legacyAgent = "slipway-guest-agent"

func reflink(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return os.ErrExist
	}
	return Clone(src, dst, 0)
}

// Clone makes dst a reflink of src, grown to size if larger.
func Clone(src, dst string, size int64) error {
	out, err := exec.Command("cp", "--reflink=always", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("reflink %s: %v: %s (DAVIT_TEST_SCRATCH must be on the image's reflink filesystem)", src, err, out)
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return err
	}
	if st, err := os.Stat(dst); err == nil && size > st.Size() {
		return os.Truncate(dst, size)
	}
	return nil
}
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var (
	secrets = &guestclient.MemorySecrets{}
	barMu   sync.Mutex
	barred  = map[string]bool{} // process identities that passed the barrier
)

// Ready runs the restore barrier for h once per VMM process (as the
// daemon's prober does), after which the agent accepts workload RPCs for
// that generation. Keys repeat across tests, so secrets from an earlier
// instance of the same key are discarded first.
func Ready(ctx context.Context, rt runtime.Runtime, h runtime.Handle) error {
	barMu.Lock()
	defer barMu.Unlock()
	if barred[h.ProcessIdentity] {
		return nil
	}
	if err := secrets.Save(ctx, h.Key, nil); err != nil {
		return err
	}
	if _, err := (&guestclient.Prober{Secrets: secrets}).Probe(ctx, rt, h); err != nil {
		return err
	}
	barred[h.ProcessIdentity] = true
	return nil
}

// Sh runs script with /bin/sh as root (requested explicitly: exec defaults to
// the image's user) and returns
// its combined output; a non-zero exit is an error.
func Sh(ctx context.Context, rt runtime.Runtime, h runtime.Handle, script string) (string, error) {
	if err := Ready(ctx, rt, h); err != nil {
		return "", err
	}
	d, err := rt.GuestTransport(ctx, h)
	if err != nil {
		return "", err
	}
	return sh(ctx, h.Key, d, script)
}

func sh(ctx context.Context, key runtime.Key, d runtime.Dialer, script string) (string, error) {
	c, err := connect(ctx, key, d)
	if err != nil {
		return "", err
	}
	defer c.Close()
	var out bytes.Buffer
	res, err := c.Exec(ctx, guestproto.Exec{Argv: []string{"/bin/sh", "-c", script}, User: "root"}, guestclient.ExecIO{Stdout: &out, Stderr: &out})
	if err != nil {
		return out.String(), err
	}
	if res.ExitCode != 0 {
		return strings.TrimSpace(out.String()), fmt.Errorf("guest command exited %d (%s): %s", res.ExitCode, res.Signal, out.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func connect(ctx context.Context, key runtime.Key, d runtime.Dialer) (*guestclient.Client, error) {
	s, err := secrets.Load(ctx, key)
	if err != nil {
		return nil, err
	}
	return guestclient.Connect(ctx, func(ctx context.Context) (net.Conn, error) { return d.DialGuest(ctx, firecracker.AgentPort) },
		guestclient.Session{VMID: string(key.VMID), Generation: uint64(key.Generation), Secrets: s})
}

// Shutdown is the adapter's GuestShutdown hook for tests that run the
// runtime without a daemon: barrier if this generation never had one, then
// the agent's Shutdown, as the daemon does.
func Shutdown(ctx context.Context, key runtime.Key, d runtime.Dialer) error {
	c, err := connect(ctx, key, d)
	if err != nil {
		// No barrier yet for this instance (or a stale secret): run it.
		dial := func(ctx context.Context) (net.Conn, error) { return d.DialGuest(ctx, firecracker.AgentPort) }
		if err := secrets.Save(ctx, key, nil); err != nil {
			return err
		}
		if _, err := (&guestclient.Prober{Secrets: secrets}).Barrier(ctx, key, dial, nil); err != nil {
			return err
		}
		if c, err = connect(ctx, key, d); err != nil {
			return err
		}
	}
	defer c.Close()
	return c.Shutdown(ctx, 0)
}
