package tokens

import (
	"io"
	"time"
)

// Live sessions (SEC-4): Verify runs on every request, so a revoked token
// is refused at once, but a stream it already opened (a tunnel, an exec
// attach, a long poll) would outlive it. Track records such a session; it
// is closed when the token is revoked, rotated without overlap, at the end
// of a rotation overlap, or when the token expires.

type tracked struct {
	token string
	c     io.Closer
	timer *time.Timer
}

// Track records c as a live session of g's token until the returned
// function runs (call it when the session ends).
func (s *Service) Track(g *Grant, c io.Closer) (untrack func()) {
	if g == nil || g.TokenID == "" || g.Member != nil {
		return func() {}
	}
	s.mu.Lock()
	if s.live == nil {
		s.live = map[uint64]*tracked{}
	}
	s.nextLive++
	id := s.nextLive
	t := &tracked{token: g.TokenID, c: c}
	if !g.ExpiresAt.IsZero() {
		t.timer = time.AfterFunc(time.Until(g.ExpiresAt), func() { s.cutSession(id) })
	}
	s.live[id] = t
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		if t := s.live[id]; t != nil && t.timer != nil {
			t.timer.Stop()
		}
		delete(s.live, id)
		s.mu.Unlock()
	}
}

func (s *Service) cutSession(id uint64) {
	s.mu.Lock()
	t := s.live[id]
	delete(s.live, id)
	s.mu.Unlock()
	if t != nil {
		t.c.Close()
	}
}

// cut closes every live session of token id.
func (s *Service) cut(id string) {
	s.mu.Lock()
	var victims []*tracked
	for k, t := range s.live {
		if t.token == id {
			victims = append(victims, t)
			delete(s.live, k)
		}
	}
	s.mu.Unlock()
	for _, t := range victims {
		if t.timer != nil {
			t.timer.Stop()
		}
		t.c.Close()
	}
}

// cutAt closes token id's live sessions at end (its shortened expiry).
func (s *Service) cutAt(id string, end time.Time) {
	time.AfterFunc(time.Until(end), func() { s.cut(id) })
}
