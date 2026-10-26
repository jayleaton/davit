package onboarding

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"davit/api"
	"davit/internal/config"
	"davit/internal/domain"
)

// legacyHost writes a pre-rename host layout under home: data dir with a
// disk and the old binary, a dedicated config dir with an auth key, the
// old unit. It returns the davit config path and data dir to migrate to.
func legacyHost(t *testing.T, home string) (cfgPath, data string) {
	t.Helper()
	oldData := filepath.Join(home, ".svm", "slipway")
	oldDir := filepath.Join(home, ".config", "slipway-host")
	for _, d := range []string{filepath.Join(oldData, "storage"), filepath.Join(oldData, "bin"), oldDir, filepath.Join(home, ".config", "systemd", "user")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(oldData, "storage", "disk.img"), []byte("disk"), 0o600)
	os.WriteFile(filepath.Join(oldData, "bin", "slipway"), []byte("old binary"), 0o755)
	os.WriteFile(filepath.Join(oldDir, "tailnet-authkey"), []byte("tskey"), 0o600)
	os.WriteFile(filepath.Join(home, ".config", "systemd", "user", "slipway.service"), []byte("[Unit]\n"), 0o644)
	c := config.Defaults()
	c.DataDir, c.Socket = oldData, filepath.Join(oldData, "slipway.sock")
	c.Images = []config.ImageSource{{Manifest: filepath.Join(oldData, "images", "manifests", "x.json"), BlobDir: filepath.Join(oldData, "images", "blobs")}}
	c.Tailnet = config.Tailnet{StateDir: filepath.Join(oldData, "tailnet"), AuthKeyFile: filepath.Join(oldDir, "tailnet-authkey")}
	b, _ := json.Marshal(c)
	os.WriteFile(filepath.Join(oldDir, "config.json"), b, 0o600)
	return filepath.Join(home, ".config", "davit-host", "config.json"), filepath.Join(home, ".svm", "davit")
}

// TestMigrate: with the old daemon down and no VMMs, the data dir (with a
// staged davit bin dir already there), config dir and unit move; the
// config's paths and socket name follow; a second run does nothing.
func TestMigrate(t *testing.T) {
	home := t.TempDir()
	cfgPath, data := legacyHost(t, home)
	os.MkdirAll(filepath.Join(data, "bin"), 0o755) // host-install stages the new binary here first
	os.WriteFile(filepath.Join(data, "bin", ".davit-install.1"), []byte("new"), 0o755)
	var mu sync.Mutex
	var calls []string
	o := MigrateOptions{ConfigPath: cfgPath, DataDir: data, UnitDir: filepath.Join(home, ".config", "systemd", "user"),
		Systemctl: func(_ context.Context, args ...string) error {
			mu.Lock()
			calls = append(calls, strings.Join(args, " "))
			mu.Unlock()
			return nil
		}}
	r, err := Migrate(context.Background(), o)
	if err != nil || !r.Migrated {
		t.Fatalf("migrate: %+v %v", r, err)
	}
	if b, err := os.ReadFile(filepath.Join(data, "storage", "disk.img")); err != nil || string(b) != "disk" {
		t.Fatalf("disk after the move: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(data, "bin", ".davit-install.1")); err != nil {
		t.Fatalf("staged binary lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "bin", "slipway")); !os.IsNotExist(err) {
		t.Fatalf("old binary kept: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".svm", "slipway")); !os.IsNotExist(err) {
		t.Fatalf("old data dir left: %v", err)
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != data || c.Socket != filepath.Join(data, "davit.sock") || c.Images[0].BlobDir != filepath.Join(data, "images", "blobs") ||
		c.Tailnet.AuthKeyFile != filepath.Join(filepath.Dir(cfgPath), "tailnet-authkey") {
		t.Fatalf("config paths after the move: %+v", c)
	}
	if b, _ := os.ReadFile(c.Tailnet.AuthKeyFile); string(b) != "tskey" {
		t.Fatalf("auth key after the move: %q", b)
	}
	if _, err := os.Stat(filepath.Join(o.UnitDir, "slipway.service")); !os.IsNotExist(err) || r.OldUnit == "" {
		t.Fatalf("old unit kept: %v %+v", err, r)
	}
	if strings.Join(calls, "; ") != "disable --now slipway.service; daemon-reload" {
		t.Fatalf("systemctl calls: %q", calls)
	}
	if r, err := Migrate(context.Background(), o); err != nil || r.Migrated {
		t.Fatalf("second run: %+v %v", r, err)
	}
}

// TestMigrateLiveVMs: a live VM on the old daemon stops the migration;
// with StopVMs the old daemon stops it first.
func TestMigrateLiveVMs(t *testing.T) {
	home := t.TempDir()
	cfgPath, data := legacyHost(t, home)
	sock := filepath.Join(home, ".svm", "slipway", "slipway.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket: %v", err) // path too long for this TMPDIR
	}
	var mu sync.Mutex
	state := domain.StateReady
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.BasePath+"/vms", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(api.VMList{Items: []api.VM{{ID: "vm_1", Name: "proj--main", State: state}}})
	})
	mux.HandleFunc("GET "+api.BasePath+"/vms/vm_1", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(api.VM{ID: "vm_1", Name: "proj--main", State: state})
	})
	mux.HandleFunc("POST "+api.BasePath+"/vms/vm_1/stop", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		state = domain.StateStopped
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("{}"))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()
	o := MigrateOptions{ConfigPath: cfgPath, DataDir: data, UnitDir: filepath.Join(home, "units"),
		Systemctl: func(context.Context, ...string) error { return nil }}
	if _, err := Migrate(context.Background(), o); err == nil || !strings.Contains(err.Error(), "proj--main") {
		t.Fatalf("live VM: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".svm", "slipway", "storage")); err != nil {
		t.Fatalf("refused migration moved the data dir: %v", err)
	}
	o.StopVMs = true
	r, err := Migrate(context.Background(), o)
	if err != nil || !r.Migrated || len(r.StoppedVMs) != 1 || r.StoppedVMs[0] != "proj--main" {
		t.Fatalf("with StopVMs: %+v %v", r, err)
	}
}
