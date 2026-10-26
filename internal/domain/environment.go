package domain

import (
	"net/url"
	"path"
	"strings"
	"time"
)

// Environments (G3, contract only): a named, versioned workspace manifest.
// Every change mints a new immutable revision; VMs pin one revision. A
// manifest never holds a secret value: secret files name host secrets
// (credential store), which the host resolves at preparation time and
// delivers over the authenticated guest session (guestproto
// WorkspacePrepare). VM ready (agent barrier) and workspace ready (clone,
// files, setup done) are distinct states.

// EnvironmentID identifies an environment ("env_…").
type EnvironmentID string

// Environment is a named series of manifest revisions.
type Environment struct {
	ID    EnvironmentID `json:"id"`
	Owner PrincipalID   `json:"-"`
	Name  string        `json:"name"`
	// LatestRevision is the newest revision number (revisions start at 1).
	LatestRevision int64     `json:"latestRevision"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// EnvironmentRevision is one immutable manifest version.
type EnvironmentRevision struct {
	EnvironmentID EnvironmentID `json:"environmentId"`
	Revision      int64         `json:"revision"`
	Manifest      EnvManifest   `json:"manifest"`
	CreatedAt     time.Time     `json:"createdAt"`
}

// EnvManifest describes how to prepare a workspace.
type EnvManifest struct {
	Repos []RepoSpec `json:"repos"`
	// Variables are non-secret environment variables of guest processes.
	Variables   map[string]string `json:"variables"`
	SecretFiles []SecretFileRef   `json:"secretFiles"`
	// Setup runs in order after repos and files; the first failure stops
	// preparation and leaves the workspace failed.
	Setup []SetupAction `json:"setup"`
	// CredentialProfile names the profile whose grants new VMs receive
	// (G4); empty means none.
	CredentialProfile string `json:"credentialProfile,omitempty"`
}

// RepoSpec is a Git repository checked out at Ref into Path. URL is any Git
// URL; private access comes from a credential grant, never from the URL.
type RepoSpec struct {
	URL  string `json:"url"`
	Ref  string `json:"ref,omitempty"` // branch, tag or commit; empty = default branch
	Path string `json:"path"`          // absolute guest path
}

// SecretFileRef places the host secret named Secret at Path.
type SecretFileRef struct {
	Path   string `json:"path"`
	Secret string `json:"secret"`
	Mode   string `json:"mode,omitempty"` // octal, default "0600"
}

// SetupAction is one setup command (argv, no implicit shell).
type SetupAction struct {
	Name           string   `json:"name"`
	Argv           []string `json:"argv"`
	Cwd            string   `json:"cwd,omitempty"`
	User           string   `json:"user,omitempty"`
	TimeoutSeconds int64    `json:"timeoutSeconds,omitempty"`
}

// EnvironmentRef selects a revision in a request: Environment is an ID or
// name; Revision 0 means the latest at the time of the request (the VM then
// pins that number).
type EnvironmentRef struct {
	Environment string `json:"environment"`
	Revision    int64  `json:"revision,omitempty"`
}

// EnvironmentPin is the revision a VM was prepared from.
type EnvironmentPin struct {
	EnvironmentID EnvironmentID `json:"environmentId"`
	Name          string        `json:"name"`
	Revision      int64         `json:"revision"`
}

// WorkspaceState is the preparation state of a VM's workspace.
type WorkspaceState string

const (
	WorkspacePending   WorkspaceState = "pending"   // VM not ready yet
	WorkspacePreparing WorkspaceState = "preparing" // clone/files/setup running
	WorkspaceReady     WorkspaceState = "ready"
	WorkspaceFailed    WorkspaceState = "failed" // see Workspace.Error; the VM stays usable
)

// Workspace is VM.workspace: present only on VMs created with an environment.
type Workspace struct {
	State     WorkspaceState `json:"state"`
	StartedAt *time.Time     `json:"startedAt,omitempty"`
	ReadyAt   *time.Time     `json:"readyAt,omitempty"`
	Error     *SetupError    `json:"error,omitempty"`
}

// SetupError says which preparation step failed.
type SetupError struct {
	// Step and Phase are empty when preparation failed outside a step
	// (e.g. the guest agent became unreachable).
	Step     string `json:"step,omitempty"`  // setup action name, repo path or file path
	Phase    string `json:"phase,omitempty"` // clone | file | setup
	ExitCode *int   `json:"exitCode,omitempty"`
	Message  string `json:"message"`
}

// Validate checks a manifest's shape. It cannot see secrets: there is no
// field to put one in.
func (m EnvManifest) Validate() error {
	bad := func(field, format string, args ...any) error {
		return Errorf(CodeInvalidArgument, format, args...).WithDetail("field", field)
	}
	abs := func(p string) bool { return path.IsAbs(p) && path.Clean(p) == p }
	paths := map[string]bool{}
	for i, r := range m.Repos {
		if strings.TrimSpace(r.URL) == "" {
			return bad("repos", "repos[%d]: url is required", i)
		}
		if u, err := url.Parse(r.URL); err == nil && u.User != nil {
			return bad("repos", "repos[%d]: url must not carry credentials; use a credential grant", i)
		}
		if !abs(r.Path) || paths[r.Path] {
			return bad("repos", "repos[%d]: path must be a unique absolute clean path", i)
		}
		paths[r.Path] = true
	}
	for k := range m.Variables {
		if !validEnvKey(k) {
			return bad("variables", "variable name %q is invalid", k)
		}
	}
	for i, f := range m.SecretFiles {
		if !abs(f.Path) || paths[f.Path] {
			return bad("secretFiles", "secretFiles[%d]: path must be a unique absolute clean path", i)
		}
		paths[f.Path] = true
		if f.Secret == "" {
			return bad("secretFiles", "secretFiles[%d]: secret name is required", i)
		}
		if f.Mode != "" && !validOctalMode(f.Mode) {
			return bad("secretFiles", "secretFiles[%d]: mode %q is not octal permission bits", i, f.Mode)
		}
	}
	names := map[string]bool{}
	for i, s := range m.Setup {
		if s.Name == "" || names[s.Name] {
			return bad("setup", "setup[%d]: name must be non-empty and unique", i)
		}
		names[s.Name] = true
		if len(s.Argv) == 0 {
			return bad("setup", "setup[%d]: argv is required", i)
		}
		if s.Cwd != "" && !abs(s.Cwd) {
			return bad("setup", "setup[%d]: cwd must be absolute", i)
		}
		if s.TimeoutSeconds < 0 {
			return bad("setup", "setup[%d]: timeoutSeconds must be >= 0", i)
		}
	}
	return nil
}

func validEnvKey(k string) bool {
	if k == "" || k[0] >= '0' && k[0] <= '9' {
		return false
	}
	for _, c := range k {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func validOctalMode(m string) bool {
	if len(m) < 3 || len(m) > 4 {
		return false
	}
	for _, c := range m {
		if c < '0' || c > '7' {
			return false
		}
	}
	return true
}
