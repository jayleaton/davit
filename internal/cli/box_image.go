package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/push"
)

// setupBudget bounds one image setup run.
const setupBudget = 2 * time.Hour

// boxImage runs `box image build|update|rm PROJECT`.
func (s *session) boxImage(ctx context.Context, o *opts, args []string) error {
	if len(args) < 1 || len(args) > 2 || args[0] != "build" && args[0] != "update" && args[0] != "rm" {
		return usageErr("box image build [PROJECT] [--from IMAGE] [--setup FILE | --setup-cmd CMD] | box image update [PROJECT] [--setup ...] | box image rm [PROJECT] [--force] --yes")
	}
	arg := defaultProject()
	if len(args) == 2 {
		arg = args[1]
	}
	project, err := boxName(arg)
	if err != nil {
		return err
	}
	if len(project) > 40 {
		return usageErr("box project %q is longer than 40 characters", arg)
	}
	if args[0] == "rm" {
		return s.imageRemove(ctx, o, project)
	}
	return s.buildImage(ctx, o, project, args[0] == "build", true)
}

// imageRemove deletes a project image: its builder VM and every generation
// snapshot, whose disks (and the goldens captured from them) the host then
// frees. It refuses while the project has instances; --force deletes them
// too (an instance never stopped still needs its generation).
func (s *session) imageRemove(ctx context.Context, o *opts, project string) error {
	rows, err := s.boxRows(ctx, project)
	if err != nil {
		return err
	}
	var instances []boxRow
	for _, r := range rows {
		if r.Kind == "instance" {
			instances = append(instances, r)
		}
	}
	gens, err := s.imageGenerations(ctx, project)
	if err != nil {
		return err
	}
	_, builderErr := s.boxVM(ctx, imageVM(project), project)
	if builderErr != nil && !isCode(builderErr, domain.CodeNotFound) {
		return builderErr
	}
	if len(gens) == 0 && builderErr != nil {
		return clientErr(domain.CodeNotFound, "box %s has no image", project)
	}
	if len(instances) > 0 && !o.force {
		names := make([]string, len(instances))
		for i, r := range instances {
			names[i] = r.Project + "/" + r.Instance
		}
		return clientErr(domain.CodeInvalidState, "box %s still has %d instances (%s): remove them with `%s box rm`, or pass --force to delete them with the image",
			project, len(instances), strings.Join(names, ", "), buildinfo.Name)
	}
	q := fmt.Sprintf("delete the image of box %s (%d generations)?", project, len(gens))
	if len(instances) > 0 {
		q = fmt.Sprintf("delete the image of box %s (%d generations) and its %d instances with their disks?", project, len(gens), len(instances))
	}
	if err := s.confirm(o, q); err != nil {
		return err
	}
	start := time.Now()
	res := map[string]any{"project": project, "instances": []string{}, "generations": []int{}}
	del := func(path string) error {
		acc, err := s.cl.mutate(ctx, http.MethodDelete, path, nil, newIdempotencyKey())
		if isCode(err, domain.CodeNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return s.await(ctx, o, &acc.Operation)
	}
	for _, r := range instances {
		if err := del(vmPath(r.VMID)); err != nil {
			return err
		}
		res["instances"] = append(res["instances"].([]string), r.Instance)
	}
	if builderErr == nil {
		if err := del(vmPath(imageVM(project))); err != nil {
			return err
		}
	}
	for _, g := range gens {
		if err := s.cl.do(ctx, http.MethodDelete, "/snapshots/"+url.PathEscape(string(g.Snapshot.ID)), nil, nil, nil, nil); err != nil && !isCode(err, domain.CodeNotFound) {
			return err
		}
		res["generations"] = append(res["generations"].([]int), g.N)
	}
	res["seconds"] = time.Since(start).Seconds()
	s.out.result("", res)
	s.out.human("box %s image removed (%d generations, %d instances) in %.1fs; its disks and goldens are freed", project, len(gens), len(instances), time.Since(start).Seconds())
	return nil
}

// buildImage builds (build) or updates the project image. report prints
// the result and streams the setup; without it (an image built on the way
// to `box up`) the setup's output goes to the job history and progress to
// stderr.
func (s *session) buildImage(ctx context.Context, o *opts, project string, build, report bool) error {
	start := time.Now()
	gens, err := s.imageGenerations(ctx, project)
	if err != nil {
		return err
	}
	if !build && len(gens) == 0 {
		return clientErr(domain.CodeNotFound, "box %s has no image; build it with `%s box image build %s`", project, buildinfo.Name, project)
	}
	// The tree: --dir, else the worktree; outside a git work tree nothing
	// is synced and the setup (detected from the current directory, or
	// --setup/--setup-cmd) runs in an empty work dir.
	dir, doPush := o.box.dir, true
	if dir == "" {
		if dir = defaultTree(); dir == "" {
			dir, doPush = ".", false
		}
	}
	tree, err := parsePushSpec(dir)
	if err != nil {
		return err
	}
	argv, setupName, err := o.setupArgv(tree.dir)
	if err != nil {
		return err
	}
	var sel []push.Entry
	if doPush {
		if sel, err = selectTree(ctx, tree); err != nil {
			return err
		}
	}
	builder := imageVM(project)
	var create func() (*api.CreateVMRequest, error)
	if build {
		if _, err := s.boxVM(ctx, builder, project); err == nil {
			return clientErr(domain.CodeInvalidState, "box %s already has an image (generation %d); update it with `%s box image update %s`",
				project, lastGen(gens), buildinfo.Name, project)
		} else if !isCode(err, domain.CodeNotFound) {
			return err
		}
		idle, err := o.boxIdle()
		if err != nil {
			return err
		}
		no := false
		create = func() (*api.CreateVMRequest, error) {
			return &api.CreateVMRequest{Name: builder, Image: o.image, Type: o.typ, Shape: s.newBoxShape(ctx, o, tree.dir), AutoStop: &no,
				IdleTimeoutSeconds: int64(idle / time.Second), Labels: map[string]string{domain.LabelBox: project, labelBoxRole: boxRoleImage}}, nil
		}
	}
	vm, _, err := s.ensureRunning(ctx, o, builder, project, create)
	if err != nil {
		return err
	}
	if tree.guest == "" {
		tree.guest = s.boxWorkdir(ctx, builder)
	}
	if !doPush {
		argv = append([]string{"sh", "-c", `mkdir -p "$1" && cd "$1" && shift && exec "$@"`, "setup", tree.guest}, argv...)
	}
	setupCwd := "/"
	if doPush {
		if _, err := s.pushTree(ctx, builder, tree, sel); err != nil {
			return err
		}
		if err := s.boxGit(ctx, builder, tree, "index"); err != nil { // setups may use git ls-files
			s.out.diag("box %s image: git metadata not set up: %v", project, err)
		}
		setupCwd = tree.guest
	}
	s.out.diag("box %s image: running %s in the builder", project, setupName)
	setupStart := time.Now()
	if err := s.runSetup(ctx, builder, argv, setupCwd, report); err != nil {
		return err
	}
	setupSecs := time.Since(setupStart).Seconds()
	if jsProject(tree.dir) {
		p, err := s.boxExecQuiet(ctx, builder, []string{"sh", "-c", installProfile, "profile", nodeBinProfile(tree.guest)}, "/", time.Minute)
		if code := processExit(p); err != nil || code != 0 {
			s.out.diag("box %s image: node_modules/.bin not put on the login PATH (exit %d): %v", project, code, err)
		}
	}
	if err := s.stopBox(ctx, o, builder); err != nil {
		return err
	}
	n := lastGen(gens) + 1
	acc, err := s.cl.mutate(ctx, http.MethodPost, vmPath(builder)+"/snapshots", api.SnapshotVMRequest{Name: imageSnapshotName(project, n)}, newIdempotencyKey())
	if err != nil {
		return err
	}
	if err := s.await(ctx, o, &acc.Operation); err != nil {
		return err
	}
	var snapID domain.SnapshotID
	if acc.Operation.Result != nil {
		snapID = acc.Operation.Result.SnapshotID
	}
	secs := time.Since(start).Seconds()
	if !report {
		s.out.diag("box %s image generation %d ready in %.1fs (setup %.1fs)", project, n, secs, setupSecs)
		return nil
	}
	s.out.result("", map[string]any{"project": project, "generation": n, "snapshotId": snapID, "builder": vm.ID,
		"seconds": secs, "setupSeconds": setupSecs})
	s.out.human("box %s image generation %d ready in %.1fs (setup %.1fs); new instances start from it, existing ones keep theirs (`box up --refresh` moves one)",
		project, n, secs, setupSecs)
	return nil
}

func lastGen(gens []imageGen) int {
	if len(gens) == 0 {
		return 0
	}
	return gens[len(gens)-1].N
}

// setupArgv is the setup step's argv and a description: --setup-cmd under
// sh -c, else the script (--setup, default .davit/setup.sh in --dir) run
// by its #! interpreter (sh without one), in the synced tree.
func (o *opts) setupArgv(dir string) ([]string, string, error) {
	// Every setup runs under a login shell, so the image's profile (its
	// toolchains, PATH) applies, then its own interpreter.
	login := func(argv ...string) []string { return append([]string{"bash", "-lc", `exec "$@"`, "setup"}, argv...) }
	if o.box.setupCmd != "" {
		if o.box.setup != "" {
			return nil, "", usageErr("--setup and --setup-cmd are exclusive")
		}
		return login("sh", "-c", o.box.setupCmd), "--setup-cmd", nil
	}
	file := o.box.setup
	if file == "" {
		file = filepath.Join(dir, defaultSetupFile)
		if _, err := os.Stat(file); os.IsNotExist(err) {
			if legacy := filepath.Join(dir, legacySetupFile); exists(legacy) {
				file = legacy
			}
		}
	}
	b, err := os.ReadFile(file)
	switch {
	case err != nil && o.box.setup == "" && os.IsNotExist(err):
		script, found := detectSetup(dir)
		return login("bash", "-c", script), "the detected setup (" + strings.Join(found, ", ") + ")", nil
	case err != nil:
		return nil, "", clientErr(domain.CodeInvalidArgument, "setup script: %v", err)
	}
	script := string(b)
	interp := []string{"sh"}
	if first, _, _ := strings.Cut(script, "\n"); strings.HasPrefix(first, "#!") {
		if f := strings.Fields(strings.TrimPrefix(first, "#!")); len(f) > 0 {
			interp = f
		}
	}
	return login(append(interp, "-c", script)...), file, nil
}

// detectSetup builds the default setup of a project without
// .davit/setup.sh from the files at its top level (dir), for an image
// with the usual toolchains (ubuntu-24.04-dev: mise, Go, Node.js, pnpm,
// uv): pinned tools, module and package installs from the lockfiles, and
// a Makefile `setup` target. It returns the script and what it found.
func detectSetup(dir string) (string, []string) {
	has := func(names ...string) bool {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
				return true
			}
		}
		return false
	}
	var found []string
	lines := []string{"set -eu"}
	step := func(what string, cmd ...string) {
		found = append(found, what)
		lines = append(lines, "echo '>> setup: "+what+"'")
		lines = append(lines, cmd...)
	}
	mise := has("mise.toml", ".mise.toml", ".tool-versions", ".config/mise.toml")
	if mise {
		step("mise install", "mise trust -q --all 2>/dev/null || mise trust -q", "mise install")
	}
	run := "" // run under the project's mise pins when it has them
	if mise {
		run = "mise exec -- "
	}
	if has("go.mod") {
		step("go mod download", run+"go mod download")
	}
	if has("package.json") {
		locked := has("pnpm-lock.yaml", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "bun.lock", "bun.lockb")
		switch {
		case vitePlus(dir):
			// Vite+ (https://viteplus.dev): its documented bootstrap, the
			// global vp CLI (unless the image or the project's mise pins
			// have one), then `vp install`, which installs with the package
			// manager and version packageManager pins. The installer puts
			// its bin dir on PATH in ~/.profile and ~/.bashrc; it manages
			// Node.js only where neither the image nor mise has one. CI
			// keeps it off the terminal.
			install := "vp install"
			if locked {
				install += " --frozen-lockfile"
			}
			step("vp install",
				"if ! "+run+"vp --version >/dev/null 2>&1; then",
				`  export VP_BIN_DIR="$HOME/.local/share/vite-plus/bin" VP_DATA_DIR="$HOME/.local/share/vite-plus" VP_CACHE_DIR="$HOME/.cache/vite-plus"`,
				"  if "+run+"node --version >/dev/null 2>&1; then vp_node=no; else vp_node=yes; fi",
				`  vp_installer=$(mktemp) && curl -fsSL https://vite.plus -o "$vp_installer"`,
				`  CI=1 VP_NODE_MANAGER=$vp_node bash "$vp_installer" </dev/null && rm -f "$vp_installer"`,
				`  export PATH="$VP_BIN_DIR:$PATH"`,
				"fi",
				run+install)
		case has("pnpm-lock.yaml"):
			step("pnpm install", run+"pnpm install --frozen-lockfile")
		case has("package-lock.json", "npm-shrinkwrap.json"):
			step("npm ci", run+"npm ci")
		case has("yarn.lock"):
			step("yarn install", run+"corepack enable --install-directory \"$HOME/.local/bin\" 2>/dev/null || true",
				run+"yarn install --frozen-lockfile || "+run+"yarn install --immutable")
		case has("bun.lock", "bun.lockb"):
			step("bun install", run+"npx --yes bun install --frozen-lockfile")
		default:
			step("npm install", run+"npm install")
		}
	}
	switch {
	case has("uv.lock"):
		step("uv sync", run+"uv sync --frozen")
	case has("pyproject.toml"):
		step("uv sync", run+"uv sync")
	case has("requirements.txt"):
		step("pip install -r requirements.txt", run+"uv venv -q .venv", run+"uv pip install -q --python .venv -r requirements.txt")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "Makefile")); err == nil && makeTarget(string(b), "setup") {
		step("make setup", run+"make setup")
	}
	if len(found) == 0 {
		found = append(found, "nothing to install")
		lines = append(lines, "echo '>> setup: no mise, go.mod, package.json, Python or Makefile setup target found'")
	}
	return strings.Join(lines, "\n") + "\n", found
}

// jsProject reports whether dir, a tree's top level, is a JavaScript
// project: it has a package.json.
func jsProject(dir string) bool { return dir != "" && exists(filepath.Join(dir, "package.json")) }

// vitePlus reports whether the JS project in dir uses Vite+: vite-plus is
// in its package.json dependencies or devDependencies, or its vite config
// imports vite-plus.
func vitePlus(dir string) bool {
	if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg struct {
			Dependencies    map[string]any `json:"dependencies"`
			DevDependencies map[string]any `json:"devDependencies"`
		}
		if json.Unmarshal(b, &pkg) == nil && (pkg.Dependencies["vite-plus"] != nil || pkg.DevDependencies["vite-plus"] != nil) {
			return true
		}
	}
	for _, ext := range []string{"ts", "mts", "cts", "js", "mjs", "cjs"} {
		if b, err := os.ReadFile(filepath.Join(dir, "vite.config."+ext)); err == nil &&
			(strings.Contains(string(b), `"vite-plus`) || strings.Contains(string(b), `'vite-plus`)) {
			return true
		}
	}
	return false
}

// DefaultJSBoxMemory is the memory a new box (builder or instance) of a JS
// project gets when neither --memory nor --type says: package installs,
// type checks and linters of a JS workspace outgrow the host default
// (domain.DefaultShape, 2 GiB).
const DefaultJSBoxMemory = 4 * domain.GiB

// newBoxShape is the shape override of a new box whose tree is dir:
// --cpus/--memory/--disk, with DefaultJSBoxMemory for a JS project unless
// --memory or --type is given. A host that can never hold that much (the
// engine's largest VM, limits.memoryBytes or the memory reservation budget
// below it) keeps its default; a full one queues the box (--wait) like any
// other.
func (s *session) newBoxShape(ctx context.Context, o *opts, dir string) *api.ShapeOverride {
	shape := o.shape()
	if o.memory.v != 0 || o.typ != "" || !jsProject(dir) {
		return shape
	}
	var caps api.HostCapabilities
	if err := s.cl.get(ctx, "/capabilities", nil, &caps); err == nil {
		bounds := []int64{caps.Runtime.Shape.MaxMemoryBytes}
		if c := caps.Capacity; c != nil {
			bounds = append(bounds, c.Limits.MemoryBytes, c.MemoryBudgetBytes)
		}
		for _, b := range bounds {
			if b > 0 && b < DefaultJSBoxMemory {
				s.out.diag("box: the host holds at most %s per VM, below the %s of a JS project box: keeping the host default (--memory sets it)",
					humanBytes(b), humanBytes(DefaultJSBoxMemory))
				return shape
			}
		}
	}
	if shape == nil {
		shape = &api.ShapeOverride{}
	}
	shape.MemoryBytes = DefaultJSBoxMemory
	return shape
}

// nodeBinProfile is the login profile snippet that puts the JS project's
// node_modules/.bin in guest directory tree on PATH, for `box ssh` (`box
// run` also puts every one from its directory up first, boxRunPrelude).
// POSIX sh: a login /bin/sh may be dash.
func nodeBinProfile(tree string) string {
	return "# davit: this project's node_modules/.bin on PATH in login shells (box ssh, box run)\n" +
		"davit_node_bin=" + sshRemoteQuote(path.Join(tree, "node_modules/.bin")) + "\n" +
		`case ":$PATH:" in *":$davit_node_bin:"*) ;; *) PATH="$davit_node_bin:$PATH"; export PATH ;; esac` + "\n" +
		"unset davit_node_bin\n"
}

// installProfile writes profile snippet $1 where every login shell of the
// image user reads it: /etc/profile.d when the user has sudo, else a file
// ~/.profile sources.
const installProfile = `set -eu
if sudo -n true 2>/dev/null; then
  printf '%s' "$1" | sudo tee /etc/profile.d/davit-node-bin.sh >/dev/null
else
  mkdir -p "$HOME/.config/davit"
  printf '%s' "$1" >"$HOME/.config/davit/node-bin.sh"
  grep -qs 'davit/node-bin.sh' "$HOME/.profile" || printf '%s\n' '. "$HOME/.config/davit/node-bin.sh"' >>"$HOME/.profile"
fi
`

// makeTarget reports whether a Makefile defines target.
func makeTarget(makefile, target string) bool {
	for _, l := range strings.Split(makefile, "\n") {
		if name, _, ok := strings.Cut(l, ":"); ok && !strings.HasPrefix(l, "\t") {
			for _, n := range strings.Fields(name) {
				if n == target && !strings.Contains(l, ":=") && !strings.Contains(l, "::=") {
					return true
				}
			}
		}
	}
	return false
}

// runSetup runs the setup argv in the builder's synced tree: streamed in
// human mode, waited for (output in `davit history`) with --json.
func (s *session) runSetup(ctx context.Context, builder string, argv []string, cwd string, stream bool) error {
	quiet := s.g.json || !stream
	body := api.ExecRequest{Argv: argv, Cwd: cwd, DeadlineSeconds: int64(setupBudget / time.Second), Detached: quiet}
	var p api.Process
	if err := s.cl.do(ctx, http.MethodPost, vmPath(builder)+"/processes", nil, body,
		map[string]string{api.HeaderIdempotencyKey: newIdempotencyKey()}, &p); err != nil {
		return err
	}
	fail := func(code int) error {
		return &exitStatus{code: code, msg: "image setup exited " + strconv.Itoa(code) + " (its output: `" + buildinfo.Name + " history " + builder + " " +
			p.ID + "`); the builder " + builder + " is left running for a look (`" + buildinfo.Name + " ssh " + builder +
			"`; it stops itself when idle). Fix the setup (.davit/setup.sh, or --setup-cmd) and run `" + buildinfo.Name + " box image build` again"}
	}
	if quiet {
		p = s.waitProcess(ctx, builder, p.ID, setupBudget+time.Minute)
		if code := processExit(p); code != 0 {
			return fail(code)
		}
		return nil
	}
	if err := s.followProcess(ctx, builder, p.ID, 0, false, nil); err != nil {
		if xs, ok := err.(*exitStatus); ok {
			if xs.code == 0 {
				return nil
			}
			return fail(xs.code)
		}
		return err
	}
	return nil
}

// boxRow is one `box ls` row.
type boxRow struct {
	Project            string     `json:"project"`
	Instance           string     `json:"instance"` // "image" for the project image's builder
	Kind               string     `json:"kind"`     // image, instance
	VM                 string     `json:"vm"`
	VMID               string     `json:"vmId"`
	State              string     `json:"state"`
	Generation         int        `json:"generation"`       // image: the latest; instance: the one it started from
	LatestGeneration   int        `json:"latestGeneration"` // the project's latest
	Stale              bool       `json:"stale,omitempty"`  // an instance from an older generation
	CreatedAt          time.Time  `json:"createdAt"`
	CPUs               int        `json:"cpus"`
	MemoryBytes        int64      `json:"memoryBytes"`
	DiskBytes          int64      `json:"diskBytes"`
	IdleTimeoutSeconds int64      `json:"idleTimeoutSeconds"`
	LastActivityAt     *time.Time `json:"lastActivityAt,omitempty"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

// boxList lists project images and instances (of one project, or all),
// under a summary of the host's memory and reservation.
func (s *session) boxList(ctx context.Context, project string) error {
	rows, err := s.boxRows(ctx, project)
	if err != nil {
		return err
	}
	host, err := s.hostSummary(ctx, rows)
	if err != nil {
		return err
	}
	s.out.result("", map[string]any{"host": host, "items": rows})
	if host != nil {
		s.out.human("%s", host.line())
	}
	for _, r := range rows {
		gen := "gen " + strconv.Itoa(r.Generation)
		if r.Stale {
			gen += " (latest " + strconv.Itoa(r.LatestGeneration) + "; up --refresh)"
		}
		idle := ""
		if r.LastActivityAt != nil && (r.State == string(domain.StateReady) || r.State == string(domain.StateRunning)) {
			idle = "  idle " + time.Since(*r.LastActivityAt).Round(time.Second).String()
		}
		s.out.human("%-36s %-7s %-9s %-14s %3d cpu %8s  age %s%s", r.Project+"/"+r.Instance, r.Kind, r.State, gen,
			r.CPUs, humanBytes(r.MemoryBytes), time.Since(r.CreatedAt).Round(time.Second), idle)
	}
	if len(rows) == 0 {
		s.out.human("no boxes; `%s box image build PROJECT` makes a project image, `%s box up PROJECT` an instance", buildinfo.Name, buildinfo.Name)
	}
	return nil
}

// boxRows lists the caller's boxes (of project, or all), images first per
// project.
func (s *session) boxRows(ctx context.Context, project string) ([]boxRow, error) {
	var states []string
	for _, st := range []domain.VMState{domain.StateProvisioning, domain.StateReady, domain.StateRunning,
		domain.StateStopping, domain.StateStopped, domain.StateError, domain.StateDeleting} {
		states = append(states, string(st))
	}
	var vms []api.VM
	for cursor := ""; ; {
		q := url.Values{"state": states, "limit": {"200"}}
		if project != "" {
			q.Set("label", domain.LabelBox+"="+project)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var l api.VMList
		if err := s.cl.get(ctx, "/vms", q, &l); err != nil {
			return nil, err
		}
		vms = append(vms, l.Items...)
		if cursor = l.NextCursor; cursor == "" {
			break
		}
	}
	latest := map[string]int{}
	rows := []boxRow{}
	for _, vm := range vms {
		p := vm.Labels[domain.LabelBox]
		if p == "" {
			continue
		}
		r := boxRow{Project: p, VM: vm.Name, VMID: string(vm.ID), State: string(vm.State), CreatedAt: vm.CreatedAt,
			CPUs: vm.Shape.CPUs, MemoryBytes: vm.Shape.MemoryBytes, DiskBytes: vm.Shape.DiskBytes, IdleTimeoutSeconds: vm.IdleTimeoutSeconds,
			LastActivityAt: vm.LastActivityAt, UpdatedAt: vm.UpdatedAt}
		switch {
		case vm.Labels[labelBoxRole] == boxRoleImage && vm.Name == imageVM(p):
			r.Kind, r.Instance = "image", imageInstance
			gens, err := s.imageGenerations(ctx, p)
			if err != nil {
				return nil, err
			}
			latest[p] = lastGen(gens)
		case vm.Labels[labelBoxInstance] != "":
			r.Kind, r.Instance = "instance", vm.Labels[labelBoxInstance]
			r.Generation, _ = strconv.Atoi(vm.Labels[labelBoxGeneration])
		default:
			continue
		}
		rows = append(rows, r)
	}
	for i := range rows {
		rows[i].LatestGeneration = latest[rows[i].Project]
		if rows[i].Kind == "image" {
			rows[i].Generation = rows[i].LatestGeneration
		} else {
			rows[i].Stale = rows[i].Generation < rows[i].LatestGeneration
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if (a.Kind == "image") != (b.Kind == "image") {
			return a.Kind == "image"
		}
		return a.Instance < b.Instance
	})
	return rows, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
