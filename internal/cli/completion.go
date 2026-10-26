package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/harness"
)

// Shell completion (`davit completion bash|zsh|fish`). The scripts are
// generated from the command table, the flag sets the commands really parse
// and the descriptions in usage(), so they cannot drift from the CLI.
//
// VM names are completed for a command's first positional argument when its
// usage starts with ID or [ID]: the script runs `davit list` under
// `timeout 2` with stdin closed and errors discarded, and offers nothing when
// `timeout` is missing, so completion never hangs on an unreachable daemon.

const completionUsage = "bash|zsh|fish"

func runCompletion(_ context.Context, s *session, _ *opts, args []string) error {
	if len(args) != 1 {
		return usageErr("usage: %s completion %s", buildinfo.Name, completionUsage)
	}
	var script string
	switch args[0] {
	case "bash":
		script = bashCompletion(completionModel())
	case "zsh":
		script = zshCompletion(completionModel())
	case "fish":
		script = fishCompletion(completionModel())
	default:
		return usageErr("unknown shell %q; want %s", args[0], completionUsage)
	}
	// A script is plain text even with --json.
	_, err := io.WriteString(s.io.Stdout, script)
	return err
}

type compFlag struct {
	name, usage          string
	value, file, repeats bool
	choices              []string // the value's fixed set (flagChoices)
}

// flagChoices are the values of flags with a fixed set, completed in every
// shell: the harnesses the daemon knows, member roles, token presets and
// egress modes.
var flagChoices = map[string][]string{
	"harness": harness.Names(),
	"role":    {string(api.RoleViewer), string(api.RoleExec), string(api.RoleAdmin)},
	"preset":  {"readonly", "ci", "admin"},
	"egress":  {string(domain.EgressOff), string(domain.EgressFiltered), string(domain.EgressAllowlist)},
}

func (f compFlag) dashed() string {
	if len(f.name) == 1 {
		return "-" + f.name
	}
	return "--" + f.name
}

type compCmd struct {
	name, desc string
	flags      []compFlag
	subs       []string // literal subcommand words
	vmArg      bool     // first positional is a VM ID or name
	fileArgs   bool     // positionals may be local paths
}

type compModel struct {
	global []compFlag // flags accepted before the command
	cmds   []compCmd  // sorted by name
}

// fileFlag reports whether a flag's value is a local path, from its help.
func fileFlag(name, usage string) bool {
	return name == "o" || pathHelp.MatchString(usage) && !guestHelp.MatchString(usage)
}

var (
	pathHelp  = regexp.MustCompile(`(?i)\b(file|directory)\b`)
	guestHelp = regexp.MustCompile(`\b(guest|VM's)\b`) // a path inside the VM
)

func collectFlags(fs *flag.FlagSet) []compFlag {
	var out []compFlag
	fs.VisitAll(func(f *flag.Flag) {
		cf := compFlag{name: f.Name, usage: strings.Join(strings.Fields(strings.ReplaceAll(f.Usage, "`", "")), " ")}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
			cf.value = true
			cf.file = fileFlag(f.Name, f.Usage)
			cf.choices = flagChoices[f.Name]
		}
		_, cf.repeats = f.Value.(*multiFlag)
		out = append(out, cf)
	})
	return out
}

var literalWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// subcommands parses a usage like "create NAME | list | revoke ID": when the
// first alternative starts with a literal word, every alternative's literal
// first word is a subcommand. Anything else (e.g. "[ID] off|filtered") has none.
func subcommands(usage string) []string {
	alts := strings.Split(usage, "|")
	if len(alts) < 2 {
		return nil
	}
	var subs []string
	for i, a := range alts {
		w, _, _ := strings.Cut(strings.TrimSpace(a), " ")
		if !literalWord.MatchString(w) {
			if i == 0 {
				return nil
			}
			continue
		}
		if !slices.Contains(subs, w) {
			subs = append(subs, w)
		}
	}
	return subs
}

// pathWords in a usage (outside "--flag VALUE") mean local path arguments.
var (
	pathWords     = regexp.MustCompile(`\b(SRC|DST|DIR|PATH|FILE|DEST)\b`)
	flagWithValue = regexp.MustCompile(`-[-a-z]+ [A-Z_]+`)
)

// usageLine is "  name ARGS   description"; usageLoose the same with one
// space after placeholder-only arguments ("  ssh [ID] [-- CMD...] OpenSSH ...").
var (
	usageLine     = regexp.MustCompile(`^  ([a-z][a-z0-9-]*)(.*?)\s{2,}(\S.*)$`)
	parenthetical = regexp.MustCompile(` ?\([^)]*\)`)
	usageLoose    = regexp.MustCompile(`^  ([a-z][a-z0-9-]*)((?: (?:\[[^\]]*\]|[A-Z][A-Z0-9_:.@\[\]-]*|-\S+))*) (\S.*)$`)
)

// usageDescriptions maps a command to its one-line description in usage().
func usageDescriptions(subs map[string][]string) map[string]string {
	var buf bytes.Buffer
	usage(&buf)
	type line struct{ head, desc string }
	lines := map[string][]line{}
	count := map[string]int{}
	for l := range strings.SplitSeq(buf.String(), "\n") {
		if w := strings.Fields(l); len(w) > 0 && strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   ") {
			count[w[0]]++
		}
		m := usageLine.FindStringSubmatch(l)
		if m == nil {
			if m = usageLoose.FindStringSubmatch(l); m == nil {
				continue
			}
		}
		lines[m[1]] = append(lines[m[1]], line{strings.TrimSpace(m[2]), m[3]})
	}
	out := map[string]string{}
	for name, ls := range lines {
		for _, l := range ls {
			first, _, _ := strings.Cut(l.head, " ")
			// "snapshot create [ID] NAME   name a stopped VM's disk"
			// describes one subcommand, not the command.
			if count[name] > 1 && slices.Contains(subs[name], first) {
				continue
			}
			// "token create NAME --action ..." is a usage, not a description.
			if w, _, _ := strings.Cut(l.desc, " "); slices.ContainsFunc(strings.Split(w, "|"), func(w string) bool {
				return slices.Contains(subs[name], w)
			}) {
				continue
			}
			// The first clause, without asides; a line that continues on
			// the next one ("...grants: the") loses its dangling end.
			d, _, _ := strings.Cut(l.desc, " (-")
			d = parenthetical.ReplaceAllString(d, "")
			d, _, _ = strings.Cut(d, " (")
			d, _, _ = strings.Cut(d, "; ")
			if i := strings.Index(d, "? "); i >= 0 {
				d = d[:i+1]
			}
			if i := strings.LastIndex(d, ": "); i >= 0 && !strings.Contains(d[i+2:], " ") {
				d = d[:i]
			}
			out[name] = strings.TrimSuffix(strings.TrimRight(d, ",;: "), ", so")
			break
		}
	}
	return out
}

func completionModel() compModel {
	var m compModel
	top := flag.NewFlagSet(buildinfo.Name, flag.ContinueOnError)
	(&globals{}).register(top)
	m.global = collectFlags(top)

	table := commands()
	subs := map[string][]string{}
	for name, c := range table {
		subs[name] = subcommands(c.usage)
	}
	descs := usageDescriptions(subs)
	for name, c := range table {
		fs := flag.NewFlagSet(name, flag.ContinueOnError)
		(&globals{}).registerFor(fs, c.argv)
		if c.flags != nil {
			c.flags(fs, &opts{})
		}
		first, _, _ := strings.Cut(strings.TrimSpace(c.usage), " ")
		cc := compCmd{name: name, flags: collectFlags(fs), subs: subs[name],
			vmArg:    first == "ID" || first == "[ID]",
			fileArgs: pathWords.MatchString(flagWithValue.ReplaceAllString(c.usage, ""))}
		switch {
		case descs[name] != "":
			cc.desc = descs[name]
		case len(cc.subs) > 0:
			cc.desc = strings.Join(cc.subs, ", ")
		case c.usage == "...":
			cc.desc = "not implemented in this build"
		default:
			cc.desc = strings.TrimSpace(c.usage)
		}
		if cc.desc == "" {
			cc.desc = name
		}
		m.cmds = append(m.cmds, cc)
	}
	// Commands Main handles before the table.
	m.cmds = append(m.cmds,
		compCmd{name: "help", desc: "show help"},
		compCmd{name: "version", desc: orElse(descs["version"], "build information"), flags: m.global})
	sort.Slice(m.cmds, func(i, j int) bool { return m.cmds[i].name < m.cmds[j].name })
	return m
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func flagNames(fs []compFlag, keep func(compFlag) bool, dashed bool) string {
	var out []string
	for _, f := range fs {
		if keep(f) {
			if dashed {
				out = append(out, f.dashed())
			} else {
				out = append(out, f.name)
			}
		}
	}
	return strings.Join(out, " ")
}

func anyFlag(compFlag) bool     { return true }
func valueFlag(f compFlag) bool { return f.value }
func pathFlag(f compFlag) bool  { return f.file }

// vmListAwk prints the IDs and names from `davit list` (header "ID NAME
// STATE CPUS MEMORY"; a VM without a name has one field less).
const vmListAwk = `'NR > 1 && NF >= 4 { print $1; if (NF >= 5) print $2 }'`

func vmListCmd() string {
	return "timeout 2 " + buildinfo.Name + " list </dev/null 2>/dev/null | awk " + vmListAwk
}

// shQuote single-quotes s for sh-like shells (bash, zsh).
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func bashCompletion(m compModel) string {
	n := buildinfo.Name
	fn := "_" + strings.ReplaceAll(n, "-", "_")
	var b strings.Builder
	fmt.Fprintf(&b, "# bash completion for %[1]s; generated by `%[1]s completion bash`.\n", n)
	fmt.Fprintf(&b, "# Load it with: source <(%[1]s completion bash)\n\n", n)

	caseFn := func(name string, line func(c compCmd) string, global string) {
		fmt.Fprintf(&b, "%s%s() {\n    case $1 in\n", fn, name)
		if global != "" {
			fmt.Fprintf(&b, "        '') echo %s ;;\n", shQuote(global))
		}
		for _, c := range m.cmds {
			if v := line(c); v != "" {
				fmt.Fprintf(&b, "        %s) echo %s ;;\n", shQuote(c.name), shQuote(v))
			}
		}
		b.WriteString("    esac\n}\n\n")
	}
	caseFn("_flags", func(c compCmd) string { return flagNames(c.flags, anyFlag, true) }, flagNames(m.global, anyFlag, true))
	caseFn("_value_flags", func(c compCmd) string { return flagNames(c.flags, valueFlag, false) }, flagNames(m.global, valueFlag, false))
	caseFn("_file_flags", func(c compCmd) string { return flagNames(c.flags, pathFlag, false) }, flagNames(m.global, pathFlag, false))
	caseFn("_subcommands", func(c compCmd) string { return strings.Join(c.subs, " ") }, "")
	fmt.Fprintf(&b, "%s_choices() {\n    case $1 in\n", fn)
	for _, name := range slices.Sorted(maps.Keys(flagChoices)) {
		fmt.Fprintf(&b, "        %s) echo %s ;;\n", shQuote(name), shQuote(strings.Join(flagChoices[name], " ")))
	}
	b.WriteString("    esac\n}\n\n")

	var names, vms []string
	for _, c := range m.cmds {
		names = append(names, c.name)
		if c.vmArg && len(c.subs) == 0 {
			vms = append(vms, c.name)
		}
	}
	fmt.Fprintf(&b, "%s_vm_arg() {\n    case $1 in\n        %s) return 0 ;;\n    esac\n    return 1\n}\n\n", fn, strings.Join(vms, "|"))
	fmt.Fprintf(&b, `%[1]s_vms() {
    command -v timeout >/dev/null 2>&1 || return 0
    %[2]s
}

%[1]s_reply() {
    local line
    while IFS= read -r line; do
        COMPREPLY+=("$line")
    done
}

%[1]s() {
    local cur=${COMP_WORDS[COMP_CWORD]} cmd='' npos=0 skip=0 i w name=''
    COMPREPLY=()
    for ((i = 1; i < COMP_CWORD; i++)); do
        w=${COMP_WORDS[i]}
        if ((skip)); then
            skip=0
            continue
        fi
        # After "--" the words are the guest command's argv.
        [[ $w == -- ]] && return 0
        if [[ $w == -* ]]; then
            name=${w#-}
            name=${name#-}
            if [[ $name != *=* && " $(%[1]s_value_flags "$cmd") " == *" $name "* ]]; then
                skip=1
            fi
            continue
        fi
        if [[ -z $cmd ]]; then
            cmd=$w
        else
            npos=$((npos + 1))
        fi
    done
    if ((skip)); then
        # The word is the value of flag $name.
        local choices
        choices=$(%[1]s_choices "$name")
        if [[ -n $choices ]]; then
            %[1]s_reply < <(compgen -W "$choices" -- "$cur")
            return 0
        fi
        if [[ " $(%[1]s_file_flags "$cmd") " == *" $name "* ]]; then
            compopt -o filenames 2>/dev/null
            %[1]s_reply < <(compgen -f -- "$cur")
        fi
        return 0
    fi
    if [[ $cur == -* ]]; then
        %[1]s_reply < <(compgen -W "$(%[1]s_flags "$cmd")" -- "$cur")
        return 0
    fi
    if [[ -z $cmd ]]; then
        %[1]s_reply < <(compgen -W %[3]s -- "$cur")
        return 0
    fi
    if ((npos == 0)); then
        local subs
        subs=$(%[1]s_subcommands "$cmd")
        if [[ -n $subs ]]; then
            %[1]s_reply < <(compgen -W "$subs" -- "$cur")
            return 0
        fi
        if %[1]s_vm_arg "$cmd"; then
            %[1]s_reply < <(compgen -W "$(%[1]s_vms)" -- "$cur")
            [[ ${#COMPREPLY[@]} -gt 0 ]] && return 0
        fi
    fi
    compopt -o filenames 2>/dev/null
    %[1]s_reply < <(compgen -f -- "$cur")
}

complete -F %[1]s %[4]s
`, fn, vmListCmd(), shQuote(strings.Join(names, " ")), n)
	return b.String()
}

// zshEscape escapes text for an _arguments/_describe spec inside single quotes.
func zshEscape(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, `:`, `\:`).Replace(s)
	return strings.ReplaceAll(s, "'", `'\''`)
}

func zshFlagSpecs(fs []compFlag) []string {
	var out []string
	for _, f := range fs {
		spec := f.dashed()
		if f.repeats {
			spec = "*" + spec
		}
		if f.value && len(f.name) > 1 {
			spec += "="
		}
		spec += "[" + zshEscape(f.usage) + "]"
		if f.value {
			if f.file {
				spec += ":file:_files"
			} else if len(f.choices) > 0 {
				spec += ":" + zshEscape(f.name) + ":(" + zshEscape(strings.Join(f.choices, " ")) + ")"
			} else {
				spec += ":" + zshEscape(f.name) + ": "
			}
		}
		out = append(out, "'"+spec+"'")
	}
	return out
}

func zshCompletion(m compModel) string {
	n := buildinfo.Name
	fn := "_" + strings.ReplaceAll(n, "-", "_")
	var b strings.Builder
	fmt.Fprintf(&b, "#compdef %[1]s\n# zsh completion for %[1]s; generated by `%[1]s completion zsh`.\n", n)
	fmt.Fprintf(&b, "# Load it with: source <(%[1]s completion zsh), or save it as %[2]s in a directory on $fpath.\n\n", n, fn)
	fmt.Fprintf(&b, `%[1]s_vms() {
    local -a vms
    (( $+commands[timeout] )) || return 1
    vms=(${(f)"$(%[2]s)"})
    (( ${#vms} )) || return 1
    compadd -a vms
}

%[1]s() {
    local curcontext="$curcontext" state line ret=1
    typeset -A opt_args
    local -a cmds
    cmds=(
`, fn, vmListCmd())
	for _, c := range m.cmds {
		fmt.Fprintf(&b, "        '%s:%s'\n", zshEscape(c.name), strings.ReplaceAll(c.desc, "'", `'\''`))
	}
	b.WriteString("    )\n    _arguments -C \\\n")
	for _, s := range zshFlagSpecs(m.global) {
		fmt.Fprintf(&b, "        %s \\\n", s)
	}
	b.WriteString(`        '1: :->command' \
        '*:: :->args' && ret=0
    case $state in
        command)
            _describe -t commands 'command' cmds && ret=0
            ;;
        args)
            curcontext="${curcontext%:*:*}:` + n + `-$words[1]:"
            case $words[1] in
`)
	for _, c := range m.cmds {
		fmt.Fprintf(&b, "                %s)\n                    _arguments -S \\\n", shQuote(c.name))
		for _, s := range zshFlagSpecs(c.flags) {
			fmt.Fprintf(&b, "                        %s \\\n", s)
		}
		switch {
		case len(c.subs) > 0:
			fmt.Fprintf(&b, "                        '1:subcommand:(%s)' \\\n", strings.Join(c.subs, " "))
		case c.vmArg:
			fmt.Fprintf(&b, "                        '1:VM:%s_vms' \\\n", fn)
		}
		b.WriteString("                        '*: :_files' && ret=0\n                    ;;\n")
	}
	fmt.Fprintf(&b, `            esac
            ;;
    esac
    return ret
}

if [ "$funcstack[1]" = "%[1]s" ]; then
    %[1]s "$@"
else
    compdef %[1]s %[2]s
fi
`, fn, n)
	return b.String()
}

// fishQuote single-quotes s for fish.
func fishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func fishCompletion(m compModel) string {
	n := buildinfo.Name
	fn := "__" + strings.ReplaceAll(n, "-", "_")
	var b strings.Builder
	fmt.Fprintf(&b, "# fish completion for %[1]s; generated by `%[1]s completion fish`.\n", n)
	fmt.Fprintf(&b, "# Load it with: %[1]s completion fish | source\n\n", n)
	fmt.Fprintf(&b, `function %[1]s_vms
    command -q timeout; or return 0
    %[2]s
end

# %[1]s_value_flag CMD NAME: does CMD's flag NAME (no dashes) take a value?
function %[1]s_value_flag -a cmd name
    if test -z "$cmd"
        contains -- $name %[3]s
        return
    end
    switch $cmd
`, fn, vmListCmd(), flagNames(m.global, valueFlag, false))
	for _, c := range m.cmds {
		if v := flagNames(c.flags, valueFlag, false); v != "" {
			fmt.Fprintf(&b, "        case %s\n            contains -- $name %s\n", fishQuote(c.name), v)
		}
	}
	fmt.Fprintf(&b, `        case '*'
            return 1
    end
end

# %[1]s_state prints "COMMAND POSITIONALS SKIP" for the words before the cursor;
# SKIP 1 means the cursor is on a flag's value.
function %[1]s_state
    set -l toks (commandline -opc)
    set -e toks[1]
    set -l cmd ''
    set -l n 0
    set -l skip 0
    for w in $toks
        if test $skip -eq 1
            set skip 0
            continue
        end
        if test "x$w" = x--
            echo "-- 0 1"
            return
        end
        if string match -q -- '-*' $w
            set -l name (string replace -r -- '^--?' '' $w)
            if not string match -q -- '*=*' $name; and %[1]s_value_flag "$cmd" $name
                set skip 1
            end
            continue
        end
        if test -z "$cmd"
            set cmd $w
        else
            set n (math $n + 1)
        end
    end
    echo "$cmd $n $skip"
end

# %[1]s_at CMD N: the cursor is on CMD's positional argument N (from 0).
function %[1]s_at -a want_cmd want_n
    set -l s (string split ' ' -- (%[1]s_state))
    test "x$s[1]" = "x$want_cmd"; and test "$s[2]" = "$want_n"; and test "$s[3]" = 0
end

complete -c %[2]s -f
`, fn, n)
	flagLine := func(cond string, f compFlag) {
		opt := "-l " + f.name
		if len(f.name) == 1 {
			opt = "-s " + f.name
		}
		fmt.Fprintf(&b, "complete -c %s -n %s %s -d %s", n, fishQuote(cond), opt, fishQuote(f.usage))
		switch {
		case f.file:
			b.WriteString(" -r -F")
		case len(f.choices) > 0:
			b.WriteString(" -x -a " + fishQuote(strings.Join(f.choices, " ")))
		case f.value:
			b.WriteString(" -x")
		}
		b.WriteString("\n")
	}
	for _, f := range m.global {
		flagLine("__fish_use_subcommand", f)
	}
	for _, c := range m.cmds {
		fmt.Fprintf(&b, "complete -c %s -n '__fish_use_subcommand' -a %s -d %s\n", n, fishQuote(c.name), fishQuote(c.desc))
	}
	for _, c := range m.cmds {
		seen := "__fish_seen_subcommand_from " + c.name
		for _, f := range c.flags {
			flagLine(seen, f)
		}
		at0 := fmt.Sprintf("%s_at %s 0", fn, c.name)
		switch {
		case len(c.subs) > 0:
			fmt.Fprintf(&b, "complete -c %s -n %s -a %s\n", n, fishQuote(at0), fishQuote(strings.Join(c.subs, " ")))
		case c.vmArg:
			fmt.Fprintf(&b, "complete -c %s -n %s -a '(%s_vms)' -d VM\n", n, fishQuote(at0), fn)
		}
		if c.fileArgs {
			cond := seen
			if len(c.subs) > 0 {
				cond += "; and not " + at0
			}
			fmt.Fprintf(&b, "complete -c %s -n %s -F\n", n, fishQuote(cond))
		}
	}
	return b.String()
}
