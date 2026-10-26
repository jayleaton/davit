package lifecycle

import (
	"context"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestproto"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
)

// fakePreparer stands in for the guest agent's WorkspacePrepare: it records
// requests and answers with result (ready by default).
type fakePreparer struct {
	mu     sync.Mutex
	reqs   []guestproto.WorkspacePrepare
	result func(guestproto.WorkspacePrepare) (guestproto.WorkspaceResult, error)
}

func (f *fakePreparer) PrepareWorkspace(_ context.Context, _ domain.PrincipalID, _ string, req guestproto.WorkspacePrepare, progress func(guestproto.WorkspaceProgress)) (guestproto.WorkspaceResult, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	result := f.result
	f.mu.Unlock()
	progress(guestproto.WorkspaceProgress{Step: "deps", Phase: guestproto.WorkspacePhaseSetup, State: guestproto.WorkspaceStepRunning})
	if result != nil {
		return result(req)
	}
	return guestproto.WorkspaceResult{Revision: req.Revision, State: guestproto.WorkspaceReady}, nil
}

func (f *fakePreparer) calls() []guestproto.WorkspacePrepare {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]guestproto.WorkspacePrepare(nil), f.reqs...)
}

var wsManifest = domain.EnvManifest{
	Repos:       []domain.RepoSpec{{URL: "https://example.com/acme/web.git", Ref: "main", Path: "/work/web"}},
	Variables:   map[string]string{"NODE_ENV": "test", "A": "1"},
	SecretFiles: []domain.SecretFileRef{{Path: "/home/davit/.npmrc", Secret: "npmrc", Mode: "0640"}},
	Setup:       []domain.SetupAction{{Name: "deps", Argv: []string{"npm", "ci"}, Cwd: "/work/web"}},
}

func newWorkspaceEnv(t *testing.T, prep WorkspacePreparer) *env {
	transport := func(context.Context, runtime.Handle) (runtime.Dialer, error) { return nil, nil }
	return newEnv(t, fake.Options{GuestTransport: transport}, func(c *Config) {
		c.Workspace = prep
		c.WorkspaceSecret = func(_ context.Context, name string) ([]byte, error) {
			if name == "npmrc" {
				return []byte("//registry/:_authToken=s3cret"), nil
			}
			return nil, domain.Errorf(domain.CodeNotFound, "no secret %s", name)
		}
	})
}

// waitWorkspace waits until ref's workspace reaches want.
func (e *env) waitWorkspace(ref string, want domain.WorkspaceState) *domain.VM {
	e.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		vm := e.vm(ref)
		if vm.Workspace != nil && vm.Workspace.State == want {
			return vm
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s workspace %+v, want %s", ref, vm.Workspace, want)
		}
	}
}

// TestEnvironmentsCRUD: revisions are numbered and immutable, creation is
// naturally idempotent, revisions replay by key and check If-Match.
func TestEnvironmentsCRUD(t *testing.T) {
	e := newEnv(t, fake.Options{}, nil)
	ctx := context.Background()
	body := api.CreateEnvironmentRequest{Name: "web", Manifest: wsManifest}
	env, created, err := e.svc.CreateEnvironment(ctx, owner, body)
	if err != nil || !created || env.LatestRevision != 1 {
		t.Fatalf("create: %+v %v %v", env, created, err)
	}
	if again, created, err := e.svc.CreateEnvironment(ctx, owner, body); err != nil || created || again.ID != env.ID {
		t.Fatalf("retry: %+v %v %v", again, created, err)
	}
	other := wsManifest
	other.Setup = nil
	if _, _, err := e.svc.CreateEnvironment(ctx, owner, api.CreateEnvironmentRequest{Name: "web", Manifest: other}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("same name, other manifest: %v", err)
	}
	if _, _, err := e.svc.CreateEnvironment(ctx, owner, api.CreateEnvironmentRequest{Name: "p", Manifest: domain.EnvManifest{CredentialProfile: "x"}}); !domain.IsCode(err, domain.CodeNotImplemented) {
		t.Fatalf("credential profile: %v", err)
	}

	stale := int64(7)
	if _, _, err := e.svc.CreateEnvironmentRevision(ctx, owner, "web", "", &stale, other); !domain.IsCode(err, domain.CodeRevisionMismatch) {
		t.Fatalf("stale If-Match: %v", err)
	}
	one := int64(1)
	r2, created, err := e.svc.CreateEnvironmentRevision(ctx, owner, string(env.ID), "k1", &one, other)
	if err != nil || !created || r2.Revision != 2 {
		t.Fatalf("revision 2: %+v %v", r2, err)
	}
	if again, created, err := e.svc.CreateEnvironmentRevision(ctx, owner, "web", "k1", &one, other); err != nil || created || again.Revision != 2 {
		t.Fatalf("keyed replay: %+v %v %v", again, created, err)
	}
	if _, _, err := e.svc.CreateEnvironmentRevision(ctx, owner, "web", "k1", nil, wsManifest); !domain.IsCode(err, domain.CodeIdempotencyConflict) {
		t.Fatalf("key reuse with another manifest: %v", err)
	}
	if r3, _, err := e.svc.CreateEnvironmentRevision(ctx, owner, "web", "k2", nil, wsManifest); err != nil || r3.Revision != 3 {
		t.Fatalf("revision 3: %+v %v", r3, err)
	}
	if again, created, err := e.svc.CreateEnvironmentRevision(ctx, owner, "web", "k1", &one, other); err != nil || created || again.Revision != 2 {
		t.Fatalf("an older key replays its own revision after a newer one: %+v %v %v", again, created, err)
	}
	var revs []int64
	for p := (Page{Limit: 1}); ; {
		page, next, err := e.svc.ListEnvironmentRevisions(ctx, owner, "web", p)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			revs = append(revs, r.Revision)
		}
		if next == "" {
			break
		}
		p.Cursor = next
	}
	if len(revs) != 3 || revs[0] != 1 || revs[2] != 3 {
		t.Fatalf("revisions %v", revs)
	}
	if r1, err := e.svc.GetEnvironmentRevision(ctx, owner, "web", 1); err != nil || len(r1.Manifest.Setup) != 1 {
		t.Fatalf("revision 1 changed: %+v %v", r1, err)
	}
	if envs, _, err := e.svc.ListEnvironments(ctx, owner, Page{}); err != nil || len(envs) != 1 {
		t.Fatalf("list: %v %v", envs, err)
	}
	if _, err := e.svc.GetEnvironment(ctx, "someone-else", "web"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("visible to another owner: %v", err)
	}
	if err := e.svc.DeleteEnvironment(ctx, owner, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetEnvironment(ctx, owner, "web"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

// TestWorkspaceLifecycle: a pinned VM prepares after it is ready (secrets
// resolved on the host, variables sorted), a fork inherits a ready
// workspace, a fork with a revision re-prepares, a failed step leaves the VM
// usable and says which step, and deleting the environment does not strand
// pinned VMs.
func TestWorkspaceLifecycle(t *testing.T) {
	prep := &fakePreparer{}
	e := newWorkspaceEnv(t, prep)
	ctx := context.Background()
	if _, _, err := e.svc.CreateEnvironment(ctx, owner, api.CreateEnvironmentRequest{Name: "web", Manifest: wsManifest}); err != nil {
		t.Fatal(err)
	}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "box", Environment: &api.EnvironmentRef{Environment: "web"}}))
	vm := e.waitWorkspace("box", domain.WorkspaceReady)
	if vm.Environment == nil || vm.Environment.Name != "web" || vm.Environment.Revision != 1 || vm.Workspace.ReadyAt == nil || vm.State != domain.StateReady {
		t.Fatalf("pinned VM: %+v %+v", vm.Environment, vm.Workspace)
	}
	req := prep.calls()[0]
	if req.Revision != string(vm.Environment.EnvironmentID)+"@1" || len(req.Repos) != 1 || req.Repos[0].Ref != "main" ||
		len(req.Files) != 1 || string(req.Files[0].Data) != "//registry/:_authToken=s3cret" || req.Files[0].Mode != 0o640 ||
		len(req.Env) != 2 || req.Env[0] != "A=1" || len(req.Setup) != 1 || req.Setup[0].Name != "deps" {
		t.Fatalf("prepare request: %+v", req)
	}

	// The environment goes away; the pin keeps its manifest copy.
	if err := e.svc.DeleteEnvironment(ctx, owner, "web"); err != nil {
		t.Fatal(err)
	}
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "box", false))
	e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "box", api.ForkVMRequest{Name: "kid"}))
	if kid := e.vm("kid"); kid.Workspace == nil || kid.Workspace.State != domain.WorkspaceReady || kid.Environment.Revision != 1 {
		t.Fatalf("fork did not inherit the ready workspace: %+v", kid.Workspace)
	}
	if n := len(prep.calls()); n != 1 {
		t.Fatalf("an inherited ready workspace was prepared again (%d calls)", n)
	}

	// A failing setup step.
	if _, _, err := e.svc.CreateEnvironment(ctx, owner, api.CreateEnvironmentRequest{Name: "web2", Manifest: wsManifest}); err != nil {
		t.Fatal(err)
	}
	exit := 1
	prep.mu.Lock()
	prep.result = func(r guestproto.WorkspacePrepare) (guestproto.WorkspaceResult, error) {
		return guestproto.WorkspaceResult{Revision: r.Revision, State: guestproto.WorkspaceFailed, FailedStep: "deps", ExitCode: &exit, Message: "npm ci failed"}, nil
	}
	prep.mu.Unlock()
	e.mustSucceed(e.svc.Fork(ctx, e.req(e.newKey()), "box", api.ForkVMRequest{Name: "kid2", Environment: &api.EnvironmentRef{Environment: "web2"}}))
	vm = e.waitWorkspace("kid2", domain.WorkspaceFailed)
	if se := vm.Workspace.Error; se == nil || se.Step != "deps" || se.Phase != "setup" || se.ExitCode == nil || *se.ExitCode != 1 || vm.State != domain.StateReady {
		t.Fatalf("failed workspace: %+v state %s", vm.Workspace.Error, vm.State)
	}
}

// TestWorkspaceMissingSecret: a secret the host cannot resolve fails the
// workspace before anything reaches the guest.
func TestWorkspaceMissingSecret(t *testing.T) {
	prep := &fakePreparer{}
	e := newWorkspaceEnv(t, prep)
	ctx := context.Background()
	m := wsManifest
	m.SecretFiles = []domain.SecretFileRef{{Path: "/etc/x", Secret: "absent"}}
	if _, _, err := e.svc.CreateEnvironment(ctx, owner, api.CreateEnvironmentRequest{Name: "web", Manifest: m}); err != nil {
		t.Fatal(err)
	}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "box", Environment: &api.EnvironmentRef{Environment: "web", Revision: 1}}))
	vm := e.waitWorkspace("box", domain.WorkspaceFailed)
	if se := vm.Workspace.Error; se == nil || se.Phase != "file" || se.Step != "/etc/x" || len(prep.calls()) != 0 {
		t.Fatalf("missing secret: %+v, %d prepare calls", se, len(prep.calls()))
	}
	if _, err := e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "b2", Environment: &api.EnvironmentRef{Environment: "web", Revision: 9}}); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("pin to a missing revision: %v", err)
	}
}

// TestWorkspaceSurvivesRestart: a daemon stopping mid-preparation leaves
// the workspace preparing, and the next start prepares it.
func TestWorkspaceSurvivesRestart(t *testing.T) {
	started := make(chan struct{}, 1)
	e := newWorkspaceEnv(t, &blockingPreparer{started: started})
	ctx := context.Background()
	if _, _, err := e.svc.CreateEnvironment(ctx, owner, api.CreateEnvironmentRequest{Name: "web", Manifest: wsManifest}); err != nil {
		t.Fatal(err)
	}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "box", Environment: &api.EnvironmentRef{Environment: "web"}}))
	<-started
	e.stop()
	e.cfg = func(c *Config) {
		c.Workspace = &fakePreparer{}
		c.WorkspaceSecret = func(context.Context, string) ([]byte, error) { return []byte("x"), nil }
	}
	e.start()
	e.waitWorkspace("box", domain.WorkspaceReady)
}

// blockingPreparer blocks until its context ends, like a guest preparing
// while the daemon shuts down.
type blockingPreparer struct{ started chan struct{} }

func (b *blockingPreparer) PrepareWorkspace(ctx context.Context, _ domain.PrincipalID, _ string, _ guestproto.WorkspacePrepare, _ func(guestproto.WorkspaceProgress)) (guestproto.WorkspaceResult, error) {
	b.started <- struct{}{}
	<-ctx.Done()
	return guestproto.WorkspaceResult{}, ctx.Err()
}
