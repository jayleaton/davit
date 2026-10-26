package guestproto

import (
	"encoding/json"
	"errors"
	"sort"
)

// MsgType names a message. Requests and their responses are distinct types.
type MsgType string

const (
	// Handshake (first exchange on every control session).
	TypeHello    MsgType = "hello"
	TypeHelloAck MsgType = "hello.ack"

	// Generic responses.
	TypeAck   MsgType = "ack"   // success with no data
	TypeError MsgType = "error" // frame.Error is set

	TypePing MsgType = "ping"
	TypePong MsgType = "pong"

	TypeReadinessProbe MsgType = "readiness.probe"
	TypeReadiness      MsgType = "readiness"

	TypeSetIdentity    MsgType = "identity.set"
	TypeIdentityResult MsgType = "identity.result"

	TypeExec           MsgType = "exec"
	TypeProcessStarted MsgType = "process.started"
	TypeProcessAttach  MsgType = "process.attach"
	TypeProcessSignal  MsgType = "process.signal"
	TypeProcessStatusQ MsgType = "process.status.get"
	TypeProcessStatus  MsgType = "process.status"

	// Process stream frames (Stream = the exec/attach request ID).
	TypeStdout        MsgType = "stdout"
	TypeStderr        MsgType = "stderr"
	TypeStdin         MsgType = "stdin"
	TypeStdinClose    MsgType = "stdin.close"
	TypeProcessExited MsgType = "process.exited"

	TypeReadFile       MsgType = "file.read"
	TypeFileReadResult MsgType = "file.read.result"
	TypeWriteFile      MsgType = "file.write"
	TypeFileChunk      MsgType = "file.chunk" // stream frame
	TypeFileCommit     MsgType = "file.commit"
	TypeFileWriteDone  MsgType = "file.write.result"

	TypeShutdown MsgType = "shutdown"

	// TypeDialTCP turns its session into a raw byte stream after the Ack.
	TypeDialTCP MsgType = "tcp.dial"

	// Managed-job handles (feature "jobs").
	TypeProcessLookup MsgType = "process.lookup"

	// Whole-guest process samples (feature "ps").
	TypeProcessListQ MsgType = "process.list.get"
	TypeProcessList  MsgType = "process.list"

	// Reverse connections (feature "listen"). TypeListenStarted and
	// TypeListenAccepted are stream frames (Stream = the Listen request ID);
	// TypeReverseConnect turns its session into a raw byte stream after the
	// Ack, like TypeDialTCP.
	TypeListen         MsgType = "tcp.listen"
	TypeListenStarted  MsgType = "tcp.listen.started"
	TypeListenAccepted MsgType = "tcp.listen.accepted"
	TypeReverseConnect MsgType = "tcp.reverse"
	TypeUnlisten       MsgType = "tcp.unlisten"

	// Environment preparation (feature "workspace"). Progress and result are
	// stream frames (Stream = the WorkspacePrepare request ID).
	TypeWorkspacePrepare  MsgType = "workspace.prepare"
	TypeWorkspaceProgress MsgType = "workspace.progress"
	TypeWorkspaceResult   MsgType = "workspace.result"
)

func isHandshake(t MsgType) bool { return t == TypeHello || t == TypeHelloAck }

// Message is implemented by every body type.
type Message interface{ MsgType() MsgType }

// Validator is implemented by request bodies; Decode calls it.
type Validator interface{ Validate() error }

// Ack is the empty success response (signal, attach, write open, shutdown).
type Ack struct{}

// Ping is a liveness check; the peer answers Pong.
type Ping struct{}

// Pong answers Ping.
type Pong struct{}

// ReadinessProbe asks the agent for readiness (SPEC §4 VM.readiness).
// Services optionally limits which named services are probed.
type ReadinessProbe struct {
	Services []string `json:"services,omitempty"`
}

// ServiceState is a probed service's state.
type ServiceState string

const (
	ServiceReady    ServiceState = "ready"
	ServiceStarting ServiceState = "starting"
	ServiceFailed   ServiceState = "failed"
	ServiceUnknown  ServiceState = "unknown"
)

// ServiceSSHRestart is reported ServiceFailed in Readiness.Services when the
// agent could not restart sshd after an identity change; SSH then never
// becomes ready in this generation.
const ServiceSSHRestart = "sshd.restart"

// Readiness answers ReadinessProbe. SSH is true only once sshd serves this
// generation's host keys (never the pre-barrier sshd). Services holds the
// names the probe asked for (and ServiceSSHRestart on failure).
type Readiness struct {
	Agent    bool                    `json:"agent"`
	SSH      bool                    `json:"ssh"`
	Services map[string]ServiceState `json:"services,omitempty"`
}

// SetIdentity is restore-barrier steps 2–4 (SPEC §5): the host assigns
// identity after cold boot, golden restore or fork. VMID and Generation come
// from the host's records; the guest never chooses them.
type SetIdentity struct {
	VMID       string `json:"vmId"`
	Generation uint64 `json:"generation"`
	Hostname   string `json:"hostname"`
	// WallClockUnixNano is trusted host time; the agent steps the clock
	// before admitting TLS/user workloads.
	WallClockUnixNano int64 `json:"wallClock"`
	// RotateMachineID/RotateSSHHostKeys are set for new VMs and forks, not
	// for resume of the same disk VM. The agent must restart (or have gated)
	// identity-dependent services; writing /etc/machine-id alone does not
	// reset identities cached by systemd/D-Bus.
	RotateMachineID   bool `json:"rotateMachineId"`
	RotateSSHHostKeys bool `json:"rotateSshHostKeys"`
	// NewAgentSecret replaces the per-generation session secret used for
	// later handshakes in this generation (MinSecretSize..MaxSecretSize).
	NewAgentSecret []byte `json:"newAgentSecret,omitempty"`
	// SSHUserCAKeys are OpenSSH-format host-CA public keys the guest sshd
	// trusts for short-lived user certificates (SPEC §4).
	SSHUserCAKeys []string `json:"sshUserCaKeys,omitempty"`
}

// IdentityResult answers SetIdentity; the host verifies it (barrier step 5)
// and records the SSH host-key fingerprints before publishing ready.
type IdentityResult struct {
	VMID                   string   `json:"vmId"`
	Generation             uint64   `json:"generation"`
	Hostname               string   `json:"hostname"`
	MachineID              string   `json:"machineId"`
	SSHHostKeyFingerprints []string `json:"sshHostKeyFingerprints"`
	// BootID is informational only. A RAM restore does NOT rotate
	// /proc/sys/kernel/random/boot_id; forks may share it (SPEC §5). Use
	// Generation as the run identity.
	BootID string `json:"bootId,omitempty"`
}

// Exec starts a process (SPEC §3/§4 ExecRequest). Argv is executed as-is;
// a shell is used only when argv itself invokes one (e.g. sh -lc ...).
type Exec struct {
	Argv            []string `json:"argv"`
	Cwd             string   `json:"cwd,omitempty"`
	Env             []string `json:"env,omitempty"` // KEY=VALUE
	DeadlineSeconds int64    `json:"deadlineSeconds,omitempty"`
	Detached        bool     `json:"detached,omitempty"`
	Stdin           bool     `json:"stdin,omitempty"`
	TTY             bool     `json:"tty,omitempty"`
	User            string   `json:"user,omitempty"` // guest user; empty = image default
	// NoFollow starts the process without streaming its output on this
	// request's stream; the host reads it with ProcessAttach instead (so
	// output crosses the transport once).
	NoFollow bool `json:"noFollow,omitempty"`
	// Handle is an optional host-assigned durable handle (feature
	// FeatureJobs; at most MaxIDLen bytes of [A-Za-z0-9_.:-]). The agent
	// records it with the process. An Exec whose Handle matches a process the
	// agent already started this boot is answered with that process's
	// ProcessStarted and never runs twice, so a host that lost its records
	// can retry the Exec or find the process with ProcessLookup.
	Handle string `json:"handle,omitempty"`
}

// ProcessStarted answers Exec. Output then flows on stream = Exec's request
// ID as Stdout/Stderr chunks and a final ProcessExited.
type ProcessStarted struct {
	ProcessID string `json:"processId"`
	PID       int    `json:"pid"`
	Handle    string `json:"handle,omitempty"` // Exec.Handle, if any
}

// ProcessAttach reattaches to a process's retained output after a new
// control session, replaying frames with seq > AfterSeq on stream = this
// request's ID. Stdin is never replayed. Answered by Ack.
type ProcessAttach struct {
	ProcessID string `json:"processId"`
	AfterSeq  uint64 `json:"afterSeq,omitempty"`
}

// ProcessSignal signals a process's process group. Answered by Ack.
type ProcessSignal struct {
	ProcessID string `json:"processId"`
	Signal    string `json:"signal"` // e.g. "TERM", "KILL", "INT"
}

// ProcessStatusRequest asks for a process's status.
type ProcessStatusRequest struct {
	ProcessID string `json:"processId"`
}

// ProcessState is a process's lifecycle state.
type ProcessState string

const (
	StatusRunning ProcessState = "running"
	StatusExited  ProcessState = "exited"
	StatusKilled  ProcessState = "killed"
	// StatusLost means the outcome is unknown (agent restarted, etc.).
	StatusLost ProcessState = "lost"
)

// ProcessStatus answers ProcessStatusRequest.
type ProcessStatus struct {
	ProcessID string       `json:"processId"`
	Status    ProcessState `json:"status"`
	ExitCode  *int         `json:"exitCode,omitempty"`
	Signal    string       `json:"signal,omitempty"`
	// TimedOut means the agent killed the process at Exec.DeadlineSeconds.
	TimedOut bool `json:"timedOut,omitempty"`
	// OutputSeq is the seq of the last stdout/stderr chunk (not the exit
	// frame), a valid ProcessAttach.AfterSeq.
	OutputSeq uint64 `json:"outputSeq,omitempty"`
	Handle    string `json:"handle,omitempty"` // Exec.Handle, if any
}

// ProcessLookup finds a managed job by its Exec.Handle (feature FeatureJobs).
// It is answered by ProcessStatus (with ProcessID and Handle set) or an error
// frame with CodeNotFound when no process this boot carries the handle.
type ProcessLookup struct {
	Handle string `json:"handle"`
}

// ProcessListRequest asks for a sample of every guest process (feature
// FeaturePS). Answered by ProcessList.
type ProcessListRequest struct{}

// ProcessList answers ProcessListRequest with at most MaxProcessSamples
// entries. Truncated says the agent cut the list at that bound.
type ProcessList struct {
	SampledAtUnixNano int64          `json:"sampledAt"`
	Truncated         bool           `json:"truncated,omitempty"`
	Processes         []GuestProcess `json:"processes"`
}

// GuestProcess is one sampled guest process. State is the kernel's state
// letter (e.g. "R", "S", "Z"); User is the owner's name or numeric uid.
type GuestProcess struct {
	PID               int      `json:"pid"`
	PPID              int      `json:"ppid"`
	User              string   `json:"user,omitempty"`
	Args              []string `json:"args,omitempty"`
	State             string   `json:"state,omitempty"`
	CPUTimeMillis     int64    `json:"cpuTimeMillis"`
	RSSBytes          int64    `json:"rssBytes"`
	StartedAtUnixNano int64    `json:"startedAt,omitempty"`
	// Handle is set when the process is a managed job (Exec.Handle).
	Handle string `json:"handle,omitempty"`
}

// Stdout, Stderr and Stdin are process stream chunks. Stdout and Stderr
// share one seq space per process stream.
type (
	Stdout struct {
		Data []byte `json:"data"`
	}
	Stderr struct {
		Data []byte `json:"data"`
	}
	Stdin struct {
		Data []byte `json:"data"`
	}
)

// StdinClose closes the process's stdin.
type StdinClose struct{}

// ProcessExited is the final frame on a process stream.
type ProcessExited struct {
	Status   ProcessState `json:"status"`
	ExitCode *int         `json:"exitCode,omitempty"`
	Signal   string       `json:"signal,omitempty"`
	// TimedOut means the agent killed the process at Exec.DeadlineSeconds.
	TimedOut bool `json:"timedOut,omitempty"`
}

// ReadFile reads a guest file as an explicit guest user. Answered by
// FileReadResult, then FileChunk frames on stream = this request's ID, the
// last with EOF set. Limit 0 means to end of file.
//
// With Archive "tar" (feature FeatureFilesTar) Path may be a directory and
// the chunks are a tar stream of its contents: regular files, directories
// and symlinks (not followed), names relative to Path, permission bits and
// mtimes; other file types are skipped. Offset/Limit must be 0.
type ReadFile struct {
	Path string `json:"path"`
	// User scopes access; empty means the agent's default user.
	User    string `json:"user,omitempty"`
	Offset  int64  `json:"offset,omitempty"`
	Limit   int64  `json:"limit,omitempty"`
	Archive string `json:"archive,omitempty"`
}

// ArchiveTar is the only archive format.
const ArchiveTar = "tar"

// Handshake features.
const (
	FeatureExec     = "exec"
	FeatureFiles    = "files"
	FeatureIdentity = "identity"
	// FeatureFilesTar: ReadFile/WriteFile accept Archive "tar".
	FeatureFilesTar = "files.tar"
	// FeatureTCP: DialTCP is supported.
	FeatureTCP = "tcp"
	// FeatureJobs: Exec.Handle, ProcessStarted/ProcessStatus.Handle and
	// ProcessLookup are supported.
	FeatureJobs = "jobs"
	// FeaturePS: ProcessListRequest is supported.
	FeaturePS = "ps"
	// FeatureListen: Listen, Unlisten and ReverseConnect are supported.
	FeatureListen = "listen"
	// FeatureWorkspace: WorkspacePrepare is supported.
	FeatureWorkspace = "workspace"
)

// FileReadResult answers ReadFile. Size is -1 for archives (unknown).
type FileReadResult struct {
	Size int64  `json:"size"`
	Mode uint32 `json:"mode"`
}

// WriteFile opens an atomic replace of Path as User; answered by Ack. The
// host then sends FileChunk frames on stream = this request's ID and a
// FileCommit (with its own request ID and Stream set). Until commit the data
// lives in a temp file; a dropped session aborts the write.
//
// With Archive "tar" the data is a tar stream (as ReadFile produces) that
// the agent verifies at commit and then extracts into directory Path,
// creating it (Mode, default 0755) if missing and replacing files inside it
// one by one; it is not atomic as a whole. Entries that are absolute,
// contain "..", pass through a symlink, or are not files, directories or
// symlinks are rejected; setuid/setgid/sticky bits are dropped.
type WriteFile struct {
	Path string `json:"path"`
	// User scopes access; empty means the agent's default user.
	User    string `json:"user,omitempty"`
	Mode    uint32 `json:"mode"` // permission bits, <= 0o7777
	Archive string `json:"archive,omitempty"`
}

// FileChunk carries file bytes in either direction.
type FileChunk struct {
	Data []byte `json:"data,omitempty"`
	EOF  bool   `json:"eof,omitempty"`
}

// FileCommit finishes a WriteFile; the agent fsyncs and renames over the
// target only if Size (and SHA256, when given) match.
type FileCommit struct {
	Size   int64  `json:"size"`
	SHA256 []byte `json:"sha256,omitempty"`
}

// FileWriteResult answers FileCommit.
type FileWriteResult struct {
	Size int64 `json:"size"`
}

// Shutdown asks the agent to stop the guest after GraceSeconds. Answered by
// Ack before shutdown begins.
type Shutdown struct {
	GraceSeconds int64 `json:"graceSeconds"`
}

// DialTCP asks the agent to connect to 127.0.0.1:Port in the guest
// (loopback only; there is deliberately no host field). It is answered by
// Ack or an error frame. After the Ack the session carries no more frames:
// both directions become a raw byte stream to that TCP connection, so
// DialTCP must be the only request on its (dedicated, authenticated)
// session and the host must not send stream bytes before reading the Ack.
type DialTCP struct {
	Port uint16 `json:"port"`
}

// Listen opens a TCP listener on guest 127.0.0.1:Port (loopback only; Port 0
// lets the agent pick) whose accepted connections reach the host (feature
// FeatureListen). It opens a stream on this request's ID: first
// ListenStarted, then one ListenAccepted per accepted guest connection. The
// host claims each with ReverseConnect. Unlisten closes the listener and ends
// the stream.
type Listen struct {
	Port int `json:"port"`
}

// ListenStarted is the first frame on a Listen stream: the listener's ID and
// the bound port (the chosen one when Listen.Port was 0).
type ListenStarted struct {
	ListenerID string `json:"listenerId"`
	Port       int    `json:"port"`
}

// ListenAccepted is a Listen stream frame announcing an accepted guest
// connection that waits for the host's ReverseConnect.
type ListenAccepted struct {
	ListenerID string `json:"listenerId"`
	ConnID     string `json:"connId"`
}

// ReverseConnect claims an accepted connection (ListenAccepted). Like
// DialTCP it is answered by Ack or an error frame, after which the session
// carries no more frames: both directions become a raw, half-close-aware
// byte stream to that guest connection. It must be the only request on its
// (dedicated, authenticated) session and the host must not send stream bytes
// before reading the Ack.
type ReverseConnect struct {
	ListenerID string `json:"listenerId"`
	ConnID     string `json:"connId"`
}

// Unlisten closes a listener; answered by Ack. The listener's stream then
// carries no more frames.
type Unlisten struct {
	ListenerID string `json:"listenerId"`
}

// WorkspacePrepare prepares the guest environment (feature
// FeatureWorkspace): clone Repos, write Files, then run Setup steps in order
// with Env. Revision is an opaque host label (e.g. "env_x@3") echoed in
// WorkspaceResult (at most MaxIDLen bytes, no NUL, "/" or space). It opens
// a stream on this request's ID: WorkspaceProgress
// frames, then exactly one WorkspaceResult ending it. A failed step stops the
// sequence.
//
// Files carry secret contents over the authenticated session; the agent never
// logs Files data or Env values. Each file's Data is at most MaxChunkSize;
// larger files are a later chunked extension. Repo credentials are not part
// of this message: they arrive as a credential-proxy placeholder in Env.
type WorkspacePrepare struct {
	Revision string          `json:"revision"`
	Repos    []RepoCheckout  `json:"repos,omitempty"`
	Files    []WorkspaceFile `json:"files,omitempty"`
	Env      []string        `json:"env,omitempty"` // KEY=VALUE
	Setup    []SetupStep     `json:"setup,omitempty"`
}

// RepoCheckout clones URL at Ref (empty = default branch) into Path.
type RepoCheckout struct {
	URL  string `json:"url"`
	Ref  string `json:"ref,omitempty"`
	Path string `json:"path"`
}

// WorkspaceFile is written atomically to Path with permission bits Mode.
type WorkspaceFile struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"` // permission bits, <= 0o7777
	Data []byte `json:"data,omitempty"`
}

// SetupStep runs Argv as User (empty = image default) in Cwd. Name is unique
// within the request; TimeoutSeconds 0 means no timeout.
type SetupStep struct {
	Name           string   `json:"name"`
	Argv           []string `json:"argv"`
	Cwd            string   `json:"cwd,omitempty"`
	User           string   `json:"user,omitempty"`
	TimeoutSeconds int      `json:"timeoutSeconds,omitempty"`
}

// WorkspacePhase is the kind of a WorkspacePrepare step.
type WorkspacePhase string

const (
	WorkspacePhaseClone WorkspacePhase = "clone"
	WorkspacePhaseFile  WorkspacePhase = "file"
	WorkspacePhaseSetup WorkspacePhase = "setup"
)

// WorkspaceStepState is a WorkspacePrepare step's state.
type WorkspaceStepState string

const (
	WorkspaceStepRunning WorkspaceStepState = "running"
	WorkspaceStepDone    WorkspaceStepState = "done"
	WorkspaceStepFailed  WorkspaceStepState = "failed"
)

// WorkspaceState is a WorkspacePrepare outcome.
type WorkspaceState string

const (
	WorkspaceReady  WorkspaceState = "ready"
	WorkspaceFailed WorkspaceState = "failed"
)

// WorkspaceProgress is a WorkspacePrepare stream frame. Step names the item:
// the repo's or file's Path, or the setup step's Name. Message is
// human-readable and never carries Files data or Env values.
type WorkspaceProgress struct {
	Step    string             `json:"step"`
	Phase   WorkspacePhase     `json:"phase"`
	State   WorkspaceStepState `json:"state"`
	Message string             `json:"message,omitempty"`
}

// WorkspaceResult is the final WorkspacePrepare stream frame. On failure
// FailedStep names the step that failed (as in WorkspaceProgress.Step) and
// ExitCode is set when it was a setup step that exited.
type WorkspaceResult struct {
	Revision   string         `json:"revision"`
	State      WorkspaceState `json:"state"`
	FailedStep string         `json:"failedStep,omitempty"`
	ExitCode   *int           `json:"exitCode,omitempty"`
	Message    string         `json:"message,omitempty"`
}

func (Hello) MsgType() MsgType                { return TypeHello }
func (HelloAck) MsgType() MsgType             { return TypeHelloAck }
func (Ack) MsgType() MsgType                  { return TypeAck }
func (Ping) MsgType() MsgType                 { return TypePing }
func (Pong) MsgType() MsgType                 { return TypePong }
func (ReadinessProbe) MsgType() MsgType       { return TypeReadinessProbe }
func (Readiness) MsgType() MsgType            { return TypeReadiness }
func (SetIdentity) MsgType() MsgType          { return TypeSetIdentity }
func (IdentityResult) MsgType() MsgType       { return TypeIdentityResult }
func (Exec) MsgType() MsgType                 { return TypeExec }
func (ProcessStarted) MsgType() MsgType       { return TypeProcessStarted }
func (ProcessAttach) MsgType() MsgType        { return TypeProcessAttach }
func (ProcessSignal) MsgType() MsgType        { return TypeProcessSignal }
func (ProcessStatusRequest) MsgType() MsgType { return TypeProcessStatusQ }
func (ProcessStatus) MsgType() MsgType        { return TypeProcessStatus }
func (Stdout) MsgType() MsgType               { return TypeStdout }
func (Stderr) MsgType() MsgType               { return TypeStderr }
func (Stdin) MsgType() MsgType                { return TypeStdin }
func (StdinClose) MsgType() MsgType           { return TypeStdinClose }
func (ProcessExited) MsgType() MsgType        { return TypeProcessExited }
func (ReadFile) MsgType() MsgType             { return TypeReadFile }
func (FileReadResult) MsgType() MsgType       { return TypeFileReadResult }
func (WriteFile) MsgType() MsgType            { return TypeWriteFile }
func (FileChunk) MsgType() MsgType            { return TypeFileChunk }
func (FileCommit) MsgType() MsgType           { return TypeFileCommit }
func (FileWriteResult) MsgType() MsgType      { return TypeFileWriteDone }
func (Shutdown) MsgType() MsgType             { return TypeShutdown }
func (DialTCP) MsgType() MsgType              { return TypeDialTCP }
func (ProcessLookup) MsgType() MsgType        { return TypeProcessLookup }
func (ProcessListRequest) MsgType() MsgType   { return TypeProcessListQ }
func (ProcessList) MsgType() MsgType          { return TypeProcessList }
func (Listen) MsgType() MsgType               { return TypeListen }
func (ListenStarted) MsgType() MsgType        { return TypeListenStarted }
func (ListenAccepted) MsgType() MsgType       { return TypeListenAccepted }
func (ReverseConnect) MsgType() MsgType       { return TypeReverseConnect }
func (Unlisten) MsgType() MsgType             { return TypeUnlisten }
func (WorkspacePrepare) MsgType() MsgType     { return TypeWorkspacePrepare }
func (WorkspaceProgress) MsgType() MsgType    { return TypeWorkspaceProgress }
func (WorkspaceResult) MsgType() MsgType      { return TypeWorkspaceResult }

// registry maps every known type to a constructor for its body.
var registry = map[MsgType]func() Message{
	TypeHello:             func() Message { return new(Hello) },
	TypeHelloAck:          func() Message { return new(HelloAck) },
	TypeAck:               func() Message { return new(Ack) },
	TypePing:              func() Message { return new(Ping) },
	TypePong:              func() Message { return new(Pong) },
	TypeReadinessProbe:    func() Message { return new(ReadinessProbe) },
	TypeReadiness:         func() Message { return new(Readiness) },
	TypeSetIdentity:       func() Message { return new(SetIdentity) },
	TypeIdentityResult:    func() Message { return new(IdentityResult) },
	TypeExec:              func() Message { return new(Exec) },
	TypeProcessStarted:    func() Message { return new(ProcessStarted) },
	TypeProcessAttach:     func() Message { return new(ProcessAttach) },
	TypeProcessSignal:     func() Message { return new(ProcessSignal) },
	TypeProcessStatusQ:    func() Message { return new(ProcessStatusRequest) },
	TypeProcessStatus:     func() Message { return new(ProcessStatus) },
	TypeStdout:            func() Message { return new(Stdout) },
	TypeStderr:            func() Message { return new(Stderr) },
	TypeStdin:             func() Message { return new(Stdin) },
	TypeStdinClose:        func() Message { return new(StdinClose) },
	TypeProcessExited:     func() Message { return new(ProcessExited) },
	TypeReadFile:          func() Message { return new(ReadFile) },
	TypeFileReadResult:    func() Message { return new(FileReadResult) },
	TypeWriteFile:         func() Message { return new(WriteFile) },
	TypeFileChunk:         func() Message { return new(FileChunk) },
	TypeFileCommit:        func() Message { return new(FileCommit) },
	TypeFileWriteDone:     func() Message { return new(FileWriteResult) },
	TypeShutdown:          func() Message { return new(Shutdown) },
	TypeDialTCP:           func() Message { return new(DialTCP) },
	TypeProcessLookup:     func() Message { return new(ProcessLookup) },
	TypeProcessListQ:      func() Message { return new(ProcessListRequest) },
	TypeProcessList:       func() Message { return new(ProcessList) },
	TypeListen:            func() Message { return new(Listen) },
	TypeListenStarted:     func() Message { return new(ListenStarted) },
	TypeListenAccepted:    func() Message { return new(ListenAccepted) },
	TypeReverseConnect:    func() Message { return new(ReverseConnect) },
	TypeUnlisten:          func() Message { return new(Unlisten) },
	TypeWorkspacePrepare:  func() Message { return new(WorkspacePrepare) },
	TypeWorkspaceProgress: func() Message { return new(WorkspaceProgress) },
	TypeWorkspaceResult:   func() Message { return new(WorkspaceResult) },
}

// Known reports whether t is a registered message type.
func Known(t MsgType) bool { _, ok := registry[t]; return ok }

// Types returns all registered message types, sorted.
func Types() []MsgType {
	ts := make([]MsgType, 0, len(registry))
	for t := range registry {
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	return ts
}

// Encode builds a frame for body with request ID id (0 for none) and
// V=ProtocolVersion (MinProtocolVersion for handshake messages).
func Encode(id uint64, body Message) (*Frame, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, Errorf(CodeMalformedFrame, "encode %s: %v", body.MsgType(), err)
	}
	v := ProtocolVersion
	if isHandshake(body.MsgType()) {
		v = MinProtocolVersion
	}
	return &Frame{V: v, ID: id, Type: body.MsgType(), Body: raw}, nil
}

// EncodeStream builds a stream frame (no request ID) carrying body.
func EncodeStream(stream, seq uint64, body Message) (*Frame, error) {
	f, err := Encode(0, body)
	if err != nil {
		return nil, err
	}
	f.Stream, f.Seq = stream, seq
	return f, nil
}

// ErrorFrame builds an error response to request id. Non-*Error values are
// reported as CodeInternal with a redacted message.
func ErrorFrame(id uint64, err error) *Frame {
	var e *Error
	if !errors.As(err, &e) {
		e = &Error{Code: CodeInternal, Message: "internal error"}
	}
	return &Frame{V: ProtocolVersion, ID: id, Type: TypeError, Error: e}
}

// UnsupportedFrame answers a frame whose type the receiver does not know.
func UnsupportedFrame(f *Frame) *Frame {
	return ErrorFrame(f.ID, Errorf(CodeUnsupported, "unsupported message type %q", f.Type))
}

// Decode unmarshals f's body as T and validates it if T is a Validator.
// A frame carrying Error returns that *Error; a type mismatch returns
// ErrUnexpectedType. Unknown JSON fields are ignored.
func Decode[T Message](f *Frame) (T, error) {
	var v T
	if f.Error != nil {
		return v, f.Error
	}
	if f.Type != v.MsgType() {
		return v, Errorf(CodeUnexpectedType, "got %q, want %q", f.Type, v.MsgType())
	}
	if err := unmarshalBody(f, &v); err != nil {
		return v, err
	}
	if val, ok := any(v).(Validator); ok {
		if err := val.Validate(); err != nil {
			return v, err
		}
	}
	return v, nil
}

// DecodeMessage decodes f into a new value of its registered type (a pointer,
// e.g. *Exec) and validates it. Unknown types return an *Error with
// CodeUnsupported; answer them with UnsupportedFrame.
func DecodeMessage(f *Frame) (Message, error) {
	if f.Error != nil {
		return nil, f.Error
	}
	ctor, ok := registry[f.Type]
	if !ok {
		return nil, Errorf(CodeUnsupported, "unsupported message type %q", f.Type)
	}
	m := ctor()
	if err := unmarshalBody(f, m); err != nil {
		return nil, err
	}
	if val, ok := m.(Validator); ok {
		if err := val.Validate(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func unmarshalBody(f *Frame, dst any) error {
	body := f.Body
	if len(body) == 0 || string(body) == "null" {
		body = json.RawMessage("{}")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return Errorf(CodeMalformedFrame, "decode %s body: %v", f.Type, err)
	}
	return nil
}
