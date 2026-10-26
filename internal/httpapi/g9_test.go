package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
)

func waitVMState(t *testing.T, h http.Handler, id domain.VMID, want domain.VMState) domain.VM {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, _ := call(t, h, "GET", "/api/v1/vms/"+string(id), "", nil)
		var vm domain.VM
		if err := json.Unmarshal(rec.Body.Bytes(), &vm); err != nil {
			t.Fatal(err)
		}
		if vm.State == want {
			return vm
		}
		if time.Now().After(deadline) {
			t.Fatalf("vm %s is %s, want %s", id, vm.State, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRecoverVM: the real recoverVM route requires an Idempotency-Key,
// honors If-Match, answers 202 with a recover operation and replays a key.
func TestRecoverVM(t *testing.T) {
	h := newServer(t, fixedAuth{p: "local:1"})
	rec, _ := call(t, h, "POST", "/api/v1/vms", `{"name":"w"}`, map[string]string{api.HeaderIdempotencyKey: "c1"})
	var created api.OperationAccepted
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &created) != nil {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	vm := waitVMState(t, h, created.VM.ID, domain.StateReady)

	path := "/api/v1/vms/w/recover"
	if rec, er := call(t, h, "POST", path, "", nil); rec.Code != http.StatusBadRequest || er.Error.Code != domain.CodeInvalidArgument {
		t.Fatalf("without a key: %d %+v", rec.Code, er.Error)
	}
	if rec, er := call(t, h, "POST", path, `{"force":true}`, map[string]string{api.HeaderIdempotencyKey: "r0"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d %+v", rec.Code, er.Error)
	}
	if rec, er := call(t, h, "POST", path, "", map[string]string{api.HeaderIdempotencyKey: "r0", api.HeaderIfMatch: "999"}); rec.Code != http.StatusPreconditionFailed ||
		er.Error.Code != domain.CodeRevisionMismatch {
		t.Fatalf("stale If-Match: %d %+v", rec.Code, er.Error)
	}
	rec, _ = call(t, h, "POST", path, "", map[string]string{api.HeaderIdempotencyKey: "r1"})
	var a api.OperationAccepted
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &a) != nil ||
		a.Operation.Kind != domain.OpRecover || a.VM.State != domain.StateProvisioning || rec.Header().Get("Location") == "" {
		t.Fatalf("recover: %d %s", rec.Code, rec.Body)
	}
	rec, _ = call(t, h, "POST", "/api/v1/vms/"+string(vm.ID)+"/recover", "{}", map[string]string{api.HeaderIdempotencyKey: "r1"})
	var again api.OperationAccepted
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &again) != nil || again.Operation.ID != a.Operation.ID {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}
	if got := waitVMState(t, h, vm.ID, domain.StateReady); got.InstanceGeneration != 2 || got.BootMode != domain.BootModeColdBoot {
		t.Fatalf("after recover: %+v", got)
	}
}
