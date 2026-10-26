package network

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"davit/internal/domain"
)

func TestRootlessStateDirReap(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StateDir = filepath.Join(t.TempDir(), "net")
	r := newRootlessOrSkip(t, cfg)
	ctx := context.Background()

	h, err := r.Create(ctx, "vm_reap", 1, domain.DefaultEgress())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Destroy(ctx, h) })
	if !strings.HasPrefix(filepath.Base(h.StateDir), "h-"+ShortID("vm_reap", 1)+"-") ||
		filepath.Dir(h.StateDir) != cfg.StateDir {
		t.Fatalf("handle state dir = %q, want %s/h-<short>-*", h.StateDir, cfg.StateDir)
	}
	want, err := filepath.EvalSymlinks(h.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if cwd, err := os.Readlink("/proc/" + strconv.Itoa(h.Holder.PID) + "/cwd"); err != nil || cwd != want {
		t.Fatalf("holder cwd = %q, %v; want %q", cwd, err, want)
	}

	// Same command, outside StateDir: Reap must never touch it.
	stranger, err := startHolder("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopProcess(stranger, stopGrace) })

	// Destroy removes the per-handle directory.
	h2, err := r.Create(ctx, "vm_reap", 2, domain.DefaultEgress())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Destroy(ctx, h2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h2.StateDir); !os.IsNotExist(err) {
		t.Errorf("state dir after Destroy: %v, want not exist", err)
	}

	if n, err := r.Reap(ctx, []Handle{h}); err != nil || n != 0 {
		t.Fatalf("Reap(keep h) = %d, %v; want 0, nil", n, err)
	}
	if !alive(h.Holder) {
		t.Fatal("kept holder was killed")
	}
	if _, err := os.Stat(h.StateDir); err != nil {
		t.Fatalf("kept state dir: %v", err)
	}

	if n, err := r.Reap(ctx, nil); err != nil || n != 1 {
		t.Fatalf("Reap(nil) = %d, %v; want 1, nil", n, err)
	}
	if alive(h.Holder) {
		t.Error("leaked holder survived Reap")
	}
	if _, err := os.Stat(h.StateDir); !os.IsNotExist(err) {
		t.Errorf("state dir after Reap: %v, want not exist", err)
	}
	if !alive(stranger) {
		t.Error("Reap killed a process outside StateDir")
	}
}
