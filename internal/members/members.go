// Package members is G13: tailnet users other than the owner, with rights
// only on what the owner shares (feature members).
//
// Members are kept in a JSON store in the tailnet state dir, next to the
// owner pins (owners.json), rather than in the daemon config or the SQL
// state store. Membership is meaningless without the tailnet gateway, it
// changes at runtime (share and unshare must take effect, and cut live
// sessions, without a restart), and its identity pins are gateway state like
// the owner's. A member is added by login and pinned to its stable tailnet
// user ID on first connection; from then on the user ID is the member, so a
// renamed login keeps working and a recycled one is refused.
//
// A member authenticates as a derived tokens.Grant: it runs as the owner
// principal, limited to its shares (viewer, exec, admin = the token actions
// view, exec, admin), and is otherwise invisible to the owner's resources.
// Changing a member's rights cuts its live sessions (gateway.Sessions).
package members

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/gateway"
	"davit/internal/lifecycle"
	"davit/internal/tokens"
)

// Default visibility of new VMs.
const (
	// VisibilityPrivate: new VMs are shared with nobody (the default).
	VisibilityPrivate = "private"
	// VisibilityMembers: a new VM is shared with every member at each
	// member's own role.
	VisibilityMembers = "members"
)

// reconcileEvery backstops lifecycle notifications.
const reconcileEvery = 2 * time.Second

// Resources resolves the owner's VMs, snapshots and environments
// (lifecycle.Service).
type Resources interface {
	GetVM(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.VM, error)
	ListVMs(ctx context.Context, principal domain.PrincipalID, states []domain.VMState, all bool, p lifecycle.Page) ([]domain.VM, string, error)
	GetSnapshot(ctx context.Context, principal domain.PrincipalID, ref string) (*domain.Snapshot, error)
	GetEnvironment(ctx context.Context, owner domain.PrincipalID, ref string) (*domain.Environment, error)
	Changed() <-chan struct{}
}

// Cutter closes users' live sessions on a VM ("" every VM) (gateway.Sessions).
type Cutter interface {
	Cut(users []int64, vm domain.VMID) int
}

// Config configures a Service.
type Config struct {
	// Path is the JSON store.
	Path string
	// Owner is the owner principal members act within.
	Owner     domain.PrincipalID
	Resources Resources
	// DefaultVisibility is VisibilityPrivate (also "") or VisibilityMembers.
	DefaultVisibility string
	Sessions          Cutter
	// OnCredentialsPaused runs when a share starts pausing vm's credential
	// grants (cut in-flight proxied requests).
	OnCredentialsPaused func(vm domain.VMID)
	Log                 *slog.Logger
}

type member struct {
	ID      string         `json:"id"`
	Login   string         `json:"login"`
	UserID  int64          `json:"userId,omitempty"`
	Role    api.MemberRole `json:"role"`
	AddedAt time.Time      `json:"addedAt"`
}

type share struct {
	ID          string         `json:"id"`
	Kind        api.ShareKind  `json:"kind"`
	ResourceID  string         `json:"resourceId"`
	Member      string         `json:"member"`
	Role        api.MemberRole `json:"role,omitempty"`
	Credentials bool           `json:"credentials,omitempty"`
	// VM is the VM whose sessions this share's revocation cuts (the VM
	// itself, or a snapshot's source VM).
	VM        domain.VMID `json:"vm,omitempty"`
	CreatedAt time.Time   `json:"createdAt"`
}

type store struct {
	Members []*member `json:"members"`
	Shares  []*share  `json:"shares"`
	// Seen are VMs that got the default visibility already.
	Seen map[domain.VMID]bool `json:"seenVMs"`
	// Primed is set once the VMs that existed when members were first
	// enabled were recorded (they keep their privacy).
	Primed bool `json:"primed"`
}

// Service is the members and shares control plane and directory.
type Service struct {
	cfg  Config
	log  *slog.Logger
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu sync.Mutex
	st store
}

// New loads the store and starts the default-visibility watcher.
func New(cfg Config) (*Service, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	switch cfg.DefaultVisibility {
	case "", VisibilityPrivate, VisibilityMembers:
	default:
		return nil, fmt.Errorf("members: default visibility %q (want %q or %q)", cfg.DefaultVisibility, VisibilityPrivate, VisibilityMembers)
	}
	s := &Service{cfg: cfg, log: cfg.Log, st: store{Seen: map[domain.VMID]bool{}}}
	if b, err := os.ReadFile(cfg.Path); err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			return nil, fmt.Errorf("members: %s: %w", cfg.Path, err)
		}
		if s.st.Seen == nil {
			s.st.Seen = map[domain.VMID]bool{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.wg.Go(func() { s.watch(ctx) })
	return s, nil
}

// Close stops the watcher.
func (s *Service) Close() {
	s.stop()
	s.wg.Wait()
}

// save persists the store; s.mu held.
func (s *Service) save() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.Path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.cfg.Path), ".members-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(b)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.cfg.Path)
}

func validRole(r api.MemberRole) bool {
	return r == api.RoleViewer || r == api.RoleExec || r == api.RoleAdmin
}

// action maps a role to the token action it equals.
func action(r api.MemberRole) string {
	switch r {
	case api.RoleAdmin:
		return api.TokenAdmin
	case api.RoleExec:
		return api.TokenExec
	}
	return api.TokenView
}

func rank(a string) int {
	switch a {
	case api.TokenAdmin:
		return 3
	case api.TokenExec:
		return 2
	case api.TokenView:
		return 1
	}
	return 0
}

func (m *member) view() api.Member {
	return api.Member{ID: m.ID, Login: m.Login, UserID: m.UserID, Role: m.Role, AddedAt: m.AddedAt}
}

func (sh *share) view() api.Share {
	return api.Share{ID: sh.ID, Kind: sh.Kind, ResourceID: sh.ResourceID, Member: sh.Member, Role: sh.Role,
		Credentials: sh.Credentials, CreatedAt: sh.CreatedAt}
}

// findMember resolves a member ID or login; s.mu held.
func (s *Service) findMember(ref string) *member {
	for _, m := range s.st.Members {
		if m.ID == ref || strings.EqualFold(m.Login, ref) {
			return m
		}
	}
	return nil
}

// usersOf are the pinned user IDs a share applies to; s.mu held.
func (s *Service) usersOf(memberRef string) []int64 {
	var out []int64
	for _, m := range s.st.Members {
		if m.UserID != 0 && (memberRef == api.ShareAllMembers || m.ID == memberRef) {
			out = append(out, m.UserID)
		}
	}
	return out
}

// MemberGrant implements gateway.MemberDirectory: the grant of a tailnet
// identity that is a member (pinning its user ID on first sight).
func (s *Service) MemberGrant(who gateway.Identity) (*tokens.Grant, bool) {
	if who.UserID == 0 || len(who.Tags) > 0 {
		return nil, false // members are tailnet users, not tagged nodes
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var m *member
	for _, c := range s.st.Members {
		if c.UserID == who.UserID {
			m = c
			break
		}
	}
	if m == nil {
		for _, c := range s.st.Members {
			if c.UserID == 0 && who.LoginName != "" && strings.EqualFold(c.Login, who.LoginName) {
				c.UserID = who.UserID
				if err := s.save(); err != nil {
					c.UserID = 0
					s.log.Warn("members: cannot pin member; refused", "login", c.Login, "err", err)
					return nil, false
				}
				s.log.Info("member pinned to tailnet user", "login", c.Login, "userId", who.UserID)
				m = c
				break
			}
		}
	}
	if m == nil {
		return nil, false
	}
	g := &tokens.Grant{TokenID: "member:" + m.ID, Member: &tokens.Member{ID: m.ID, Login: m.Login, UserID: m.UserID},
		Snapshots: map[string]string{}, Environments: map[string]string{}}
	vms := map[domain.VMID]string{}
	for _, sh := range s.st.Shares {
		if sh.Member != m.ID && sh.Member != api.ShareAllMembers {
			continue
		}
		role := sh.Role
		if role == "" {
			role = m.Role
		}
		a := action(role)
		var into map[string]string
		switch sh.Kind {
		case api.ShareVM:
			if rank(a) > rank(vms[domain.VMID(sh.ResourceID)]) {
				vms[domain.VMID(sh.ResourceID)] = a
			}
			continue
		case api.ShareSnapshot:
			into = g.Snapshots
		case api.ShareEnvironment:
			into = g.Environments
		default:
			continue
		}
		if rank(a) > rank(into[sh.ResourceID]) {
			into[sh.ResourceID] = a
		}
	}
	for vm, a := range vms {
		g.Scopes = append(g.Scopes, api.TokenScope{VMID: vm, Actions: []string{a}})
	}
	return g, true
}

// CredentialsUsable reports whether vm's credential grants may be used:
// not while it is shared with exec or admin to a member without the
// credentials flag (one guest cannot tell its users apart).
func (s *Service) CredentialsUsable(vm domain.VMID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pausesLocked(vm) {
		return domain.Errorf(domain.CodePermissionDenied,
			"credential grants of vm %s are paused while it is shared with exec or admin without credentials", vm)
	}
	return nil
}

func (s *Service) pausesLocked(vm domain.VMID) bool {
	for _, sh := range s.st.Shares {
		if sh.Kind != api.ShareVM || sh.ResourceID != string(vm) || sh.Credentials {
			continue
		}
		if sh.Role != "" {
			if rank(action(sh.Role)) >= rank(api.TokenExec) {
				return true
			}
			continue
		}
		for _, m := range s.st.Members { // each member at its own role
			if (sh.Member == api.ShareAllMembers || sh.Member == m.ID) && rank(action(m.Role)) >= rank(api.TokenExec) {
				return true
			}
		}
	}
	return false
}

// ListMembers returns the members, oldest first.
func (s *Service) ListMembers(context.Context) (api.MemberList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := api.MemberList{Items: []api.Member{}}
	for _, m := range s.st.Members {
		out.Items = append(out.Items, m.view())
	}
	return out, nil
}

// AddMember adds a tailnet login.
func (s *Service) AddMember(_ context.Context, req api.AddMemberRequest) (api.Member, error) {
	login := strings.TrimSpace(req.Login)
	if login == "" || strings.ContainsAny(login, " \t\n/") || login == api.ShareAllMembers {
		return api.Member{}, domain.Errorf(domain.CodeInvalidArgument, "login must be a tailnet login name").WithDetail("field", "login")
	}
	if !validRole(req.Role) {
		return api.Member{}, domain.Errorf(domain.CodeInvalidArgument, "role must be viewer, exec or admin").WithDetail("field", "role")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.findMember(login); m != nil {
		return api.Member{}, domain.Errorf(domain.CodeInvalidState, "%s is already member %s", login, m.ID).WithDetail("memberId", m.ID)
	}
	m := &member{ID: domain.NewID("mem"), Login: login, Role: req.Role, AddedAt: time.Now().UTC()}
	s.st.Members = append(s.st.Members, m)
	if err := s.save(); err != nil {
		s.st.Members = s.st.Members[:len(s.st.Members)-1]
		return api.Member{}, err
	}
	s.log.Info("member added", "id", m.ID, "login", login, "role", req.Role)
	return m.view(), nil
}

// UpdateMember changes a member's role and cuts its live sessions.
func (s *Service) UpdateMember(_ context.Context, ref string, req api.UpdateMemberRequest) (api.Member, error) {
	if !validRole(req.Role) {
		return api.Member{}, domain.Errorf(domain.CodeInvalidArgument, "role must be viewer, exec or admin").WithDetail("field", "role")
	}
	s.mu.Lock()
	m := s.findMember(ref)
	if m == nil {
		s.mu.Unlock()
		return api.Member{}, domain.Errorf(domain.CodeNotFound, "member %q not found", ref)
	}
	prev := m.Role
	before := s.pausedVMsLocked()
	m.Role = req.Role
	paused := newly(before, s.pausedVMsLocked())
	if err := s.save(); err != nil {
		m.Role = prev
		s.mu.Unlock()
		return api.Member{}, err
	}
	out, uid := m.view(), m.UserID
	s.mu.Unlock()
	if prev != req.Role {
		s.cut([]int64{uid}, "")
		s.paused(paused)
	}
	return out, nil
}

// RemoveMember removes a member and its shares, cutting its sessions.
func (s *Service) RemoveMember(_ context.Context, ref string) error {
	s.mu.Lock()
	m := s.findMember(ref)
	if m == nil {
		s.mu.Unlock()
		return domain.Errorf(domain.CodeNotFound, "member %q not found", ref)
	}
	members, shares := s.st.Members, s.st.Shares
	s.st.Members = slices.DeleteFunc(slices.Clone(members), func(c *member) bool { return c == m })
	s.st.Shares = slices.DeleteFunc(slices.Clone(shares), func(sh *share) bool { return sh.Member == m.ID })
	if err := s.save(); err != nil {
		s.st.Members, s.st.Shares = members, shares
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	s.cut([]int64{m.UserID}, "")
	s.log.Info("member removed", "id", m.ID, "login", m.Login)
	return nil
}

// resolve finds a share resource's ID and the VM its sessions belong to.
func (s *Service) resolve(ctx context.Context, p domain.PrincipalID, kind api.ShareKind, ref string) (id string, vm domain.VMID, err error) {
	switch kind {
	case api.ShareVM:
		v, err := s.cfg.Resources.GetVM(ctx, p, ref)
		if err != nil {
			return "", "", err
		}
		return string(v.ID), v.ID, nil
	case api.ShareSnapshot:
		sn, err := s.cfg.Resources.GetSnapshot(ctx, p, ref)
		if err != nil {
			return "", "", err
		}
		return string(sn.ID), sn.SourceVMID, nil
	case api.ShareEnvironment:
		e, err := s.cfg.Resources.GetEnvironment(ctx, p, ref)
		if err != nil {
			return "", "", err
		}
		return string(e.ID), "", nil
	}
	return "", "", domain.Errorf(domain.CodeInvalidArgument, "kind must be vm, snapshot or environment").WithDetail("field", "kind")
}

// ListShares lists shares, optionally of one resource.
func (s *Service) ListShares(ctx context.Context, p domain.PrincipalID, kind api.ShareKind, resource string) (api.ShareList, error) {
	id := ""
	if resource != "" {
		var err error
		if id, _, err = s.resolve(ctx, p, kind, resource); err != nil {
			return api.ShareList{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := api.ShareList{Items: []api.Share{}}
	for _, sh := range s.st.Shares {
		if (kind == "" || sh.Kind == kind) && (id == "" || sh.ResourceID == id) {
			out.Items = append(out.Items, sh.view())
		}
	}
	return out, nil
}

// CreateShare shares a resource (an upsert per resource and member).
func (s *Service) CreateShare(ctx context.Context, p domain.PrincipalID, req api.CreateShareRequest) (api.Share, error) {
	if req.Role != "" && !validRole(req.Role) {
		return api.Share{}, domain.Errorf(domain.CodeInvalidArgument, "role must be viewer, exec or admin").WithDetail("field", "role")
	}
	if req.Credentials && req.Kind != api.ShareVM {
		return api.Share{}, domain.Errorf(domain.CodeInvalidArgument, "credentials applies to VM shares").WithDetail("field", "credentials")
	}
	id, vm, err := s.resolve(ctx, p, req.Kind, req.Resource)
	if err != nil {
		return api.Share{}, err
	}
	s.mu.Lock()
	memberID := api.ShareAllMembers
	if req.Member != api.ShareAllMembers {
		m := s.findMember(req.Member)
		if m == nil {
			s.mu.Unlock()
			return api.Share{}, domain.Errorf(domain.CodeNotFound, "member %q not found (add it with addMember)", req.Member).WithDetail("field", "member")
		}
		memberID = m.ID
	}
	role := req.Role
	if role == "" && memberID != api.ShareAllMembers {
		role = s.findMember(memberID).Role
	}
	before := s.pausedVMsLocked()
	var sh *share
	for _, c := range s.st.Shares {
		if c.Kind == req.Kind && c.ResourceID == id && c.Member == memberID {
			sh = c
		}
	}
	replaced := sh != nil
	var prev share
	if replaced {
		prev = *sh
		sh.Role, sh.Credentials = role, req.Credentials
	} else {
		sh = &share{ID: domain.NewID("shr"), Kind: req.Kind, ResourceID: id, Member: memberID, Role: role,
			Credentials: req.Credentials, VM: vm, CreatedAt: time.Now().UTC()}
		s.st.Shares = append(s.st.Shares, sh)
	}
	if err := s.save(); err != nil {
		if replaced {
			*sh = prev
		} else {
			s.st.Shares = s.st.Shares[:len(s.st.Shares)-1]
		}
		s.mu.Unlock()
		return api.Share{}, err
	}
	out, users := sh.view(), s.usersOf(memberID)
	after := s.pausedVMsLocked()
	s.mu.Unlock()
	if replaced && (prev.Role != sh.Role || prev.Credentials != sh.Credentials) {
		s.cut(users, vm) // a changed right starts fresh sessions
	}
	s.paused(newly(before, after))
	s.log.Info("shared", "kind", req.Kind, "resource", id, "member", memberID, "role", role, "credentials", req.Credentials)
	return out, nil
}

// DeleteShare unshares and cuts the affected members' sessions.
func (s *Service) DeleteShare(_ context.Context, id string) error {
	s.mu.Lock()
	i := slices.IndexFunc(s.st.Shares, func(sh *share) bool { return sh.ID == id })
	if i < 0 {
		s.mu.Unlock()
		return domain.Errorf(domain.CodeNotFound, "share %q not found", id)
	}
	sh := s.st.Shares[i]
	shares := s.st.Shares
	s.st.Shares = slices.Delete(slices.Clone(shares), i, i+1)
	if err := s.save(); err != nil {
		s.st.Shares = shares
		s.mu.Unlock()
		return err
	}
	users := s.usersOf(sh.Member)
	s.mu.Unlock()
	n := s.cut(users, sh.VM)
	s.log.Info("unshared", "kind", sh.Kind, "resource", sh.ResourceID, "member", sh.Member, "sessionsCut", n)
	return nil
}

func (s *Service) cut(users []int64, vm domain.VMID) int {
	users = slices.DeleteFunc(users, func(u int64) bool { return u == 0 })
	if s.cfg.Sessions == nil || len(users) == 0 {
		return 0
	}
	return s.cfg.Sessions.Cut(users, vm)
}

// pausedVMsLocked are the VMs whose credential grants are paused.
func (s *Service) pausedVMsLocked() map[domain.VMID]bool {
	out := map[domain.VMID]bool{}
	for _, sh := range s.st.Shares {
		if sh.Kind == api.ShareVM && !out[domain.VMID(sh.ResourceID)] && s.pausesLocked(domain.VMID(sh.ResourceID)) {
			out[domain.VMID(sh.ResourceID)] = true
		}
	}
	return out
}

func newly(before, after map[domain.VMID]bool) map[domain.VMID]bool {
	out := map[domain.VMID]bool{}
	for vm := range after {
		if !before[vm] {
			out[vm] = true
		}
	}
	return out
}

func (s *Service) paused(vms map[domain.VMID]bool) {
	if s.cfg.OnCredentialsPaused == nil {
		return
	}
	for vm := range vms {
		s.cfg.OnCredentialsPaused(vm)
	}
}

func (s *Service) watch(ctx context.Context) {
	tick := time.NewTicker(reconcileEvery)
	defer tick.Stop()
	for {
		changed := s.cfg.Resources.Changed()
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-tick.C:
		}
	}
}

// reconcile applies the default visibility to VMs not seen before (VMs that
// existed when members were first enabled stay private) and drops shares of
// deleted VMs.
func (s *Service) reconcile(ctx context.Context) {
	var vms []domain.VM
	page := lifecycle.Page{Limit: api.MaxListLimit}
	for {
		batch, next, err := s.cfg.Resources.ListVMs(ctx, s.cfg.Owner, nil, true, page)
		if err != nil {
			return
		}
		vms = append(vms, batch...)
		if next == "" {
			break
		}
		page.Cursor = next
	}
	live := map[domain.VMID]bool{}
	s.mu.Lock()
	changed := false
	paused := s.pausedVMsLocked()
	for _, vm := range vms {
		if vm.State == domain.StateDeleted {
			continue
		}
		live[vm.ID] = true
		if s.st.Seen[vm.ID] {
			continue
		}
		s.st.Seen[vm.ID], changed = true, true
		if s.st.Primed && s.cfg.DefaultVisibility == VisibilityMembers {
			s.st.Shares = append(s.st.Shares, &share{ID: domain.NewID("shr"), Kind: api.ShareVM, ResourceID: string(vm.ID),
				Member: api.ShareAllMembers, VM: vm.ID, CreatedAt: time.Now().UTC()})
		}
	}
	if !s.st.Primed {
		s.st.Primed, changed = true, true
	}
	for vm := range s.st.Seen {
		if !live[vm] {
			delete(s.st.Seen, vm)
			changed = true
		}
	}
	n := len(s.st.Shares)
	s.st.Shares = slices.DeleteFunc(s.st.Shares, func(sh *share) bool { return sh.Kind == api.ShareVM && !live[domain.VMID(sh.ResourceID)] })
	changed = changed || len(s.st.Shares) != n
	if changed {
		if err := s.save(); err != nil {
			s.log.Warn("members: save", "err", err)
		}
	}
	after := s.pausedVMsLocked()
	s.mu.Unlock()
	s.paused(newly(paused, after))
}
