package api

import (
	"time"

	"davit/internal/domain"
)

// CI runners (docs/ci-runners.md): one fresh box per queued GitHub Actions
// job, deleted when the job ends.

// CIJobState is where a CI job is. Clients must tolerate unknown values.
type CIJobState string

const (
	// CIJobQueued waits for a slot (maxConcurrentJobs) or host capacity.
	CIJobQueued CIJobState = "queued"
	// CIJobStarting boots the box and registers the runner.
	CIJobStarting CIJobState = "starting"
	// CIJobRunning runs the runner in the box.
	CIJobRunning CIJobState = "running"
	// CIJobStopping deletes the box.
	CIJobStopping CIJobState = "stopping"
	// CIJobDone is over: the box is gone. Result says how it ended.
	CIJobDone CIJobState = "done"
	// CIJobRefused was refused by the host's policy (error says why);
	// no box was made.
	CIJobRefused CIJobState = "refused"
)

// CIJobResult is how a done CI job ended.
type CIJobResult string

const (
	// CIResultSucceeded: the runner exited 0 (it ran its job; conclusion
	// is the job's own result).
	CIResultSucceeded CIJobResult = "succeeded"
	// CIResultFailed: the runner exited non-zero, or the box or runner
	// could not start (error says why).
	CIResultFailed CIJobResult = "failed"
	// CIResultCancelled: the job ended, or went to another runner, before
	// this runner got any.
	CIResultCancelled CIJobResult = "cancelled"
	// CIResultTimedOut: the runner outlived ci.github.jobTimeout.
	CIResultTimedOut CIJobResult = "timedOut"
	// CIResultRefused: the runner was given a job the policy refuses (a
	// fork's, or another repository's) and its box was deleted.
	CIResultRefused CIJobResult = "refused"
)

// CIJob is one queued GitHub Actions job and the runner box made for it.
type CIJob struct {
	ID string `json:"id"`
	// Repo is the job's repository ("owner/name").
	Repo string `json:"repo"`
	// JobID, RunID, Job and Workflow name the GitHub job that asked for
	// the runner.
	JobID    int64  `json:"jobId"`
	RunID    int64  `json:"runId"`
	Job      string `json:"job"`
	Workflow string `json:"workflow,omitempty"`
	// Labels are the job's runs-on labels; Shape the box size they chose.
	Labels []string    `json:"labels"`
	Shape  string      `json:"shape,omitempty"`
	State  CIJobState  `json:"state"`
	Result CIJobResult `json:"result,omitempty"`
	// Conclusion is GitHub's conclusion of the job the runner ran, once
	// known (success, failure, cancelled, ...).
	Conclusion string `json:"conclusion,omitempty"`
	Error      *Error `json:"error,omitempty"`
	// Runner and RunnerID are the single-use runner registered for it.
	Runner   string `json:"runner,omitempty"`
	RunnerID int64  `json:"runnerId,omitempty"`
	// AssignedJobID is the job GitHub gave the runner, when that is not
	// JobID (any queued job with the same labels may take it).
	AssignedJobID int64 `json:"assignedJobId,omitempty"`
	// VMID is the runner box; ProcessID the runner process, whose output
	// `davit history VMID PROCESSID` prints.
	VMID      domain.VMID `json:"vmId,omitempty"`
	ProcessID string      `json:"processId,omitempty"`
	ExitCode  *int        `json:"exitCode,omitempty"`
	// QueuedAt: the job was seen; BoxReadyAt: its box booted; StartedAt:
	// the runner started; CompletedAt: the runner (or job) ended; EndedAt:
	// the box is gone.
	QueuedAt    time.Time  `json:"queuedAt"`
	BoxReadyAt  *time.Time `json:"boxReadyAt,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
}

// CIStatus is the host's CI runner state (getCIStatus).
type CIStatus struct {
	// Enabled is false when ci.github is off; the lists are then empty.
	Enabled           bool `json:"enabled"`
	MaxConcurrentJobs int  `json:"maxConcurrentJobs"`
	// Queued jobs in FIFO order; Running: starting, running and stopping
	// ones; Recent: done and refused ones, newest first.
	Queued  []CIJob `json:"queued"`
	Running []CIJob `json:"running"`
	Recent  []CIJob `json:"recent"`
}

// CI job event types (data: state, previousState, repo, jobId, runner,
// result, conclusion, error code).
const (
	EventCIJobCreated = "ci.job.created"
	EventCIJobState   = "ci.job.state"
	EventCIJobRefused = "ci.job.refused"
)
