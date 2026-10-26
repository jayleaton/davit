package domain

import (
	"errors"
	"testing"
	"time"
)

// TestTransitionsMatchSpec pins the state machine to SPEC §2 exactly: every
// listed edge is allowed and every unlisted edge is rejected.
func TestTransitionsMatchSpec(t *testing.T) {
	allowed := map[[2]VMState]bool{}
	edge := func(from, to VMState) { allowed[[2]VMState{from, to}] = true }
	edge(StateProvisioning, StateReady)
	edge(StateReady, StateRunning)
	edge(StateRunning, StateReady)
	for _, s := range []VMState{StateReady, StateRunning} {
		edge(s, StateStopping)
		edge(s, StateError)
		edge(s, StateProvisioning) // recover's power cycle (G9)
	}
	edge(StateStopping, StateStopped)
	edge(StateStopped, StateProvisioning)
	edge(StateProvisioning, StateError)
	edge(StateStopping, StateError)
	edge(StateError, StateProvisioning)
	for _, s := range AllStates {
		if s != StateDeleted {
			edge(s, StateDeleting)
		}
	}
	edge(StateDeleting, StateDeleted)

	for _, from := range AllStates {
		for _, to := range AllStates {
			if got, want := CanTransition(from, to), allowed[[2]VMState{from, to}]; got != want {
				t.Errorf("%s -> %s: allowed=%v, spec says %v", from, to, got, want)
			}
		}
	}
	if !StateDeleted.Terminal() || StateStopped.Terminal() {
		t.Fatal("only deleted is terminal")
	}
}

func TestTransitionBumpsRevisionAndRejectsIllegal(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	vm := &VM{ID: "vm_a", State: StateStopped, StateRevision: 4, InstanceGeneration: 1}
	if err := vm.Transition(StateReady, now); !IsCode(err, CodeInvalidState) {
		t.Fatalf("stopped -> ready: got %v, want invalid_state", err)
	}
	if vm.StateRevision != 4 {
		t.Fatal("rejected transition changed revision")
	}
	if err := vm.Transition(StateProvisioning, now); err != nil {
		t.Fatal(err)
	}
	if vm.StateRevision != 5 || !vm.UpdatedAt.Equal(now) {
		t.Fatalf("revision %d updatedAt %v", vm.StateRevision, vm.UpdatedAt)
	}
	vm.State, vm.ActiveOperationID = StateDeleting, "op_x"
	if err := vm.Transition(StateDeleted, now); err != nil || vm.DeletedAt == nil {
		t.Fatalf("deleted: %v %v", err, vm.DeletedAt)
	}
	for _, to := range AllStates {
		if vm.Transition(to, now) == nil {
			t.Fatalf("deleted -> %s allowed", to)
		}
	}
}

func TestInvariants(t *testing.T) {
	ok := VM{ID: "vm_a", State: StateReady, InstanceGeneration: 1, BootMode: BootModeColdBoot}
	if err := ok.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	bad := []VM{
		{ID: "a", State: "bogus", InstanceGeneration: 1},
		{ID: "a", State: StateStopped, InstanceGeneration: 0},
		{ID: "a", State: StateReady, InstanceGeneration: 1},
		{ID: "a", State: StateProvisioning, InstanceGeneration: 1},
		{ID: "a", State: StateDeleted, InstanceGeneration: 1},
	}
	for _, vm := range bad {
		if err := vm.CheckInvariants(); !IsCode(err, CodeInternal) {
			t.Errorf("%+v: want invariant violation, got %v", vm, err)
		}
	}
}

func TestErrorsMatchByCode(t *testing.T) {
	err := error(Errorf(CodeNotFound, "vm %s", "x"))
	if !errors.Is(err, &Error{Code: CodeNotFound}) || errors.Is(err, &Error{Code: CodeInternal}) {
		t.Fatal("errors.Is must match on code only")
	}
	if got := AsError(errors.New("secret /host/path")); got.Code != CodeInternal || got.Message != "internal error" {
		t.Fatalf("unknown errors must be redacted, got %+v", got)
	}
}

func TestShapeAndLimits(t *testing.T) {
	c := ShapeConstraints{MaxCPUs: 4, MinMemoryBytes: 128 * MiB, MaxMemoryBytes: 8 * GiB, MemoryAlignBytes: MiB, MinDiskBytes: GiB}
	if err := DefaultShape.Validate(c); err != nil {
		t.Fatal(err)
	}
	if err := (Shape{CPUs: 8, MemoryBytes: GiB, DiskBytes: GiB}).Validate(c); !IsCode(err, CodeUnsupportedCapability) {
		t.Fatalf("cpus over max: %v", err)
	}
	l := Limits{CPUs: 4}
	if err := l.Admit(Shape{CPUs: 3}, Shape{CPUs: 2}); !IsCode(err, CodeResourceExhausted) {
		t.Fatalf("admit: %v", err)
	}
	if got := (Shape{CPUs: 1}).Merge(DefaultShape); got.CPUs != 1 || got.MemoryBytes != DefaultShape.MemoryBytes {
		t.Fatalf("merge %+v", got)
	}
	if !ValidName("web-1") || ValidName("-x") || ValidName("Web") || ValidName("") {
		t.Fatal("ValidName")
	}
}
