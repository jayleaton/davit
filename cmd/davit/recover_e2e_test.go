package main_test

import (
	"testing"
	"time"

	"davit/internal/cli"
	"davit/internal/domain"
)

// TestEndToEndRecover drives `davit recover` against a fake-runtime
// daemon: it asks before killing the guest, waits like resume, and a
// stopped VM is pointed at resume.
func TestEndToEndRecover(t *testing.T) {
	h := newHost(t)
	h.start(20*time.Millisecond, 0)

	web := h.waited(h.cli("new", "rw", "--cpus", "1", "--memory", "512MiB"), domain.OpCreate)
	h.expectError(h.cli("recover", "rw"), cli.ExitUsage, domain.CodeInvalidArgument) // --yes required
	vm := h.waited(h.cli("recover", "rw", "--yes"), domain.OpRecover)
	if vm.State != domain.StateReady || vm.ID != web.ID || vm.InstanceGeneration != 2 || vm.BootMode != domain.BootModeColdBoot {
		t.Fatalf("recover: %+v", vm)
	}
	if h.instances(vm.ID) != 1 {
		t.Fatalf("recover left %d instances", h.instances(vm.ID))
	}

	h.waited(h.cli("stop", "rw"), domain.OpStop)
	h.expectError(h.cli("recover", "rw", "--yes"), cli.ExitConflict, domain.CodeInvalidState)
}
