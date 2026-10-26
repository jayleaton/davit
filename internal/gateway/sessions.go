package gateway

import (
	"io"
	"sync"

	"davit/internal/domain"
	"davit/internal/tokens"
)

// MemberDirectory decides whether a tailnet identity that is not the owner
// is a member, and returns the grant of what is shared with it.
type MemberDirectory interface {
	MemberGrant(who Identity) (*tokens.Grant, bool)
}

// Sessions tracks every live connection a tailnet user holds to a VM (API
// streams, SSH front-door sessions, app endpoint connections, forwards) so
// that unsharing or removing a member cuts them at once. It is safe for
// concurrent use; the nil *Sessions tracks nothing.
type Sessions struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]session
}

type session struct {
	user int64
	vm   domain.VMID
	c    io.Closer
}

// CloserFunc adapts a function to io.Closer.
type CloserFunc func() error

// Close implements io.Closer.
func (f CloserFunc) Close() error { return f() }

// Track records that user holds c on vm ("" when the session is not about
// one VM); untrack forgets it (call it when c ends).
func (s *Sessions) Track(user int64, vm domain.VMID, c io.Closer) (untrack func()) {
	if s == nil || user == 0 {
		return func() {}
	}
	s.mu.Lock()
	if s.m == nil {
		s.m = map[uint64]session{}
	}
	s.next++
	id := s.next
	s.m[id] = session{user: user, vm: vm, c: c}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.m, id)
		s.mu.Unlock()
	}
}

// Cut closes the sessions of users on vm (every VM when vm is "", and
// sessions not about one VM always) and returns how many it closed.
func (s *Sessions) Cut(users []int64, vm domain.VMID) int {
	if s == nil || len(users) == 0 {
		return 0
	}
	want := map[int64]bool{}
	for _, u := range users {
		want[u] = true
	}
	s.mu.Lock()
	var victims []io.Closer
	for id, se := range s.m {
		if want[se.user] && (vm == "" || se.vm == "" || se.vm == vm) {
			victims = append(victims, se.c)
			delete(s.m, id)
		}
	}
	s.mu.Unlock()
	for _, c := range victims {
		c.Close()
	}
	return len(victims)
}
