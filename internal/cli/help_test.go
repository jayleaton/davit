package cli

import (
	"bytes"
	"flag"
	"io"
	"strings"
	"testing"
)

func helpOf(args ...string) (string, int) {
	var o, e bytes.Buffer
	code := Main(args, Stdio{Stdin: strings.NewReader(""), Stdout: &o, Stderr: &e})
	return o.String() + e.String(), code
}

// TestHelpNeverPanics: flag.PrintDefaults calls String on a zero value of
// every flag type; one that dereferences a nil pointer printed "panic
// calling String method" into `box --help`.
func TestHelpNeverPanics(t *testing.T) {
	args := [][]string{{"--help"}}
	for name := range commands() {
		args = append(args, []string{name, "--help"})
	}
	for _, b := range boxSubs {
		args = append(args, []string{"box", b.name, "--help"})
	}
	for _, a := range args {
		out, code := helpOf(a...)
		if code != ExitOK || strings.Contains(out, "panic") {
			t.Errorf("%v: exit %d\n%s", a, code, out)
		}
	}
}

func TestHelpShowsProjectBoxes(t *testing.T) {
	out, _ := helpOf("--help")
	for _, want := range []string{"connect USER@HOST", "client-config", "box run [PROJECT[/INSTANCE]] -- CMD..."} {
		if !strings.Contains(out, want) {
			t.Errorf("top-level help lacks %q", want)
		}
	}
	out, _ = helpOf("box", "--help")
	if !strings.Contains(out, "usage: davit box SUBCOMMAND [PROJECT[/INSTANCE]]") || !strings.Contains(out, "box run -- make test") {
		t.Errorf("box help:\n%s", out)
	}
	out, _ = helpOf("box", "run", "--help")
	if !strings.Contains(out, "usage: davit box run [PROJECT[/INSTANCE]]") || !strings.Contains(out, "-cwd") || strings.Contains(out, "-rm-older-than") {
		t.Errorf("box run help:\n%s", out)
	}
}

// TestBoxHelpCoversFlags: every box flag is listed under some subcommand.
func TestBoxHelpCoversFlags(t *testing.T) {
	fs := flag.NewFlagSet("box", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	boxFlags(fs, &opts{})
	listed := map[string]bool{}
	for _, b := range boxSubs {
		for _, f := range b.flags {
			if fs.Lookup(f) == nil && f != "timeout" {
				t.Errorf("box %s lists unknown flag %s", b.name, f)
			}
			listed[f] = true
		}
	}
	fs.VisitAll(func(f *flag.Flag) {
		if !listed[f.Name] {
			t.Errorf("box flag --%s is under no subcommand's help", f.Name)
		}
	})
}
