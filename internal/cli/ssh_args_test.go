package cli

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sshFilesWithSpace is the client's SSH state under a config directory with a
// space in it, as on macOS ("~/Library/Application Support/davit/ssh").
func sshFilesWithSpace(t *testing.T) sshFiles {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Library", "Application Support", "davit", "ssh")
	return filesFor(dir, "davit-box")
}

// TestOpenSSHArgsKeyPathWithSpace pins how a path containing a space reaches
// OpenSSH: the identity file is its own argv element after -i (ssh's -o parser
// splits a one-argument keyword value on whitespace), and the options that
// have no short form quote the path inside their value.
func TestOpenSSHArgsKeyPathWithSpace(t *testing.T) {
	f := sshFilesWithSpace(t)
	args := openSSHArgs("/opt/davit ssh --proxy box", f, "user")

	i := slices.Index(args, "-i")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no -i followed by the key path in %q", args)
	}
	if got := args[i+1]; got != f.key {
		t.Fatalf("-i argument = %q, want the whole key path %q", got, f.key)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "IdentityFile=") {
			t.Fatalf("identity file still passed through -o, which splits it at the space: %q", a)
		}
	}
	for _, want := range []string{
		`CertificateFile="` + f.cert + `"`,
		`UserKnownHostsFile="` + f.knownHosts + `"`,
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing quoted option %q in %q", want, args)
		}
	}
}

// TestOpenSSHArgsParseInOpenSSH gives the generated arguments to the real
// OpenSSH client in config-dump mode: a space in a one-argument option value
// is a parse error before any connection ("keyword ... extra arguments at end
// of line"), and the certificate path must survive as one value.
func TestOpenSSHArgsParseInOpenSSH(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no OpenSSH client in PATH")
	}
	f := sshFilesWithSpace(t)
	args := append(openSSHArgs("/bin/false", f, "user"), "-G", "davit-box.invalid")
	out, err := exec.Command(ssh, args...).CombinedOutput()
	if strings.Contains(string(out), "extra arguments at end of line") {
		t.Fatalf("OpenSSH rejected a key path with a space:\n%s", out)
	}
	if err != nil {
		t.Fatalf("ssh -G: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "certificatefile "+f.cert) {
		t.Fatalf("ssh -G did not accept the certificate path %q:\n%s", f.cert, out)
	}
}

// TestOpenSSHQuotedPathsWithQuoteAndBackslash covers values that need quoting
// and also contain a double quote or a backslash. Inside an ssh_config
// double-quoted value both are special, so they must be escaped or ssh rejects
// the line ("invalid quotes") or silently reads a different path. ControlPath
// keeps its intentional %C token.
func TestOpenSSHQuotedPathsWithQuoteAndBackslash(t *testing.T) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no OpenSSH client in PATH")
	}
	dir := filepath.Join(t.TempDir(), `Application "Support\ssh`)
	f := filesFor(dir, "davit-box")
	args := append(openSSHArgs("/bin/false", f, "user"),
		"-o", "ControlPath="+sshConfigQuote(filepath.Join(dir, "%C")),
		"-G", "davit-box.invalid")
	out, err := exec.Command(ssh, args...).CombinedOutput()
	for _, bad := range []string{"invalid quotes", "extra arguments at end of line"} {
		if strings.Contains(string(out), bad) {
			t.Fatalf("OpenSSH rejected quoted paths containing a quote and a backslash (%s):\n%s", bad, out)
		}
	}
	if err != nil {
		t.Fatalf("ssh -G: %v\n%s", err, out)
	}
	// -G prints the unquoted values; ControlPath's %C expands to a hash.
	for _, want := range []string{
		"certificatefile " + f.cert,
		"userknownhostsfile " + f.knownHosts,
		"controlpath " + dir + string(filepath.Separator),
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("ssh -G did not keep %q:\n%s", want, out)
		}
	}
}
