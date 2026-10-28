// Package cli implements the davit command line (SPEC §3): parsing, the
// --json JSONL envelope, exit codes, lifecycle waits and ID context.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"davit/internal/buildinfo"
	"davit/internal/config"
)

// Stdio is the process I/O.
type Stdio struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Interactive reports whether stdin is a terminal.
	Interactive bool
}

type globals struct {
	host, apiURL string
	apiURLSet    bool // --api-url or DAVIT_API_URL chose the endpoint
	json         bool
	noUpdate     bool
	help         bool
	version      bool
	timeout      time.Duration
}

func (g *globals) register(fs *flag.FlagSet) { g.registerFor(fs, false) }

// registerFor registers the global flags; commandTimeout leaves --timeout
// to the command (exec's --timeout is the process deadline, SPEC §3).
func (g *globals) registerFor(fs *flag.FlagSet, commandTimeout bool) {
	fs.StringVar(&g.host, "host", g.host, "remote host FQDN or URL")
	fs.StringVar(&g.apiURL, "api-url", g.apiURL, "API URL (unix:///path.sock)")
	fs.BoolVar(&g.json, "json", g.json, "JSONL output")
	fs.BoolVar(&g.noUpdate, "no-update", g.noUpdate, "suppress update notices")
	if !commandTimeout {
		fs.DurationVar(&g.timeout, "timeout", g.timeout, "client wait budget")
	}
	fs.BoolVar(&g.help, "help", false, "show help")
	fs.BoolVar(&g.help, "h", false, "show help")
	fs.BoolVar(&g.version, "version", false, "show version")
}

// cmd is one subcommand.
type cmd struct {
	name  string
	usage string
	flags func(fs *flag.FlagSet, o *opts)
	run   func(ctx context.Context, e *session, o *opts, args []string) error
	// argv: everything after a literal "--" is the command's argv
	// (opts.exec.argv) rather than positional arguments; the command owns
	// --timeout.
	argv bool
	// help replaces the generic --help (usage line plus every flag); pos
	// are the positional arguments given with --help.
	help func(w io.Writer, fs *flag.FlagSet, pos []string)
}

// opts holds every subcommand flag; each command registers what it uses.
type opts struct {
	image, from, typ, snapshot, cursor, filter, key string
	egress                                          string
	allow                                           multiFlag
	cpus                                            int
	memory, disk                                    sizeFlag
	env                                             multiFlag
	ownerLogins, ownerTags                          multiFlag
	ttl, waitTimeout                                time.Duration
	noAutoStop, noWait, requireGolden, all          bool
	live, stopFirst                                 bool
	force, yes, watch                               bool
	limit                                           int
	count                                           int    // new --count
	until                                           string // info --watch --until
	environment, file                               string // --environment NAME[@REV]; env --file
	ifRevision                                      int64  // env revise --if-revision
	stream                                          string // history --stream
	privacy                                         privacyOpts

	// host onboarding (doctor/init)
	config, dataDir, runtime, storage, hostname, controlURL string
	authKeyStdin, service, nonInteractive, apparmor         bool
	migrateLegacy, stopVMs                                  bool
	verbose, fixes                                          bool // doctor
	images                                                  multiFlag
	imageBlobs, apparmorBinary                              string

	// exec, run, cp, prompt, ssh/scp/forward
	exec    execOpts
	run     runOpts
	cp      cpOpts
	prompt  promptOpts
	ssh     sshOpts
	token   tokenOpts
	expose  exposeOpts
	desktop desktopOpts
	share   shareOpts
	cred    credOpts
	// snapshot tree/pull/export
	snapFiles snapFileOpts
	// events, webhook
	events eventsOpts
	// box; dial-stdio --socket; client-config
	box         boxOpts
	socket      string
	clientHost  string
	clientClear bool
	connect     connectOpts
}

type session struct {
	g   *globals
	out *printer
	io  Stdio
	cl  *client
	// workdirs caches vmWorkdir per VM for this invocation.
	workdirs map[string]string
}

func newRequestID() string {
	var b [8]byte
	rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

func newIdempotencyKey() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Main runs the CLI and returns the exit code.
func Main(args []string, stdio Stdio) int {
	g := &globals{apiURL: os.Getenv(buildinfo.Env("API_URL")), host: os.Getenv(buildinfo.Env("HOST"))}
	g.apiURLSet = g.apiURL != ""
	if g.apiURL == "" {
		g.apiURL = "unix://" + config.DefaultSocket()
	}
	// JSON mode must be known before any parse error is reported.
	// Only davit's own arguments count: a --json after "--" belongs to the
	// guest command (exec ID -- tool --json).
	own := args
	if i := slices.Index(args, "--"); i >= 0 {
		own = args[:i]
	}
	g.json = slices.Contains(own, "--json") || slices.Contains(own, "-json")
	out := &printer{json: g.json, stdout: stdio.Stdout, stderr: stdio.Stderr, requestID: newRequestID()}

	name, rest, err := splitCommand(g, args)
	if err != nil {
		return out.fail(err)
	}
	if (g.version && name == "") || name == "version" {
		if slices.Contains(rest, "--check") || slices.Contains(rest, "-check") { // whoami.go
			return versionCheck(context.Background(), out, "")
		}
		info := buildinfo.Get()
		out.human("%s", info)
		out.result("", info)
		return ExitOK
	}
	c, ok := commands()[name]
	if !ok {
		if name == "" || name == "help" {
			usage(stdio.Stderr)
			if name == "" && !g.help {
				return ExitUsage
			}
			return ExitOK
		}
		return out.fail(usageErr("unknown command %q; run `%s --help`", name, buildinfo.Name))
	}

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.registerFor(fs, c.argv)
	var o opts
	if c.flags != nil {
		c.flags(fs, &o)
	}
	if c.argv {
		if i := slices.Index(rest, "--"); i >= 0 {
			rest, o.exec.argv = rest[:i], rest[i+1:]
		}
	}
	pos, err := parseInterleaved(fs, rest)
	if err != nil {
		return out.fail(usageErr("%s: %v", name, err))
	}
	fs.Visit(func(f *flag.Flag) { g.apiURLSet = g.apiURLSet || f.Name == "api-url" })
	out.json = g.json
	if g.help && c.help != nil {
		c.help(stdio.Stderr, fs, pos)
		return ExitOK
	}
	if g.help {
		fmt.Fprintf(stdio.Stderr, "usage: %s %s %s\n", buildinfo.Name, name, c.usage)
		fs.SetOutput(stdio.Stderr)
		fs.PrintDefaults()
		return ExitOK
	}
	if g.host == "" && !g.apiURLSet && !hostLocal[name] {
		// No endpoint chosen: client.json's default host, if any.
		if cc, _, err := loadClientConfig(); err != nil {
			return out.fail(err)
		} else if cc.Host != "" {
			g.host = cc.Host
		}
	}
	if g.host != "" {
		// A remote host's tailnet gateway serves the same API. A bare name
		// means plain HTTP on :80 (the tailnet encrypts it); pass an https://
		// URL when the gateway has tailnet HTTPS (tailnet.https).
		u, err := hostURL(g.host)
		if err != nil {
			return out.fail(err)
		}
		g.apiURL, g.apiURLSet = u, true
	}
	cl, err := newClient(g.apiURL, out.requestID)
	if err != nil {
		return out.fail(err)
	}
	// Nothing chose an endpoint: on a client machine that is a missing
	// `davit connect`, which a failed dial of the local socket says.
	cl.noHost = !g.apiURLSet && !hostLocal[name]
	ctx := context.Background()
	if err := c.run(ctx, &session{g: g, out: out, io: stdio, cl: cl}, &o, pos); err != nil {
		var xs *exitStatus
		if errors.As(err, &xs) {
			if xs.msg != "" && !g.json {
				fmt.Fprintln(stdio.Stderr, "error: "+xs.msg)
			}
			return xs.code
		}
		return out.fail(err)
	}
	return ExitOK
}

// hostLocal are the commands about this machine's own daemon (or, for
// connect, the host they set up), which never follow client.json's default
// host. status does: on a client it checks the saved host.
var hostLocal = map[string]bool{"doctor": true, "init": true, "dial-stdio": true,
	"client-config": true, "completion": true, "connect": true}

// hostURL turns --host (a tailnet name, IP, USER@HOST, or URL) into an API
// URL. USER@HOST is the ssh:// transport.
func hostURL(h string) (string, error) {
	if !strings.Contains(h, "://") && strings.Contains(h, "@") && !strings.ContainsAny(h, "/ ") {
		h = "ssh://" + h
	}
	if strings.Contains(h, "://") {
		if strings.HasPrefix(h, "ssh://") { // the daemon's socket over ssh (sshtransport.go)
			return h, nil
		}
		if !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") {
			return "", usageErr("--host must be a name, USER@HOST, an http(s):// URL or ssh://[USER@]HOST")
		}
		return strings.TrimSuffix(h, "/"), nil
	}
	if h == "" || strings.ContainsAny(h, "/ ") {
		return "", usageErr("--host must be a name, USER@HOST or an http(s):// URL")
	}
	return "http://" + h, nil
}

// splitCommand parses leading global flags and returns the command name and
// its arguments.
func splitCommand(g *globals, args []string) (string, []string, error) {
	fs := flag.NewFlagSet(buildinfo.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	g.register(fs)
	if err := fs.Parse(args); err != nil {
		return "", nil, usageErr("%v", err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return "", nil, nil
	}
	return rest[0], rest[1:], nil
}

// parseInterleaved allows flags after positional arguments; everything after
// a literal "--" is positional and passed through untouched.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, tail = args[:i], args[i+1:]
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return append(pos, tail...), nil
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}
}

// sizeFlag parses byte quantities with explicit units: 512MiB, 2GiB, or
// plain bytes.
type sizeFlag struct{ v int64 }

func (s *sizeFlag) String() string { return strconv.FormatInt(s.v, 10) }

func (s *sizeFlag) Set(v string) error {
	units := []struct {
		suffix string
		mul    int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}, {"", 1}}
	for _, u := range units {
		if num, ok := strings.CutSuffix(v, u.suffix); ok {
			n, err := strconv.ParseInt(num, 10, 64)
			if err != nil || n <= 0 {
				break
			}
			s.v = n * u.mul
			return nil
		}
	}
	return errors.New("want a size like 512MiB, 2GiB or a byte count")
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func maxArgs(args []string, n int, usage string) error {
	if len(args) > n {
		return usageErr("too many arguments; usage: %s %s", buildinfo.Name, usage)
	}
	return nil
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `%[1]s %[2]s: fast persistent VMs for AI agents on your own hardware.

Usage: %[1]s [--json] [--host NAME|--api-url URL] [--timeout D] <command> [args]

  --host NAME     talk to a remote host: USER@HOST or ssh://[USER@]HOST[:PORT][/SOCKET][?jump=J]
                  reaches its daemon socket over OpenSSH; NAME is its tailnet gateway
                  (http://NAME; or an https:// URL). Default: the host "connect" saved

Get started (on your laptop):
  connect USER@HOST    set up this machine for a host: SSH key (created if missing, never
                       replaced), key auth (prints how to authorize the key, then waits),
                       davit on the host, saved as the default host (--via JUMP for a
                       jump box; --check re-verifies; --json)
  client-config       show or change the saved default host (--set-host USER@HOST, --clear)

Project boxes (run your repository's commands on the host; "box --help" for each):
  box run [PROJECT[/INSTANCE]] -- CMD...
                       sync this worktree into its box and run CMD; exits with CMD's status
  box up|down|ssh|sync|ls|rm [PROJECT[/INSTANCE]]
                       start, stop, shell into, copy files to or from (--pull), list, delete
  box image build|update|rm [PROJECT]
                       the project image: dependencies installed once (.davit/setup.sh)
  box gc               stop idle boxes, delete long-stopped ones
                       PROJECT/INSTANCE default to this repository and git worktree

Lifecycle:
  new [NAME]          create a VM (--image, --from, --cpus, --memory, --disk, -e, --ttl, --no-wait,
                       --environment NAME[@REV] prepares its workspace: repos, files, setup;
                       --ephemeral, --no-snapshot, --no-output-retention, --no-process-samples)
  list                list VMs (--all, --filter STATES, --limit, --cursor)
  info [ID]           show a VM (--watch [--until STATE] follows changes)
  stop [ID]           stop and commit the disk (--force)
  resume [ID]         boot the last committed disk (--environment re-pins and re-prepares;
                       --type/--cpus/--memory/--disk resize it)
  resize [ID]         change CPUs, memory or (grow only) disk at the next boot (--stop stops a running VM first)
  recover [ID]        power-cycle a wedged VM: kill it, boot its current disk as is (--yes)
  fork ID [NAME]      new VM from a committed snapshot (--snapshot; inherits egress and the
                       environment pin unless --environment)
  egress [ID] MODE    set egress live: off | filtered | allowlist --allow github.com,10.1.2.0/24
                      (new/fork take --egress MODE --allow ... too)
  privacy [ID]        show or change the privacy policy: --ephemeral (delete on stop),
                       --no-snapshot, --no-output-retention, --no-process-samples
                       (each =true|false; flags not given keep their value); not secure erasure
  delete ID           delete a VM (--yes)
  extend [ID] [DUR]   restart the auto-stop countdown (DUR becomes the VM's TTL)
  rename ID NEW       rename a VM
  snapshots [ID]      list disk snapshots
  snapshot create [ID] NAME   name a stopped VM's committed disk
  snapshot delete SNAPSHOT    delete a snapshot (--yes)
  snapshot tree SNAPSHOT [PATH]        list a directory of a snapshot, no boot (--all)
  snapshot pull SNAPSHOT PATH [DEST]   copy a file or directory out of a snapshot (SHA-256 checked)
  snapshot export SNAPSHOT -o FILE     the filesystem as tar, or --format raw for the disk image
  context use ID|show|clear   default VM for interactive terminals
  credentials ...      credential profiles, write-only secrets (stdin) and per-VM grants: the
                       guest gets a placeholder, the host proxy adds the real key
                       (profiles, profile create/show/delete, set, rm, grant, grants, revoke)
  history [ID] [PID]  managed jobs across generations, newest first; with PID its retained
                       output (works while the VM is stopped; --stream stdout|stderr)
  ps [ID]             every guest process (a stopped VM: its last sample)
  env list|create NAME --file M|show NAME[@REV]|revise NAME --file M|revisions NAME|delete NAME
                       versioned workspace environments (JSON manifest: repos, variables,
                       secretFiles, setup); VMs pin one with --environment
  members              tailnet users with rights on what you share: members add LOGIN --role R,
                       members role MEMBER R, members remove MEMBER (owner only)
  share [ID] MEMBER    share a VM (or --snapshot S, --environment E) with a member or "all"
                       (--role viewer|exec|admin, --credentials); unshare [ID] MEMBER cuts
                       its live sessions; shares [ID] lists them
  desktop open|url|close [ID]
                       the VM's VNC desktop as a tailnet-private web viewer (--port, --browser);
                       view rights are view-only, exec adds input
  expose [ID] PORT     stable tailnet-private HTTPS URL for the app on guest port PORT
                       (--name N; --forward-identity sends the caller's tailnet login);
                       owner or a token with exec on the VM
  endpoints [ID]       list app endpoints; endpoints hide|show|delete [ID] NAME,
                       endpoints rename [ID] NAME NEW
  events [--follow]    the host event stream (VM, operation, run, prompt changes); --types vm.,operation.state
  ci status            GitHub Actions runner jobs: queued, running, recent (--limit N; docs/ci-runners.md)
  webhook create URL   signed event deliveries (--event T...); list, show, update, delete, rotate, deliveries, retry
  token create NAME --action view|exec|admin [--vm VM]... [--ttl 30d]
                       service token for automation on the tailnet API (secret shown
                       once; use as DAVIT_TOKEN with --host; --preset ci|readonly|admin);
                       token list, revoke ID, rotate ID [--overlap 10m], usage [ID]
  whoami              who you are to the daemon: principal, auth, token scopes, endpoint
  exec [ID] -- CMD...  run a command (--cwd, -e, --user, --timeout, --stdin, --detach;
                       --status/--attach [--after]/--kill PID); exits with its status
  run [flags] -- CMD...  run a command in a fresh VM and delete it afterwards
                       (--image, --from, --type, --cpus, --memory, --disk, -e, --cwd,
                       --timeout, --keep, --push DIR[:GUEST_PATH]); exits with its
                       status (124 on --timeout); Ctrl-C cancels the run
  push [ID] DIR[:GUEST_PATH]  upload DIR's tracked and untracked-not-ignored files
                       (submodules included, never .git) under GUEST_PATH
                       (default /home/davit/work), adding and replacing files
  cp [-r] SRC DST      copy files through the guest agent; one side is VM:/path
                       (--user; -r streams a directory as tar, keeping modes and
                       symlinks)
  ssh [ID] [-- CMD...] OpenSSH session to the VM (--user) with a short-lived
                       certificate and the pinned host key; -L/-R work, agent
                       forwarding is not permitted. --proxy ID is the ProxyCommand mode
  ssh-config          print a "Host davit-*" block for ~/.ssh/config, so
                       ssh/scp/rsync davit-<vm> just work
  scp [-r] SRC DST     OpenSSH scp; one side is VM:/path (--user, --preserve)
  forward [ID] [LOCAL:]REMOTE   local port -> guest port until Ctrl-C
                       (--bind, default 127.0.0.1; --json events)
  forward --reverse [ID] GUESTPORT:HOST:PORT
                       guest 127.0.0.1:GUESTPORT -> HOST:PORT as reached from here, until Ctrl-C
                       (--host-service: an operator-listed host service; --list, --revoke ID)
  prompt [ID] TEXT     one turn of a coding agent in the VM (--harness claude|codex|opencode|pi,
                       --new, --resume CONV, --model, --env NAME, --secret NAME,
                       --attach FILE, --attach-guest PATH, --effort E, --fast, --turn-timeout D)
  prompt-events [ID]   every turn's events on the VM after a cursor (--after, --conversation,
                       --follow); reconnect with the printed cursor
  interrupt [ID]       cancel a conversation's active turn (--conversation CONV) or all (--all)
  steer [ID] PROMPT TEXT   message a running turn: native, or a reported restart
  conversations [ID]   list the VM's harness conversations
  operations ID       show an operation (--watch)
  capabilities        what this host supports
  version             build information (--json for machine-readable; --check compares with the
                       latest release, or DAVIT_RELEASE_URL / releaseBaseUrl; never installs anything)
  completion SHELL    shell completion script for bash, zsh or fish (see docs/quickstart.md)

Host:
  init                set up config, data dirs and the user service (--service, --force; optional
                       tailnet gateway: --owner-login, --auth-key-stdin)
  status              is this endpoint usable? endpoint, credential, TLS trust and one
                       authenticated request, each failure with its fix (use with --host)
  doctor              check this host (KVM, namespaces, storage, space, service); -v lists every
                       check, --fixes prints the root commands that fix it
  daemon              run the host daemon (--config FILE)
  guest-agent         guest agent (runs inside VMs)

--json prints one JSON object per line on stdout. Exit codes: 0 ok, 1 internal,
2 usage, 3 authn, 4 authz, 5 not found, 6 conflict/unsupported, 7 transport or
wait timeout, 8 resource exhaustion.
`, buildinfo.Name, buildinfo.Version)
}
