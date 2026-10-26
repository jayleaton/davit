package api

import (
	"time"

	"davit/internal/domain"
)

// Parity v1 contract (docs/research/CONTRACT-v1.md). Every operation here is
// in openapi.yaml with x-davit-implemented: false and answers
// not_implemented until its feature (HostCapabilities.features) is built.

// Wire types that are exactly the domain shape.
type (
	Feature             = domain.Feature
	HarnessCapabilities = domain.HarnessCapabilities
	Environment         = domain.Environment
	EnvironmentRevision = domain.EnvironmentRevision
	EnvManifest         = domain.EnvManifest
	RepoSpec            = domain.RepoSpec
	SecretFileRef       = domain.SecretFileRef
	SetupAction         = domain.SetupAction
	EnvironmentRef      = domain.EnvironmentRef
	EnvironmentPin      = domain.EnvironmentPin
	Workspace           = domain.Workspace
	SetupError          = domain.SetupError
	CredentialProfile   = domain.CredentialProfile
	Credential          = domain.Credential
	CredentialGrant     = domain.CredentialGrant
)

// G3 environments.

// CreateEnvironmentRequest is the body of createEnvironment (revision 1).
type CreateEnvironmentRequest struct {
	Name     string      `json:"name"`
	Manifest EnvManifest `json:"manifest"`
}

// CreateEnvironmentRevisionRequest is the body of createEnvironmentRevision:
// the complete next manifest (revisions are immutable, never patched).
type CreateEnvironmentRevisionRequest struct {
	Manifest EnvManifest `json:"manifest"`
}

// EnvironmentList is a page of environments.
type EnvironmentList struct {
	Items      []Environment `json:"items"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// EnvironmentRevisionList is a page of revisions, oldest first.
type EnvironmentRevisionList struct {
	Items      []EnvironmentRevision `json:"items"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

// G4 credentials.

// CreateCredentialProfileRequest is the body of createCredentialProfile.
type CreateCredentialProfileRequest struct {
	Name    string `json:"name"`
	Default bool   `json:"default,omitempty"`
}

// PutCredentialRequest is the body of putCredential. Secret is write-only:
// stored on the host, never returned and never sent to a guest.
type PutCredentialRequest struct {
	Kind     domain.CredentialKind `json:"kind"`
	Provider string                `json:"provider,omitempty"`
	BaseURL  string                `json:"baseUrl,omitempty"`
	EnvVar   string                `json:"envVar"`
	Secret   string                `json:"secret"`
}

// CredentialProfileList is a page of profiles.
type CredentialProfileList struct {
	Items      []CredentialProfile `json:"items"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

// CreateCredentialGrantRequest is the body of createCredentialGrant.
type CreateCredentialGrantRequest struct {
	Profile    string `json:"profile"` // ID or name
	Credential string `json:"credential"`
}

// CredentialGrantList is a VM's grants, active and revoked.
type CredentialGrantList struct {
	Items      []CredentialGrant `json:"items"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

// G5 prompt loop.

// PromptAttachment is a file given to a turn: Path names a guest file
// (upload it first with writeFile) or Data carries it inline (base64,
// bounded by the request size limit). Exactly one is set.
type PromptAttachment struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Path      string `json:"path,omitempty"`
	Data      string `json:"data,omitempty"` // base64 (standard encoding)
}

// PromptOptions are harness controls; harnesses[] says which apply.
type PromptOptions struct {
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	Fast            bool   `json:"fast,omitempty"`
}

// PromptFeedItem is one event of the VM-wide prompt feed.
type PromptFeedItem struct {
	// Cursor orders the feed across prompts; pass the last one as `after`.
	Cursor string      `json:"cursor"`
	Event  PromptEvent `json:"event"`
}

// PromptFeed is a page of listVMPromptEvents.
type PromptFeed struct {
	Items []PromptFeedItem `json:"items"`
	// NextCursor is always set, also on an empty page: pass it as after.
	NextCursor string `json:"nextCursor"`
}

// InterruptRequest is the body of interruptPrompts: one conversation's
// active turn, or every active turn on the VM with all.
type InterruptRequest struct {
	ConversationID string `json:"conversationId,omitempty"`
	All            bool   `json:"all,omitempty"`
}

// InterruptResult lists the turns that were cancelled.
type InterruptResult struct {
	Cancelled []Prompt `json:"cancelled"`
}

// SteerRequest is the body of steerPrompt.
type SteerRequest struct {
	Text string `json:"text"`
}

// SteerResult says how the message reached the harness: native (the
// running turn took it) or restart (that turn was cancelled and Prompt is
// the new turn carrying the message).
type SteerResult struct {
	Delivery domain.SteerSupport `json:"delivery"`
	Prompt   Prompt              `json:"prompt"`
}

// G6 run and push.

// RunState is a run's state.
type RunState string

const (
	RunCreating  RunState = "creating"  // VM being created and prepared
	RunWaiting   RunState = "waiting"   // created with hold: waiting for startRun
	RunRunning   RunState = "running"   // command running
	RunSucceeded RunState = "succeeded" // exit code 0
	RunFailed    RunState = "failed"    // non-zero exit, timeout or setup failure; see error
	RunCancelled RunState = "cancelled"
)

// RunRequest is the body of createRun: create a VM, run Argv in it, keep
// the exit code and delete the VM (unless Keep). Image and SnapshotID are
// exclusive. Hold stops in state waiting so the client can push files
// (pushTree) before startRun.
type RunRequest struct {
	Image          string            `json:"image,omitempty"`
	SnapshotID     domain.SnapshotID `json:"snapshotId,omitempty"`
	Type           string            `json:"type,omitempty"`
	Shape          *ShapeOverride    `json:"shape,omitempty"`
	Environment    *EnvironmentRef   `json:"environment,omitempty"`
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int64             `json:"timeoutSeconds,omitempty"`
	Keep           bool              `json:"keep,omitempty"`
	Hold           bool              `json:"hold,omitempty"`
	// OutputGraceSeconds is how long the VM outlives the command so a
	// client can still read its output (0: none, at most MaxRunOutputGrace
	// seconds); omitted means the host default (daemon config
	// runs.outputGrace).
	OutputGraceSeconds *int64 `json:"outputGraceSeconds,omitempty"`
}

// MaxRunOutputGrace bounds RunRequest.OutputGraceSeconds.
const MaxRunOutputGrace = 600

// Run is one createRun job.
type Run struct {
	ID        string      `json:"id"`
	State     RunState    `json:"state"`
	VMID      domain.VMID `json:"vmId,omitempty"`
	ProcessID string      `json:"processId,omitempty"`
	ExitCode  *int        `json:"exitCode,omitempty"`
	// Signal names the signal that killed the command (outcome failed).
	Signal string `json:"signal,omitempty"`
	// Outcome is the run's result as soon as it is known; State becomes
	// Outcome once cleanup ends (output grace, VM delete). DecidedAt is
	// when the outcome was decided.
	Outcome   RunState          `json:"outcome,omitempty"`
	DecidedAt *time.Time        `json:"decidedAt,omitempty"`
	Keep      bool              `json:"keep"`
	Error     *Error            `json:"error,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	EndedAt   *time.Time        `json:"endedAt,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// RunList is a page of the caller's runs, oldest first.
type RunList struct {
	Items      []Run  `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// Push modes (pushTree mode=…).
const (
	PushMerge  = "merge"  // add and replace files; keep others
	PushMirror = "mirror" // also delete files under path absent from the tree
)

// PushResult is the result of pushTree. The client chose the files (git
// tracked plus untracked-not-ignored, submodules included).
type PushResult struct {
	Path       string `json:"path"`
	Files      int    `json:"files"`
	Bytes      int64  `json:"bytes"`
	Removed    int    `json:"removed"`
	SHA256     string `json:"sha256"`
	Generation int64  `json:"generation"`
}

// G7 history.

// HistoryEntry is a managed job: the process record plus what the host
// retained of its output, readable while the VM is stopped and after a
// daemon restart.
type HistoryEntry struct {
	Process Process `json:"process"`
	// Handle is the durable job handle the guest agent knows the process
	// by (guestproto feature "jobs"), so a host that lost its records can
	// find it again instead of marking it lost.
	Handle        string `json:"handle"`
	StdoutBytes   int64  `json:"stdoutBytes"`
	StderrBytes   int64  `json:"stderrBytes"`
	OutputDropped bool   `json:"outputDropped"` // retention limit cut the oldest output
	// OutputNotRetained says why the host kept none (or not all) of the
	// output other than the retention limit; empty when it kept it.
	OutputNotRetained OutputNotRetainedReason `json:"outputNotRetained,omitempty"`
}

// OutputNotRetainedReason is why a job's output was not kept on the host.
type OutputNotRetainedReason string

// OutputNotRetainedPolicy: the VM's privacy policy (noOutputRetention) was
// on while the job ran, or was turned on later and purged its output.
const OutputNotRetainedPolicy OutputNotRetainedReason = "policy"

// HistoryList is a page of managed jobs across generations, newest first.
type HistoryList struct {
	Items      []HistoryEntry `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

// ProcessOutput is a page of retained output (stdout/stderr messages, the
// same shape as the attach stream) after a cursor.
type ProcessOutput struct {
	Items      []ProcessStreamMessage `json:"items"`
	NextCursor string                 `json:"nextCursor,omitempty"`
	// Complete: the process ended and every retained chunk was returned.
	Complete bool `json:"complete"`
}

// GuestProcess is one process of a whole-guest sample (ps).
type GuestProcess struct {
	PID           int       `json:"pid"`
	PPID          int       `json:"ppid"`
	User          string    `json:"user"`
	Args          []string  `json:"args"`
	State         string    `json:"state"`
	CPUTimeMillis int64     `json:"cpuTimeMillis"`
	RSSBytes      int64     `json:"rssBytes"`
	StartedAt     time.Time `json:"startedAt"`
	// ProcessID is set when it is a davit-managed process.
	ProcessID string `json:"processId,omitempty"`
}

// GuestProcessList is a whole-guest process sample. For a stopped VM it is
// the last sample taken (Stale).
type GuestProcessList struct {
	SampledAt time.Time      `json:"sampledAt"`
	Stale     bool           `json:"stale"`
	Truncated bool           `json:"truncated"`
	Items     []GuestProcess `json:"items"`
}

// G12 snapshot files.

// SnapshotEntry is one entry of a snapshot's filesystem.
type SnapshotEntry struct {
	Path       string    `json:"path"`
	Type       string    `json:"type"` // file | dir | symlink | other
	SizeBytes  int64     `json:"sizeBytes"`
	Mode       string    `json:"mode"`
	ModifiedAt time.Time `json:"modifiedAt"`
	LinkTarget string    `json:"linkTarget,omitempty"`
}

// SnapshotTree is a page of a directory listing in a snapshot.
type SnapshotTree struct {
	Path       string          `json:"path"`
	Items      []SnapshotEntry `json:"items"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// Snapshot export formats (exportSnapshot format=…).
const (
	ExportTar = "tar" // the filesystem as a tar stream
	ExportRaw = "raw" // the raw disk image (sparse-aware copy is the client's job)
)
