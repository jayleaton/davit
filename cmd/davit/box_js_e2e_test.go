//go:build linux

package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/internal/cli"
)

// TestBoxVitePlus: on real guests of the base image (no Node.js), a Vite+
// project without .davit/setup.sh gets the detected setup: vp's installer
// (managing Node.js itself here), then `vp install` with the pnpm version
// packageManager pins. The builder and the instance get the JS default of
// cli.DefaultJSBoxMemory; `box run -- vp --version` resolves vp, and a
// login /bin/sh (dash, as `box ssh` gets) has the project's
// node_modules/.bin on PATH. Needs the guests' internet egress (vite.plus,
// the npm registry).
func TestBoxVitePlus(t *testing.T) {
	h := newFCHost(t, nil)
	env := append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=", "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"))
	tree := filepath.Join(h.dir, "vpfix")
	gitTree(t, tree, "main")
	const pnpm = "10.18.3"
	os.WriteFile(filepath.Join(tree, "package.json"), []byte(`{"name":"vpfix","private":true,"type":"module",`+
		`"devDependencies":{"vite-plus":"1.0.0"},"packageManager":"pnpm@`+pnpm+`"}`+"\n"), 0o644)

	start := time.Now()
	so, se, code := human(t, env, tree, "box", "image", "build")
	if code != 0 || !strings.Contains(se, "the detected setup (vp install)") || !strings.Contains(so, "box vpfix image generation 1 ready") {
		t.Fatalf("image build: exit %d\nstdout %s\nstderr %s", code, so, se)
	}
	t.Logf("image build (vp installer + vp install): %.1fs", time.Since(start).Seconds())
	if b := h.info("vpfix--image"); b.Shape.MemoryBytes != cli.DefaultJSBoxMemory {
		t.Fatalf("builder memory %d, want %d", b.Shape.MemoryBytes, cli.DefaultJSBoxMemory)
	}

	start = time.Now()
	so, se, code = human(t, env, tree, "box", "run", "--", "vp", "--version")
	if code != 0 || !strings.Contains(so, "vite-plus  v1.0.0") {
		t.Fatalf("box run -- vp --version: exit %d\nstdout %s\nstderr %s", code, so, se)
	}
	t.Logf("box run -- vp --version (instance create + sync + run): %.1fs\n%s", time.Since(start).Seconds(), so)
	if vm := h.info("vpfix--main"); vm.Shape.MemoryBytes != cli.DefaultJSBoxMemory {
		t.Fatalf("instance memory %d, want %d", vm.Shape.MemoryBytes, cli.DefaultJSBoxMemory)
	}

	so, se, code = human(t, env, tree, "box", "run", "--no-sync", "--", "sh", "-c", "grep '^packageManager:' node_modules/.modules.yaml")
	if code != 0 || strings.TrimSpace(so) != "packageManager: pnpm@"+pnpm {
		t.Fatalf("installed with %q (exit %d %s), want pnpm@%s", so, code, se, pnpm)
	}
	// What box ssh gets: a login shell from the image's profile, here dash.
	so, se, code = human(t, env, tree, "box", "run", "--no-sync", "--raw", "--", "sh", "-lc",
		`readlink -f /bin/sh; printf '%s\n' "$PATH"; command -v vp`)
	if code != 0 || !strings.Contains(so, "/work/node_modules/.bin:") || !strings.Contains(so, "dash") {
		t.Fatalf("login sh: exit %d\nstdout %s\nstderr %s", code, so, se)
	}
	t.Logf("login sh:\n%s", so)
}
