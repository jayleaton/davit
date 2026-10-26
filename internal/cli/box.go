package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// Project boxes (docs/project-boxes.md), two layers:
//
//   - The project image: a builder VM (PROJECT--image) where a setup step
//     installs the project's dependencies, kept as numbered disk snapshots
//     (generations). `box image build` makes generation 1, `box image
//     update` runs the setup again on top and makes the next one; older
//     generations stay, so instances made from them keep working.
//   - Instances: one VM per worktree (PROJECT--INSTANCE), created from the
//     latest generation and persistent on their own between down and up.
//     INSTANCE defaults to the current git worktree's branch (else its
//     directory name).
//
// Every box VM is labelled davit.box=PROJECT plus davit.box.role=image
// or davit.box.instance=INSTANCE and davit.box.generation=N. Boxes
// never TTL-stop; they stop themselves after --idle (default 15 min) with
// no exec, process stream or ssh session. ssh, run and sync bring a stopped
// instance up first.

// Box labels.
const (
	labelBoxInstance   = domain.LabelBox + ".instance"
	labelBoxRole       = domain.LabelBox + ".role"
	labelBoxGeneration = domain.LabelBox + ".generation"
	boxRoleImage       = "image"
	imageInstance      = "image" // reserved: the builder
	defaultSetupFile   = ".davit/setup.sh"
	legacySetupFile    = ".slipway/setup.sh" // the name before the davit rename, still read
)

// BoxIdleEnv overrides the default idle timeout of new boxes.
var BoxIdleEnv = buildinfo.Env("BOX_IDLE")

// DefaultBoxIdle is the idle timeout of a box when neither --idle nor
// $DAVIT_BOX_IDLE says otherwise.
const DefaultBoxIdle = 15 * time.Minute

type boxOpts struct {
	idle     optDuration
	wait     optDuration // up/run/ssh/sync: capacity wait (DefaultBoxWait)
	rmOlder  optDuration // gc --rm-older-than
	dryRun   bool        // gc
	pull     string
	noUp     bool
	noSync   bool   // run: do not sync first
	raw      bool   // run: argv as is, no login shell
	git      string // sync: index (default), full, none
	refresh  bool
	setup    string // a local script file
	setupCmd string // or a shell command
	dir      string // the tree to sync into the builder
}

// optDuration is a duration flag that knows whether it was given.
type optDuration struct {
	set bool
	d   time.Duration
}

func (f *optDuration) String() string { return f.d.String() }
func (f *optDuration) Set(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return fmt.Errorf("want a duration such as 15m, got %q", s)
	}
	f.set, f.d = true, d
	return nil
}

func boxFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.image, "image", "", "up without a project image: image for the instance (ref or digest)")
	fs.StringVar(&o.image, "from", "", "image build: base image (ref or digest)")
	shapeFlags(fs, o, true)
	// On an existing box they resize it (box_resize.go).
	for _, f := range []string{"type", "cpus", "memory", "disk"} {
		fs.Lookup(f).Usage += "; resizes an existing box (a running one restarts, disk kept) and it keeps the shape"
	}
	fs.Var(&o.box.idle, "idle", "stop the box after this long unused (default 15m, $"+BoxIdleEnv+"; 0s: never); gc: stop boxes idle at least this long (default 10m)")
	fs.Var(&o.box.wait, "wait", "up/run/ssh/sync: when the host is full, wait this long in its queue for capacity (default 3m; 0s: fail at once)")
	fs.Var(ageFlag{&o.box.rmOlder}, "rm-older-than", "gc: delete instances stopped at least this long, e.g. 7d (never a project image)")
	fs.BoolVar(&o.box.dryRun, "dry-run", false, "gc: list what would be stopped or deleted")
	fs.BoolVar(&o.box.refresh, "refresh", false, "up: recreate the instance from the latest image generation (same CPUs and memory; the disk is discarded)")
	fs.StringVar(&o.box.setup, "setup", "", "image build/update: setup script (default "+defaultSetupFile+", else "+legacySetupFile+", in the tree)")
	fs.StringVar(&o.box.setupCmd, "setup-cmd", "", "image build/update: setup shell command instead of a script")
	fs.StringVar(&o.box.dir, "dir", "", "image build/update: DIR[:GUEST_PATH] synced into the builder first (default the worktree); the setup runs there")
	fs.BoolVar(&o.force, "force", false, "down: kill without clean shutdown; image rm: also delete the project's instances")
	fs.BoolVar(&o.yes, "yes", false, "rm: confirm")
	fs.StringVar(&o.box.pull, "pull", "", "sync: copy GUEST_PATH (relative to the box's work dir) back here")
	fs.BoolVar(&o.box.noUp, "no-up", false, "ssh/run/sync: fail instead of bringing a stopped box up")
	fs.BoolVar(&o.box.noSync, "no-sync", false, "run: do not sync the worktree first")
	fs.BoolVar(&o.box.raw, "raw", false, "run: exec COMMAND as is (no login shell, no mise shims)")
	fs.StringVar(&o.box.git, "git", "index", "sync/run: git metadata in the box: index (git ls-files/status work), full (this branch's history too), none")
	e := &o.exec
	fs.StringVar(&e.cwd, "cwd", "", "run: working directory in the box (default: this directory's place in the synced tree, ~/work of the image's user or $"+BoxWorkdirEnv+")")
	fs.Var(&e.env, "env", "run: KEY=VALUE (repeatable)")
	fs.Var(&e.env, "e", "run: KEY=VALUE (repeatable)")
	fs.Var(&e.timeout, "timeout", "run: process deadline (exit 124 when it fires)")
	fs.BoolVar(&e.stdin, "stdin", false, "run: forward stdin")
	fs.StringVar(&o.ssh.user, "user", "", "ssh/run: guest user (default the image's)")
	waitFlags(fs, o)
}

// boxName turns a project or instance name into a VM name part:
// lowercase, runs of other characters become '-'.
func boxName(project string) (string, error) {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(project) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if !ok && !unicode.IsSpace(r) && r != '-' && r != '_' && r != '.' {
			return "", usageErr("box name %q: use letters, digits, '-', '_' or '.'", project)
		}
		if !ok {
			dash = b.Len() > 0
			continue
		}
		if dash {
			b.WriteByte('-')
			dash = false
		}
		b.WriteRune(r)
	}
	name := b.String()
	if !domain.ValidName(name) {
		return "", usageErr("box name %q does not make a valid name (1-63 lowercase letters, digits and '-')", project)
	}
	return name, nil
}

// boxRef is a parsed PROJECT[/INSTANCE].
type boxRef struct {
	project, instance string
}

// vm is the instance's VM name: PROJECT--INSTANCE, the instance shortened
// with a hash suffix when the name would pass 63 bytes.
func (r boxRef) vm() string {
	n := r.project + "--" + r.instance
	if len(n) <= 63 {
		return n
	}
	sum := sha256.Sum256([]byte(r.instance))
	keep := 63 - len(r.project) - 2 - 9
	return r.project + "--" + strings.TrimRight(r.instance[:max(keep, 1)], "-") + "-" + hex.EncodeToString(sum[:4])
}

func imageVM(project string) string { return project + "--" + imageInstance }

// parseBoxRef parses PROJECT[/INSTANCE]; without INSTANCE it is the current
// worktree's (defaultInstance).
func parseBoxRef(arg string) (boxRef, error) {
	p, inst, has := strings.Cut(arg, "/")
	project, err := boxName(p)
	if err != nil {
		return boxRef{}, err
	}
	if len(project) > 40 {
		return boxRef{}, usageErr("box project %q is longer than 40 characters", p)
	}
	if !has {
		inst = defaultInstance()
	}
	if inst = sanitizeInstance(inst); inst == "" {
		return boxRef{}, usageErr("box instance in %q is empty", arg)
	}
	if inst == imageInstance {
		return boxRef{}, usageErr("instance name %q is reserved for the project image (box image ...)", imageInstance)
	}
	return boxRef{project: project, instance: inst}, nil
}

// sanitizeInstance maps a branch or directory name to an instance name:
// lowercase letters and digits, every other run as one '-'.
func sanitizeInstance(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	return b.String()
}

func runBox(ctx context.Context, s *session, o *opts, args []string) error {
	return boxHint(runBoxCmd(ctx, s, o, args))
}

// boxHint adds what to try to the failures an agent can act on.
func boxHint(err error) error {
	var ce *cliError
	if !errors.As(err, &ce) {
		return err
	}
	switch ce.Code {
	case domain.CodeResourceExhausted:
		ce.Message += "; the host is full: free capacity with `" + buildinfo.Name + " box ls` then `" + buildinfo.Name +
			" box down PROJECT/INSTANCE` (or `box rm`), ask for less with --cpus/--memory/--disk, or queue longer with --wait 10m"
	case domain.CodeUnavailable:
		if !strings.Contains(ce.Message, "connect") {
			ce.Message += "; `" + buildinfo.Name + " connect --check` diagnoses the default host (`" +
				buildinfo.Name + " client-config` shows it), then retry"
		}
	}
	return err
}

func runBoxCmd(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) == 0 {
		return usageErr("usage: %s box %s", buildinfo.Name, boxUsage)
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "ls", "list":
		if len(args) > 1 {
			return usageErr("box ls [PROJECT]")
		}
		project := ""
		if len(args) == 1 {
			p, err := boxName(args[0])
			if err != nil {
				return err
			}
			project = p
		}
		return s.boxList(ctx, project)
	case "gc":
		if len(args) > 1 {
			return usageErr("box gc [PROJECT] [--idle 10m] [--rm-older-than 7d] [--dry-run]")
		}
		project := ""
		if len(args) == 1 {
			p, err := boxName(args[0])
			if err != nil {
				return err
			}
			project = p
		}
		return s.boxGC(ctx, o, project)
	case "image":
		return s.boxImage(ctx, o, args)
	}
	// PROJECT[/INSTANCE] is optional: by default the current repository
	// and worktree (box_repo.go). `box sync DIR` with a path-like first
	// argument still names DIR.
	refArg, rest := "", args
	if len(args) > 0 && !(sub == "sync" && looksLikePath(args[0])) {
		refArg, rest = args[0], args[1:]
	}
	if refArg == "" {
		refArg = defaultProject()
	} else if refArg[0] == '/' { // "/INSTANCE": this project, another worktree
		refArg = defaultProject() + refArg
	}
	ref, err := parseBoxRef(refArg)
	if err != nil {
		return err
	}
	switch sub {
	case "up":
		if len(rest) > 0 {
			return usageErr("box up takes one PROJECT[/INSTANCE]")
		}
		_, err := s.boxUp(ctx, o, ref, true)
		return err
	case "down":
		if len(rest) > 0 {
			return usageErr("box down takes one PROJECT[/INSTANCE]")
		}
		return s.boxDown(ctx, o, ref.vm(), ref.project)
	case "rm":
		if len(rest) > 0 {
			return usageErr("box rm takes one PROJECT[/INSTANCE]")
		}
		if _, err := s.boxVM(ctx, ref.vm(), ref.project); err != nil {
			return err
		}
		if err := s.confirm(o, fmt.Sprintf("delete box %s/%s and its disk?", ref.project, ref.instance)); err != nil {
			return err
		}
		return s.lifecycle(ctx, o, http.MethodDelete, vmPath(ref.vm()), nil)
	case "ssh":
		if len(rest) > 0 {
			return usageErr("box ssh PROJECT[/INSTANCE] [-- COMMAND...]")
		}
		if err := s.boxReady(ctx, o, ref); err != nil {
			return err
		}
		return runSSH(ctx, s, o, []string{ref.vm()})
	case "run":
		if len(rest) > 0 || len(o.exec.argv) == 0 {
			return usageErr("box run needs a command after --: %s box run PROJECT[/INSTANCE] -- COMMAND...", buildinfo.Name)
		}
		if err := s.boxReady(ctx, o, ref); err != nil {
			return err
		}
		// By default the box runs this worktree's current files: sync first
		// (a merge of the git selection; cheap when little changed).
		if tree := defaultTree(); !o.box.noSync && tree != "" {
			if err := s.boxSync(ctx, o, ref, tree, true); err != nil {
				return err
			}
		}
		if o.exec.cwd == "" {
			o.exec.cwd = s.boxCwd(ctx, ref.vm())
		} else if !strings.HasPrefix(o.exec.cwd, "/") {
			o.exec.cwd = path.Join(s.boxWorkdir(ctx, ref.vm()), o.exec.cwd)
		}
		o.exec.user = o.ssh.user
		if !o.box.raw {
			// A login shell: the image's profile (toolchains, PATH) and the
			// project's mise pins (shims resolve per directory).
			o.exec.argv = append([]string{"bash", "-lc", boxRunPrelude, "box-run"}, o.exec.argv...)
		}
		return runExec(ctx, s, o, []string{ref.vm()})
	case "sync":
		if err := s.boxReady(ctx, o, ref); err != nil {
			return err
		}
		if o.box.pull != "" {
			return s.boxPull(ctx, ref.vm(), o.box.pull, rest)
		}
		if len(rest) > 1 {
			return usageErr("box sync [PROJECT[/INSTANCE]] [DIR[:GUEST_PATH]]")
		}
		spec := defaultTree()
		if len(rest) == 1 {
			spec = rest[0]
		}
		if spec == "" {
			return usageErr("not in a git work tree: run box sync inside one, or pass DIR (a git work tree)")
		}
		return s.boxSync(ctx, o, ref, spec, false)
	}
	return usageErr("unknown box command %q: %s box %s", sub, buildinfo.Name, boxUsage)
}

// boxVM returns a box's VM: not_found when there is none, invalid_state
// when a VM of that name exists but is not a box of project.
func (s *session) boxVM(ctx context.Context, name, project string) (api.VM, error) {
	vm, err := s.getVM(ctx, name)
	if err != nil {
		if isCode(err, domain.CodeNotFound) {
			return vm, clientErr(domain.CodeNotFound, "no box %s", name)
		}
		return vm, err
	}
	if vm.State == domain.StateDeleted {
		return vm, clientErr(domain.CodeNotFound, "box %s was deleted", name)
	}
	if vm.Labels[domain.LabelBox] != project {
		return vm, clientErr(domain.CodeInvalidState, "a VM named %s exists but is not a box of project %s (no label %s=%s)",
			name, project, domain.LabelBox, project)
	}
	return vm, nil
}

// boxIdle is the idle timeout new boxes get.
func (o *opts) boxIdle() (time.Duration, error) {
	if o.box.idle.set {
		return o.box.idle.d, nil
	}
	if v := os.Getenv(BoxIdleEnv); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return 0, usageErr("%s=%q: want a duration such as 15m", BoxIdleEnv, v)
		}
		return d, nil
	}
	return DefaultBoxIdle, nil
}

// imageGen is one generation of a project image.
type imageGen struct {
	N        int
	Snapshot api.Snapshot
}

// imageSnapshotName names generation n of project's image.
func imageSnapshotName(project string, n int) string { return fmt.Sprintf("%s-image-g%d", project, n) }

// imageGenerations lists project's image generations, oldest first (none
// without an image).
func (s *session) imageGenerations(ctx context.Context, project string) ([]imageGen, error) {
	var l api.SnapshotList
	if err := s.cl.get(ctx, vmPath(imageVM(project))+"/snapshots", url.Values{"limit": {"200"}}, &l); err != nil {
		if isCode(err, domain.CodeNotFound) {
			return nil, nil
		}
		return nil, err
	}
	prefix := project + "-image-g"
	var gens []imageGen
	for _, sn := range l.Items {
		if n, err := strconv.Atoi(strings.TrimPrefix(sn.Name, prefix)); err == nil && strings.HasPrefix(sn.Name, prefix) && n > 0 {
			gens = append(gens, imageGen{N: n, Snapshot: sn})
		}
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i].N < gens[j].N })
	return gens, nil
}

// boxUpResult is `box up`'s JSON result.
type boxUpResult struct {
	Box        string  `json:"box"`
	Project    string  `json:"project"`
	Instance   string  `json:"instance"`
	Action     string  `json:"action"` // created, resumed, running, refreshed
	Generation int     `json:"generation"`
	Seconds    float64 `json:"seconds"`
	SSHHost    string  `json:"sshHost"`
	VM         api.VM  `json:"vm"`
}

// boxUp brings an instance up: resumes it if it is stopped (or failed),
// waits out an operation in progress, creates it from the latest image
// generation if there is none (or, with --refresh, if it is older), and
// restarts it with --type/--cpus/--memory/--disk when they change its
// shape (box_resize.go). report prints the result (box up itself);
// ssh/run/sync only say what they did.
func (s *session) boxUp(ctx context.Context, o *opts, ref boxRef, report bool) (api.VM, error) {
	start := time.Now()
	name := ref.vm()
	refreshed := false
	if o.box.refresh {
		old, err := s.boxRefresh(ctx, o, ref)
		if err != nil {
			return api.VM{}, err
		}
		if refreshed = old != nil; refreshed && o.typ == "" {
			// The new disk starts from the image (at its size); the CPUs
			// and memory stay unless given.
			if o.cpus == 0 {
				o.cpus = old.CPUs
			}
			if o.memory.v == 0 {
				o.memory.v = old.MemoryBytes
			}
		}
	}
	vm, action, err := s.ensureRunning(ctx, o, name, ref.project, func() (*api.CreateVMRequest, error) {
		return s.instanceRequest(ctx, o, ref)
	})
	if err != nil {
		return vm, err
	}
	if refreshed {
		action = "refreshed"
	}
	gen, _ := strconv.Atoi(vm.Labels[labelBoxGeneration])
	res := boxUpResult{Box: ref.project + "/" + ref.instance, Project: ref.project, Instance: ref.instance, Action: action,
		Generation: gen, Seconds: time.Since(start).Seconds(), SSHHost: sshHostPrefix + string(vm.ID), VM: vm}
	if report {
		s.out.result("", res)
		s.out.human("box %s/%s %s in %.1fs (image generation %d, %s); ssh: %s box ssh %s/%s",
			ref.project, ref.instance, action, res.Seconds, gen, vm.ID, buildinfo.Name, ref.project, ref.instance)
	} else if action != "running" {
		s.out.diag("box %s/%s %s in %.1fs", ref.project, ref.instance, action, res.Seconds)
	}
	return vm, nil
}

// instanceRequest is the createVM body of a new instance: from the latest
// image generation, or from --image (the host default image) when the
// project has no image yet.
func (s *session) instanceRequest(ctx context.Context, o *opts, ref boxRef) (*api.CreateVMRequest, error) {
	idle, err := o.boxIdle()
	if err != nil {
		return nil, err
	}
	gens, err := s.imageGenerations(ctx, ref.project)
	if err != nil {
		return nil, err
	}
	// The project's tree (as the image build reads it) decides a JS default.
	tree := defaultTree()
	if o.box.dir != "" {
		if ps, err := parsePushSpec(o.box.dir); err == nil {
			tree = ps.dir
		}
	}
	no := false
	body := &api.CreateVMRequest{Name: ref.vm(), Type: o.typ, Shape: s.newBoxShape(ctx, o, tree), AutoStop: &no, IdleTimeoutSeconds: int64(idle / time.Second),
		Labels: map[string]string{domain.LabelBox: ref.project, labelBoxInstance: ref.instance, labelBoxGeneration: "0"}}
	if len(gens) == 0 {
		// The first instance of a project builds its image (the setup from
		// .davit/setup.sh or detected from the tree; on --image, else the
		// host's default image) and starts from it.
		s.out.diag("box %s has no project image yet: building it first (`%s box image build` does this alone)", ref.project, buildinfo.Name)
		if err := s.buildImage(ctx, o, ref.project, true, false); err != nil {
			return nil, err
		}
		if gens, err = s.imageGenerations(ctx, ref.project); err != nil {
			return nil, err
		}
		if len(gens) == 0 {
			return nil, clientErr(domain.CodeInternal, "box %s: the image build left no generation", ref.project)
		}
	} else if o.image != "" {
		return nil, usageErr("--image applies only to a project without an image; this one has generation %d (`box image update`)", gens[len(gens)-1].N)
	}
	last := gens[len(gens)-1]
	body.SnapshotID = last.Snapshot.ID
	body.Labels[labelBoxGeneration] = strconv.Itoa(last.N)
	return body, nil
}

// boxRefresh deletes an instance that is older than the latest image
// generation (it is then created again). It returns the deleted
// instance's shape, nil when it kept the instance.
func (s *session) boxRefresh(ctx context.Context, o *opts, ref boxRef) (*domain.Shape, error) {
	vm, err := s.boxVM(ctx, ref.vm(), ref.project)
	if isCode(err, domain.CodeNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	gens, err := s.imageGenerations(ctx, ref.project)
	if err != nil || len(gens) == 0 {
		return nil, err
	}
	have, _ := strconv.Atoi(vm.Labels[labelBoxGeneration])
	latest := gens[len(gens)-1].N
	if have >= latest {
		s.out.diag("box %s/%s already runs image generation %d", ref.project, ref.instance, have)
		return nil, nil
	}
	s.out.diag("warning: refreshing box %s/%s from image generation %d to %d discards its disk: anything not synced out "+
		"(`box sync --pull`) or pushed from inside it is lost", ref.project, ref.instance, have, latest)
	acc, err := s.cl.mutate(ctx, http.MethodDelete, vmPath(string(vm.ID)), nil, newIdempotencyKey())
	if err != nil {
		return nil, err
	}
	return &vm.Shape, s.await(ctx, o, &acc.Operation)
}

// ensureRunning brings VM name (a box of project) to ready: waits out an
// operation, resumes it when stopped or failed, creates it with create()
// when there is none (create nil: not_found), and restarts a running one
// whose shape --type/--cpus/--memory/--disk change (box_resize.go). The
// action is running (nothing to do), created, resumed or resized.
func (s *session) ensureRunning(ctx context.Context, o *opts, name, project string, create func() (*api.CreateVMRequest, error)) (vm api.VM, action string, err error) {
	action = "running"
	checked := o.typ == "" && o.shape() == nil // nothing to resize to
	for attempt := 0; ; attempt++ {
		if attempt > 8 {
			return vm, action, clientErr(domain.CodeUnavailable, "box %s did not settle; try again", name)
		}
		vm, err = s.boxVM(ctx, name, project)
		switch {
		case isCode(err, domain.CodeNotFound):
			if create == nil {
				return vm, action, err
			}
			body, err := create()
			if err != nil {
				return vm, action, err
			}
			acc, err := s.capacityMutate(ctx, "/vms", body, o.boxWait())
			if isCode(err, domain.CodeInvalidArgument) && strings.Contains(err.Error(), "already exists") {
				continue // created concurrently: look again
			}
			if err != nil {
				return vm, action, err
			}
			s.out.diag("box %s: creating (operation %s)", name, acc.Operation.ID)
			if err := s.await(ctx, o, &acc.Operation); err != nil {
				return vm, action, err
			}
			action, checked = "created", true
			continue
		case err != nil:
			return vm, action, err
		}
		if vm.ActiveOperationID != "" {
			s.out.diag("box %s: waiting for operation %s", name, vm.ActiveOperationID)
			op := api.Operation{ID: vm.ActiveOperationID}
			if err := s.await(ctx, o, &op); err != nil {
				return vm, action, err
			}
			continue
		}
		switch vm.State {
		case domain.StateReady, domain.StateRunning:
			if checked {
				return vm, action, nil
			}
			checked = true
			step, err := s.stopToResize(ctx, o, vm)
			switch {
			case err != nil:
				return vm, action, err
			case step == resizeStopped:
				action = "resized"
			case step == resizeResumed && action == "running":
				action = "resumed"
			case step == resizeAgain:
				checked = false
			}
			continue
		case domain.StateStopped, domain.StateError:
			body := api.ResumeVMRequest{Type: o.typ, Shape: o.shape()}
			acc, err := s.capacityMutate(ctx, vmPath(name)+"/resume", body, o.boxWait())
			if isCode(err, domain.CodeOperationInProgress) {
				continue
			}
			if err != nil {
				if action == "resized" {
					err = resizeStranded(err, vm)
				}
				return vm, action, err
			}
			s.out.diag("box %s: resuming (operation %s)", name, acc.Operation.ID)
			if err := s.await(ctx, o, &acc.Operation); err != nil {
				return vm, action, err
			}
			if action == "running" {
				action = "resumed"
			}
			checked = true // the resume booted the new shape
			continue
		}
		time.Sleep(200 * time.Millisecond) // provisioning, stopping: an operation is about to show
	}
}

// boxExecQuiet runs argv detached in name and waits for it (budget),
// returning its exit status; its output stays in the job history.
func (s *session) boxExecQuiet(ctx context.Context, name string, argv []string, cwd string, budget time.Duration) (api.Process, error) {
	var p api.Process
	body := api.ExecRequest{Argv: argv, Cwd: cwd, Detached: true, DeadlineSeconds: int64(budget / time.Second)}
	if err := s.cl.do(ctx, http.MethodPost, vmPath(name)+"/processes", nil, body,
		map[string]string{api.HeaderIdempotencyKey: newIdempotencyKey()}, &p); err != nil {
		return p, err
	}
	return s.waitProcess(ctx, name, p.ID, budget+10*time.Second), nil
}

// boxReady makes sure the instance runs (bringing it up unless --no-up).
func (s *session) boxReady(ctx context.Context, o *opts, ref boxRef) error {
	if o.box.noUp {
		vm, err := s.boxVM(ctx, ref.vm(), ref.project)
		if err != nil {
			return err
		}
		if vm.State != domain.StateReady && vm.State != domain.StateRunning {
			return clientErr(domain.CodeInvalidState, "box %s/%s is %s; bring it up with `%s box up %s/%s`",
				ref.project, ref.instance, vm.State, buildinfo.Name, ref.project, ref.instance)
		}
		return nil
	}
	_, err := s.boxUp(ctx, o, ref, false)
	return err
}

// boxDown flushes the guest's page cache and stops a box, committing its
// disk; the next `box up` boots that disk.
func (s *session) boxDown(ctx context.Context, o *opts, name, project string) error {
	start := time.Now()
	vm, err := s.boxVM(ctx, name, project)
	if err != nil {
		return err
	}
	if vm.ActiveOperationID != "" {
		op := api.Operation{ID: vm.ActiveOperationID}
		if err := s.await(ctx, o, &op); err != nil {
			return err
		}
		if vm, err = s.boxVM(ctx, name, project); err != nil {
			return err
		}
	}
	if vm.State != domain.StateReady && vm.State != domain.StateRunning {
		s.out.result("", map[string]any{"box": name, "action": "none", "state": vm.State})
		s.out.human("box %s is already %s", name, vm.State)
		return nil
	}
	if err := s.stopBox(ctx, o, name); err != nil {
		return err
	}
	secs := time.Since(start).Seconds()
	s.out.result("", map[string]any{"box": name, "action": "stopped", "seconds": secs})
	s.out.human("box %s down in %.1fs (disk kept; `%s box up` resumes it)", name, secs, buildinfo.Name)
	return nil
}

// stopBox syncs the guest's writes and stops it.
func (s *session) stopBox(ctx context.Context, o *opts, name string) error {
	if !o.force {
		// A clean stop flushes too; sync first so a guest that ignores the
		// shutdown request still leaves its writes on disk.
		if _, err := s.boxExecQuiet(ctx, name, []string{"sync"}, "/", time.Minute); err != nil {
			s.out.diag("box %s: sync before stop skipped: %v", name, err)
		}
	}
	acc, err := s.cl.mutate(ctx, http.MethodPost, vmPath(name)+"/stop", api.StopVMRequest{Force: o.force}, newIdempotencyKey())
	if err != nil {
		return err
	}
	return s.await(ctx, o, &acc.Operation)
}

// waitProcess polls a process until it exits or budget passes and returns
// its last state.
func (s *session) waitProcess(ctx context.Context, vm, pid string, budget time.Duration) api.Process {
	var p api.Process
	for deadline := time.Now().Add(budget); time.Now().Before(deadline); {
		if err := s.cl.get(ctx, procPath(vm, pid), nil, &p); err != nil || p.ExitedAt != nil {
			return p
		}
		select {
		case <-ctx.Done():
			return p
		case <-time.After(100 * time.Millisecond):
		}
	}
	return p
}

// boxPull copies GUEST_PATH (relative to the work dir) to LOCAL (default:
// its base name here).
func (s *session) boxPull(ctx context.Context, name, remote string, rest []string) error {
	if len(rest) > 1 {
		return usageErr("box sync PROJECT[/INSTANCE] --pull GUEST_PATH [LOCAL]")
	}
	if !strings.HasPrefix(remote, "/") {
		remote = path.Join(s.boxWorkdir(ctx, name), remote)
	}
	dst := "."
	if len(rest) == 1 {
		dst = rest[0]
	}
	vm, err := s.getVM(ctx, name)
	if err != nil {
		return err
	}
	// A regular file comes as itself; anything else (a directory) as a tar.
	recursive := false
	p := &progress{s: s, start: time.Now()}
	stop := p.run()
	res, err := s.download(ctx, cpOpts{}, vm, remote, dst, p)
	if isCode(err, domain.CodeInvalidArgument) {
		recursive = true
		res, err = s.download(ctx, cpOpts{recursive: true}, vm, remote, dst, p)
	}
	stop()
	if err != nil {
		return err
	}
	res.Source, res.Recursive = name+":"+remote, recursive
	s.out.result("", res)
	s.out.diag("pulled %s to %s (%s in %s)", res.Source, res.Destination, humanBytes(res.Bytes), time.Since(p.start).Round(time.Millisecond))
	return nil
}
