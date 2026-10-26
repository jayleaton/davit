package domain

import (
	"slices"
	"time"
)

// VMState is the VM lifecycle state (SPEC §2). Clients must tolerate unknown
// values: later suspend states are added behind capabilities.
type VMState string

const (
	StateProvisioning VMState = "provisioning"
	StateReady        VMState = "ready"
	StateRunning      VMState = "running"
	StateStopping     VMState = "stopping"
	StateStopped      VMState = "stopped"
	StateError        VMState = "error"
	StateDeleting     VMState = "deleting"
	StateDeleted      VMState = "deleted"
)

// AllStates lists every v0 state, in lifecycle order.
var AllStates = []VMState{
	StateProvisioning, StateReady, StateRunning, StateStopping,
	StateStopped, StateError, StateDeleting, StateDeleted,
}

// DefaultListStates is what `list` shows without --all.
var DefaultListStates = []VMState{StateProvisioning, StateReady, StateRunning}

// transitions is the complete v0 state machine. Deletion is reachable from
// every non-deleted state; deleting may be retried; deleted is terminal.
var transitions = map[VMState][]VMState{
	StateProvisioning: {StateReady, StateError, StateDeleting},
	// ready/running -> provisioning is recover's power cycle (G9).
	StateReady:    {StateRunning, StateStopping, StateError, StateDeleting, StateProvisioning},
	StateRunning:  {StateReady, StateStopping, StateError, StateDeleting, StateProvisioning},
	StateStopping: {StateStopped, StateError, StateDeleting},
	StateStopped:  {StateProvisioning, StateDeleting},
	StateError:    {StateProvisioning, StateDeleting},
	StateDeleting: {StateDeleting, StateDeleted},
	StateDeleted:  nil,
}

// Valid reports whether s is a known v0 state.
func (s VMState) Valid() bool { _, ok := transitions[s]; return ok }

// Terminal reports whether no transition leaves s.
func (s VMState) Terminal() bool { return s == StateDeleted }

// Live reports whether a runtime instance may exist in state s.
func (s VMState) Live() bool {
	return s == StateProvisioning || s == StateReady || s == StateRunning || s == StateStopping
}

// CanTransition reports whether from -> to is allowed.
func CanTransition(from, to VMState) bool { return slices.Contains(transitions[from], to) }

// Transition moves vm to state `to`, bumping StateRevision. It is the only
// way lifecycle code changes VM.State.
func (vm *VM) Transition(to VMState, now time.Time) error {
	if !CanTransition(vm.State, to) {
		return Errorf(CodeInvalidState, "vm %s cannot go from %s to %s", vm.ID, vm.State, to).
			WithDetail("state", vm.State)
	}
	vm.State = to
	vm.StateRevision++
	vm.UpdatedAt = now
	if to == StateDeleted {
		vm.DeletedAt = &now
	}
	return nil
}

// CheckInvariants verifies the cross-field rules the state machine relies on.
// State stores call it before every write.
func (vm *VM) CheckInvariants() error {
	bad := func(format string, args ...any) error {
		return Errorf(CodeInternal, "vm %s invariant: "+format, append([]any{vm.ID}, args...)...)
	}
	switch {
	case !vm.State.Valid():
		return bad("unknown state %q", vm.State)
	case vm.InstanceGeneration < 1:
		return bad("instanceGeneration %d < 1", vm.InstanceGeneration)
	case vm.State == StateDeleted && vm.ActiveOperationID != "":
		return bad("deleted with active operation")
	case vm.State == StateDeleted && vm.DeletedAt == nil:
		return bad("deleted without deletedAt")
	case (vm.State == StateReady || vm.State == StateRunning) && vm.BootMode == BootModeNone:
		return bad("%s without bootMode", vm.State)
	// A failed delete leaves the VM deleting with no operation until retried.
	case (vm.State == StateProvisioning || vm.State == StateStopping) && vm.ActiveOperationID == "":
		return bad("transitional state %s without active operation", vm.State)
	}
	return nil
}

// OperationKind is the kind of durable lifecycle intent.
type OperationKind string

const (
	OpCreate   OperationKind = "create"
	OpFork     OperationKind = "fork"
	OpStop     OperationKind = "stop"
	OpResume   OperationKind = "resume"
	OpDelete   OperationKind = "delete"
	OpSnapshot OperationKind = "snapshot"
	// OpRecover power-cycles a VM on its current disk (G9): the VMM is
	// killed without committing RAM or disk, and the same disk cold boots.
	OpRecover OperationKind = "recover"
)

// OperationState is the operation's journaled state.
type OperationState string

const (
	OpQueued    OperationState = "queued"
	OpRunning   OperationState = "running"
	OpSucceeded OperationState = "succeeded"
	OpFailed    OperationState = "failed"
)

// Terminal reports whether s is final.
func (s OperationState) Terminal() bool { return s == OpSucceeded || s == OpFailed }

var opTransitions = map[OperationState][]OperationState{
	OpQueued:  {OpRunning, OpFailed},
	OpRunning: {OpRunning, OpSucceeded, OpFailed},
}

// CanTransitionOp reports whether an operation may move from -> to.
func CanTransitionOp(from, to OperationState) bool { return slices.Contains(opTransitions[from], to) }

// Operation is a durable asynchronous intent with progress and a terminal
// result. It survives client disconnect and daemon restart.
type Operation struct {
	ID       OperationID    `json:"id"`
	Kind     OperationKind  `json:"kind"`
	VMID     VMID           `json:"vmId"`
	Owner    PrincipalID    `json:"owner"`
	State    OperationState `json:"state"`
	Phase    string         `json:"phase"`
	Progress float64        `json:"progress"`
	Result   *OpResult      `json:"result,omitempty"`
	Error    *Error         `json:"error,omitempty"`
	// Reason is why the host stopped a VM on its own: "idle" (its idle
	// timeout), "ttl" (auto-stop), "reclaim" (to make room for another VM)
	// or "gc" (reclaimVMs); empty for a stop a client asked for.
	Reason      string     `json:"reason,omitempty"`
	Params      OpParams   `json:"-"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// OpParams is the journaled input needed to (re)execute an operation after a
// crash. It is host-internal.
type OpParams struct {
	SourceVMID       VMID       `json:"sourceVmId,omitempty"`
	SourceSnapshotID SnapshotID `json:"sourceSnapshotId,omitempty"`
	// SnapshotID is chosen when a stop is accepted so a replayed stop
	// publishes the same snapshot.
	SnapshotID    SnapshotID `json:"snapshotId,omitempty"`
	Generation    int64      `json:"generation,omitempty"`
	Force         bool       `json:"force,omitempty"`
	SnapshotName  string     `json:"snapshotName,omitempty"`
	RequireGolden bool       `json:"requireGolden,omitempty"`
	// BootPlan is lifecycle's journaled boot decision for create/fork/resume:
	// "" undecided, "golden:<id>" disk cloned from that golden and not yet
	// opened by a VMM, "cold" anything else.
	BootPlan string `json:"bootPlan,omitempty"`
	// Live marks a live fork. LiveCommitID (journaled before the source
	// pauses) names the source-disk commit taken with the checkpoint
	// CheckpointID (journaled before the child's disk is cloned), so a
	// replay reuses the RAM image and its matching disk, never mixes them.
	Live                  bool   `json:"live,omitempty"`
	CheckpointID          string `json:"checkpointId,omitempty"`
	CheckpointFingerprint string `json:"checkpointFingerprint,omitempty"`
	LiveCommitID          string `json:"liveCommitId,omitempty"`
	NoOp                  bool   `json:"noop,omitempty"`
	// Discard (a stop of an ephemeral or noSnapshot VM, decided when the
	// stop is accepted) commits nothing; Delete (ephemeral) then deletes the
	// VM. A replayed stop follows the journaled decision.
	Discard bool `json:"discard,omitempty"`
	Delete  bool `json:"delete,omitempty"`
	// ReuseDisk boots the VM's current working disk as it is (recover)
	// instead of creating or cloning one.
	ReuseDisk bool `json:"reuseDisk,omitempty"`
	// Shape is the shape a resize applies at this resume (G9); zero keeps
	// the VM's shape.
	Shape Shape `json:"shape,omitzero"`
}

// OpResult is the terminal success payload.
type OpResult struct {
	VMID       VMID       `json:"vmId,omitempty"`
	SnapshotID SnapshotID `json:"snapshotId,omitempty"`
	BootMode   BootMode   `json:"bootMode,omitempty"`
	NoOp       bool       `json:"noop,omitempty"`
	// DurationMs is wall time from start to terminal state, measured.
	DurationMs int64 `json:"durationMs"`
}

// OperationEvent is one ordered progress record. (OperationID, Seq) is the
// stream cursor.
type OperationEvent struct {
	OperationID OperationID    `json:"operationId"`
	Seq         int64          `json:"seq"`
	Time        time.Time      `json:"time"`
	State       OperationState `json:"state"`
	Phase       string         `json:"phase"`
	Progress    float64        `json:"progress"`
	Message     string         `json:"message,omitempty"`
}
