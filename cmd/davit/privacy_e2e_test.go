package main_test

import (
	"testing"
	"time"

	"davit/internal/domain"
)

// TestEndToEndEphemeral: `new --ephemeral` reaches the daemon and its stop
// deletes the VM (G17); `privacy` changes the policy of another VM.
func TestEndToEndEphemeral(t *testing.T) {
	h := newHost(t)
	h.start(10*time.Millisecond, 0)
	vm := h.waited(h.cli("new", "tmp", "--ephemeral"), domain.OpCreate)
	if !vm.Privacy.Ephemeral {
		t.Fatalf("privacy not set: %+v", vm.Privacy)
	}
	h.waited(h.cli("stop", "tmp"), domain.OpStop)
	if got := h.info(string(vm.ID)); got.State != domain.StateDeleted {
		t.Fatalf("ephemeral VM after stop: %s", got.State)
	}

	h.waited(h.cli("new", "keep"), domain.OpCreate)
	if r := h.cli("privacy", "keep", "--no-snapshot=true"); r.code != 0 {
		t.Fatalf("privacy: exit %d %+v", r.code, r.last().Error)
	}
	if got := h.info("keep"); !got.Privacy.NoSnapshot {
		t.Fatalf("privacy not applied: %+v", got.Privacy)
	}
}
