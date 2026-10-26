package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"davit/internal/harness"
)

func completionScript(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(append([]string{"completion"}, args...), Stdio{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), stderr.String(), code
}

func TestCompletionCoversCommandTable(t *testing.T) {
	names := []string{"help", "version"}
	for name := range commands() {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, shell := range []string{"bash", "zsh", "fish"} {
		script, stderr, code := completionScript(t, shell)
		if code != ExitOK || stderr != "" {
			t.Fatalf("%s: exit %d, stderr %q", shell, code, stderr)
		}
		for _, name := range names {
			var want string
			switch shell {
			case "bash":
				_, list, _ := strings.Cut(script, "compgen -W '")
				list, _, _ = strings.Cut(list, "'")
				if !slices.Contains(strings.Fields(list), name) {
					t.Errorf("bash command list lacks %s", name)
				}
				continue
			case "zsh":
				want = "'" + name + ":"
			case "fish":
				want = "-a '" + name + "' -d "
			}
			if !strings.Contains(script, want) {
				t.Errorf("%s script lacks command %s (%q)", shell, name, want)
			}
		}
		// Flags come from the commands' real flag sets.
		for _, cf := range [][2]string{{"new", "count"}, {"new", "image"}, {"stop", "force"}, {"exec", "detach"}, {"list", "json"}} {
			var want string
			switch shell {
			case "bash":
				want = "        '" + cf[0] + "') echo '"
			case "zsh":
				want = "'--" + cf[1]
			case "fish":
				want = "'__fish_seen_subcommand_from " + cf[0] + "' -l " + cf[1] + " "
			}
			if !strings.Contains(script, want) {
				t.Errorf("%s script lacks %s --%s (%q)", shell, cf[0], cf[1], want)
			}
			if shell == "bash" {
				line := script[strings.Index(script, want):]
				line = line[:strings.Index(line, "\n")]
				if !strings.Contains(line+" ", " --"+cf[1]+" ") && !strings.Contains(line, "'--"+cf[1]+" ") {
					t.Errorf("bash flags for %s lack --%s: %s", cf[0], cf[1], line)
				}
			}
		}
	}
}

func TestCompletionModel(t *testing.T) {
	m := completionModel()
	byName := map[string]compCmd{}
	for _, c := range m.cmds {
		byName[c.name] = c
	}
	if got := byName["snapshot"].subs; !slices.Equal(got, []string{"create", "delete", "tree", "pull", "export"}) {
		t.Errorf("snapshot subcommands = %v", got)
	}
	if got := byName["token"].subs; !slices.Equal(got, []string{"create", "list", "revoke", "rotate", "usage"}) {
		t.Errorf("token subcommands = %v", got)
	}
	for _, name := range []string{"egress", "endpoints", "history", "stop", "exec"} {
		if c := byName[name]; len(c.subs) != 0 || !c.vmArg {
			t.Errorf("%s: subs %v vmArg %v; want no subcommands, a VM argument", name, c.subs, c.vmArg)
		}
	}
	for _, tc := range []struct {
		cmd, flag   string
		value, file bool
	}{{"env", "file", true, true}, {"init", "data-dir", true, true}, {"exec", "attach", true, false},
		{"exec", "cwd", true, false}, {"new", "cpus", true, false}, {"stop", "force", false, false}} {
		i := slices.IndexFunc(byName[tc.cmd].flags, func(f compFlag) bool { return f.name == tc.flag })
		if i < 0 {
			t.Errorf("%s has no --%s", tc.cmd, tc.flag)
			continue
		}
		if f := byName[tc.cmd].flags[i]; f.value != tc.value || f.file != tc.file {
			t.Errorf("%s --%s: value %v file %v, want %v %v", tc.cmd, tc.flag, f.value, f.file, tc.value, tc.file)
		}
	}
	for name, want := range map[string]string{"new": "create a VM", "list": "list VMs", "version": "build information",
		"cp": "copy files through the guest agent", "image": "not implemented in this build"} {
		if got := byName[name].desc; got != want {
			t.Errorf("%s description = %q, want %q", name, got, want)
		}
	}
}

func TestCompletionUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"powershell"}, {"bash", "zsh"}} {
		out, stderr, code := completionScript(t, args...)
		if code != ExitUsage || out != "" || !strings.Contains(stderr, "bash|zsh|fish") {
			t.Errorf("completion %v: exit %d, stdout %q, stderr %q", args, code, out, stderr)
		}
	}
}

// writeScript saves the generated script for a shell.
func writeScript(t *testing.T, shell string) string {
	t.Helper()
	script, _, code := completionScript(t, shell)
	if code != ExitOK {
		t.Fatalf("completion %s: exit %d", shell, code)
	}
	p := filepath.Join(t.TempDir(), "davit."+shell)
	if err := os.WriteFile(p, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompletionSyntax(t *testing.T) {
	for shell, check := range map[string][]string{"bash": {"-n"}, "zsh": {"-n"}, "fish": {"--no-execute"}} {
		t.Run(shell, func(t *testing.T) {
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s not installed", shell)
			}
			out, err := exec.Command(bin, append(check, writeScript(t, shell))...).CombinedOutput()
			if err != nil {
				t.Fatalf("%s %v: %v\n%s", shell, check, err, out)
			}
		})
	}
}

// TestCompletionBash drives _davit the way bash does, with a fake davit
// on PATH answering `list`.
func TestCompletionBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	script := writeScript(t, "bash")
	bin := t.TempDir()
	fake := "#!/bin/sh\n[ \"$1\" = list ] || exit 1\n" +
		"printf '%s\\n' 'ID                       NAME                 STATE         CPUS   MEMORY' " +
		"'vm_aaaa                  box                  ready            2     1GiB' " +
		"'vm_bbbb                                       stopped          1   512MiB'\n"
	if err := os.WriteFile(filepath.Join(bin, "davit"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		words []string
		want  string
	}{
		{[]string{"davit", "sto"}, "stop"},
		{[]string{"davit", "--host", "h", "st"}, "status steer stop"},
		{[]string{"davit", "stop", "--fo"}, "--force"},
		{[]string{"davit", "new", "--image", "x", "--cou"}, "--count"},
		{[]string{"davit", "snapshot", ""}, "create delete export pull tree"},
		{[]string{"davit", "token", "--limit", "5", "re"}, "revoke"},
		{[]string{"davit", "completion", "--j"}, "--json"},
		{[]string{"davit", "--j"}, "--json"},
		{[]string{"davit", "new", "--cpus", ""}, ""},
		{[]string{"davit", "prompt", "box", "--harness", ""}, strings.Join(slices.Sorted(slices.Values(harness.Names())), " ")},
		{[]string{"davit", "members", "add", "alice", "--role", "e"}, "exec"},
		{[]string{"davit", "token", "create", "ci", "--preset", ""}, "admin ci readonly"},
		{[]string{"davit", "sha"}, "share shares"},
		{[]string{"davit", "uns"}, "unshare"},
		{[]string{"davit", "desk"}, "desktop"},
		{[]string{"davit", "who"}, "whoami"},
		{[]string{"davit", "exec", "box", "--", "l"}, ""},
	} {
		words := make([]string, len(tc.words))
		for i, w := range tc.words {
			words[i] = shQuote(w)
		}
		prog := "source " + shQuote(script) + "\nCOMP_WORDS=(" + strings.Join(words, " ") + ")\nCOMP_CWORD=" +
			strconv.Itoa(len(words)-1) + "\n_davit\nprintf '%s\\n' \"${COMPREPLY[@]}\" | sort | tr '\\n' ' '\n"
		cmd := exec.Command(bash, "--norc", "--noprofile", "-c", prog)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", tc.words, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("%v: got %q, want %q", tc.words, got, tc.want)
		}
	}
	// VM IDs and names, when timeout(1) exists.
	if _, err := exec.LookPath("timeout"); err == nil {
		prog := "source " + shQuote(script) + "\nCOMP_WORDS=(davit stop '')\nCOMP_CWORD=2\n_davit\nprintf '%s\\n' \"${COMPREPLY[@]}\" | sort | tr '\\n' ' '\n"
		cmd := exec.Command(bash, "--norc", "--noprofile", "-c", prog)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if got := strings.TrimSpace(string(out)); err != nil || got != "box vm_aaaa vm_bbbb" {
			t.Errorf("stop VM completion: %v %q", err, got)
		}
	}
}
