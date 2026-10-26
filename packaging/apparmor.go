package packaging

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

//go:embed apparmor/davit.tmpl
var apparmorProfile string

var apparmorTemplate = template.Must(template.New("apparmor").Option("missingkey=error").Parse(apparmorProfile))

// AppArmorDir is where the system loads AppArmor profiles from.
const AppArmorDir = "/etc/apparmor.d"

var apparmorName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// AppArmorProfileName is the profile name (and file name under
// /etc/apparmor.d) for the program command installed at the resolved path
// bin: command plus the first 12 hex digits of sha256(bin). Each binary
// path gets its own profile, so users who each install their own copy on
// one host never replace each other's profile.
func AppArmorProfileName(command, bin string) string {
	sum := sha256.Sum256([]byte(bin))
	return command + "-" + hex.EncodeToString(sum[:])[:12]
}

// AppArmorParams fills the AppArmor profile template.
type AppArmorParams struct {
	// Name is the profile name and the file name under /etc/apparmor.d
	// (AppArmorProfileName).
	Name string
	// Command is the program the comments name; default Name.
	Command string
	// Binary is the absolute, symlink-free path of the binary the profile
	// attaches to. AppArmor attaches by the resolved path at exec, so a
	// symlink path would never match.
	Binary string
}

// AppArmorAttachment validates bin as a profile attachment and returns it as
// written in the profile: bare, or double-quoted when it contains spaces.
// Characters that AppArmor reads as globs, variables, escapes or comments are
// rejected rather than escaped, so the attachment always matches exactly
// one literal path.
func AppArmorAttachment(bin string) (string, error) {
	if !filepath.IsAbs(bin) || filepath.Clean(bin) != bin {
		return "", fmt.Errorf("packaging: AppArmor binary path %q must be absolute and clean", bin)
	}
	for _, r := range bin {
		if r < 0x20 || r > 0x7e {
			return "", fmt.Errorf("packaging: AppArmor binary path %q must be printable ASCII", bin)
		}
	}
	if i := strings.IndexAny(bin, "\"\\*?[]{}^@#,"); i >= 0 {
		return "", fmt.Errorf("packaging: AppArmor binary path %q contains %q, which AppArmor treats as a pattern or syntax character; move the binary to a plain path", bin, bin[i])
	}
	if strings.ContainsAny(bin, " \t") {
		return `"` + bin + `"`, nil
	}
	return bin, nil
}

// AppArmorProfile renders the profile that lets p.Binary create user
// namespaces under kernel.apparmor_restrict_unprivileged_userns=1.
func AppArmorProfile(p AppArmorParams) (string, error) {
	if !apparmorName.MatchString(p.Name) {
		return "", fmt.Errorf("packaging: AppArmor profile name %q must match %s", p.Name, apparmorName)
	}
	attach, err := AppArmorAttachment(p.Binary)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if p.Command == "" {
		p.Command = p.Name
	}
	err = apparmorTemplate.Execute(&b, struct{ Name, Command, Attach string }{p.Name, p.Command, attach})
	return b.String(), err
}

// AppArmorCommands returns the one-time root commands that install and load
// the rendered profile file src as profile name, and the ones that remove it.
// Nothing here runs them.
func AppArmorCommands(src, name string) (install, remove []string) {
	dst := AppArmorDir + "/" + name
	install = []string{
		"sudo install -m 0644 -o root -g root " + ShellQuote(src) + " " + dst,
		"sudo apparmor_parser -r " + dst,
	}
	remove = []string{
		"sudo apparmor_parser -R " + dst,
		"sudo rm " + dst,
	}
	return install, remove
}

// ShellQuote quotes s for POSIX sh when it holds anything but safe characters.
func ShellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+=:,%@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
