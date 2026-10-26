// Package buildinfo holds the product name and version in one place, so a
// rename of the placeholder product name is a single edit plus a module-path
// sed.
package buildinfo

import (
	goruntime "runtime"
	"runtime/debug"
	"strings"
)

// Name is the product, binary and directory name.
const Name = "davit"

// Repo is the GitHub repository releases are published from (owner/name).
// packaging/release.sh stamps it into install.sh.
const Repo = "jayleaton/davit"

// Version and Commit are set at link time (see Makefile `build`):
// -ldflags "-X davit/internal/buildinfo.Version=... -X davit/internal/buildinfo.Commit=...".
// Without them, Commit falls back to the VCS stamp Go embeds, when present.
var (
	Version = "0.0.0-dev"
	Commit  = ""
)

// EnvPrefix is the prefix for environment variables, e.g. DAVIT_CURRENT_ID.
var EnvPrefix = strings.ToUpper(Name) + "_"

// Env returns the full environment variable name for suffix.
func Env(suffix string) string { return EnvPrefix + suffix }

// Info describes this binary.
type Info struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Modified  bool   `json:"modified,omitempty"` // built from a dirty tree
	BuildTime string `json:"buildTime,omitempty"`
	GoVersion string `json:"goVersion"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get returns the build information, using the VCS stamps Go embeds.
func Get() Info {
	i := Info{Name: Name, Version: Version, Commit: Commit, GoVersion: goruntime.Version(), OS: goruntime.GOOS, Arch: goruntime.GOARCH}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if i.Commit == "" {
					i.Commit = s.Value
				}
			case "vcs.modified":
				i.Modified = i.Modified || s.Value == "true"
			case "vcs.time":
				i.BuildTime = s.Value
			}
		}
	}
	return i
}

// String is the one-line human form, e.g. "davit 0.1.0 (abc1234, go1.26.8 linux/amd64)".
func (i Info) String() string {
	var b strings.Builder
	b.WriteString(i.Name + " " + i.Version + " (")
	if i.Commit != "" {
		c := i.Commit
		if len(c) > 12 {
			c = c[:12]
		}
		b.WriteString(c)
		if i.Modified {
			b.WriteString("-dirty")
		}
		b.WriteString(", ")
	}
	b.WriteString(i.GoVersion + " " + i.OS + "/" + i.Arch + ")")
	return b.String()
}
