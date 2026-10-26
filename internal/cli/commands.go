package cli

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// DefaultWait is the lifecycle wait budget when neither --wait-timeout nor
// --timeout is set.
const DefaultWait = 10 * time.Minute

func waitFlags(fs *flag.FlagSet, o *opts) {
	fs.BoolVar(&o.noWait, "no-wait", false, "return once the operation is accepted")
	fs.DurationVar(&o.waitTimeout, "wait-timeout", 0, "how long to wait for the operation")
	fs.StringVar(&o.key, "idempotency-key", "", "deduplicate separate invocations")
}

func ttlFlags(fs *flag.FlagSet, o *opts) {
	fs.DurationVar(&o.ttl, "ttl", 0, "auto-stop after this long (default 1h)")
	fs.BoolVar(&o.noAutoStop, "no-auto-stop", false, "disable TTL auto-stop")
}

func shapeFlags(fs *flag.FlagSet, o *opts, disk bool) {
	fs.StringVar(&o.typ, "type", "", "administrator-defined profile (see `capabilities`)")
	fs.IntVar(&o.cpus, "cpus", 0, "vCPUs")
	fs.Var(&o.memory, "memory", "memory, e.g. 2GiB")
	if disk {
		fs.Var(&o.disk, "disk", "disk size, e.g. 20GiB (cannot shrink)")
	}
}

func commands() map[string]cmd {
	cs := []cmd{
		{name: "new", usage: "[NAME] [flags]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.image, "image", "", "image ref or digest")
			fs.StringVar(&o.from, "from", "", "snapshot ID to start from")
			fs.Var(&o.env, "env", "KEY=VALUE (repeatable)")
			fs.Var(&o.env, "e", "KEY=VALUE (repeatable)")
			fs.BoolVar(&o.requireGolden, "require-golden", false, "fail instead of cold booting")
			fs.StringVar(&o.environment, "environment", "", "environment NAME[@REV] to prepare the workspace from")
			fs.IntVar(&o.count, "count", 1, "create N VMs (1-100) from these flags; NAME names them NAME-1..NAME-N")
			egressFlags(fs, o)
			privacyFlags(fs, o)
			shapeFlags(fs, o, true)
			ttlFlags(fs, o)
			waitFlags(fs, o)
		}, run: runNew},
		{name: "list", usage: "[flags]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.BoolVar(&o.all, "all", false, "include every state")
			fs.StringVar(&o.filter, "filter", "", "comma-separated states")
			fs.IntVar(&o.limit, "limit", 0, "page size (max 200)")
			fs.StringVar(&o.cursor, "cursor", "", "page cursor")
		}, run: runList},
		{name: "info", usage: "[ID] [--watch [--until STATE[,STATE]]]", flags: infoFlags, run: runInfo},
		{name: "stop", usage: "[ID] [flags]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.BoolVar(&o.force, "force", false, "kill without clean shutdown (crash-consistent disk)")
			waitFlags(fs, o)
		}, run: runStop},
		{name: "resume", usage: "[ID] [flags]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.environment, "environment", "", "re-pin to environment NAME[@REV] and prepare again")
			shapeFlags(fs, o, true)
			ttlFlags(fs, o)
			waitFlags(fs, o)
		}, run: runResume},
		{name: "resize", usage: resizeUsage, flags: resizeFlags, run: runResize},
		{name: "recover", usage: "[ID] [--yes] [--no-wait]", flags: recoverFlags, run: runRecover},
		{name: "fork", usage: "ID [NAME] [flags]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.snapshot, "snapshot", "", "snapshot of ID to fork (default: last committed)")
			fs.BoolVar(&o.live, "live", false, "fork the running VM itself: RAM, processes and disk at one instant (source pauses briefly)")
			fs.StringVar(&o.environment, "environment", "", "prepare the fork from environment NAME[@REV] (default: inherit the source's)")
			egressFlags(fs, o)
			shapeFlags(fs, o, false)
			ttlFlags(fs, o)
			waitFlags(fs, o)
		}, run: runFork},
		{name: "egress", usage: "[ID] off|filtered|allowlist [--allow a,b]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.Var(&o.allow, "allow", "allowlist entries: domains (github.com covers subdomains) and IPv4 CIDRs; comma-separated or repeated")
		}, run: runEgress},
		{name: "privacy", usage: privacyUsage, flags: privacyFlags, run: runPrivacy},
		{name: "delete", usage: "ID [--yes]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
			waitFlags(fs, o)
		}, run: runDelete},
		{name: "snapshots", usage: "[ID]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.IntVar(&o.limit, "limit", 0, "page size (max 200)")
			fs.StringVar(&o.cursor, "cursor", "", "page cursor")
		}, run: runSnapshots},
		{name: "operations", usage: "OPERATION_ID [--watch]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.BoolVar(&o.watch, "watch", false, "follow until the operation finishes")
		}, run: runOperations},
		{name: "capabilities", usage: "", run: runCapabilities},
		{name: "extend", usage: "[ID] [DURATION]", run: runExtend},
		{name: "rename", usage: "ID NEW-NAME", run: runRename},
		{name: "snapshot", usage: "create [ID] NAME | delete SNAPSHOT [--yes] | tree SNAPSHOT [PATH] [--all] | pull SNAPSHOT PATH [DEST] | export SNAPSHOT -o FILE [--format tar|raw]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation")
			waitFlags(fs, o)
			snapshotFileFlags(fs, o)
		}, run: runSnapshot},
		{name: "context", usage: "use ID | show | clear", run: runContext},
		{name: "history", usage: "[ID] [PROCESS_ID] [--stream stdout|stderr]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.stream, "stream", "", "only stdout or stderr (with PROCESS_ID)")
			fs.IntVar(&o.limit, "limit", 0, "page size (max 200)")
			fs.StringVar(&o.cursor, "cursor", "", "page cursor")
		}, run: runHistory},
		{name: "ps", usage: "[ID]", run: runPS},
		{name: "env", usage: envUsage, flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.file, "file", "", "manifest JSON file (- for stdin)")
			fs.Int64Var(&o.ifRevision, "if-revision", 0, "revise only if the latest revision is N")
			fs.StringVar(&o.key, "idempotency-key", "", "deduplicate separate invocations (revise)")
			fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation (delete)")
			fs.IntVar(&o.limit, "limit", 0, "page size (list, revisions)")
			fs.StringVar(&o.cursor, "cursor", "", "page cursor (list, revisions)")
		}, run: runEnv},
		{name: "members", usage: "[list] | add LOGIN --role ROLE | role MEMBER ROLE | remove MEMBER", flags: memberFlags, run: runMembers},
		{name: "share", usage: "[ID] MEMBER|all [--role ROLE] [--credentials] [--snapshot S | --environment E]", flags: shareFlags, run: runShare},
		{name: "unshare", usage: "[ID] MEMBER|all [--snapshot S | --environment E]", flags: shareFlags, run: runUnshare},
		{name: "shares", usage: "[ID] [--snapshot S | --environment E]", flags: shareFlags, run: runShares},
		{name: "desktop", usage: "open|url|close [ID] [--port N] [--browser]", flags: desktopFlags, run: runDesktop},
		{name: "expose", usage: "[ID] PORT [--name NAME] [--forward-identity]", flags: exposeFlags, run: runExpose},
		{name: "endpoints", usage: "[ID] | hide|show|delete [ID] NAME | rename [ID] NAME NEW", run: runEndpoints},
		{name: "events", usage: "[--follow] [--types T,..] [--vm VM] [--start oldest|latest]", flags: eventsFlags, run: runEvents},
		{name: "webhook", usage: "create URL [--event T]... | list | show ID | update ID ... | delete ID | rotate ID | deliveries ID | retry ID DELIVERY", flags: webhookFlags, run: runWebhook},
		{name: "whoami", usage: "", run: runWhoami},
		{name: "token", usage: "create NAME --action ACTION or --preset NAME [--vm VM]... [--ttl 30d] | list | revoke TOKEN_ID [--yes] | rotate TOKEN_ID [--overlap 10m] | usage [TOKEN_ID]", flags: func(fs *flag.FlagSet, o *opts) {
			tokenFlags(fs, o)
			fs.IntVar(&o.limit, "limit", 0, "page size (list)")
			fs.StringVar(&o.cursor, "cursor", "", "page cursor (list)")
		}, run: runToken},
		{name: "status", usage: "[--config FILE]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.config, "config", "", "daemon config file (local host only)")
		}, run: runStatus},
		{name: "credentials", usage: credUsage, flags: credFlags, run: runCredentials},
		{name: "doctor", usage: "[--config FILE] [-v|--verbose] [--fixes]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.config, "config", "", "daemon config file (default: the init location, if present)")
			fs.BoolVar(&o.verbose, "v", false, "print every check (passed and skipped too) with its full detail and fix")
			fs.BoolVar(&o.verbose, "verbose", false, "same as -v")
			fs.BoolVar(&o.fixes, "fixes", false, "print only the root commands that fix failing or warning checks (`# id: detail` then one command per line; nothing when none is needed) and exit 0; renders the AppArmor profile into the config dir if one is needed")
		}, run: runDoctor},
		{name: "init", usage: "[flags]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.config, "config", "", "config file to write (absolute)")
			fs.StringVar(&o.dataDir, "data-dir", "", "data directory (absolute)")
			fs.StringVar(&o.runtime, "runtime", "", "runtime (default: firecracker; init fails when KVM is unusable; fake is for development only and starts no real VMs)")
			fs.StringVar(&o.storage, "storage", "", `storage mode: "" (probe), reflink or copy`)
			fs.StringVar(&o.hostname, "hostname", "", "requested tailnet hostname")
			fs.StringVar(&o.controlURL, "control-url", "", "tailnet coordination server (empty: Tailscale)")
			fs.BoolVar(&o.authKeyStdin, "auth-key-stdin", false, "read a tailnet auth key from stdin and store it (0600)")
			fs.Var(&o.ownerLogins, "owner-login", "tailnet login that owns this host (repeatable); with an auth key, enables the tailnet gateway")
			fs.Var(&o.ownerTags, "owner-tag", "tailnet tag (tag:NAME) whose nodes own this host (repeatable)")
			fs.Var(&o.images, "image", "guest image manifest to verify and add (repeatable)")
			fs.StringVar(&o.imageBlobs, "image-blobs", "", "blob directory (with sha256/) for --image; default found next to the manifest")
			fs.BoolVar(&o.nonInteractive, "non-interactive", false, "never prompt (init never prompts today)")
			fs.BoolVar(&o.force, "force", false, "regenerate config (old one backed up) and service unit")
			fs.BoolVar(&o.service, "service", false, "install and start a systemd --user unit")
			fs.BoolVar(&o.migrateLegacy, "migrate-from-slipway", false, "only move a host set up before the rename (~/.svm/slipway, ~/.config/slipway*, slipway.service) to --config/--data-dir; see docs/migrating-from-slipway.md")
			fs.BoolVar(&o.stopVMs, "stop-vms", false, "with --migrate-from-slipway: stop the old daemon's live VMs cleanly first (disks kept) instead of refusing")
			fs.BoolVar(&o.apparmor, "apparmor", false, "only write the AppArmor user-namespace profile for this binary (Ubuntu 23.10+) and print the one-time root commands that load it; runs nothing as root")
			fs.StringVar(&o.apparmorBinary, "apparmor-binary", "", "binary the --apparmor profile attaches to (default: this executable)")
		}, run: runInit},
		{name: "prompt", usage: "[ID] TEXT [--harness claude|codex|opencode|pi] [--new|--resume CONV] [--model M] [--env NAME] [--secret NAME] " +
			"[--attach FILE] [--attach-guest PATH] [--effort E] [--fast] [--turn-timeout D]", flags: promptFlags, run: runPrompt},
		{name: "prompt-events", usage: "[ID] [--conversation CONV] [--after CURSOR] [--follow] [--limit N]", flags: promptControlFlags, run: runPromptEvents},
		{name: "interrupt", usage: "[ID] --conversation CONV|--all", flags: promptControlFlags, run: runInterrupt},
		{name: "steer", usage: "[ID] PROMPT TEXT [--no-wait]", flags: promptControlFlags, run: runSteer},
		{name: "conversations", usage: "[ID] [--limit N] [--cursor C]", flags: func(fs *flag.FlagSet, o *opts) {
			fs.IntVar(&o.limit, "limit", 0, "page size (max 200)")
			fs.StringVar(&o.cursor, "cursor", "", "page cursor")
		}, run: runConversations},
		{name: "cp", usage: "[-r] [--user U] SRC DST (one side VM:/path)", flags: cpFlags, run: runCp},
		{name: "ssh", usage: "[ID] [--user USER] [-- COMMAND...] | --proxy ID", flags: sshFlags, run: runSSH, argv: true},
		{name: "ssh-config", usage: "", run: runSSHConfig},
		{name: "scp", usage: "[-r] [--preserve] [--user USER] SRC DST (one side VM:/path)", flags: scpFlags, run: runSCP},
		{name: "forward", usage: "[ID] [LOCAL:]REMOTE | --remote PORT [--local PORT] [--bind ADDR]", flags: forwardFlags, run: runForward},
		{name: "exec", usage: "[ID] [flags] -- COMMAND... | [ID] --status|--attach|--kill PID", flags: execFlags, run: runExec, argv: true},
		{name: "run", usage: "[--image IMG|--from SNAPSHOT] [--keep] [--push DIR[:GUEST_PATH]] [flags] -- COMMAND...", flags: runFlags, run: runRun, argv: true},
		{name: "push", usage: "[ID] DIR[:GUEST_PATH]", run: runPush},
		{name: "box", usage: boxUsage, flags: boxFlags, run: runBox, argv: true, help: boxHelp},
		{name: "connect", usage: connectUsage, flags: connectFlags, run: runConnect},
		{name: "client-config", usage: "[--set-host HOST | --clear] (this machine's default host for the CLI)", flags: clientConfigFlags, run: runClientConfig},
		{name: "dial-stdio", usage: "[--socket PATH] (host side of --host ssh://; splices stdio to the daemon socket)", flags: func(fs *flag.FlagSet, o *opts) {
			fs.StringVar(&o.socket, "socket", "", "daemon socket (default: the configured one)")
		}, run: runDialStdio},
		{name: "completion", usage: completionUsage, run: runCompletion},
	}
	// Commands specified for v0 that later workstreams implement. They fail
	// explicitly instead of running a substitute.
	for _, name := range []string{"image", "suspend"} {
		cs = append(cs, cmd{name: name, usage: "...", run: func(context.Context, *session, *opts, []string) error {
			return clientErr(domain.CodeNotImplemented, "`%s %s` is not implemented in this build", buildinfo.Name, name)
		}})
	}
	m := map[string]cmd{}
	for _, c := range cs {
		m[c.name] = c
	}
	return m
}

func (o *opts) shape() *api.ShapeOverride {
	if o.cpus == 0 && o.memory.v == 0 && o.disk.v == 0 {
		return nil
	}
	return &api.ShapeOverride{CPUs: o.cpus, MemoryBytes: o.memory.v, DiskBytes: o.disk.v}
}

func (o *opts) ttlFields() (*int64, *bool) {
	var ttl *int64
	var auto *bool
	if o.ttl != 0 {
		v := int64(o.ttl / time.Second)
		ttl = &v
	}
	if o.noAutoStop {
		f := false
		auto = &f
	}
	return ttl, auto
}

func (o *opts) idempotencyKey() string {
	if o.key != "" {
		return o.key
	}
	return newIdempotencyKey()
}

func vmPath(id string) string { return "/vms/" + url.PathEscape(id) }

func runNew(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 1, "new [NAME]"); err != nil {
		return err
	}
	body := api.CreateVMRequest{Type: o.typ, Image: o.image, SnapshotID: domain.SnapshotID(o.from), Shape: o.shape(), RequireGolden: o.requireGolden}
	eg, err := o.egressSpec()
	if err != nil {
		return err
	}
	body.Egress = eg
	body.Privacy = o.privacySpec()
	if len(args) == 1 {
		body.Name = args[0]
	}
	for _, kv := range o.env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return usageErr("--env wants KEY=VALUE, got %q", kv)
		}
		if body.Env == nil {
			body.Env = map[string]string{}
		}
		body.Env[k] = v
	}
	body.TTLSeconds, body.AutoStop = o.ttlFields()
	if body.Environment, err = parseEnvRef(o.environment); err != nil {
		return err
	}
	if o.count != 1 {
		return s.newBatch(ctx, o, body)
	}
	return s.lifecycle(ctx, o, "POST", "/vms", body)
}

func runFork(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) < 1 {
		return usageErr("fork needs the source VM: %s fork ID [NAME]", buildinfo.Name)
	}
	if err := maxArgs(args, 2, "fork ID [NAME]"); err != nil {
		return err
	}
	body := api.ForkVMRequest{Type: o.typ, SnapshotID: domain.SnapshotID(o.snapshot), Shape: o.shape(), Live: o.live}
	eg, err := o.egressSpec()
	if err != nil {
		return err
	}
	body.Egress = eg
	if len(args) == 2 {
		body.Name = args[1]
	}
	body.TTLSeconds, body.AutoStop = o.ttlFields()
	if body.Environment, err = parseEnvRef(o.environment); err != nil {
		return err
	}
	return s.lifecycle(ctx, o, "POST", vmPath(args[0])+"/fork", body)
}

func runStop(ctx context.Context, s *session, o *opts, args []string) error {
	id, err := s.currentID(args)
	if err == nil {
		err = maxArgs(args, 1, "stop [ID]")
	}
	if err != nil {
		return err
	}
	return s.lifecycle(ctx, o, "POST", vmPath(id)+"/stop", api.StopVMRequest{Force: o.force})
}

func runResume(ctx context.Context, s *session, o *opts, args []string) error {
	id, err := s.currentID(args)
	if err == nil {
		err = maxArgs(args, 1, "resume [ID]")
	}
	if err != nil {
		return err
	}
	body := api.ResumeVMRequest{Type: o.typ, Shape: o.shape()}
	body.TTLSeconds, body.AutoStop = o.ttlFields()
	if body.Environment, err = parseEnvRef(o.environment); err != nil {
		return err
	}
	return s.lifecycle(ctx, o, "POST", vmPath(id)+"/resume", body)
}

func runDelete(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) != 1 {
		return usageErr("usage: %s delete ID [--yes]", buildinfo.Name)
	}
	if err := s.confirm(o, "Delete "+args[0]+" and its unsaved data?"); err != nil {
		return err
	}
	return s.lifecycle(ctx, o, "DELETE", vmPath(args[0]), nil)
}

// confirm asks before a destructive action unless --yes; scripts and --json
// must pass --yes explicitly.
func (s *session) confirm(o *opts, question string) error {
	if o.yes {
		return nil
	}
	if !s.io.Interactive || s.g.json {
		return usageErr("this is destructive: pass --yes")
	}
	fmt.Fprintf(s.io.Stderr, "%s [y/N] ", question)
	var answer string
	fmt.Fscanln(s.io.Stdin, &answer)
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		return usageErr("aborted")
	}
	return nil
}

func runList(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 0, "list"); err != nil {
		return err
	}
	q := url.Values{}
	if o.all {
		q.Set("all", "true")
	}
	if o.filter != "" {
		q.Set("state", o.filter)
	}
	if o.limit != 0 {
		q.Set("limit", strconv.Itoa(o.limit))
	}
	if o.cursor != "" {
		q.Set("cursor", o.cursor)
	}
	var list api.VMList
	if err := s.cl.get(ctx, "/vms", q, &list); err != nil {
		return err
	}
	if len(list.Items) > 0 {
		s.out.human("%-24s %-20s %-13s %4s %8s", "ID", "NAME", "STATE", "CPUS", "MEMORY")
	}
	for _, vm := range list.Items {
		s.out.emit("item", "", 0, vm, nil)
		s.out.human("%-24s %-20s %-13s %4d %8s", vm.ID, vm.Name, vm.State, vm.Shape.CPUs, bytesHuman(vm.Shape.MemoryBytes))
	}
	if list.NextCursor != "" {
		s.out.diag("more: --cursor %s", list.NextCursor)
	}
	s.out.result("", map[string]any{"count": len(list.Items), "nextCursor": list.NextCursor})
	return nil
}

func runInfo(ctx context.Context, s *session, o *opts, args []string) error {
	id, err := s.currentID(args)
	if err == nil {
		err = maxArgs(args, 1, "info [ID]")
	}
	if err != nil {
		return err
	}
	if o.watch || o.until != "" {
		return s.watchVM(ctx, o, id)
	}
	var vm api.VM
	if err := s.cl.get(ctx, vmPath(id), nil, &vm); err != nil {
		return err
	}
	s.out.result("", vm)
	printVM(s.out, &vm)
	return nil
}

func runSnapshots(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 1, "snapshots [ID]"); err != nil {
		return err
	}
	path := "/snapshots"
	if len(args) == 1 {
		path = vmPath(args[0]) + "/snapshots"
	}
	q := url.Values{}
	if o.limit != 0 {
		q.Set("limit", strconv.Itoa(o.limit))
	}
	if o.cursor != "" {
		q.Set("cursor", o.cursor)
	}
	var list api.SnapshotList
	if err := s.cl.get(ctx, path, q, &list); err != nil {
		return err
	}
	for _, sn := range list.Items {
		s.out.emit("item", "", 0, sn, nil)
		s.out.human("%-26s %-12s %-24s gen %-3d %-6s %s", sn.ID, sn.Name, sn.SourceVMID, sn.Generation, sn.Consistency, sn.CreatedAt.Format(time.RFC3339))
	}
	s.out.result("", map[string]any{"count": len(list.Items), "nextCursor": list.NextCursor})
	return nil
}

func runOperations(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) != 1 {
		return usageErr("usage: %s operations OPERATION_ID [--watch]", buildinfo.Name)
	}
	id := domain.OperationID(args[0])
	if o.watch {
		var op api.Operation
		if err := s.cl.get(ctx, "/operations/"+url.PathEscape(string(id)), nil, &op); err != nil {
			return err
		}
		s.out.emit("operation", op.ID, 0, op, nil)
		_, err := s.follow(ctx, o, &op)
		return err
	}
	var op api.Operation
	if err := s.cl.get(ctx, "/operations/"+url.PathEscape(string(id)), nil, &op); err != nil {
		return err
	}
	s.out.result(op.ID, op)
	s.out.human("%s %s %s phase=%s progress=%.0f%%", op.ID, op.Kind, op.State, op.Phase, op.Progress*100)
	return nil
}

func runCapabilities(ctx context.Context, s *session, o *opts, args []string) error {
	var c api.HostCapabilities
	if err := s.cl.get(ctx, "/capabilities", nil, &c); err != nil {
		return err
	}
	s.out.result("", c)
	s.out.human("runtime %s (arch %v), boot modes %v, storage %s fastClone=%v", c.Runtime.Engine, c.Runtime.Arch, c.BootModes, c.Storage.Mode, c.Storage.FastClone)
	if cp := c.Capacity; cp != nil {
		s.out.human("capacity: %d more default VMs, %d live, %s memory and %s disk free", cp.Slots, cp.LiveVMs, bytesHuman(cp.FreeMemoryBytes), bytesHuman(cp.FreeDiskBytes))
	}
	return nil
}

// lifecycle submits a mutation and, unless --no-wait, follows it: one
// `operation` line, ordered `progress` lines, then exactly one `result` or
// `error`.
func (s *session) lifecycle(ctx context.Context, o *opts, method, path string, body any) error {
	key := o.idempotencyKey()
	acc, err := s.cl.mutate(ctx, method, path, body, key)
	if err != nil {
		return err
	}
	op := acc.Operation
	s.out.emit("operation", op.ID, 0, acc, nil)
	if o.noWait {
		s.out.result(op.ID, acc)
		s.out.human("%s accepted: operation %s (query: %s operations %s)", op.Kind, op.ID, buildinfo.Name, op.ID)
		return nil
	}
	s.out.diag("%s %s: operation %s", op.Kind, op.VMID, op.ID)
	vm, err := s.follow(ctx, o, &op)
	if err != nil {
		return err
	}
	switch op.Kind {
	case domain.OpCreate, domain.OpFork:
		s.selectContext(vm)
	case domain.OpDelete:
		s.forgetContext(op.VMID)
	}
	return nil
}

func (s *session) waitBudget(o *opts) time.Duration {
	switch {
	case o.waitTimeout > 0:
		return o.waitTimeout
	case s.g.timeout > 0:
		return s.g.timeout
	}
	return DefaultWait
}

// follow streams events until op is terminal or the wait budget ends. A
// client timeout never cancels the accepted operation.
func (s *session) follow(ctx context.Context, o *opts, op *api.Operation) (*api.VM, error) {
	if err := s.await(ctx, o, op); err != nil {
		return nil, err
	}
	res := map[string]any{"operation": op}
	var vm *api.VM
	if op.VMID != "" {
		var v api.VM
		if err := s.cl.get(ctx, vmPath(string(op.VMID)), nil, &v); err == nil {
			vm, res["vm"] = &v, v
			defer printVM(s.out, vm)
		}
	}
	if op.Kind == domain.OpSnapshot && op.Result != nil {
		var snap api.Snapshot
		if err := s.cl.get(ctx, "/snapshots/"+url.PathEscape(string(op.Result.SnapshotID)), nil, &snap); err == nil {
			res["snapshot"] = snap
			s.out.human("snapshot %s (%s) of %s, generation %d", snap.Name, snap.ID, snap.SourceVMID, snap.Generation)
		}
	}
	s.out.result(op.ID, res)
	if op.Result != nil {
		s.out.human("%s %s in %dms", op.Kind, op.State, op.Result.DurationMs)
	}
	return vm, nil
}

// await streams op's progress until it is terminal or the wait budget
// ends, and returns its failure; it prints no result (follow does).
func (s *session) await(ctx context.Context, o *opts, op *api.Operation) error {
	budget := s.waitBudget(o)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var after int64
	for {
		wait := min(time.Until(deadline(ctx)), 25*time.Second)
		page, err := s.cl.events(ctx, op.ID, after, max(wait, 0))
		if err != nil {
			if ctx.Err() != nil {
				e := clientErr(domain.CodeTimeout, "stopped waiting after %s; operation %s continues (query: %s operations %s --watch)", budget, op.ID, buildinfo.Name, op.ID)
				e.OperationID = op.ID
				return e
			}
			return err
		}
		for _, ev := range page.Items {
			s.out.emit("progress", op.ID, ev.Seq, ev, nil)
			if ev.Phase != "" && ev.Phase != "done" {
				s.out.diag("  %-18s %3.0f%% %s", ev.Phase, ev.Progress*100, ev.Message)
			}
			after = ev.Seq
		}
		*op = page.Operation
		if op.State.Terminal() && len(page.Items) == 0 {
			break
		}
	}
	if op.State == domain.OpFailed {
		e := &cliError{ErrorBody{Code: domain.CodeInternal, Message: "operation failed", Origin: "operation", OperationID: op.ID}}
		if op.Error != nil {
			e.Code, e.Message, e.Retryable, e.Details = op.Error.Code, op.Error.Message, op.Error.Retryable, op.Error.Details
		}
		return e
	}
	return nil
}

func deadline(ctx context.Context) time.Time {
	d, ok := ctx.Deadline()
	if !ok {
		return time.Now().Add(time.Hour)
	}
	return d
}

func printVM(p *printer, vm *api.VM) {
	live := vm.State == domain.StateReady || vm.State == domain.StateRunning
	boot := string(vm.BootMode)
	if !live && vm.State != domain.StateProvisioning {
		boot = "" // how it last booted is not how it runs now
	}
	p.human("%s (%s)\n  state      %s (rev %d, gen %d)\n  boot       %s\n  shape      %d cpus, %s memory, %s disk\n  storage    %s (fastClone=%v)",
		vm.Name, vm.ID, vm.State, vm.StateRevision, vm.InstanceGeneration, orDash(boot),
		vm.Shape.CPUs, bytesHuman(vm.Shape.MemoryBytes), bytesHuman(vm.Shape.DiskBytes), vm.Storage.Mode, vm.Storage.FastClone)
	eg := string(vm.Egress.Mode)
	if len(vm.Egress.Allow) > 0 {
		eg += " (" + strings.Join(vm.Egress.Allow, ", ") + ")"
	}
	p.human("  egress     %s", orDash(eg))
	if w := privacyWords(vm.Privacy); w != "" {
		p.human("  privacy    %s", w)
	}
	if vm.Environment != nil && vm.Workspace != nil {
		ws := fmt.Sprintf("%s (%s@%d)", vm.Workspace.State, vm.Environment.Name, vm.Environment.Revision)
		if e := vm.Workspace.Error; e != nil {
			ws += fmt.Sprintf(": %s %s failed: %s", e.Phase, e.Step, e.Message)
		}
		p.human("  workspace  %s", ws)
	}
	if vm.LastSnapshotID != "" {
		p.human("  snapshot   %s", vm.LastSnapshotID)
	}
	if vm.ExpiresAt != nil && live {
		p.human("  auto-stop  %s", vm.ExpiresAt.Local().Format(time.RFC3339))
	}
	autoStopNotice(p, vm)
	if vm.LastError != nil {
		p.human("  last error %s: %s", vm.LastError.Code, vm.LastError.Message)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func bytesHuman(n int64) string {
	switch {
	case n >= domain.GiB && n%domain.GiB == 0:
		return fmt.Sprintf("%dGiB", n/domain.GiB)
	case n >= domain.MiB:
		return fmt.Sprintf("%dMiB", n/domain.MiB)
	}
	return fmt.Sprintf("%dB", n)
}
