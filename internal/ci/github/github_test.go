package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"davit/internal/ci/github"
	"davit/internal/ci/githubtest"
)

func TestVerifySignature(t *testing.T) {
	secret, body := []byte("It's a Secret to Everybody"), []byte("Hello, World!")
	// GitHub's documented example (docs: validating webhook deliveries).
	const want = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if got := github.Sign(secret, body); got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
	if err := github.VerifySignature(secret, body, want); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "sha1=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17", "sha256=zz",
		"sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e18", strings.TrimPrefix(want, "sha256=")} {
		if err := github.VerifySignature(secret, body, bad); !errors.Is(err, github.ErrSignature) {
			t.Errorf("signature %q: %v, want ErrSignature", bad, err)
		}
	}
	if err := github.VerifySignature(secret, append(body, '!'), want); !errors.Is(err, github.ErrSignature) {
		t.Errorf("altered body accepted: %v", err)
	}
	if err := github.VerifySignature(nil, body, want); err == nil {
		t.Error("no secret accepted a delivery")
	}
}

func TestLoadPrivateKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	for name, blk := range map[string]*pem.Block{
		"pkcs1": {Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)},
		"pkcs8": {Type: "PRIVATE KEY", Bytes: pkcs8},
	} {
		p := filepath.Join(dir, name+".pem")
		if err := os.WriteFile(p, pem.EncodeToMemory(blk), 0o600); err != nil {
			t.Fatal(err)
		}
		k, err := github.LoadPrivateKey(p)
		if err != nil || !k.Equal(key) {
			t.Fatalf("%s: %v", name, err)
		}
		if err := os.Chmod(p, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := github.LoadPrivateKey(p); err == nil || !strings.Contains(err.Error(), "readable by others") {
			t.Fatalf("%s 0640: %v, want refused", name, err)
		}
	}
	p := filepath.Join(dir, "junk.pem")
	_ = os.WriteFile(p, []byte("not a key"), 0o600)
	if _, err := github.LoadPrivateKey(p); err == nil {
		t.Fatal("junk accepted as a key")
	}
}

func TestAppAuthAndTokenCache(t *testing.T) {
	gh := githubtest.New(t)
	gh.AddRepo("acme/app", false)
	app := gh.App()
	in := app.Installation(7)
	for range 3 {
		r, err := in.Repo(context.Background(), "acme/app")
		if err != nil || r.FullName != "acme/app" || r.Public() {
			t.Fatalf("repo %+v %v", r, err)
		}
	}
	if n := gh.Calls("POST /app/installations/{id}/access_tokens"); n != 1 {
		t.Fatalf("%d token exchanges for 3 calls, want 1 (cached)", n)
	}
	// A token near its expiry is replaced.
	app.Now = func() time.Time { return time.Now().Add(56 * time.Minute) }
	if _, err := in.Repo(context.Background(), "acme/app"); err != nil {
		t.Fatal(err)
	}
	if n := gh.Calls("POST /app/installations/{id}/access_tokens"); n != 2 {
		t.Fatalf("%d token exchanges, want a fresh one near expiry", n)
	}
	// Another App's key is refused by GitHub.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	bad := &github.App{BaseURL: gh.URL, AppID: gh.AppID, Key: other}
	if _, err := bad.Installation(7).Repo(context.Background(), "acme/app"); !github.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestRateLimitHints(t *testing.T) {
	reset := time.Now().Add(90 * time.Second).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/1/access_tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"t","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
		case "/repos/a/secondary":
			w.Header().Set("Retry-After", "42")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
		case "/repos/a/primary":
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	in := (&github.App{BaseURL: srv.URL, AppID: 1, Key: key}).Installation(1)
	var ge *github.Error
	_, err := in.Repo(context.Background(), "a/secondary")
	if !errors.As(err, &ge) || ge.RetryAfter != 42*time.Second || !strings.Contains(ge.Message, "secondary rate limit") {
		t.Fatalf("secondary: %#v", err)
	}
	_, err = in.Repo(context.Background(), "a/primary")
	if !errors.As(err, &ge) || ge.RetryAfter < 80*time.Second || ge.RetryAfter > 95*time.Second {
		t.Fatalf("primary: %#v", err)
	}
	if err := in.DeleteRunner(context.Background(), github.Scope{Repo: "a/gone"}, 1); err != nil {
		t.Fatalf("deleting a runner that is gone: %v", err)
	}
}

func TestRunFromFork(t *testing.T) {
	same := github.Run{Repository: github.Repository{FullName: "acme/app"}, HeadRepository: github.Repository{FullName: "Acme/App"}}
	fork := github.Run{Repository: github.Repository{FullName: "acme/app"}, HeadRepository: github.Repository{FullName: "mallory/app"}}
	if same.FromFork() || !fork.FromFork() || (github.Run{}).FromFork() {
		t.Fatal("FromFork")
	}
	if !(github.Repository{Visibility: "public", Private: true}).Public() || (github.Repository{Visibility: "internal"}).Public() ||
		!(github.Repository{Private: false}).Public() {
		t.Fatal("Public")
	}
}
