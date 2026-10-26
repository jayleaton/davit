package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"davit/api"
	"davit/internal/cli"
	"davit/internal/domain"
)

// TestBoxResize: --cpus/--memory on box up and box run change an existing
// instance. A running one is stopped and resumed with the new shape (a
// note on stderr), a stopped one resumes with it, and later flagless ups
// keep it; the same shape again changes nothing. A box running a process
// started through davit is refused with what to run instead, a shape the
// host refuses fails before anything stops, and a resize the host has no
// room for leaves the box stopped and says how to start it again; --refresh
// keeps the CPUs and memory it is not given.
func TestBoxResize(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.extraConfig = `"limits":{"memoryBytes":3221225472,"memoryOvercommit":1}`
	h.start(0, 0)
	work := filepath.Join(h.dir, "work")
	h.env = append(h.env, "DAVIT_BOX_WORKDIR="+work)
	env := append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=", "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"),
		"DAVIT_BOX_WORKDIR="+work)
	tree := filepath.Join(h.dir, "proj")
	gitTree(t, tree, "main")
	if _, se, code := human(t, env, tree, "box", "image", "build", "--setup-cmd", "true", "--cpus", "1", "--memory", "1GiB"); code != 0 {
		t.Fatalf("image build: exit %d %s", code, se)
	}
	shape := func(vm api.VM, cpus int, mem int64) {
		t.Helper()
		if vm.Shape.CPUs != cpus || vm.Shape.MemoryBytes != mem || vm.State != domain.StateReady {
			t.Fatalf("%s: %s %+v, want %d cpu %d bytes", vm.Name, vm.State, vm.Shape, cpus, mem)
		}
	}

	up := h.boxUp("proj/a", "--cpus", "1", "--memory", "1GiB")
	shape(up.VM, 1, domain.GiB)
	id := up.VM.ID

	// Running: stopped and resumed with the new memory; the CPUs stay.
	_, se, code := human(t, env, tree, "box", "up", "proj/a", "--memory", "2GiB")
	if code != 0 || !strings.Contains(se, "resizing from 1 cpu, 1.0 GiB memory") || !strings.Contains(se, "to --memory 2GiB") {
		t.Fatalf("resize a running box: exit %d %q", code, se)
	}
	vm := h.info("proj--a")
	shape(vm, 1, 2*domain.GiB)
	if vm.ID != id {
		t.Fatalf("resize replaced the box: %s, was %s", vm.ID, id)
	}
	if u := h.boxUp("proj/a"); u.Action != "running" {
		t.Fatalf("flagless up after a resize: %+v", u)
	} else {
		shape(u.VM, 1, 2*domain.GiB)
	}
	if u := h.boxUp("proj/a", "--memory", "2GiB"); u.Action != "running" {
		t.Fatalf("up with the shape it has: %+v", u)
	}
	h.cli("box", "down", "proj/a")
	if u := h.boxUp("proj/a"); u.Action != "resumed" {
		t.Fatalf("flagless up of the stopped box: %+v", u)
	} else {
		shape(u.VM, 1, 2*domain.GiB) // the host kept the shape
	}

	// Stopped: resumes with the new shape.
	h.cli("box", "down", "proj/a")
	if u := h.boxUp("proj/a", "--cpus", "2"); u.Action != "resumed" {
		t.Fatalf("resize a stopped box: %+v", u)
	} else {
		shape(u.VM, 2, 2*domain.GiB)
	}

	// box run --memory resizes too, and box ls --json shows the shape.
	if _, se, code := human(t, env, tree, "box", "run", "proj/a", "--no-sync", "--memory", "1GiB", "--", "true"); code != 0 || !strings.Contains(se, "resized") {
		t.Fatalf("box run --memory: exit %d %q", code, se)
	}
	r := h.cli("box", "ls", "proj")
	var ls struct {
		Items []struct {
			Instance    string
			CPUs        int   `json:"cpus"`
			MemoryBytes int64 `json:"memoryBytes"`
		} `json:"items"`
	}
	if json.Unmarshal(r.raw[len(r.raw)-1], &ls) != nil || len(ls.Items) != 2 || ls.Items[1].CPUs != 2 || ls.Items[1].MemoryBytes != domain.GiB {
		t.Fatalf("box ls after box run --memory: %s", r.raw[len(r.raw)-1])
	}

	// A process started through davit still runs: refused, nothing stops.
	r = h.cli("exec", "proj--a", "--detach", "--", "sleep", "600")
	var p api.Process
	if r.code != 0 || json.Unmarshal(r.raw[len(r.raw)-1], &p) != nil || p.ID == "" {
		t.Fatalf("exec --detach: exit %d %s", r.code, r.stderr)
	}
	r = h.cli("box", "up", "proj/a", "--memory", "2GiB")
	h.expectError(r, cli.ExitConflict, domain.CodeStopRequired)
	if msg := r.last().Error.Message; !strings.Contains(msg, p.ID) || !strings.Contains(msg, "box down proj/a` and then `davit box up proj/a --memory 2GiB`") {
		t.Fatalf("refusal message: %q", msg)
	}
	shape(h.info("proj--a"), 2, domain.GiB)
	if r := h.cli("exec", "proj--a", "--kill", p.ID, "--signal", "KILL"); r.code != 0 {
		t.Fatalf("kill: exit %d %s", r.code, r.stderr)
	}
	h.cli("exec", "proj--a", "--status", p.ID) // settles the exit

	// A shape the host refuses fails before the box stops.
	h.expectError(h.cli("box", "up", "proj/a", "--disk", "1GiB"), cli.ExitUsage, domain.CodeInvalidArgument)
	shape(h.info("proj--a"), 2, domain.GiB)

	// No room for the new shape (the budget is 3 GiB): the box stays
	// stopped with its old shape, and the error says how to start it.
	r = h.cli("box", "up", "proj/a", "--memory", "4GiB", "--wait", "0s")
	h.expectError(r, 8, domain.CodeResourceExhausted)
	if msg := r.last().Error.Message; !strings.Contains(msg, "stays stopped, its disk kept: `davit box up proj/a` without") {
		t.Fatalf("stranded message: %q", msg)
	}
	if vm := h.info("proj--a"); vm.State != domain.StateStopped || vm.Shape.MemoryBytes != domain.GiB {
		t.Fatalf("after a resize without room: %s %+v", vm.State, vm.Shape)
	}
	if u := h.boxUp("proj/a"); u.Action != "resumed" {
		t.Fatalf("up after a resize without room: %+v", u)
	} else {
		shape(u.VM, 2, domain.GiB)
	}

	// --refresh makes a new instance from the latest image with the same
	// CPUs and memory, unless given.
	if _, se, code := human(t, env, tree, "box", "image", "update", "--setup-cmd", "true"); code != 0 {
		t.Fatalf("image update: exit %d %s", code, se)
	}
	if u := h.boxUp("proj/a", "--refresh", "--cpus", "1"); u.Action != "refreshed" || u.VM.ID == id {
		t.Fatalf("refresh: %+v", u)
	} else {
		shape(u.VM, 1, domain.GiB)
	}
}

// TestBoxResizeJSDefault: a JS project's new boxes get cli.DefaultJSBoxMemory,
// but an existing one keeps the shape it has: --memory resizes it below the
// default, flagless up, run, down/up, --cpus alone and --refresh never
// bring it back to the default, while a new instance still gets it.
func TestBoxResizeJSDefault(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	h.start(0, 0)
	work := filepath.Join(h.dir, "work")
	h.env = append(h.env, "DAVIT_BOX_WORKDIR="+work)
	env := append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=", "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"),
		"DAVIT_BOX_WORKDIR="+work)
	tree := filepath.Join(h.dir, "web")
	gitTree(t, tree, "main")
	os.WriteFile(filepath.Join(tree, "package.json"), []byte(`{"name":"web","private":true}`+"\n"), 0o644)
	box := func(want string, args ...string) {
		t.Helper()
		so, se, code := human(t, env, tree, append([]string{"box"}, args...)...)
		if code != 0 || !strings.Contains(so+se, want) {
			t.Fatalf("box %v: exit %d, want %q\nstdout %s\nstderr %s", args, code, want, so, se)
		}
	}
	mem := func(vm string, cpus int, want int64) {
		t.Helper()
		if got := h.info(vm); got.Shape.MemoryBytes != want || got.Shape.CPUs != cpus {
			t.Fatalf("%s: %+v, want %d cpu %d bytes", vm, got.Shape, cpus, want)
		}
	}

	box("image generation 1 ready", "image", "build", "--setup-cmd", "true")
	mem("web--image", 2, cli.DefaultJSBoxMemory)
	box("web/a created", "up", "web/a")
	mem("web--a", 2, cli.DefaultJSBoxMemory)

	box("web/a resized", "up", "web/a", "--memory", "2GiB")
	mem("web--a", 2, 2*domain.GiB)
	box("web/a running", "up", "web/a")
	box("", "run", "web/a", "--no-sync", "--", "true")
	mem("web--a", 2, 2*domain.GiB)
	box("down", "down", "web/a")
	box("web/a resumed", "up", "web/a")
	mem("web--a", 2, 2*domain.GiB)
	box("web/a resized", "up", "web/a", "--cpus", "1")
	mem("web--a", 1, 2*domain.GiB)

	// A new instance still gets the default; an image update and a
	// refresh keep the shapes there are.
	box("web/b created", "up", "web/b")
	mem("web--b", 2, cli.DefaultJSBoxMemory)
	box("image generation 2 ready", "image", "update", "--setup-cmd", "true")
	mem("web--image", 2, cli.DefaultJSBoxMemory)
	box("web/a refreshed", "up", "web/a", "--refresh")
	mem("web--a", 1, 2*domain.GiB)
}
