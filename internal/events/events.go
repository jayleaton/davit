// Package events serves the host event stream (G16): VM, operation, run and
// prompt changes in one cursor-ordered sequence. The stream is written by
// SQLite triggers in the same transaction as each change (state migration
// 7), so it is lossless and ordered and lifecycle code is unchanged; this
// package reads, filters, long-polls and prunes it.
//
// Event data is an allowlisted summary chosen by the triggers: IDs, states,
// generations, error codes. Environment values, prompt text and secrets are
// never copied, so nothing downstream (API, webhooks, OTLP) can leak them.
package events

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

const (
	defaultLimit = 50
	maxLimit     = 200
	// MaxWait bounds one long-poll.
	MaxWait = 30 * time.Second
	// DefaultRetention keeps a week of events.
	DefaultRetention = 7 * 24 * time.Hour
)

// Service reads the stream.
type Service struct {
	DB *state.DB
	// Changed wakes long-polls on lifecycle journal writes
	// (lifecycle.Service.Changed); nil polls only.
	Changed func() <-chan struct{}
	// Poll is the fallback interval for changes Changed does not signal
	// (prompts, runs); default 250 ms.
	Poll time.Duration
	// Retention prunes older events; default DefaultRetention.
	Retention time.Duration
	// MaxWaiters, MaxWaitersPerCaller and MaxWaitersPerOwner bound
	// long-polls waiting at once (each holds a request and a timer for up
	// to MaxWait); default 1024 per host, 64 per caller (Query.Caller: each
	// token, each member, the principal's own session) and 4× per caller
	// for all grants of one principal together. The principal's own session
	// is not counted in that ceiling, so its grants cannot starve it. A
	// list that would wait beyond them is refused resource_exhausted
	// (retryable); one that does not wait is not.
	MaxWaiters, MaxWaitersPerCaller, MaxWaitersPerOwner int

	mu      sync.Mutex
	callers map[waiterKey]int          // per caller
	grants  map[domain.PrincipalID]int // per principal, grants only
	waiting int
}

type waiterKey struct {
	principal domain.PrincipalID
	caller    string
}

// enter takes a long-poll slot for caller of principal; leave gives it back.
func (s *Service) enter(principal domain.PrincipalID, caller string) error {
	maxAll, maxOne, maxOwner := s.MaxWaiters, s.MaxWaitersPerCaller, s.MaxWaitersPerOwner
	if maxAll <= 0 {
		maxAll = 1024
	}
	if maxOne <= 0 {
		maxOne = 64
	}
	if maxOwner <= 0 {
		maxOwner = 4 * maxOne
	}
	k := waiterKey{principal, caller}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callers == nil {
		s.callers, s.grants = map[waiterKey]int{}, map[domain.PrincipalID]int{}
	}
	if s.waiting >= maxAll || s.callers[k] >= maxOne || (caller != "" && s.grants[principal] >= maxOwner) {
		e := domain.Errorf(domain.CodeResourceExhausted, "too many event long-polls waiting (limits %d per caller, %d for an owner's tokens and members, %d per host); retry", maxOne, maxOwner, maxAll)
		e.Retryable = true
		return e
	}
	s.waiting++
	s.callers[k]++
	if caller != "" {
		s.grants[principal]++
	}
	return nil
}

// Waiting is the number of long-polls waiting now.
func (s *Service) Waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waiting
}

func (s *Service) leave(principal domain.PrincipalID, caller string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waiting--
	k := waiterKey{principal, caller}
	if s.callers[k]--; s.callers[k] == 0 {
		delete(s.callers, k)
	}
	if caller != "" {
		if s.grants[principal]--; s.grants[principal] == 0 {
			delete(s.grants, principal)
		}
	}
}

// Query is one listEvents request.
type Query struct {
	Cursor string
	Limit  int
	Types  []string
	VMID   domain.VMID
	// Latest starts after the newest event when there is no cursor.
	Latest bool
	Wait   time.Duration
	// Caller is the grant the request came with ("token:<id>",
	// "member:<id>"); "" is the principal's own session.
	Caller string
}

var typeRE = regexp.MustCompile(`^[a-z]+(\.[a-z]+)?\.?$`)

// ParseTypes splits and checks a comma-separated type filter.
func ParseTypes(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	for _, t := range strings.Split(s, ",") {
		t = strings.TrimSpace(t)
		if !typeRE.MatchString(t) {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "bad event type %q (a type like vm.state, or a prefix like vm.)", t).WithDetail("field", "types")
		}
		out = append(out, t)
	}
	return out, nil
}

type cursor struct {
	Seq    int64  `json:"s"`
	Filter string `json:"f"`
}

func filterKey(q Query) string { return strings.Join(q.Types, ",") + "|" + string(q.VMID) }

func encode(seq int64, filter string) string {
	b, _ := json.Marshal(cursor{Seq: seq, Filter: filter})
	return base64.RawURLEncoding.EncodeToString(b)
}

// Render turns a stored row into an API event, dropping absent (null)
// data fields.
func Render(r state.EventRow) api.Event {
	data := map[string]any{}
	json.Unmarshal(r.Data, &data)
	for k, v := range data {
		if v == nil {
			delete(data, k)
		}
	}
	return api.Event{Seq: r.Seq, Type: r.Type, Time: time.Unix(0, r.NS).UTC(), VMID: r.VMID, Subject: r.Subject, Data: data}
}

// List returns principal's events after the cursor, waiting up to q.Wait
// for the first one.
func (s *Service) List(ctx context.Context, principal domain.PrincipalID, q Query) (*api.EventList, error) {
	if principal == "" {
		return nil, domain.Errorf(domain.CodeUnauthenticated, "no principal")
	}
	switch {
	case q.Limit == 0:
		q.Limit = defaultLimit
	case q.Limit < 1 || q.Limit > maxLimit:
		return nil, domain.Errorf(domain.CodeInvalidArgument, "limit must be 1..%d", maxLimit).WithDetail("field", "limit")
	}
	if q.Wait < 0 || q.Wait > MaxWait {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "waitMs must be 0..%d", MaxWait.Milliseconds()).WithDetail("field", "waitMs")
	}
	filter := filterKey(q)
	var after int64
	switch {
	case q.Cursor != "":
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		var c cursor
		if err == nil {
			err = json.Unmarshal(raw, &c)
		}
		if err != nil || c.Filter != filter || c.Seq < 0 {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "cursor does not belong to this filter").WithDetail("field", "cursor")
		}
		after = c.Seq
	case q.Latest:
		if err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) { after, err = tx.LastEventSeq(); return err }); err != nil {
			return nil, err
		}
	}
	if q.Wait > 0 {
		if err := s.enter(principal, q.Caller); err != nil {
			return nil, err
		}
		defer s.leave(principal, q.Caller)
	}
	deadline := time.Now().Add(q.Wait)
	poll := s.Poll
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	for {
		var changed <-chan struct{}
		if s.Changed != nil {
			changed = s.Changed() // before reading: no change is missed
		}
		var rows []state.EventRow
		if err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) {
			rows, err = tx.ListEvents(principal, after, q.Limit, state.EventFilter{Types: q.Types, VMID: q.VMID})
			return err
		}); err != nil {
			return nil, err
		}
		if len(rows) > 0 || !time.Now().Before(deadline) {
			out := &api.EventList{Items: make([]api.Event, 0, len(rows))}
			for _, r := range rows {
				out.Items = append(out.Items, Render(r))
				after = r.Seq
			}
			out.NextCursor = encode(after, filter)
			return out, nil
		}
		wait := min(poll, time.Until(deadline))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		case <-time.After(wait):
		}
	}
}

// Wait blocks until an event newer than seq may exist, d passes or ctx
// ends (for consumers that tail the stream, like webhooks).
func (s *Service) Wait(ctx context.Context, d time.Duration) {
	var changed <-chan struct{}
	if s.Changed != nil {
		changed = s.Changed()
	}
	poll := s.Poll
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	select {
	case <-ctx.Done():
	case <-changed:
	case <-time.After(min(d, poll)):
	}
}

// RunRetention prunes events older than the retention hourly until ctx
// ends.
func (s *Service) RunRetention(ctx context.Context) {
	keep := s.Retention
	if keep <= 0 {
		keep = DefaultRetention
	}
	for {
		s.DB.Tx(ctx, func(tx *state.Tx) error {
			_, err := tx.PruneEvents(time.Now().Add(-keep).UnixNano())
			return err
		})
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// Tail calls fn with every event (all principals) appended after it
// starts, in order, until ctx ends. Consumers that must never slow the
// host (telemetry) keep fn non-blocking.
func (s *Service) Tail(ctx context.Context, fn func(api.Event)) {
	var after int64
	if err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) { after, err = tx.LastEventSeq(); return err }); err != nil {
		return
	}
	for ctx.Err() == nil {
		var rows []state.EventRow
		err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) {
			rows, err = tx.ListEvents("", after, maxLimit, state.EventFilter{})
			return err
		})
		if err == nil {
			for _, r := range rows {
				fn(Render(r))
				after = r.Seq
			}
		}
		if err != nil || len(rows) < maxLimit {
			s.Wait(ctx, time.Second)
		}
	}
}
