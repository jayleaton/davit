package lifecycle

import (
	"slices"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
)

func TestEgressPolicy(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := t.Context()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "dflt"}))
	if vm := e.vm("dflt"); vm.Egress.Mode != domain.EgressFiltered {
		t.Fatalf("default egress %+v", vm.Egress)
	}
	locked := &domain.Egress{Mode: domain.EgressAllowlist, Allow: []string{"GitHub.com.", "140.82.112.0/20"}}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "lock", Egress: locked}))
	want := []string{"140.82.112.0/20", "github.com"}
	if vm := e.vm("lock"); vm.Egress.Mode != domain.EgressAllowlist || !slices.Equal(vm.Egress.Allow, want) {
		t.Fatalf("allowlist egress %+v", vm.Egress)
	}
	if _, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "bad", Egress: &domain.Egress{Mode: "open"}}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("bad mode: %v", err)
	}

	// A fork inherits its source's policy unless told otherwise.
	e.mustSucceed(e.svc.Stop(ctx, e.req(e.newKey()), "lock", false))
	e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "lock", api.ForkVMRequest{Name: "lock-fork"}))
	if vm := e.vm("lock-fork"); !slices.Equal(vm.Egress.Allow, want) {
		t.Fatalf("fork did not inherit egress: %+v", vm.Egress)
	}
	off := &domain.Egress{Mode: domain.EgressOff}
	e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "lock", api.ForkVMRequest{Name: "lock-off", Egress: off}))
	if vm := e.vm("lock-off"); vm.Egress.Mode != domain.EgressOff {
		t.Fatalf("fork egress override: %+v", vm.Egress)
	}

	// Update on a stopped and on a running VM; other fields change with it.
	for _, name := range []string{"lock", "dflt"} {
		vm, err := e.svc.Update(ctx, e.req(""), name, api.UpdateVMRequest{Egress: off, TTLSeconds: ptr(int64(600))})
		if err != nil || vm.Egress.Mode != domain.EgressOff || vm.TTLSeconds != 600 || e.vm(name).Egress.Mode != domain.EgressOff {
			t.Fatalf("Update(%s, egress off): %+v %v", name, vm, err)
		}
	}
	// A rejected update changes nothing: neither the policy nor the name.
	bad := &domain.Egress{Mode: domain.EgressFiltered, Allow: []string{"x.com"}}
	if _, err := e.svc.Update(ctx, e.req(""), "dflt", api.UpdateVMRequest{Egress: bad, Name: ptr("other")}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("allow without allowlist: %v", err)
	}
	if _, err := e.svc.Update(ctx, e.req(""), "dflt", api.UpdateVMRequest{Egress: locked, Name: ptr("lock")}); err == nil {
		t.Fatal("rename onto a taken name succeeded")
	}
	if vm := e.vm("dflt"); vm.Egress.Mode != domain.EgressOff {
		t.Fatalf("failed update changed egress: %+v", vm.Egress)
	}
	if _, err := e.svc.Update(ctx, e.req(""), "nope", api.UpdateVMRequest{Egress: off}); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("unknown vm: %v", err)
	}
}
