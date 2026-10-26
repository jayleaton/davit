package httpapi

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"sync"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/tokens"
)

// Members (G13): a tailnet member authenticates with a derived grant
// (tokens.Grant.Member) and runs as the owner principal limited to its
// shares. Unlike a token it gets 404 for what is not shared with it, sees
// filtered lists, and is refused member-excluded routes whatever its role.

// SessionTracker records a member's live request so that unsharing cuts it
// (gateway.Sessions).
type SessionTracker interface {
	Track(user int64, vm domain.VMID, c io.Closer) (untrack func())
}

// MembersAPI is the members and shares control plane (members.Service).
type MembersAPI interface {
	ListMembers(ctx context.Context) (api.MemberList, error)
	AddMember(ctx context.Context, req api.AddMemberRequest) (api.Member, error)
	UpdateMember(ctx context.Context, ref string, req api.UpdateMemberRequest) (api.Member, error)
	RemoveMember(ctx context.Context, ref string) error
	ListShares(ctx context.Context, p domain.PrincipalID, kind api.ShareKind, resource string) (api.ShareList, error)
	CreateShare(ctx context.Context, p domain.PrincipalID, req api.CreateShareRequest) (api.Share, error)
	DeleteShare(ctx context.Context, id string) error
}

// SetMembers enables the members and shares operations and member session
// tracking. Without it they answer unsupported_capability.
func (s *Server) SetMembers(m MembersAPI, t SessionTracker) { s.members, s.sessions = m, t }

const keyGrant ctxKey = 100

// grantOf is the request's token or member grant (nil for the owner).
func grantOf(r *http.Request) *tokens.Grant {
	g, _ := r.Context().Value(keyGrant).(*tokens.Grant)
	return g
}

// pollCaller names the grant a long-poll counts against
// (events.Query.Caller): "" for the owner's own session.
func pollCaller(r *http.Request) string {
	g := grantOf(r)
	switch {
	case g == nil:
		return ""
	case g.Member != nil:
		return "member:" + g.Member.ID
	}
	return "token:" + g.TokenID
}

// memberOf is the request's member grant, nil for the owner and tokens.
func memberOf(r *http.Request) *tokens.Grant {
	if g := grantOf(r); g != nil && g.Member != nil {
		return g
	}
	return nil
}

// Access spec extensions for members.

// listAll is a list across VMs: tokens need an all-VM scope, members get
// the list filtered to what is shared with them.
func listAll(action string) access { return access{action: action, list: true} }

// ownerOrMemberList is an owner-only list (no token) that members may read
// filtered (environments).
func ownerOrMemberList() access { return access{owner: true, list: true} }

// noMembers refuses members whatever their role on the VM: routes that
// create host resources (forks) or reach credential grants.
func noMembers(a access) access { a.noMembers = true; return a }

// onEnvironment: the owner; members with an environment share; no token.
func onEnvironment(action string) access { return access{action: action, owner: true, env: true} }

func (s *Server) checkMember(r *http.Request, p domain.PrincipalID, g *tokens.Grant, need access) (domain.VMID, error) {
	refuse := func() error {
		return domain.Errorf(domain.CodePermissionDenied, "member %s may not do this here", g.Member.Login).
			WithDetail("member", g.Member.ID)
	}
	switch {
	case need.noMembers:
		return "", refuse()
	case need.env:
		env, err := s.svc.GetEnvironment(r.Context(), p, r.PathValue("environmentId"))
		if err != nil {
			return "", err
		}
		if !g.AllowsEnvironment(need.action, string(env.ID)) {
			return "", domain.Errorf(domain.CodeNotFound, "environment %q not found", r.PathValue("environmentId"))
		}
		return "", nil
	case need.list:
		return "", nil // the handler filters
	case need.owner:
		return "", refuse()
	case need.any:
		return "", nil
	case need.snapshot:
		snap, err := s.svc.GetSnapshot(r.Context(), p, r.PathValue("snapshotId"))
		if err != nil {
			return "", err
		}
		if !g.Allows(api.TokenView, snap.SourceVMID) && !g.AllowsSnapshot(api.TokenView, string(snap.ID)) {
			return "", domain.Errorf(domain.CodeNotFound, "snapshot %q not found", r.PathValue("snapshotId"))
		}
		if !g.Allows(need.action, snap.SourceVMID) && !g.AllowsSnapshot(need.action, string(snap.ID)) {
			return "", refuse()
		}
		return snap.SourceVMID, nil
	case need.vm == nil:
		return "", refuse() // creates across VMs (createVM, runs)
	}
	vm, err := need.vm(s, r, p)
	if err != nil {
		return "", err
	}
	if !g.Allows(api.TokenView, vm) {
		return "", domain.Errorf(domain.CodeNotFound, "not found") // invisible to the member
	}
	if !g.Allows(need.action, vm) {
		return "", refuse()
	}
	return vm, nil
}

// trackedWriter records the connection a handler hijacks (WebSocket
// streams) so that a cut closes it.
type trackedWriter struct {
	http.ResponseWriter
	mu     sync.Mutex
	conn   net.Conn
	closed bool
}

func (t *trackedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	c, rw, err := h.Hijack()
	if err == nil {
		t.mu.Lock()
		t.conn = c
		if t.closed {
			c.Close()
		}
		t.mu.Unlock()
	}
	return c, rw, err
}

func (t *trackedWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (t *trackedWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

func (t *trackedWriter) cut() {
	t.mu.Lock()
	t.closed = true
	c := t.conn
	t.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// trackMember makes a member's request revocable: a cut cancels its
// context and closes a hijacked connection.
func (s *Server) trackMember(w http.ResponseWriter, r *http.Request, g *tokens.Grant, vm domain.VMID) (http.ResponseWriter, *http.Request, func()) {
	if s.sessions == nil {
		return w, r, func() {}
	}
	ctx, cancel := context.WithCancel(r.Context())
	tw := &trackedWriter{ResponseWriter: w}
	untrack := s.sessions.Track(g.Member.UserID, vm, closerFunc(func() error { cancel(); tw.cut(); return nil }))
	return tw, r.WithContext(ctx), func() { untrack(); cancel() }
}

// trackToken makes a token's request revocable the same way: revoking,
// rotating or expiring the token cancels its context and closes a hijacked
// stream.
func (s *Server) trackToken(w http.ResponseWriter, r *http.Request, g *tokens.Grant) (http.ResponseWriter, *http.Request, func()) {
	if s.tokens == nil {
		return w, r, func() {}
	}
	ctx, cancel := context.WithCancel(r.Context())
	tw := &trackedWriter{ResponseWriter: w}
	untrack := s.tokens.Track(g, closerFunc(func() error { cancel(); tw.cut(); return nil }))
	return tw, r.WithContext(ctx), func() { untrack(); cancel() }
}

// memberVMs filters vms to those the member may view.
func memberVMs(g *tokens.Grant, vms []domain.VM) []domain.VM {
	out := vms[:0:0]
	for _, vm := range vms {
		if g.Allows(api.TokenView, vm.ID) {
			out = append(out, vm)
		}
	}
	return out
}

func memberSnapshots(g *tokens.Grant, snaps []domain.Snapshot) []domain.Snapshot {
	out := snaps[:0:0]
	for _, sn := range snaps {
		if g.Allows(api.TokenView, sn.SourceVMID) || g.AllowsSnapshot(api.TokenView, string(sn.ID)) {
			out = append(out, sn)
		}
	}
	return out
}

func memberEnvironments(g *tokens.Grant, envs []domain.Environment) []domain.Environment {
	out := envs[:0:0]
	for _, e := range envs {
		if g.AllowsEnvironment(api.TokenView, string(e.ID)) {
			out = append(out, e)
		}
	}
	return out
}

// Members and shares operations (owner only).

func (s *Server) membersService() (MembersAPI, error) {
	if s.members == nil {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "members need the tailnet gateway (tailnet.enabled)").
			WithDetail("feature", domain.FeatureMembers)
	}
	return s.members, nil
}

func (s *Server) memberRoutes(route func(string, access, handler)) {
	b := api.BasePath
	route("GET "+b+"/members", ownerOnly, s.listMembers)
	route("POST "+b+"/members", ownerOnly, s.addMember)
	route("PATCH "+b+"/members/{memberId}", ownerOnly, s.updateMember)
	route("DELETE "+b+"/members/{memberId}", ownerOnly, s.removeMember)
	route("GET "+b+"/shares", ownerOnly, s.listShares)
	route("POST "+b+"/shares", ownerOnly, s.createShare)
	route("DELETE "+b+"/shares/{shareId}", ownerOnly, s.deleteShare)
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) error {
	m, err := s.membersService()
	if err != nil {
		return err
	}
	l, err := m.ListMembers(r.Context())
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) error {
	m, err := s.membersService()
	if err != nil {
		return err
	}
	var body api.AddMemberRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	mem, err := m.AddMember(r.Context(), body)
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/members/"+mem.ID)
	return writeJSON(w, http.StatusCreated, mem)
}

func (s *Server) updateMember(w http.ResponseWriter, r *http.Request) error {
	m, err := s.membersService()
	if err != nil {
		return err
	}
	var body api.UpdateMemberRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	mem, err := m.UpdateMember(r.Context(), r.PathValue("memberId"), body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, mem)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) error {
	m, err := s.membersService()
	if err != nil {
		return err
	}
	if err := m.RemoveMember(r.Context(), r.PathValue("memberId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) error {
	m, err := s.membersService()
	if err != nil {
		return err
	}
	q := r.URL.Query()
	l, err := m.ListShares(r.Context(), principal(r), api.ShareKind(q.Get("kind")), q.Get("resource"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}

func (s *Server) createShare(w http.ResponseWriter, r *http.Request) error {
	m, err := s.membersService()
	if err != nil {
		return err
	}
	var body api.CreateShareRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	sh, err := m.CreateShare(r.Context(), principal(r), body)
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/shares/"+sh.ID)
	return writeJSON(w, http.StatusCreated, sh)
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request) error {
	m, err := s.membersService()
	if err != nil {
		return err
	}
	if err := m.DeleteShare(r.Context(), r.PathValue("shareId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
