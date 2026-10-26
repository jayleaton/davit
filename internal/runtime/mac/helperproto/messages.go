package helperproto

import (
	"path"
	"strings"
	"time"
)

// Key identifies the VM run a helper serves.
type Key struct {
	VMID       string `json:"vmId"`
	Generation int64  `json:"generation"`
}

// Validate checks the key's shape (the adapter's key rules).
func (k Key) Validate() error {
	if k.VMID == "" || len(k.VMID) > 64 || strings.ContainsAny(k.VMID, "/\x00") || k.Generation < 1 {
		return Errorf(CodeInvalidArgument, "bad key %q generation %d", k.VMID, k.Generation)
	}
	return nil
}

// Hello opens every connection (client to helper).
type Hello struct {
	MinVersion uint16 `json:"minVersion"`
	MaxVersion uint16 `json:"maxVersion"`
	// Client names the daemon build, for the helper's log.
	Client string `json:"client"`
}

func (h *Hello) Validate() error {
	if h.MinVersion == 0 || h.MaxVersion < h.MinVersion {
		return Errorf(CodeInvalidArgument, "hello: version range %d..%d", h.MinVersion, h.MaxVersion)
	}
	return nil
}

// HelloResult identifies the helper.
type HelloResult struct {
	Version uint16 `json:"version"` // negotiated
	// Incarnation is a random ID chosen at helper start; it is in
	// helper.json too, and a client compares the two before trusting the
	// helper (a reused PID or a stale socket never matches).
	Incarnation string `json:"incarnation"`
	Key         Key    `json:"key"`
	// HelperVersion and Build identify the helper binary.
	HelperVersion string `json:"helperVersion"`
	Build         string `json:"build"`
	// Operations lists the message types this helper implements.
	Operations []MsgType `json:"operations"`
}

func (h *HelloResult) Validate() error {
	if h.Version == 0 || h.Incarnation == "" {
		return Errorf(CodeInvalidArgument, "hello result without version or incarnation")
	}
	return h.Key.Validate()
}

// Network modes of a VM configuration.
const (
	// NetworkNone: no NIC; the guest is reachable over vsock only.
	NetworkNone = "none"
)

// VMConfig is the VM a Create asks for. Paths are absolute, clean, and name
// regular files the daemon prepared; the helper refuses symlinks and other
// file types (it opens them with O_NOFOLLOW and checks the type).
type VMConfig struct {
	Key         Key    `json:"key"`
	CPUs        int    `json:"cpus"`
	MemoryBytes int64  `json:"memoryBytes"`
	Kernel      string `json:"kernel"`
	Initrd      string `json:"initrd,omitempty"`
	CommandLine string `json:"commandLine"`
	// RootDisk is the private raw ext4 disk (virtio block, read-write).
	RootDisk string `json:"rootDisk"`
	// Network is NetworkNone; packet-link attachments come with the
	// network contract (MAC-BACKEND §3).
	Network string `json:"network"`
	// VsockPort is the guest agent's vsock port (1024).
	VsockPort uint32 `json:"vsockPort"`
}

func cleanAbs(field, p string, required bool) error {
	switch {
	case p == "" && !required:
		return nil
	case p == "" || !path.IsAbs(p) || path.Clean(p) != p || strings.ContainsRune(p, 0) || len(p) > 1024:
		return Errorf(CodeInvalidArgument, "%s must be a clean absolute path: %q", field, p)
	}
	return nil
}

func (c *VMConfig) Validate() error {
	if err := c.Key.Validate(); err != nil {
		return err
	}
	switch {
	case c.CPUs < 1 || c.CPUs > 1024:
		return Errorf(CodeInvalidArgument, "cpus %d", c.CPUs)
	case c.MemoryBytes < 1<<20 || c.MemoryBytes%(1<<20) != 0:
		return Errorf(CodeInvalidArgument, "memoryBytes %d (whole MiB required)", c.MemoryBytes)
	case len(c.CommandLine) > 4096:
		return Errorf(CodeInvalidArgument, "commandLine longer than 4096 bytes")
	case c.Network != NetworkNone:
		return Errorf(CodeUnsupported, "network %q", c.Network)
	case c.VsockPort == 0:
		return Errorf(CodeInvalidArgument, "vsockPort is required")
	}
	for _, p := range []struct {
		field, path string
		required    bool
	}{{"kernel", c.Kernel, true}, {"initrd", c.Initrd, false}, {"rootDisk", c.RootDisk, true}} {
		if err := cleanAbs(p.field, p.path, p.required); err != nil {
			return err
		}
	}
	return nil
}

// VM states.
type State string

const (
	StateCreated  State = "created"  // configured, not started
	StateStarting State = "starting" // VZ start in progress
	StateRunning  State = "running"
	StatePaused   State = "paused"
	StateStopping State = "stopping" // guest shutdown requested
	StateStopped  State = "stopped"  // terminal: the helper writes exit.json and exits
	StateError    State = "error"    // terminal: VZ reported an error
)

// Exit reasons (StateResult.Reason, Exit.Reason).
const (
	ReasonShutdown = "shutdown" // the guest powered off (Stop or on its own)
	ReasonKilled   = "killed"   // Kill
	ReasonCrashed  = "crashed"  // VZ stopped the VM with an error
)

// StateResult answers Create, Boot, State, Pause, Resume, Stop and Kill,
// and is the body of each Event.
type StateResult struct {
	State     State     `json:"state"`
	StartedAt time.Time `json:"startedAt,omitzero"`
	Reason    string    `json:"reason,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

func (s *StateResult) Validate() error {
	switch s.State {
	case StateCreated, StateStarting, StateRunning, StatePaused, StateStopping, StateStopped, StateError:
		return nil
	}
	return Errorf(CodeInvalidArgument, "unknown state %q", s.State)
}

// Terminal reports a state the VM never leaves.
func (s State) Terminal() bool { return s == StateStopped || s == StateError }

// Stop asks the guest to shut down and waits up to GraceMillis for the VM
// to stop. On timeout the reply is an error (code timeout) and the VM keeps
// running: forcing is the client's explicit Kill.
type Stop struct {
	GraceMillis int64 `json:"graceMillis"`
}

func (s *Stop) Validate() error {
	if s.GraceMillis < 0 || s.GraceMillis > int64(time.Hour/time.Millisecond) {
		return Errorf(CodeInvalidArgument, "graceMillis %d", s.GraceMillis)
	}
	return nil
}

// Dial opens a guest vsock stream (str.sock only). After a successful
// reply the connection carries raw guest bytes.
type Dial struct {
	Port uint32 `json:"port"`
	// TimeoutMillis bounds the connect (default 10 s).
	TimeoutMillis int64 `json:"timeoutMillis,omitempty"`
}

func (d *Dial) Validate() error {
	if d.Port == 0 || d.TimeoutMillis < 0 {
		return Errorf(CodeInvalidArgument, "dial port %d timeout %d", d.Port, d.TimeoutMillis)
	}
	return nil
}

// Record is helper.json: written atomically by the helper at start, before
// it listens, so a daemon finds and identifies it after a restart.
type Record struct {
	Protocol    uint16    `json:"protocol"`
	Incarnation string    `json:"incarnation"`
	Key         Key       `json:"key"`
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"startedAt"`
}

func (r *Record) Validate() error {
	if r.Protocol == 0 || r.Incarnation == "" || r.PID <= 0 {
		return Errorf(CodeInvalidArgument, "helper record incomplete")
	}
	return r.Key.Validate()
}

// Exit is exit.json: written atomically by the helper after its VM stopped,
// before it exits.
type Exit struct {
	Incarnation string    `json:"incarnation"`
	State       State     `json:"state"` // stopped or error
	Reason      string    `json:"reason"`
	Detail      string    `json:"detail,omitempty"`
	At          time.Time `json:"at"`
}

// Limits are the VZ shape limits the helper reports.
type Limits struct {
	MaxCPUs          int   `json:"maxCPUs"`
	MinMemoryBytes   int64 `json:"minMemoryBytes"`
	MaxMemoryBytes   int64 `json:"maxMemoryBytes"`
	MemoryAlignBytes int64 `json:"memoryAlignBytes"`
}

// ProbeResult is what `helper probe` prints (one JSON object) and exits 0.
type ProbeResult struct {
	MinVersion    uint16 `json:"minVersion"`
	MaxVersion    uint16 `json:"maxVersion"`
	HelperVersion string `json:"helperVersion"`
	Build         string `json:"build"`
	OSVersion     string `json:"osVersion"` // e.g. "14.6.1"
	OSBuild       string `json:"osBuild"`   // e.g. "23G93"
	Arch          string `json:"arch"`      // "arm64"
	VZSupported   bool   `json:"vzSupported"`
	// Entitled reports whether the process holds
	// com.apple.security.virtualization (VZ refuses to start VMs otherwise).
	Entitled   bool      `json:"entitled"`
	Limits     Limits    `json:"limits"`
	Operations []MsgType `json:"operations"`
}

func (p *ProbeResult) Validate() error {
	if p.MinVersion == 0 || p.MaxVersion < p.MinVersion || p.HelperVersion == "" {
		return Errorf(CodeInvalidArgument, "probe: incomplete")
	}
	if p.Limits.MaxCPUs < 1 || p.Limits.MinMemoryBytes <= 0 || p.Limits.MaxMemoryBytes < p.Limits.MinMemoryBytes {
		return Errorf(CodeInvalidArgument, "probe: bad limits %+v", p.Limits)
	}
	return nil
}
