package lifecycle

import (
	"context"
	"slices"
	"sync"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/network"
	"davit/internal/runtime/fake"
)

type credHooks struct {
	mu     sync.Mutex
	events []string
}

func (c *credHooks) add(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, s)
}

func (c *credHooks) AttachNetwork(_ context.Context, vm domain.VMID, h network.Handle) {
	c.add("attach " + string(vm))
}
func (c *credHooks) DetachNetwork(vm domain.VMID) { c.add("detach " + string(vm)) }
func (c *credHooks) Lifecycle(_ context.Context, vm domain.VMID, e domain.GrantEvent) error {
	c.add(string(e) + " " + string(vm))
	return nil
}

// TestCredentialHooks: the credential proxy serves a VM's network while it
// is up and sees every grant event, fork on its source.
func TestCredentialHooks(t *testing.T) {
	hooks := &credHooks{}
	e := newEnv(t, fake.Options{}, func(c *Config) { c.Credentials = hooks })
	ctx := t.Context()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "src"}))
	src := string(e.vm("src").ID)
	e.mustSucceed(e.svc.Stop(ctx, e.req(e.newKey()), "src", false))
	e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "src", api.ForkVMRequest{Name: "child"}))
	child := string(e.vm("child").ID)
	e.mustSucceed(e.svc.Resume(ctx, e.req(e.newKey()), "src", api.ResumeVMRequest{}))
	e.mustSucceed(e.svc.Delete(ctx, e.req(e.newKey()), "src"))
	hooks.mu.Lock()
	got := slices.Clone(hooks.events)
	hooks.mu.Unlock()
	for _, want := range []string{"attach " + src, "detach " + src, "stop " + src, "fork " + src, "attach " + child,
		"resume " + src, "delete " + src} {
		if !slices.Contains(got, want) {
			t.Errorf("no %q in %q", want, got)
		}
	}
	if slices.Contains(got, "fork "+child) || slices.Contains(got, "delete "+child) {
		t.Errorf("the fork child got grant events of its own: %q", got)
	}
	// stop releases the network before the stop event; attach precedes it.
	if i, j := slices.Index(got, "detach "+src), slices.Index(got, "stop "+src); i > j {
		t.Errorf("stop event before the detach: %q", got)
	}
}
