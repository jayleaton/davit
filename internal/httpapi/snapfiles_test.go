package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/lifecycle"
	"davit/internal/runtime/fake"
	"davit/internal/snapfiles"
	"davit/internal/state"
	"davit/internal/storage"
	"davit/internal/tokens"
)

// scopeAuth authenticates the owner, with the token grant named by the
// X-Test-Action header (none: the owner identity itself).
type scopeAuth struct{}

func (scopeAuth) Authenticate(*http.Request) (domain.PrincipalID, error) { return "owner", nil }

func (scopeAuth) AuthenticateGrant(r *http.Request) (domain.PrincipalID, *tokens.Grant, error) {
	a := r.Header.Get("X-Test-Action")
	if a == "" {
		return "owner", nil, nil
	}
	return "owner", &tokens.Grant{TokenID: "tok_test", Scopes: []api.TokenScope{{Actions: []string{a}}}}, nil
}

// TestSnapshotFilesNeedExec: snapshot file reads expose a VM's disk, so
// they need exec, as readFile does; view is not enough.
func TestSnapshotFilesNeedExec(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, err := state.Open(ctx, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := fake.New(fake.Options{Dir: filepath.Join(dir, "rt")})
	st, _ := storage.Open(filepath.Join(dir, "st"), storage.ModeCopy)
	img, _ := lifecycle.DevImage(filepath.Join(dir, "img"))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := lifecycle.New(lifecycle.Config{DB: db, Runtime: rt, Storage: st, Images: lifecycle.NewStaticCatalog(img), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close(); db.Close() })
	srv := New(svc, scopeAuth{}, log)
	srv.SetSnapshotFiles(snapfiles.New(svc, st))
	h := srv.Handler()

	wait := func(rec int, body []byte) {
		t.Helper()
		var acc api.OperationAccepted
		if rec/100 != 2 || json.Unmarshal(body, &acc) != nil {
			t.Fatalf("mutation: %d %s", rec, body)
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			r, _ := call(t, h, "GET", "/api/v1/operations/"+string(acc.Operation.ID), "", nil)
			var op api.Operation
			json.Unmarshal(r.Body.Bytes(), &op)
			if op.State == domain.OpSucceeded {
				return
			}
			if op.State == domain.OpFailed || time.Now().After(deadline) {
				t.Fatalf("operation %s: %+v", op.ID, op)
			}
		}
	}
	key := map[string]string{api.HeaderIdempotencyKey: "k1"}
	r, _ := call(t, h, "POST", "/api/v1/vms", `{"name":"box"}`, key)
	wait(r.Code, r.Body.Bytes())
	key[api.HeaderIdempotencyKey] = "k2"
	r, _ = call(t, h, "POST", "/api/v1/vms/box/stop", `{}`, key)
	wait(r.Code, r.Body.Bytes())
	r, _ = call(t, h, "GET", "/api/v1/vms/box", "", nil)
	var vm api.VM
	if json.Unmarshal(r.Body.Bytes(), &vm) != nil || vm.LastSnapshotID == "" {
		t.Fatalf("no snapshot after stop: %s", r.Body.String())
	}
	base := "/api/v1/snapshots/" + string(vm.LastSnapshotID)
	// export with an unknown format: authorization comes first, and nothing
	// disk-sized streams into the recorder.
	for _, path := range []string{base + "/tree?path=/", base + "/files?path=/etc/hostname", base + "/export?format=zip"} {
		if r, er := call(t, h, "GET", path, "", map[string]string{"X-Test-Action": api.TokenView}); r.Code != http.StatusForbidden {
			t.Errorf("%s with a view token: %d %+v, want 403", path, r.Code, er.Error)
		}
		// exec passes authorization (the dev image is not ext4: 422; the
		// bad format: 400).
		if r, er := call(t, h, "GET", path, "", map[string]string{"X-Test-Action": api.TokenExec}); r.Code == http.StatusForbidden || r.Code == http.StatusNotImplemented {
			t.Errorf("%s with an exec token: %d %+v", path, r.Code, er.Error)
		}
	}
	if r, er := call(t, h, "GET", base+"/tree?path=/", "", nil); r.Code != http.StatusUnprocessableEntity || er.Error.Code != domain.CodeUnsupportedCapability {
		t.Errorf("owner, non-ext disk: %d %+v", r.Code, er.Error)
	}
}
