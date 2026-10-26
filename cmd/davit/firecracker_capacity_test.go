//go:build linux

package main_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/config"
	"davit/internal/domain"
)

// TestFirecrackerCapacityReclaim: on real VMs, with a reservation that fits
// two 1 GiB VMs, a third create stops the idle one (reason reclaim) and not
// the one running a process; with both left busy, a resume with waitSeconds
// queues (seen in capabilities.capacity.waiting) and gets in once a VM
// stops.
func TestFirecrackerCapacityReclaim(t *testing.T) {
	h := newFCHost(t, func(c *config.Daemon) {
		c.Limits.MemoryBytes, c.Limits.MemoryOvercommit, c.Limits.ReclaimIdleAfterSeconds = 2*domain.GiB, 1, 3
	})
	a := apiConn{t: t, c: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.sock)
	}}}}
	vmPath := func(name string) string { return "/vms/" + name }
	no, mem := false, int64(domain.GiB)
	create := func(name string) (api.OperationAccepted, int, []byte) {
		body := api.CreateVMRequest{Name: name, AutoStop: &no, IdleTimeoutSeconds: 3600, Shape: &api.ShapeOverride{MemoryBytes: mem}}
		st, b := a.call("POST", "/vms", body, "k-cap-"+name)
		var acc api.OperationAccepted
		json.Unmarshal(b, &acc)
		return acc, st, b
	}
	ready := func(name string) {
		t.Helper()
		acc, st, b := create(name)
		if st != http.StatusAccepted {
			t.Fatalf("create %s: HTTP %d %s", name, st, b)
		}
		if op := a.waitOp(string(acc.Operation.ID)); op.State != domain.OpSucceeded {
			t.Fatalf("create %s: %+v", name, op)
		}
	}
	busy := func(name string) {
		t.Helper()
		var p api.Process
		a.must("POST", vmPath(name)+"/processes", api.ExecRequest{Argv: []string{"sleep", "600"}, Detached: true}, "k-cap-sleep-"+name, &p)
	}
	capacity := func() *api.HostCapacity {
		var c api.HostCapabilities
		a.must("GET", "/capabilities", nil, "", &c)
		return c.Capacity
	}

	ready("idle")
	ready("worker")
	busy("worker")
	h.sh("idle", "true") // activity: not idle long enough yet
	if _, st, b := create("full"); !strings.Contains(string(b), "resource_exhausted") {
		t.Fatalf("third VM before anything is idle long enough: HTTP %d %s", st, b)
	}
	time.Sleep(4 * time.Second) // past reclaimIdleAfterSeconds
	if c := capacity(); c.Reclaimable != 1 || c.MemoryBudgetBytes != 2*domain.GiB {
		t.Fatalf("capacity before the reclaim: %+v", c)
	}
	start := time.Now()
	ready("third")
	t.Logf("create with a reclaim: %s (stop of the idle VM + boot)", time.Since(start).Round(time.Millisecond))
	if s := h.info("idle").State; s != domain.StateStopped {
		t.Fatalf("idle VM after the reclaim: %s", s)
	}
	if s := h.info("worker").State; s != domain.StateReady {
		t.Fatalf("busy VM after the reclaim: %s", s)
	}
	ev := h.cli("events", "--types", "operation.created", "--vm", string(h.info("idle").ID))
	if !strings.Contains(strings.Join(rawLines(ev), "\n"), `"reason":"reclaim"`) {
		t.Fatalf("no reclaim event for the idle VM: %v", rawLines(ev))
	}

	// Both live VMs busy: a resume with a wait queues until one stops.
	busy("third")
	type result struct {
		st int
		b  []byte
	}
	done := make(chan result, 1)
	go func() {
		st, b := a.call("POST", vmPath("idle")+"/resume?waitSeconds=120", api.ResumeVMRequest{}, "k-cap-resume")
		done <- result{st, b}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for c := capacity(); len(c.Waiting) != 1; c = capacity() {
		if time.Now().After(deadline) {
			t.Fatalf("resume never queued: %+v", c)
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(5 * time.Second) // still queued: both VMs run a process
	select {
	case r := <-done:
		t.Fatalf("resume answered while both VMs were busy: HTTP %d %s", r.st, r.b)
	default:
	}
	stopped := time.Now()
	var acc api.OperationAccepted
	a.must("POST", vmPath("worker")+"/stop", api.StopVMRequest{}, "k-cap-stop", &acc)
	a.waitOp(string(acc.Operation.ID))
	select {
	case r := <-done:
		if r.st != http.StatusAccepted {
			t.Fatalf("queued resume: HTTP %d %s", r.st, r.b)
		}
		json.Unmarshal(r.b, &acc)
		if op := a.waitOp(string(acc.Operation.ID)); op.State != domain.OpSucceeded {
			t.Fatalf("queued resume: %+v", op)
		}
		t.Logf("queued resume admitted %s after the stop began", time.Since(stopped).Round(time.Millisecond))
	case <-time.After(60 * time.Second):
		t.Fatal("queued resume not admitted after a VM stopped")
	}
	if s := h.info("idle").State; s != domain.StateReady {
		t.Fatalf("resumed VM: %s", s)
	}
}

func rawLines(r run) []string {
	out := make([]string, len(r.raw))
	for i, l := range r.raw {
		out[i] = string(l)
	}
	return out
}
