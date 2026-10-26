// Package api holds the public HTTP contract: the OpenAPI document and thin
// hand-written Go wire types that mirror it. openapi_test.go keeps the two in
// sync by reflection, so drift fails the build.
package api

import (
	_ "embed"
	"time"

	"davit/internal/domain"
)

// OpenAPI is the v1 OpenAPI document.
//
//go:embed openapi.yaml
var OpenAPI []byte

// BasePath prefixes every route.
const BasePath = "/api/v1"

// Headers.
const (
	HeaderIdempotencyKey = "Idempotency-Key"
	HeaderIfMatch        = "If-Match"
	HeaderRequestID      = "X-Request-Id"
	HeaderRetryAfter     = "Retry-After"
)

// List limits (SPEC §4).
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// Wire types that are exactly the domain shape.
type (
	VM               = domain.VM
	Shape            = domain.Shape
	Snapshot         = domain.Snapshot
	Operation        = domain.Operation
	OperationEvent   = domain.OperationEvent
	HostCapabilities = domain.HostCapabilities
	Readiness        = domain.Readiness
	Endpoint         = domain.Endpoint
	StorageInfo      = domain.StorageInfo
	OpResult         = domain.OpResult
	Image            = domain.Image
	Profile          = domain.Profile
	Egress           = domain.Egress
	PrivacyPolicy    = domain.PrivacyPolicy
	HostCapacity     = domain.HostCapacity
	CapacityWaiter   = domain.CapacityWaiter
	Limits           = domain.Limits
)

// ShapeOverride is a partial shape; zero fields keep the default/source value.
type ShapeOverride struct {
	CPUs        int   `json:"cpus,omitempty"`
	MemoryBytes int64 `json:"memoryBytes,omitempty"`
	DiskBytes   int64 `json:"diskBytes,omitempty"`
}

// CreateVMRequest is the body of createVM. Image and SnapshotID are exclusive.
type CreateVMRequest struct {
	Name          string            `json:"name,omitempty"`
	Type          string            `json:"type,omitempty"`
	Image         string            `json:"image,omitempty"`
	SnapshotID    domain.SnapshotID `json:"snapshotId,omitempty"`
	Shape         *ShapeOverride    `json:"shape,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	TTLSeconds    *int64            `json:"ttlSeconds,omitempty"`
	AutoStop      *bool             `json:"autoStop,omitempty"`
	RequireGolden bool              `json:"requireGolden,omitempty"`
	// Egress is the VM's egress policy; omitted means the host default.
	Egress *Egress `json:"egress,omitempty"`
	// Environment prepares the workspace from an environment revision
	// (feature workspacePrepare).
	Environment *EnvironmentRef `json:"environment,omitempty"`
	// Privacy is the VM's ephemeral/privacy policy (feature privacyPolicy);
	// omitted means the usual retention. Forks inherit their source's.
	Privacy *PrivacyPolicy `json:"privacy,omitempty"`
	// Labels tag the VM (listVMs filters on them); forks do not inherit them.
	Labels map[string]string `json:"labels,omitempty"`
	// IdleTimeoutSeconds stops the VM after this long with no exec, process
	// stream or tunnel activity (0 or omitted: never).
	IdleTimeoutSeconds int64 `json:"idleTimeoutSeconds,omitempty"`
}

// ForkVMRequest is the body of forkVM.
type ForkVMRequest struct {
	SnapshotID domain.SnapshotID `json:"snapshotId,omitempty"`
	Name       string            `json:"name,omitempty"`
	Type       string            `json:"type,omitempty"`
	Shape      *ShapeOverride    `json:"shape,omitempty"`
	TTLSeconds *int64            `json:"ttlSeconds,omitempty"`
	AutoStop   *bool             `json:"autoStop,omitempty"`
	// Egress is the fork's egress policy; omitted inherits the source VM's.
	Egress *Egress `json:"egress,omitempty"`
	// Live forks the running VM itself (RAM, processes and disk at one
	// instant) instead of a committed disk snapshot. Needs the runtime's
	// liveFork capability; the source pauses briefly.
	Live bool `json:"live,omitempty"`
	// Environment re-prepares the fork's workspace from a revision
	// (feature workspacePrepare); omitted keeps the source's pin.
	Environment *EnvironmentRef `json:"environment,omitempty"`
}

// UpdateVMRequest is the body of updateVM (PATCH): every field is optional
// and only the fields present change.
type UpdateVMRequest struct {
	Name *string `json:"name,omitempty"`
	// TTLSeconds sets the VM's TTL and, if it is ready or running, restarts
	// the auto-stop countdown from now (`davit extend`).
	TTLSeconds *int64 `json:"ttlSeconds,omitempty"`
	AutoStop   *bool  `json:"autoStop,omitempty"`
	// Egress replaces the VM's egress policy (live on a running VM). Refused
	// while a lifecycle operation runs.
	Egress *Egress `json:"egress,omitempty"`
	// Privacy replaces the VM's privacy policy (feature privacyPolicy).
	// Refused while a lifecycle operation runs.
	Privacy *PrivacyPolicy `json:"privacy,omitempty"`
	// IdleTimeoutSeconds replaces the VM's idle timeout (0: never).
	IdleTimeoutSeconds *int64 `json:"idleTimeoutSeconds,omitempty"`
}

// StopVMRequest is the body of stopVM.
type StopVMRequest struct {
	Force bool `json:"force,omitempty"`
}

// ResumeVMRequest is the body of resumeVM.
type ResumeVMRequest struct {
	TTLSeconds *int64 `json:"ttlSeconds,omitempty"`
	AutoStop   *bool  `json:"autoStop,omitempty"`
	// Type and Shape resize the VM for this and later boots (feature
	// resize): Shape fields override the profile Type names, which
	// overrides the VM's current shape. The disk can only grow.
	Type  string         `json:"type,omitempty"`
	Shape *ShapeOverride `json:"shape,omitempty"`
	// Environment moves the VM to another revision and re-runs preparation
	// on resume (feature workspacePrepare).
	Environment *EnvironmentRef `json:"environment,omitempty"`
}

// SnapshotVMRequest is the body of snapshotVM.
type SnapshotVMRequest struct {
	Name string `json:"name"`
}

// OperationAccepted is the 202 body of every lifecycle mutation.
type OperationAccepted struct {
	Operation Operation `json:"operation"`
	VM        *VM       `json:"vm,omitempty"`
}

// VMList is a page of VMs.
type VMList struct {
	Items      []VM   `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// SnapshotList is a page of snapshots.
type SnapshotList struct {
	Items      []Snapshot `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// OperationEventList is a page of operation events after a cursor.
type OperationEventList struct {
	Operation Operation        `json:"operation"`
	Items     []OperationEvent `json:"items"`
	// NextCursor is the last returned seq, as a string; pass it as `after`.
	NextCursor string `json:"nextCursor,omitempty"`
}

// Error is the error body field.
type Error struct {
	Code        domain.Code        `json:"code"`
	Message     string             `json:"message"`
	Retryable   bool               `json:"retryable"`
	Details     map[string]any     `json:"details,omitempty"`
	RequestID   string             `json:"requestId"`
	OperationID domain.OperationID `json:"operationId,omitempty"`
}

// ErrorResponse is every non-2xx JSON body.
type ErrorResponse struct {
	Error Error `json:"error"`
}

// ImageList is a page of images.
type ImageList struct {
	Items      []Image `json:"items"`
	NextCursor string  `json:"nextCursor,omitempty"`
}

// ExecRequest is the body of execProcess. Argv runs directly; a shell only
// when argv invokes one.
type ExecRequest struct {
	Argv            []string          `json:"argv"`
	Cwd             string            `json:"cwd,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	User            string            `json:"user,omitempty"`
	DeadlineSeconds int64             `json:"deadlineSeconds,omitempty"`
	Detached        bool              `json:"detached,omitempty"`
	Stdin           bool              `json:"stdin,omitempty"`
	TTY             bool              `json:"tty,omitempty"`
}

// ProcessStatus is a process's lifecycle state. Clients must tolerate
// unknown values.
type ProcessStatus string

const (
	ProcessStarting ProcessStatus = "starting"
	ProcessRunning  ProcessStatus = "running"
	ProcessExited   ProcessStatus = "exited"
	ProcessKilled   ProcessStatus = "killed"
	ProcessUnknown  ProcessStatus = "unknown"
	ProcessLost     ProcessStatus = "lost"
)

// Process is a davit-managed guest process.
type Process struct {
	ID         string        `json:"id"`
	VMID       domain.VMID   `json:"vmId"`
	Generation int64         `json:"generation"`
	Status     ProcessStatus `json:"status"`
	Argv       []string      `json:"argv"`
	Detached   bool          `json:"detached"`
	ExitCode   *int          `json:"exitCode,omitempty"`
	Signal     string        `json:"signal,omitempty"`
	// TimedOut is true when the process was killed by its deadline.
	TimedOut bool `json:"timedOut,omitempty"`
	// OutputCursor is the seq of the last output message produced.
	OutputCursor int64      `json:"outputCursor"`
	CreatedAt    time.Time  `json:"createdAt"`
	ExitedAt     *time.Time `json:"exitedAt,omitempty"`
}

// ProcessList is a page of processes.
type ProcessList struct {
	Items      []Process `json:"items"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

// Process stream message types (attachProcess WebSocket, one JSON text
// message each).
const (
	StreamStdout     = "stdout"      // server: output chunk
	StreamStderr     = "stderr"      // server: output chunk
	StreamExit       = "exit"        // server: final message, process set
	StreamError      = "error"       // server: stream failed, error set
	StreamStdin      = "stdin"       // client: input chunk
	StreamStdinClose = "stdin.close" // client: end of input
	StreamSignal     = "signal"      // client: signal the process group
)

// ProcessStreamMessage is one attachProcess WebSocket message.
type ProcessStreamMessage struct {
	Type string `json:"type"`
	// Seq orders output; reconnect with outputCursor = last seq handled.
	Seq int64 `json:"seq,omitempty"`
	// Data is base64 (standard encoding) bytes for stdout/stderr/stdin.
	Data    string   `json:"data,omitempty"`
	Signal  string   `json:"signal,omitempty"`
	Process *Process `json:"process,omitempty"`
	Error   *Error   `json:"error,omitempty"`
}

// File transfer (readFile/writeFile).
const (
	// HeaderFileMode carries the octal permission bits of a read file or
	// archive root, e.g. "0644".
	HeaderFileMode = "X-Davit-File-Mode"
	// HeaderContentSHA256 is the hex SHA-256 of a snapshot file read (a header)
	// or of an archive or export stream (a trailer).
	HeaderContentSHA256 = "X-Davit-Content-Sha256"
	// ArchiveTar selects tar for directory transfer (archive=tar).
	ArchiveTar = "tar"
	// ContentTypeTar is the media type of archive transfers.
	ContentTypeTar = "application/x-tar"
)

// FileInfo is the result of writeFile.
type FileInfo struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	// Mode is the octal permission bits now on the file, e.g. "0644"; for
	// archives, the mode requested for a newly created directory.
	Mode string `json:"mode"`
	// SHA256 is the hex digest of the uploaded bytes (the tar stream for
	// archives), verified by the agent before it applied them.
	SHA256     string `json:"sha256,omitempty"`
	Generation int64  `json:"generation"`
	Archive    bool   `json:"archive,omitempty"`
}

// PromptRequest is the body of createPrompt: one turn of a harness
// conversation in the VM.
type PromptRequest struct {
	Text string `json:"text"`
	// Harness is claude (default), codex, opencode or pi.
	Harness string `json:"harness,omitempty"`
	// ConversationID continues that conversation; New starts one. With
	// neither, the most recently active conversation of the same harness
	// continues, else a new one starts.
	ConversationID string `json:"conversationId,omitempty"`
	New            bool   `json:"new,omitempty"`
	Model          string `json:"model,omitempty"`
	// Cwd is the guest working directory of a new conversation.
	Cwd string `json:"cwd,omitempty"`
	// Env is passed to the harness process (e.g. an API key the caller
	// forwarded). Values enter the guest and are never stored.
	Env map[string]string `json:"env,omitempty"`
	// Secrets names daemon-configured secrets to pass as environment
	// variables of the same name.
	Secrets []string `json:"secrets,omitempty"`
	// Attachments, Options and TurnTimeoutSeconds: features
	// promptAttachments, promptOptions and promptTurnLease; harnesses[] in
	// capabilities says what each harness takes.
	Attachments []PromptAttachment `json:"attachments,omitempty"`
	Options     *PromptOptions     `json:"options,omitempty"`
	// TurnTimeoutSeconds bounds the turn; while it runs, the turn holds the
	// VM's auto-stop (active-turn lease) until it ends or this elapses.
	TurnTimeoutSeconds int64 `json:"turnTimeoutSeconds,omitempty"`
}

// PromptState is a turn's state.
type PromptState string

const (
	PromptStarting  PromptState = "starting"
	PromptRunning   PromptState = "running"
	PromptSucceeded PromptState = "succeeded"
	PromptFailed    PromptState = "failed"
	PromptCancelled PromptState = "cancelled"
)

// PromptUsage is token accounting reported by the harness.
type PromptUsage struct {
	InputTokens       int64   `json:"inputTokens"`
	OutputTokens      int64   `json:"outputTokens"`
	CachedInputTokens int64   `json:"cachedInputTokens,omitempty"`
	CostUSD           float64 `json:"costUsd,omitempty"`
}

// PromptError is why a turn failed. Codes: harness_missing,
// harness_failed, harness_exit, cancelled, lost, turn_timeout, attachment.
type PromptError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Prompt is one turn.
type Prompt struct {
	ID             string       `json:"id"`
	ConversationID string       `json:"conversationId"`
	VMID           domain.VMID  `json:"vmId"`
	Harness        string       `json:"harness"`
	Model          string       `json:"model,omitempty"`
	State          PromptState  `json:"state"`
	ProcessID      string       `json:"processId,omitempty"`
	EventCursor    int64        `json:"eventCursor"`
	Usage          *PromptUsage `json:"usage,omitempty"`
	Error          *PromptError `json:"error,omitempty"`
	CreatedAt      time.Time    `json:"createdAt"`
	FinishedAt     *time.Time   `json:"finishedAt,omitempty"`
	// TurnDeadlineAt is when the active-turn lease ends (promptTurnLease).
	TurnDeadlineAt *time.Time `json:"turnDeadlineAt,omitempty"`
}

// Conversation is a sequence of turns sharing a harness-native session.
type Conversation struct {
	ID             string      `json:"id"`
	VMID           domain.VMID `json:"vmId"`
	Harness        string      `json:"harness"`
	Model          string      `json:"model,omitempty"`
	Cwd            string      `json:"cwd,omitempty"`
	SessionID      string      `json:"sessionId,omitempty"`
	ActivePromptID string      `json:"activePromptId,omitempty"`
	Turns          int         `json:"turns"`
	CreatedAt      time.Time   `json:"createdAt"`
	UpdatedAt      time.Time   `json:"updatedAt"`
}

// ConversationList is a page of conversations.
type ConversationList struct {
	Items      []Conversation `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

// Prompt event types.
const (
	PromptEventConversationStarted = "conversation.started"
	PromptEventMessageDelta        = "message.delta"
	PromptEventToolCall            = "tool.call"
	PromptEventToolResult          = "tool.result"
	PromptEventTurnCompleted       = "turn.completed"
	PromptEventRaw                 = "raw"
)

// PromptEvent is one normalized harness event; Raw is the harness line it
// came from.
type PromptEvent struct {
	Seq            int64        `json:"seq"`
	Time           time.Time    `json:"time"`
	PromptID       string       `json:"promptId"`
	ConversationID string       `json:"conversationId"`
	Type           string       `json:"type"`
	SessionID      string       `json:"sessionId,omitempty"`
	Role           string       `json:"role,omitempty"`
	Text           string       `json:"text,omitempty"`
	ToolCallID     string       `json:"toolCallId,omitempty"`
	ToolName       string       `json:"toolName,omitempty"`
	Input          any          `json:"input,omitempty"`
	Output         string       `json:"output,omitempty"`
	IsError        bool         `json:"isError,omitempty"`
	Status         string       `json:"status,omitempty"`
	Usage          *PromptUsage `json:"usage,omitempty"`
	Error          *PromptError `json:"error,omitempty"`
	Raw            any          `json:"raw,omitempty"`
}

// PromptEventList is a page of events after a cursor.
type PromptEventList struct {
	Prompt Prompt        `json:"prompt"`
	Items  []PromptEvent `json:"items"`
	// NextCursor is the last returned seq, as a string; pass it as `after`.
	NextCursor string `json:"nextCursor,omitempty"`
}

// OpenTunnelRequest is the body of openTunnel.
type OpenTunnelRequest struct {
	Port int `json:"port"`
}

// Tunnel is a TCP connection to a port on a VM's guest loopback, opened by
// openTunnel and attached once through attachTunnel before ExpiresAt.
type Tunnel struct {
	ID         string      `json:"id"`
	VMID       domain.VMID `json:"vmId"`
	Generation int64       `json:"generation"`
	Port       int         `json:"port"`
	CreatedAt  time.Time   `json:"createdAt"`
	ExpiresAt  time.Time   `json:"expiresAt"`
}

// IssueSSHCredentialRequest is the body of issueSSHCredential.
type IssueSSHCredentialRequest struct {
	// PublicKey is the caller's temporary key, authorized_keys format.
	PublicKey  string `json:"publicKey"`
	GuestUser  string `json:"guestUser,omitempty"`
	TTLSeconds int64  `json:"ttlSeconds,omitempty"`
}

// SSHCredential is a short-lived user certificate for one VM generation and
// the guest host keys to pin.
type SSHCredential struct {
	// Certificate is an OpenSSH user certificate (authorized_keys format).
	Certificate string `json:"certificate"`
	GuestUser   string `json:"guestUser"`
	Generation  int64  `json:"generation"`
	// HostKeys are guest host public keys (authorized_keys format) whose
	// fingerprints the restore barrier reported.
	HostKeys  []string  `json:"hostKeys"`
	SSHTarget string    `json:"sshTarget"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Token actions, from least to most: exec implies view, admin implies both.
const (
	TokenView  = "view"
	TokenExec  = "exec"
	TokenAdmin = "admin"
)

// TokenScope grants actions on one VM (VMID set) or on all of the issuer's
// VMs.
type TokenScope struct {
	VMID    domain.VMID `json:"vmId,omitempty"`
	Actions []string    `json:"actions"`
}

// CreateTokenRequest is the body of createToken.
type CreateTokenRequest struct {
	Name       string       `json:"name"`
	Scopes     []TokenScope `json:"scopes"`
	TTLSeconds int64        `json:"ttlSeconds"`
}

// Token is a service token's metadata; the secret is never returned again.
type Token struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Scopes     []TokenScope `json:"scopes"`
	CreatedAt  time.Time    `json:"createdAt"`
	ExpiresAt  time.Time    `json:"expiresAt"`
	LastUsedAt *time.Time   `json:"lastUsedAt,omitempty"`
	// UseCount is how many requests presented the token (feature
	// tokenUsage). It is written in batches with lastUsedAt, so after a
	// daemon crash the last minute of uses may be missing.
	UseCount int64 `json:"useCount"`
}

// CreatedToken is a new token with its secret, returned only by createToken.
type CreatedToken struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Scopes     []TokenScope `json:"scopes"`
	CreatedAt  time.Time    `json:"createdAt"`
	ExpiresAt  time.Time    `json:"expiresAt"`
	LastUsedAt *time.Time   `json:"lastUsedAt,omitempty"`
	// Secret is the bearer token: Authorization: Bearer <secret>.
	Secret string `json:"secret"`
}

// TokenList is a page of tokens.
type TokenList struct {
	Items      []Token `json:"items"`
	NextCursor string  `json:"nextCursor,omitempty"`
}

// RecoverVMRequest is the (optional) body of recoverVM.
type RecoverVMRequest struct{}

// MaxBatchCount bounds createVMs.
const MaxBatchCount = 100

// CreateVMsRequest is the body of createVMs: Count VMs from one template.
// A template name N names the items N-1 ... N-Count; without one they are
// unnamed.
type CreateVMsRequest struct {
	Count    int             `json:"count"`
	Template CreateVMRequest `json:"template"`
}

// ReclaimVMsRequest is the body of reclaimVMs: stop the caller's VMs that
// have been idle at least IdleSeconds (feature capacityReclaim).
type ReclaimVMsRequest struct {
	IdleSeconds int64 `json:"idleSeconds"`
	// Labels limits it to VMs carrying all of these labels.
	Labels map[string]string `json:"labels,omitempty"`
	// DryRun lists the VMs without stopping them.
	DryRun bool `json:"dryRun,omitempty"`
}

// ReclaimVMsResult lists the VMs reclaimVMs stopped (or would stop), oldest
// activity first.
type ReclaimVMsResult struct {
	Items []ReclaimedVM `json:"items"`
}

// ReclaimedVM is one idle VM of reclaimVMs.
type ReclaimedVM struct {
	VMID        domain.VMID `json:"vmId"`
	Name        string      `json:"name"`
	IdleSeconds int64       `json:"idleSeconds"`
	MemoryBytes int64       `json:"memoryBytes"`
	// OperationID is the stop (absent with dryRun).
	OperationID domain.OperationID `json:"operationId,omitempty"`
}

// CreateVMsResult reports every item of a batch, in order.
type CreateVMsResult struct {
	Items []BatchItem `json:"items"`
}

// BatchItem is one item of a batch: its accepted create, or why it was
// refused. Each item is idempotent on its own (the batch key plus its
// index), so a replayed batch returns the same operations.
type BatchItem struct {
	Index    int                `json:"index"`
	Name     string             `json:"name,omitempty"`
	Accepted *OperationAccepted `json:"accepted,omitempty"`
	Error    *Error             `json:"error,omitempty"`
}

// RotateTokenRequest is the body of rotateToken: a new secret with the same
// name and scopes; the old one keeps working for OverlapSeconds (0: revoked
// at once).
type RotateTokenRequest struct {
	OverlapSeconds int64 `json:"overlapSeconds,omitempty"`
	// TTLSeconds is the new token's lifetime; omitted keeps the old one's.
	TTLSeconds *int64 `json:"ttlSeconds,omitempty"`
}

// Auth methods of Me.Auth.
const (
	AuthLocal   = "local"   // the owner over the Unix socket (peer credentials)
	AuthTailnet = "tailnet" // an owner identity on the tailnet gateway
	AuthToken   = "token"   // a service token on the tailnet gateway
	AuthMember  = "member"  // a tailnet member (G13) using what is shared with it
)

// Me is who the caller is to this daemon (whoami). It never carries a
// secret.
type Me struct {
	Principal string `json:"principal"`
	Auth      string `json:"auth"`
	// Identity describes the transport identity, e.g. "uid 1000" or a
	// tailnet login and node.
	Identity string `json:"identity,omitempty"`
	// Token is the presented service token (auth=token), without its secret.
	Token *Token `json:"token,omitempty"`
	// Member is the caller's member access (auth=member).
	Member *MeMember `json:"member,omitempty"`
	// Host and Version describe the daemon that answered.
	Host    string `json:"host"`
	Version string `json:"version"`
}

// MeMember is what a member (G13) may use: its shared VMs (scopes, with
// the member role as a token action: viewer=view, exec, admin), snapshots
// and environments (ID -> action).
type MeMember struct {
	ID           string            `json:"id"`
	Login        string            `json:"login"`
	Scopes       []TokenScope      `json:"scopes"`
	Snapshots    map[string]string `json:"snapshots,omitempty"`
	Environments map[string]string `json:"environments,omitempty"`
}
