package cli

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"davit/internal/buildinfo"
)

// box's help: `box --help` lists the subcommands with an example, and
// `box SUB --help` shows SUB's usage and only the flags SUB takes. All
// subcommands share one flag set (boxFlags), so each lists its own.

type boxSub struct {
	name, usage, summary string
	flags                []string
}

// Every flag in boxFlags belongs to at least one subcommand
// (TestBoxHelpCoversFlags).
var boxSubs = []boxSub{
	{"run", "[PROJECT[/INSTANCE]] [flags] -- COMMAND...", "sync this worktree into its box, run COMMAND there, stream its output; exits with COMMAND's status",
		[]string{"cwd", "env", "e", "timeout", "stdin", "user", "raw", "no-sync", "no-up", "git", "wait", "image", "type", "cpus", "memory", "disk", "idle"}},
	{"up", "[PROJECT[/INSTANCE]] [flags]", "create or start the box (the first time, the project image is built: dependencies installed once); --cpus/--memory/--disk resize it",
		[]string{"refresh", "image", "type", "cpus", "memory", "disk", "idle", "keep", "wait", "no-wait", "wait-timeout", "idempotency-key"}},
	{"down", "[PROJECT[/INSTANCE]] [--force]", "stop the box; its disk is kept (it also stops by itself after 15 min unused)",
		[]string{"force", "no-wait", "wait-timeout", "idempotency-key"}},
	{"ssh", "[PROJECT[/INSTANCE]] [-- COMMAND...]", "a shell (or COMMAND) in the box over OpenSSH",
		[]string{"user", "no-up", "wait"}},
	{"sync", "[PROJECT[/INSTANCE]] [DIR[:GUEST_PATH]] | --pull GUEST_PATH [LOCAL]", "copy this worktree into the box (adds and replaces files), or --pull a path back out",
		[]string{"pull", "git", "no-up", "wait"}},
	{"ls", "[PROJECT] [--deleted]", "list boxes and project images (and whether each is on hot or cold storage), with the host's free memory and disk; --deleted: the boxes the host auto-deleted for disk space",
		[]string{"deleted"}},
	{"pin", "[PROJECT[/INSTANCE] | PROJECT/image] [--hot]", "keep the box (or project image) when the host is low on disk: unused boxes older than " +
		"3 days (on cold storage 12 hours, images without instances 1 day: the host's defaults; with the disk still short, 1 hour) " +
		"can be auto-deleted then; pin to keep. --hot also keeps it on " +
		"hot storage: a host with a cold storage tier never moves it there", []string{"hot"}},
	{"unpin", "[PROJECT[/INSTANCE] | PROJECT/image]", "undo box pin (and --hot)", nil},
	{"rm", "[PROJECT[/INSTANCE]] --yes", "delete the box and its disk", []string{"yes"}},
	{"image", "build|update [PROJECT] [--from IMAGE] [--setup FILE | --setup-cmd CMD] [--dir DIR] | rm [PROJECT] [--force] --yes | prune [--dry-run]",
		"build or update the project image (setup: " + defaultSetupFile + ", or detected from the repo), or remove it; prune removes the host's image blobs no image uses (host owner)",
		[]string{"from", "setup", "setup-cmd", "dir", "force", "yes", "type", "cpus", "memory", "disk", "dry-run"}},
	{"gc", "[PROJECT] [--idle 10m] [--rm-older-than 7d] [--dry-run]", "stop idle boxes and delete long-stopped ones (never a project image)",
		[]string{"idle", "rm-older-than", "dry-run"}},
}

// boxUsage is box's usage line.
const boxUsage = "run|up|down|ssh|sync|ls|rm|pin|unpin|image|gc [PROJECT[/INSTANCE]] [flags] (PROJECT/INSTANCE default to this repository and worktree)"

// boxHelp prints box's help, for one subcommand when pos names one.
func boxHelp(w io.Writer, fs *flag.FlagSet, pos []string) {
	n := buildinfo.Name
	if len(pos) > 0 {
		if i := slices.IndexFunc(boxSubs, func(b boxSub) bool { return b.name == pos[0] || pos[0] == "list" && b.name == "ls" }); i >= 0 {
			b := boxSubs[i]
			fmt.Fprintf(w, "usage: %s box %s %s\n\n%s.\nPROJECT/INSTANCE default to this repository and git worktree.\n", n, b.name, b.usage, upperFirst(b.summary))
			if len(b.flags) > 0 {
				fmt.Fprintln(w, "\nFlags:")
				printFlags(w, fs, b.flags)
			}
			fmt.Fprintf(w, "\nGlobal: --host USER@HOST|NAME, --json (see `%s --help`)\n", n)
			if b.name == "run" {
				fmt.Fprintf(w, "\nExample:\n  cd your-repo && %s box run -- make test\n", n)
			}
			return
		}
	}
	fmt.Fprintf(w, `usage: %[1]s box SUBCOMMAND [PROJECT[/INSTANCE]] [flags]

Project boxes run your repository's commands on the host: the project image holds
its installed dependencies, and each git worktree gets its own persistent box.
PROJECT/INSTANCE default to this repository and worktree, so most commands
need no name.

`, n)
	for _, b := range boxSubs {
		fmt.Fprintf(w, "  %-6s %s\n         %s\n", b.name, b.usage, b.summary)
	}
	fmt.Fprintf(w, `
Example:
  %[1]s connect user@host               # once per machine
  cd your-repo && %[1]s box run -- make test
  %[1]s box down

Run `+"`%[1]s box SUBCOMMAND --help`"+` for its flags. Docs: docs/project-boxes.md
`, n)
}

// printFlags prints the named flags of fs in PrintDefaults' format.
func printFlags(w io.Writer, fs *flag.FlagSet, names []string) {
	sub := flag.NewFlagSet("", flag.ContinueOnError)
	sub.SetOutput(w)
	for _, name := range names {
		if f := fs.Lookup(name); f != nil {
			sub.Var(f.Value, f.Name, f.Usage)
			sub.Lookup(f.Name).DefValue = f.DefValue
		}
	}
	sub.PrintDefaults()
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
