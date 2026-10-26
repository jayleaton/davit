package members

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/gateway"
	"davit/internal/lifecycle"
)

type fakeRes struct {
	mu      sync.Mutex
	vms     []domain.VM
	changed chan struct{}
}

func (f *fakeRes) GetVM(_ context.Context, p domain.PrincipalID, ref string) (*domain.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, vm := range f.vms {
		if p == "owner" && (string(vm.ID) == ref || vm.Name == ref) {
			return &vm, nil
		}
	}
	return nil, domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
}

func (f *fakeRes) ListVMs(_ context.Context, _ domain.PrincipalID, _ []domain.VMState, _ bool, _ lifecycle.Page) ([]domain.VM, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.vms), "", nil
}

func (f *fakeRes) GetSnapshot(_ context.Context, _ domain.PrincipalID, ref string) (*domain.Snapshot, error) {
	if ref == "golden" || ref == "snap_1" {
		return &domain.Snapshot{ID: "snap_1", Name: "golden", SourceVMID: "vm_b"}, nil
	}
	return nil, domain.Errorf(domain.CodeNotFound, "snapshot %s not found", ref)
}

func (f *fakeRes) GetEnvironment(_ context.Context, _ domain.PrincipalID, ref string) (*domain.Environment, error) {
	if ref == "web" {
		return &domain.Environment{ID: "env_1", Name: "web"}, nil
	}
	return nil, domain.Errorf(domain.CodeNotFound, "environment %s not found", ref)
}

func (f *fakeRes) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *fakeRes) add(vm domain.VM) {
	f.mu.Lock()
	f.vms = append(f.vms, vm)
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

type cuts struct {
	mu  sync.Mutex
	log []string
}

func (c *cuts) Cut(users []int64, vm domain.VMID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, u := range users {
		c.log = append(c.log, string(vm)+"/"+string(rune('0'+u)))
	}
	return len(users)
}

func (c *cuts) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.log
	c.log = nil
	return out
}

func code(err error) domain.Code {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func newService(t *testing.T, vis string) (*Service, *fakeRes, *cuts, *[]domain.VMID) {
	t.Helper()
	res := &fakeRes{vms: []domain.VM{{ID: "vm_a", Name: "a"}, {ID: "vm_b", Name: "b"}}, changed: make(chan struct{})}
	c := &cuts{}
	var paused []domain.VMID
	var mu sync.Mutex
	s, err := New(Config{Path: filepath.Join(t.TempDir(), "members.json"), Owner: "owner", Resources: res,
		DefaultVisibility: vis, Sessions: c, OnCredentialsPaused: func(vm domain.VMID) { mu.Lock(); paused = append(paused, vm); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, res, c, &paused
}

var bob = gateway.Identity{UserID: 2, LoginName: "bob@example.com"}

func TestMemberGrantPinsAndScopes(t *testing.T) {
	ctx := context.Background()
	s, _, c, paused := newService(t, "")
	if _, ok := s.MemberGrant(bob); ok {
		t.Fatal("a stranger is a member")
	}
	if _, err := s.AddMember(ctx, api.AddMemberRequest{Login: "Bob@example.com", Role: api.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMember(ctx, api.AddMemberRequest{Login: "bob@example.com", Role: api.RoleExec}); code(err) != domain.CodeInvalidState {
		t.Fatalf("duplicate login: %v", err)
	}
	g, ok := s.MemberGrant(bob)
	if !ok || g.Member.UserID != 2 || len(g.Scopes) != 0 {
		t.Fatalf("new member grant %+v %v", g, ok)
	}
	// Pinned: a recycled login on another user is refused; a renamed login
	// on the same user keeps working; tagged nodes are never members.
	if _, ok := s.MemberGrant(gateway.Identity{UserID: 9, LoginName: "bob@example.com"}); ok {
		t.Fatal("recycled login accepted")
	}
	if _, ok := s.MemberGrant(gateway.Identity{UserID: 2, LoginName: "robert@example.com"}); !ok {
		t.Fatal("renamed login refused")
	}
	if _, ok := s.MemberGrant(gateway.Identity{UserID: 2, LoginName: "bob@example.com", Tags: []string{"tag:ci"}}); ok {
		t.Fatal("tagged node accepted as a member")
	}

	// Shares: role per VM, max over shares, snapshot and environment.
	share := func(kind api.ShareKind, res, member string, role api.MemberRole, creds bool) api.Share {
		t.Helper()
		sh, err := s.CreateShare(ctx, "owner", api.CreateShareRequest{Kind: kind, Resource: res, Member: member, Role: role, Credentials: creds})
		if err != nil {
			t.Fatal(err)
		}
		return sh
	}
	share(api.ShareVM, "a", "bob@example.com", api.RoleViewer, false)
	all := share(api.ShareVM, "a", "*", api.RoleExec, true)
	share(api.ShareSnapshot, "golden", "bob@example.com", "", false)
	share(api.ShareEnvironment, "web", "bob@example.com", api.RoleViewer, false)
	g, _ = s.MemberGrant(bob)
	if !g.Allows(api.TokenExec, "vm_a") || g.Allows(api.TokenAdmin, "vm_a") || g.Allows(api.TokenView, "vm_b") {
		t.Fatalf("vm scopes %+v", g.Scopes)
	}
	if !g.AllowsSnapshot(api.TokenView, "snap_1") || g.AllowsSnapshot(api.TokenExec, "snap_1") || !g.AllowsEnvironment(api.TokenView, "env_1") {
		t.Fatalf("resource shares %+v %+v", g.Snapshots, g.Environments)
	}
	if _, err := s.CreateShare(ctx, "owner", api.CreateShareRequest{Kind: api.ShareVM, Resource: "a", Member: "carol"}); code(err) != domain.CodeNotFound {
		t.Fatalf("share with a non-member: %v", err)
	}
	if _, err := s.CreateShare(ctx, "owner", api.CreateShareRequest{Kind: api.ShareVM, Resource: "zzz", Member: "*"}); code(err) != domain.CodeNotFound {
		t.Fatalf("share of a missing VM: %v", err)
	}

	// Credential grants: an exec share with credentials does not pause them;
	// the viewer share does not; replacing it with exec without credentials does.
	if err := s.CredentialsUsable("vm_a"); err != nil {
		t.Fatalf("credentials while shared with credentials: %v", err)
	}
	share(api.ShareVM, "a", "bob@example.com", api.RoleExec, false)
	if err := s.CredentialsUsable("vm_a"); code(err) != domain.CodePermissionDenied {
		t.Fatalf("credentials while shared exec without them: %v", err)
	}
	if len(*paused) != 1 || (*paused)[0] != "vm_a" {
		t.Fatalf("paused hook %v", *paused)
	}
	// The upsert changed bob's right on vm_a: his sessions there are cut.
	if got := c.take(); !slices.Equal(got, []string{"vm_a/2"}) {
		t.Fatalf("cuts after a role change %v", got)
	}

	// Unshare cuts; removing the member cuts everything and drops shares.
	if err := s.DeleteShare(ctx, all.ID); err != nil {
		t.Fatal(err)
	}
	if got := c.take(); !slices.Equal(got, []string{"vm_a/2"}) {
		t.Fatalf("cuts after unshare %v", got)
	}
	if err := s.RemoveMember(ctx, "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := c.take(); !slices.Equal(got, []string{"/2"}) {
		t.Fatalf("cuts after remove %v", got)
	}
	if l, _ := s.ListShares(ctx, "owner", "", ""); len(l.Items) != 0 {
		t.Fatalf("shares left %+v", l.Items)
	}
	if err := s.CredentialsUsable("vm_a"); err != nil {
		t.Fatalf("credentials after the member left: %v", err)
	}
}

// TestDefaultVisibility: VMs that existed when members were enabled stay
// private; new ones are shared with every member at its own role.
func TestDefaultVisibility(t *testing.T) {
	ctx := context.Background()
	s, res, _, _ := newService(t, VisibilityMembers)
	s.AddMember(ctx, api.AddMemberRequest{Login: "bob@example.com", Role: api.RoleExec})
	for primed := false; !primed; time.Sleep(5 * time.Millisecond) { // the existing VMs were recorded
		s.mu.Lock()
		primed = s.st.Primed
		s.mu.Unlock()
	}
	res.add(domain.VM{ID: "vm_new", Name: "new"})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		g, _ := s.MemberGrant(bob)
		if g.Allows(api.TokenExec, "vm_new") {
			if g.Allows(api.TokenView, "vm_a") || g.Allows(api.TokenAdmin, "vm_new") {
				t.Fatalf("grant %+v", g.Scopes)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new VM not shared with members")
		}
	}
	// The member's own role is exec: the default share pauses credentials.
	if err := s.CredentialsUsable("vm_new"); err == nil {
		t.Fatal("default exec visibility did not pause credential grants")
	}
}
