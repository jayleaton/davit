// Package packaging holds release and service artifacts: install.sh (a
// checksum-verifying installer), the systemd user unit template (rendered by
// `init --service`) and the AppArmor userns profile template (rendered by
// `init --apparmor`). Both templates are embedded.
package packaging

import (
	"bytes"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"
)

//go:embed systemd/davit.service.tmpl
var systemdUnit string

var unitTemplate = template.Must(template.New("unit").Option("missingkey=error").Parse(systemdUnit))

// UnitParams fills the systemd unit template. Paths must be absolute.
type UnitParams struct {
	Name, Binary, Config, EnvFile, AuthKeyEnv string
}

// SystemdUserUnit renders the systemd --user unit. Paths are escaped for
// systemd ('%' specifiers); characters that cannot be quoted safely in a
// unit file are rejected.
func SystemdUserUnit(p UnitParams) (string, error) {
	for _, path := range []*string{&p.Binary, &p.Config, &p.EnvFile} {
		if !filepath.IsAbs(*path) || strings.ContainsAny(*path, "\"\\\n\r") {
			return "", fmt.Errorf("packaging: unit path %q must be absolute and free of quotes, backslashes and newlines", *path)
		}
		*path = strings.ReplaceAll(*path, "%", "%%")
	}
	var b bytes.Buffer
	err := unitTemplate.Execute(&b, p)
	return b.String(), err
}
