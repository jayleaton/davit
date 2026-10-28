package preflight

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"davit/internal/config"
)

func TestCIChecks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	goodKey, openKey := write("app.pem", pemKey, 0o600), write("open.pem", pemKey, 0o644)
	secret, openSecret, empty := write("secret", "s3cret", 0o600), write("open-secret", "s3cret", 0o644), write("empty", "", 0o600)
	manifest := func(name string) config.ImageSource {
		return config.ImageSource{Manifest: write(name+".json", fmt.Sprintf(`{"name":%q}`, name), 0o600)}
	}
	base := config.GitHubCI{Enabled: true, AppID: 7, PrivateKeyFile: goodKey,
		Installations: []config.GitHubInstallation{{ID: 1, Repos: []config.GitHubRepo{{Name: "acme/app"}, {Name: "acme/web"}}}}}
	with := func(fn func(*config.GitHubCI)) config.GitHubCI { g := base; fn(&g); return g }
	ciImage := []config.ImageSource{manifest("ubuntu-24.04-dev"), manifest(config.DefaultCIImage)}
	devOnly := ciImage[:1]

	for name, tc := range map[string]struct {
		g      config.GitHubCI
		images []config.ImageSource
		want   map[string]Status
		detail string
	}{
		"off": {g: config.GitHubCI{}, want: map[string]Status{"ci_github_app": Skip, "ci_github_webhook": Skip, "ci_image": Skip}},
		"polling only": {g: base, images: ciImage,
			want: map[string]Status{"ci_github_app": Pass, "ci_github_webhook": Pass, "ci_image": Pass}, detail: "2 allowed repo(s)"},
		"key missing": {g: with(func(g *config.GitHubCI) { g.PrivateKeyFile = filepath.Join(dir, "nope.pem") }), images: ciImage,
			want: map[string]Status{"ci_github_app": Fail}},
		"key readable by others": {g: with(func(g *config.GitHubCI) { g.PrivateKeyFile = openKey }), images: ciImage,
			want: map[string]Status{"ci_github_app": Fail}, detail: "readable by others"},
		"webhook on loopback": {g: with(func(g *config.GitHubCI) {
			g.Webhook = config.GitHubWebhook{Listen: "127.0.0.1:8475", SecretFile: secret}
		}),
			images: ciImage, want: map[string]Status{"ci_github_webhook": Pass}, detail: "polling every 30s as a fallback"},
		"webhook on every interface": {g: with(func(g *config.GitHubCI) { g.Webhook = config.GitHubWebhook{Listen: ":8475", SecretFile: secret} }),
			images: ciImage, want: map[string]Status{"ci_github_webhook": Warn}},
		"secret missing": {g: with(func(g *config.GitHubCI) {
			g.Webhook = config.GitHubWebhook{Listen: "127.0.0.1:1", SecretFile: filepath.Join(dir, "nope")}
		}),
			images: ciImage, want: map[string]Status{"ci_github_webhook": Fail}},
		"secret readable by others": {g: with(func(g *config.GitHubCI) {
			g.Webhook = config.GitHubWebhook{Listen: "127.0.0.1:1", SecretFile: openSecret}
		}),
			images: ciImage, want: map[string]Status{"ci_github_webhook": Fail}, detail: "readable by others"},
		"secret empty": {g: with(func(g *config.GitHubCI) { g.Webhook = config.GitHubWebhook{Listen: "127.0.0.1:1", SecretFile: empty} }),
			images: ciImage, want: map[string]Status{"ci_github_webhook": Fail}, detail: "empty"},
		"image not installed": {g: base, images: devOnly, want: map[string]Status{"ci_image": Fail}, detail: "have: ubuntu-24.04-dev"},
		"custom image": {g: with(func(g *config.GitHubCI) { g.Image = "ubuntu-24.04-dev" }), images: devOnly,
			want: map[string]Status{"ci_image": Pass}},
	} {
		t.Run(name, func(t *testing.T) {
			checks := CIChecks(Options{Runtime: "firecracker", CI: tc.g, Images: tc.images})
			details := ""
			for _, c := range checks {
				details += c.Detail + "\n"
				if want, ok := tc.want[c.ID]; ok && c.Status != want {
					t.Errorf("%s: %s, want %s: %+v", c.ID, c.Status, want, c)
				}
				if (c.Status == Fail || c.Status == Warn) && (c.Hint == "" || strings.Contains(c.Hint, "\n")) {
					t.Errorf("%s %s without a one-line fix: %+v", c.ID, c.Status, c)
				}
			}
			if !strings.Contains(details, tc.detail) {
				t.Errorf("details lack %q:\n%s", tc.detail, details)
			}
		})
	}
	if c := ciImageCheck(Options{Runtime: "fake", CI: base}); c.Status != Skip {
		t.Fatalf("fake runtime: %+v", c)
	}
}
