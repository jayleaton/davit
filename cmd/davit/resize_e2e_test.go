package main_test

import (
	"encoding/json"
	"testing"

	"davit/api"
	"davit/internal/cli"
	"davit/internal/domain"
)

// TestEndToEndResize drives `davit resize` and `resume --cpus` against
// the fake runtime: a running VM needs --stop, a stopped one resizes on
// resume, and the disk only grows.
func TestEndToEndResize(t *testing.T) {
	h := newHost(t)
	h.extraConfig = `"profiles":[{"name":"big","shape":{"cpus":4,"memoryBytes":4294967296}}]`
	h.start(0, 0)
	h.waited(h.cli("new", "rs", "--cpus", "1", "--memory", "512MiB", "--disk", "2GiB"), domain.OpCreate)

	h.expectError(h.cli("resize", "rs"), cli.ExitUsage, domain.CodeInvalidArgument) // no shape
	h.expectError(h.cli("resize", "rs", "--cpus", "2"), cli.ExitConflict, domain.CodeStopRequired)

	// --stop: stop, then resume resized; exactly one result (the resume's).
	r := h.cli("resize", "rs", "--cpus", "2", "--memory", "1GiB", "--stop")
	if r.code != 0 || r.last().Event != "result" {
		t.Fatalf("resize --stop: exit %d %+v", r.code, r.last().Error)
	}
	var res struct {
		Operation api.Operation `json:"operation"`
		VM        api.VM        `json:"vm"`
	}
	if err := json.Unmarshal(r.raw[len(r.raw)-1], &res); err != nil {
		t.Fatal(err)
	}
	if res.Operation.Kind != domain.OpResume || res.VM.State != domain.StateReady ||
		res.VM.Shape != (domain.Shape{CPUs: 2, MemoryBytes: domain.GiB, DiskBytes: 2 * domain.GiB}) {
		t.Fatalf("resize --stop result: %+v %+v", res.Operation, res.VM)
	}

	h.waited(h.cli("stop", "rs"), domain.OpStop)
	h.expectError(h.cli("resize", "rs", "--disk", "1GiB"), cli.ExitUsage, domain.CodeInvalidArgument) // shrink
	vm := h.waited(h.cli("resize", "rs", "--type", "big", "--disk", "3GiB"), domain.OpResume)
	if vm.Shape != (domain.Shape{CPUs: 4, MemoryBytes: 4 * domain.GiB, DiskBytes: 3 * domain.GiB}) {
		t.Fatalf("resize --type big --disk 3GiB: %+v", vm.Shape)
	}

	h.waited(h.cli("stop", "rs"), domain.OpStop)
	if vm = h.waited(h.cli("resume", "rs", "--cpus", "1"), domain.OpResume); vm.Shape.CPUs != 1 || vm.Shape.DiskBytes != 3*domain.GiB {
		t.Fatalf("resume --cpus 1: %+v", vm.Shape)
	}
}
