package guestproto

import (
	"path"
	"strings"
)

// Request-size bounds (the frame limit bounds total size; these bound shape).
const (
	MaxArgs        = 4096
	MaxEnv         = 4096
	MaxPathLen     = 4096
	MaxIDLen       = 128
	MaxUserLen     = 32
	MaxHostnameLen = 253
	MaxFeatures    = 64
	MaxGraceSecs   = 3600
	// MaxProcessSamples bounds ProcessList.Processes.
	MaxProcessSamples = 4096
	// MaxWorkspaceItems bounds each of WorkspacePrepare's Repos, Files and
	// Setup.
	MaxWorkspaceItems = 1024
)

// signals allowed in ProcessSignal.
var signals = map[string]bool{
	"HUP": true, "INT": true, "QUIT": true, "KILL": true, "TERM": true,
	"USR1": true, "USR2": true, "CONT": true, "STOP": true, "WINCH": true,
}

// ValidGuestPath reports whether p is an absolute, cleaned guest path with
// no NUL and no ".." component.
func ValidGuestPath(p string) error {
	switch {
	case p == "":
		return invalidf("path is required")
	case len(p) > MaxPathLen:
		return invalidf("path longer than %d bytes", MaxPathLen)
	case strings.IndexByte(p, 0) >= 0:
		return invalidf("path contains NUL")
	case !strings.HasPrefix(p, "/"):
		return invalidf("path %q is not absolute", p)
	case path.Clean(p) != p:
		return invalidf("path %q is not clean", p)
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return invalidf("path %q contains ..", p)
		}
	}
	return nil
}

// validUser accepts POSIX-portable user names: [a-z_][a-z0-9_-]*.
func validUser(u string, required bool) error {
	if u == "" {
		if required {
			return invalidf("user is required")
		}
		return nil
	}
	if len(u) > MaxUserLen {
		return invalidf("user longer than %d bytes", MaxUserLen)
	}
	for i, r := range u {
		ok := r >= 'a' && r <= 'z' || r == '_' || i > 0 && (r >= '0' && r <= '9' || r == '-')
		if !ok {
			return invalidf("invalid user %q", u)
		}
	}
	return nil
}

func validateID(field, id string) error {
	if id == "" {
		return invalidf("%s is required", field)
	}
	if len(id) > MaxIDLen || strings.ContainsAny(id, "\x00/ ") {
		return invalidf("invalid %s", field)
	}
	return nil
}

func validateVMID(id string) error { return validateID("vmId", id) }

func validateSecret(s []byte) error {
	if len(s) < MinSecretSize || len(s) > MaxSecretSize {
		return invalidf("secret must be %d..%d bytes", MinSecretSize, MaxSecretSize)
	}
	return nil
}

func validHostname(h string) error {
	if h == "" || len(h) > MaxHostnameLen {
		return invalidf("hostname must be 1..%d bytes", MaxHostnameLen)
	}
	for _, l := range strings.Split(h, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return invalidf("invalid hostname %q", h)
		}
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return invalidf("invalid hostname %q", h)
			}
		}
	}
	return nil
}

func validChunk(data []byte) error {
	if len(data) > MaxChunkSize {
		return invalidf("chunk of %d bytes exceeds %d", len(data), MaxChunkSize)
	}
	return nil
}

// Validate checks ReadinessProbe.
func (r ReadinessProbe) Validate() error {
	if len(r.Services) > MaxFeatures {
		return invalidf("too many services")
	}
	return nil
}

// Validate checks SetIdentity.
func (s SetIdentity) Validate() error {
	if err := validateVMID(s.VMID); err != nil {
		return err
	}
	if s.Generation == 0 {
		return invalidf("generation must be > 0")
	}
	if err := validHostname(s.Hostname); err != nil {
		return err
	}
	if s.WallClockUnixNano <= 0 {
		return invalidf("wallClock must be a positive unix-nanosecond time")
	}
	if s.NewAgentSecret != nil {
		if err := validateSecret(s.NewAgentSecret); err != nil {
			return err
		}
	}
	return nil
}

func validArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return invalidf("argv must be non-empty with a non-empty argv[0]")
	}
	if len(argv) > MaxArgs {
		return invalidf("argv has more than %d entries", MaxArgs)
	}
	for _, a := range argv {
		if strings.IndexByte(a, 0) >= 0 {
			return invalidf("argv entry contains NUL")
		}
	}
	return nil
}

// validEnv accepts KEY=VALUE entries. Errors name the key, never the value.
func validEnv(env []string) error {
	if len(env) > MaxEnv {
		return invalidf("env has more than %d entries", MaxEnv)
	}
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.IndexByte(kv, 0) >= 0 {
			return invalidf("env entry %q is not KEY=VALUE", k)
		}
	}
	return nil
}

// validHandle accepts a durable job handle: at most MaxIDLen bytes of
// [A-Za-z0-9_.:-].
func validHandle(h string, required bool) error {
	if h == "" {
		if required {
			return invalidf("handle is required")
		}
		return nil
	}
	if len(h) > MaxIDLen {
		return invalidf("handle longer than %d bytes", MaxIDLen)
	}
	for _, r := range h {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '_' || r == '.' || r == ':' || r == '-'
		if !ok {
			return invalidf("invalid handle %q", h)
		}
	}
	return nil
}

// Validate checks Exec: non-empty argv, KEY=VALUE env, absolute cwd.
func (e Exec) Validate() error {
	if err := validArgv(e.Argv); err != nil {
		return err
	}
	if err := validEnv(e.Env); err != nil {
		return err
	}
	if e.Cwd != "" {
		if err := ValidGuestPath(e.Cwd); err != nil {
			return err
		}
	}
	if e.DeadlineSeconds < 0 {
		return invalidf("deadlineSeconds must be >= 0")
	}
	if err := validHandle(e.Handle, false); err != nil {
		return err
	}
	return validUser(e.User, false)
}

// Validate checks ProcessAttach.
func (p ProcessAttach) Validate() error { return validateID("processId", p.ProcessID) }

// Validate checks ProcessSignal.
func (p ProcessSignal) Validate() error {
	if err := validateID("processId", p.ProcessID); err != nil {
		return err
	}
	if !signals[p.Signal] {
		return invalidf("unsupported signal %q", p.Signal)
	}
	return nil
}

// Validate checks ProcessStatusRequest.
func (p ProcessStatusRequest) Validate() error { return validateID("processId", p.ProcessID) }

// Validate checks Stdout.
func (c Stdout) Validate() error { return validChunk(c.Data) }

// Validate checks Stderr.
func (c Stderr) Validate() error { return validChunk(c.Data) }

// Validate checks Stdin.
func (c Stdin) Validate() error { return validChunk(c.Data) }

// Validate checks FileChunk.
func (c FileChunk) Validate() error { return validChunk(c.Data) }

// Validate checks ReadFile.
func (r ReadFile) Validate() error {
	if err := ValidGuestPath(r.Path); err != nil {
		return err
	}
	if err := validUser(r.User, false); err != nil {
		return err
	}
	if r.Offset < 0 || r.Limit < 0 {
		return invalidf("offset and limit must be >= 0")
	}
	if err := validArchive(r.Archive); err != nil {
		return err
	}
	if r.Archive != "" && (r.Offset != 0 || r.Limit != 0) {
		return invalidf("offset and limit do not apply to archives")
	}
	return nil
}

func validArchive(a string) error {
	if a != "" && a != ArchiveTar {
		return invalidf("unsupported archive %q", a)
	}
	return nil
}

// Validate checks WriteFile.
func (w WriteFile) Validate() error {
	if err := ValidGuestPath(w.Path); err != nil {
		return err
	}
	if w.Path == "/" {
		return invalidf("cannot write to /")
	}
	if err := validUser(w.User, false); err != nil {
		return err
	}
	if w.Mode > 0o7777 {
		return invalidf("mode %#o has non-permission bits", w.Mode)
	}
	return validArchive(w.Archive)
}

// Validate checks FileCommit.
func (c FileCommit) Validate() error {
	if c.Size < 0 {
		return invalidf("size must be >= 0")
	}
	if c.SHA256 != nil && len(c.SHA256) != 32 {
		return invalidf("sha256 must be 32 bytes")
	}
	return nil
}

// Validate checks Shutdown.
func (s Shutdown) Validate() error {
	if s.GraceSeconds < 0 || s.GraceSeconds > MaxGraceSecs {
		return invalidf("graceSeconds must be 0..%d", MaxGraceSecs)
	}
	return nil
}

// Validate checks DialTCP: port 1..65535.
func (d DialTCP) Validate() error {
	if d.Port == 0 {
		return invalidf("port must be 1..65535")
	}
	return nil
}

// Validate checks ProcessLookup.
func (p ProcessLookup) Validate() error { return validHandle(p.Handle, true) }

// Validate checks ProcessList: at most MaxProcessSamples entries with
// positive PIDs and valid handles.
func (p ProcessList) Validate() error {
	if len(p.Processes) > MaxProcessSamples {
		return invalidf("process list has more than %d entries", MaxProcessSamples)
	}
	for _, g := range p.Processes {
		if g.PID <= 0 || g.PPID < 0 {
			return invalidf("invalid pid %d (ppid %d)", g.PID, g.PPID)
		}
		if err := validHandle(g.Handle, false); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks Listen: port 0..65535 (0 = agent picks).
func (l Listen) Validate() error {
	if l.Port < 0 || l.Port > 65535 {
		return invalidf("port must be 0..65535")
	}
	return nil
}

// Validate checks ListenStarted: a listener ID and a bound port 1..65535.
func (l ListenStarted) Validate() error {
	if err := validateID("listenerId", l.ListenerID); err != nil {
		return err
	}
	if l.Port < 1 || l.Port > 65535 {
		return invalidf("port must be 1..65535")
	}
	return nil
}

// Validate checks ListenAccepted.
func (l ListenAccepted) Validate() error { return validConnRef(l.ListenerID, l.ConnID) }

// Validate checks ReverseConnect.
func (r ReverseConnect) Validate() error { return validConnRef(r.ListenerID, r.ConnID) }

// Validate checks Unlisten.
func (u Unlisten) Validate() error { return validateID("listenerId", u.ListenerID) }

func validConnRef(listenerID, connID string) error {
	if err := validateID("listenerId", listenerID); err != nil {
		return err
	}
	return validateID("connId", connID)
}

// Validate checks WorkspacePrepare: a revision label, repos with a URL and
// an absolute path, files with an absolute path, permission bits and at most
// MaxChunkSize bytes, KEY=VALUE env, and uniquely named setup steps.
func (w WorkspacePrepare) Validate() error {
	if err := validateID("revision", w.Revision); err != nil {
		return err
	}
	if len(w.Repos) > MaxWorkspaceItems || len(w.Files) > MaxWorkspaceItems || len(w.Setup) > MaxWorkspaceItems {
		return invalidf("repos, files and setup are limited to %d entries each", MaxWorkspaceItems)
	}
	for _, r := range w.Repos {
		if r.URL == "" || len(r.URL) > MaxPathLen || strings.IndexByte(r.URL, 0) >= 0 {
			return invalidf("repo url must be 1..%d bytes without NUL", MaxPathLen)
		}
		if strings.IndexByte(r.Ref, 0) >= 0 || len(r.Ref) > MaxPathLen {
			return invalidf("invalid repo ref")
		}
		if err := ValidGuestPath(r.Path); err != nil {
			return err
		}
	}
	for _, f := range w.Files {
		if err := ValidGuestPath(f.Path); err != nil {
			return err
		}
		if f.Path == "/" {
			return invalidf("cannot write to /")
		}
		if f.Mode > 0o7777 {
			return invalidf("mode %#o has non-permission bits", f.Mode)
		}
		if len(f.Data) > MaxChunkSize {
			return invalidf("file %q has more than %d bytes", f.Path, MaxChunkSize)
		}
	}
	if err := validEnv(w.Env); err != nil {
		return err
	}
	names := make(map[string]bool, len(w.Setup))
	for _, s := range w.Setup {
		if s.Name == "" || len(s.Name) > MaxIDLen || strings.IndexByte(s.Name, 0) >= 0 {
			return invalidf("setup step name must be 1..%d bytes without NUL", MaxIDLen)
		}
		if names[s.Name] {
			return invalidf("duplicate setup step %q", s.Name)
		}
		names[s.Name] = true
		if err := validArgv(s.Argv); err != nil {
			return err
		}
		if s.Cwd != "" {
			if err := ValidGuestPath(s.Cwd); err != nil {
				return err
			}
		}
		if err := validUser(s.User, false); err != nil {
			return err
		}
		if s.TimeoutSeconds < 0 {
			return invalidf("timeoutSeconds must be >= 0")
		}
	}
	return nil
}

// Validate checks WorkspaceProgress: a step name and known phase and state.
func (p WorkspaceProgress) Validate() error {
	if p.Step == "" {
		return invalidf("step is required")
	}
	switch p.Phase {
	case WorkspacePhaseClone, WorkspacePhaseFile, WorkspacePhaseSetup:
	default:
		return invalidf("unknown phase %q", p.Phase)
	}
	switch p.State {
	case WorkspaceStepRunning, WorkspaceStepDone, WorkspaceStepFailed:
	default:
		return invalidf("unknown step state %q", p.State)
	}
	return nil
}

// Validate checks WorkspaceResult: a revision, a known state, and no
// FailedStep or ExitCode when ready.
func (r WorkspaceResult) Validate() error {
	if err := validateID("revision", r.Revision); err != nil {
		return err
	}
	switch r.State {
	case WorkspaceReady:
		if r.FailedStep != "" || r.ExitCode != nil {
			return invalidf("a ready workspace has no failed step")
		}
	case WorkspaceFailed:
	default:
		return invalidf("unknown workspace state %q", r.State)
	}
	return nil
}
