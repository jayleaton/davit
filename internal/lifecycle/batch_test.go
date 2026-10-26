package lifecycle

import (
	"context"
	"strings"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
)

func (e *env) liveVMs() int {
	e.t.Helper()
	vms, _, err := e.svc.ListVMs(context.Background(), owner, nil, true, Page{Limit: 200})
	if err != nil {
		e.t.Fatal(err)
	}
	n := 0
	for _, vm := range vms {
		if vm.State != domain.StateDeleted {
			n++
		}
	}
	return n
}

func TestCreateVMsNames(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	res, err := e.svc.CreateVMs(ctx, e.req("b1"), api.CreateVMsRequest{Count: 3, Template: api.CreateVMRequest{Name: "web"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range res.Items {
		want := "web-" + string(rune('1'+i))
		if it.Index != i+1 || it.Name != want || it.Error != nil || it.Accepted == nil || it.Accepted.VM.Name != want {
			t.Fatalf("item %d: %+v", i, it)
		}
		e.mustSucceed(&Accepted{Operation: it.Accepted.Operation}, nil)
	}
	if e.vm("web-3").State != domain.StateReady {
		t.Fatal("web-3 not ready")
	}
	res, err = e.svc.CreateVMs(ctx, e.req("b2"), api.CreateVMsRequest{Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 || res.Items[0].Accepted == nil || res.Items[1].Accepted == nil ||
		res.Items[0].Name == "" || res.Items[0].Name == res.Items[1].Name {
		t.Fatalf("unnamed batch: %+v", res.Items)
	}
}

// TestCreateVMsPartial: items are admitted independently; a retry with the
// same key replays the accepted items and creates only the missing ones.
func TestCreateVMsPartial(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "web-2"}))
	body := api.CreateVMsRequest{Count: 3, Template: api.CreateVMRequest{Name: "web"}}
	first, err := e.svc.CreateVMs(ctx, e.req("batch"), body)
	if err != nil {
		t.Fatal(err)
	}
	it := first.Items
	if it[0].Accepted == nil || it[2].Accepted == nil || it[1].Accepted != nil || it[1].Error == nil ||
		it[1].Error.Code != domain.CodeInvalidArgument || it[1].Name != "web-2" {
		t.Fatalf("name conflict mid-batch: %+v", it)
	}
	e.mustSucceed(e.svc.Delete(ctx, e.req(""), "web-2"))
	again, err := e.svc.CreateVMs(ctx, e.req("batch"), body)
	if err != nil {
		t.Fatal(err)
	}
	if again.Items[0].Accepted.Operation.ID != it[0].Accepted.Operation.ID ||
		again.Items[2].Accepted.Operation.ID != it[2].Accepted.Operation.ID {
		t.Fatalf("replay changed operations: %+v", again.Items)
	}
	if again.Items[1].Accepted == nil {
		t.Fatalf("retry did not create the missing item: %+v", again.Items[1])
	}
	third, _ := e.svc.CreateVMs(ctx, e.req("batch"), body)
	for i := range third.Items {
		if third.Items[i].Accepted.Operation.ID != again.Items[i].Accepted.Operation.ID {
			t.Fatalf("second replay item %d differs", i+1)
		}
	}
	if n := e.liveVMs(); n != 3 {
		t.Fatalf("%d VMs, want 3", n)
	}

	// The same key with another template conflicts per item.
	ttl := int64(60)
	other := body
	other.Template.TTLSeconds = &ttl
	res, err := e.svc.CreateVMs(ctx, e.req("batch"), other)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Error == nil || it.Error.Code != domain.CodeIdempotencyConflict {
			t.Fatalf("conflicting body: %+v", it)
		}
	}
}

// TestCreateVMsAdmission: a limit reached mid-batch refuses the remaining
// items with a retryable resource_exhausted; earlier items stay accepted.
func TestCreateVMsAdmission(t *testing.T) {
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Limits = domain.Limits{VMs: 2} })
	res, err := e.svc.CreateVMs(context.Background(), e.req("k"), api.CreateVMsRequest{Count: 4, Template: api.CreateVMRequest{Name: "w"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, it := range res.Items {
		ok := it.Accepted != nil && it.Error == nil
		if want := i < 2; ok != want {
			t.Fatalf("item %d accepted=%v: %+v", i+1, ok, it)
		}
		if !ok && (it.Error.Code != domain.CodeResourceExhausted || !it.Error.Retryable || it.Error.Details["resource"] != "vms") {
			t.Fatalf("item %d: %+v", i+1, it.Error)
		}
	}
}

// TestCreateVMsInvalid: request errors fail the whole batch and create
// nothing.
func TestCreateVMsInvalid(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	for name, c := range map[string]struct {
		key  string
		body api.CreateVMsRequest
		code domain.Code
	}{
		"count 0":       {"k", api.CreateVMsRequest{Count: 0}, domain.CodeInvalidArgument},
		"count 101":     {"k", api.CreateVMsRequest{Count: api.MaxBatchCount + 1}, domain.CodeInvalidArgument},
		"no key":        {"", api.CreateVMsRequest{Count: 1}, domain.CodeInvalidArgument},
		"long key":      {strings.Repeat("k", 252), api.CreateVMsRequest{Count: 1}, domain.CodeInvalidArgument},
		"unknown type":  {"k", api.CreateVMsRequest{Count: 2, Template: api.CreateVMRequest{Type: "huge"}}, domain.CodeInvalidArgument},
		"unknown image": {"k", api.CreateVMsRequest{Count: 2, Template: api.CreateVMRequest{Image: "nope"}}, domain.CodeNotFound},
		"bad env":       {"k", api.CreateVMsRequest{Count: 2, Template: api.CreateVMRequest{Env: map[string]string{"A=B": "x"}}}, domain.CodeInvalidArgument},
		"derived name":  {"k", api.CreateVMsRequest{Count: 10, Template: api.CreateVMRequest{Name: strings.Repeat("a", 62)}}, domain.CodeInvalidArgument},
		"bad name":      {"k", api.CreateVMsRequest{Count: 1, Template: api.CreateVMRequest{Name: "Web"}}, domain.CodeInvalidArgument},
		"environment": {"k", api.CreateVMsRequest{Count: 1, Template: api.CreateVMRequest{
			Environment: &api.EnvironmentRef{Environment: "web"}}}, domain.CodeNotImplemented},
	} {
		if _, err := e.svc.CreateVMs(ctx, e.req(c.key), c.body); !domain.IsCode(err, c.code) {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	if n := e.liveVMs(); n != 0 {
		t.Fatalf("invalid batches created %d VMs", n)
	}
}
