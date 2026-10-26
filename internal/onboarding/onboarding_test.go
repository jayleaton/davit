package onboarding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"davit/internal/config"
	"davit/internal/preflight"
)

func kvm(ok bool) func() preflight.Check {
	return func() preflight.Check {
		if ok {
			return preflight.Check{ID: "kvm", Status: preflight.Pass, Detail: "ok"}
		}
		return preflight.Check{ID: "kvm", Status: preflight.Fail, Detail: "/dev/kvm is not writable by this user (group kvm)",
			Hint: "add alice to group kvm", Sudo: []string{"sudo usermod -aG kvm alice"}}
	}
}

type systemctl struct{ calls []string }

func (s *systemctl) run(_ context.Context, args ...string) error {
	s.calls = append(s.calls, strings.Join(args, " "))
	return nil
}

func opts(t *testing.T) Options {
	dir := t.TempDir()
	return Options{
		ConfigPath: filepath.Join(dir, "config", "davit", "config.json"),
		DataDir:    filepath.Join(dir, "data", "davit"),
		UnitDir:    filepath.Join(dir, "config", "systemd", "user"),
		Binary:     "/opt/davit/bin/davit",
		// No KVM here, so init only runs with an explicit --runtime fake.
		Runtime: "fake", KVM: kvm(false), SupportedRuntimes: []string{"fake"},
		Systemctl: (&systemctl{}).run,
		User:      "alice", Linger: func(context.Context, string) bool { return true },
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestInitFreshThenIdempotent(t *testing.T) {
	o := opts(t)
	r, err := Init(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ConfigWritten || r.Runtime != "fake" || !strings.Contains(strings.Join(r.Warnings, " "), FakeRuntimeWarning) {
		t.Fatalf("fresh init: %+v", r)
	}
	if mode(t, o.ConfigPath) != 0o600 || mode(t, o.DataDir) != 0o700 || mode(t, filepath.Join(o.DataDir, "tailnet")) != 0o700 {
		t.Fatal("config must be 0600 and data/tailnet dirs 0700")
	}
	cfg, err := config.Load(o.ConfigPath)
	if err != nil || cfg.DataDir != o.DataDir || cfg.Tailnet.AuthKeyFile == "" {
		t.Fatalf("written config does not load: %+v %v", cfg, err)
	}
	// Tailscale is optional: one closing line, after the steps that matter.
	if len(r.NextSteps) < 3 || !strings.HasPrefix(r.NextSteps[len(r.NextSteps)-1], "Optional: reach this host over Tailscale: davit init --owner-login") ||
		strings.Contains(strings.Join(r.NextSteps[:len(r.NextSteps)-1], "\n"), "ailnet") {
		t.Fatalf("next steps must end with the optional tailnet line only: %v", r.NextSteps)
	}

	before, _ := os.ReadFile(o.ConfigPath)
	o.Runtime = "fake"
	r2, err := Init(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(o.ConfigPath)
	if r2.ConfigWritten || string(before) != string(after) || len(r2.Warnings) == 0 {
		t.Fatalf("re-run must keep the config and warn that flags need --force: %+v", r2)
	}
}

func TestInitForceBacksUpAndRegenerates(t *testing.T) {
	o := opts(t)
	os.MkdirAll(filepath.Dir(o.ConfigPath), 0o700)
	os.WriteFile(o.ConfigPath, []byte("{not json"), 0o600)
	if _, err := Init(context.Background(), o); err == nil {
		t.Fatal("an invalid user config must not be overwritten without --force")
	}
	o.Force = true
	r, err := Init(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(r.ConfigBackup); string(b) != "{not json" || !r.ConfigWritten {
		t.Fatalf("backup %q: %q", r.ConfigBackup, b)
	}
}

func TestRuntimeChoice(t *testing.T) {
	for name, tc := range map[string]struct {
		kvm       bool
		supported []string
		explicit  string
		want      string
		err       bool
	}{
		"kvm and firecracker built": {true, []string{"fake", "firecracker"}, "", "firecracker", false},
		"kvm, no firecracker yet":   {true, []string{"fake"}, "", "", true},
		"no kvm":                    {false, []string{"fake", "firecracker"}, "", "", true},
		"explicit unsupported":      {true, []string{"fake"}, "firecracker", "", true},
		"explicit fake":             {true, []string{"fake", "firecracker"}, "fake", "fake", false},
		"explicit fake, no kvm":     {false, []string{"fake", "firecracker"}, "fake", "fake", false},
	} {
		o := opts(t)
		o.KVM, o.SupportedRuntimes, o.Runtime = kvm(tc.kvm), tc.supported, tc.explicit
		r, err := Init(context.Background(), o)
		if goruntime.GOOS != "linux" && tc.explicit == "" && !tc.kvm {
			continue // no KVM by design: the fake runtime, as before
		}
		if tc.err != (err != nil) || (err == nil && r.Runtime != tc.want) {
			t.Errorf("%s: %+v %v", name, r, err)
		}
		if err == nil && r.Runtime == "fake" && !strings.Contains(strings.Join(r.Warnings, " "), FakeRuntimeWarning) {
			t.Errorf("%s: the fake runtime must be labelled", name)
		}
		if err != nil && tc.explicit == "" && !strings.Contains(err.Error(), "--runtime fake") {
			t.Errorf("%s: the error must name the development-only escape: %v", name, err)
		}
	}
	if goruntime.GOOS == "linux" {
		o := opts(t)
		o.Runtime, o.SupportedRuntimes = "", []string{"fake", "firecracker"}
		_, err := Init(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "/dev/kvm is not writable by this user (group kvm); fix: sudo usermod -aG kvm alice") {
			t.Fatalf("no KVM must fail with the check's detail and fix: %v", err)
		}
		if _, statErr := os.Stat(o.ConfigPath); statErr == nil {
			t.Fatal("a refused init must not write a config")
		}
	}
}

func TestAuthKeyIsStoredPrivatelyAndNeverReported(t *testing.T) {
	o := opts(t)
	o.AuthKey = []byte("tskey-auth-kSECRET123\n")
	r, err := Init(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.AuthKeyWritten || mode(t, r.AuthKeyFile) != 0o600 {
		t.Fatalf("auth key: %+v", r)
	}
	if b, _ := os.ReadFile(r.AuthKeyFile); string(b) != "tskey-auth-kSECRET123\n" {
		t.Fatalf("stored key %q", b)
	}
	out, _ := json.Marshal(r)
	if strings.Contains(string(out), "SECRET") {
		t.Fatal("auth key leaked into the result")
	}
	if strings.Contains(strings.Join(r.NextSteps, " "), "auth-key-stdin") {
		t.Fatal("next steps still ask for a key that was just stored")
	}
	o.AuthKey = []byte("two\nlines")
	if _, err := Init(context.Background(), o); err == nil {
		t.Fatal("multi-line key accepted")
	}
}

func TestServiceInstall(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd units are Linux-only")
	}
	o := opts(t)
	sc := &systemctl{}
	o.Service, o.Systemctl = true, sc.run
	r, err := Init(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	unit, _ := os.ReadFile(r.ServiceUnit)
	for _, want := range []string{`ExecStart="/opt/davit/bin/davit" daemon --config "` + o.ConfigPath + `"`, "KillMode=process", "WantedBy=default.target"} {
		if !strings.Contains(string(unit), want) {
			t.Fatalf("unit lacks %q:\n%s", want, unit)
		}
	}
	if !r.ServiceStarted || strings.Join(sc.calls, ";") != "daemon-reload;enable --now davit.service" {
		t.Fatalf("systemctl calls %v, result %+v", sc.calls, r)
	}
	if steps := strings.Join(r.NextSteps, "\n"); strings.Contains(steps, "linger") {
		t.Fatalf("lingering is on: %v", r.NextSteps)
	}
	o.Linger = func(context.Context, string) bool { return false }
	if r, _ := Init(context.Background(), o); !strings.Contains(strings.Join(r.NextSteps, "\n"), "sudo loginctl enable-linger alice") {
		t.Fatalf("lingering off: %v", r.NextSteps)
	}

	os.WriteFile(r.ServiceUnit, append(unit, []byte("# local edit\n")...), 0o644)
	if r2, _ := Init(context.Background(), o); !strings.Contains(strings.Join(r2.Warnings, " "), "kept") {
		t.Fatalf("an edited unit must be kept without --force: %+v", r2)
	}
	o.Systemctl = func(context.Context, ...string) error { return errors.New("Failed to connect to bus") }
	r3, err := Init(context.Background(), o)
	if err != nil || r3.ServiceStarted || !strings.Contains(strings.Join(r3.NextSteps, " "), "systemctl --user") {
		t.Fatalf("no user bus must degrade to instructions: %+v %v", r3, err)
	}
}

func TestInitTailnetOwnerEnablesGateway(t *testing.T) {
	t.Setenv(config.AuthKeyEnv, "")
	o := opts(t)
	ctx := context.Background()
	load := func() config.Tailnet {
		t.Helper()
		cfg, err := config.Load(o.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Tailnet
	}

	// Owner without a key: stored, gateway stays off, next steps ask for a key.
	o.OwnerLogins = []string{"alice@example.com"}
	r, err := Init(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if tn := load(); tn.Enabled || r.TailnetEnabled || len(tn.OwnerLogins) != 1 {
		t.Fatalf("owner without key: %+v %+v", tn, r)
	}
	if steps := strings.Join(r.NextSteps, "\n"); !strings.Contains(steps, "auth-key-stdin") {
		t.Fatalf("next steps: %v", r.NextSteps)
	}

	// Adding the key to the existing config enables the gateway.
	o.OwnerLogins, o.AuthKey = nil, []byte("tskey-auth-x\n")
	r, err = Init(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if tn := load(); !tn.Enabled || !r.TailnetEnabled || !r.ConfigWritten || r.ConfigBackup != "" {
		t.Fatalf("owner + key: %+v %+v", tn, r)
	}
	if steps := strings.Join(r.NextSteps, "\n"); !strings.Contains(steps, "Tailnet gateway enabled for alice@example.com") {
		t.Fatalf("next steps: %v", r.NextSteps)
	}

	// Plain re-runs and a repeated owner (any case) change nothing.
	before, _ := os.ReadFile(o.ConfigPath)
	o.AuthKey = nil
	for _, logins := range [][]string{nil, {"ALICE@example.com"}} {
		o.OwnerLogins = logins
		if r, err = Init(ctx, o); err != nil || r.ConfigWritten {
			t.Fatalf("re-run %v rewrote the config: %+v %v", logins, r, err)
		}
	}
	if after, _ := os.ReadFile(o.ConfigPath); string(after) != string(before) {
		t.Fatal("idempotent re-run changed the config")
	}

	// A gateway the user disabled stays off on a plain re-run.
	cfg, _ := config.Load(o.ConfigPath)
	cfg.Tailnet.Enabled = false
	b, _ := json.Marshal(cfg)
	os.WriteFile(o.ConfigPath, b, 0o600)
	o.OwnerLogins = nil
	if _, err := Init(ctx, o); err != nil || load().Enabled {
		t.Fatalf("plain re-run re-enabled a disabled gateway: %v", err)
	}
	// Naming another owner is explicit intent: enabled again.
	o.OwnerTags = []string{"tag:ci"}
	if _, err := Init(ctx, o); err != nil || !load().Enabled || len(load().OwnerTags) != 1 {
		t.Fatalf("owner tag: %+v %v", load(), err)
	}

	o.OwnerTags = []string{"ci"}
	if _, err := Init(ctx, o); err == nil {
		t.Fatal("accepted an owner tag without the tag: prefix")
	}
}

// fakeImage writes an images/-layout manifest (images/manifests/x.json with
// blobs in images/out/blobs/sha256) and returns the manifest path.
func fakeImage(t *testing.T, dir, name string, corrupt bool) string {
	t.Helper()
	blobs := filepath.Join(dir, "images", "out", "blobs", "sha256")
	os.MkdirAll(blobs, 0o700)
	put := func(content string) (string, int) {
		sum := sha256.Sum256([]byte(content))
		h := hex.EncodeToString(sum[:])
		os.WriteFile(filepath.Join(blobs, h), []byte(content), 0o600)
		return "sha256:" + h, len(content)
	}
	kd, ks := put("kernel-" + name)
	rd, rs := put("rootfs-" + name)
	if corrupt {
		os.WriteFile(filepath.Join(blobs, strings.TrimPrefix(rd, "sha256:")), []byte("tampered"+strings.Repeat("x", rs-8)), 0o600)
	}
	m := fmt.Sprintf(`{"id":"sha256:%064x","name":%q,"arch":"x86_64","kernel":{"digest":%q,"size":%d,"bootArgs":"console=ttyS0"},
		"rootfs":{"digest":%q,"size":%d},"guest":{"user":{"name":"davit"},"agent":{"binary":{"digest":"sha256:%064x"}}}}`, 1, name, kd, ks, rd, rs, 2)
	p := filepath.Join(dir, "images", "manifests", name+".json")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(m), 0o600)
	return p
}

func TestInitImages(t *testing.T) {
	o := opts(t)
	o.Images = []config.ImageSource{{Manifest: fakeImage(t, t.TempDir(), "base", false)}}
	r, err := Init(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(o.ConfigPath)
	if len(r.Images) != 1 || len(cfg.Images) != 1 || !strings.HasSuffix(cfg.Images[0].BlobDir, filepath.Join("images", "out", "blobs")) {
		t.Fatalf("image not configured with discovered blobs: %+v %+v", r.Images, cfg.Images)
	}
	// Re-running with another image adds it to the existing config; the same
	// image again is not duplicated.
	o.Images = append(o.Images, config.ImageSource{Manifest: fakeImage(t, t.TempDir(), "other", false)})
	if _, err := Init(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	cfg, _ = config.Load(o.ConfigPath)
	if len(cfg.Images) != 2 {
		t.Fatalf("images after re-run: %+v", cfg.Images)
	}
	o.Images = []config.ImageSource{{Manifest: fakeImage(t, t.TempDir(), "bad", true)}}
	if _, err := Init(context.Background(), o); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("a corrupt image must be refused: %v", err)
	}
	if _, err := ImageBlobDir(filepath.Join(t.TempDir(), "x.json")); err == nil {
		t.Fatal("no blob dir must be an error asking for --image-blobs")
	}
}
