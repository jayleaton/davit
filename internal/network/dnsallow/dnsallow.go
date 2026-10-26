// Package dnsallow implements egress allowlists: domain/CIDR matching, the
// filtering DNS resolver that answers only for allowed names, and the pins
// that admit the addresses those answers carried for their TTL. The
// userspace egress stack and the in-netns resolver of the nft-only backends
// (pasta, slirp4netns, privileged) share it, so allowlists behave the same
// on every backend.
package dnsallow

import (
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Pin lifetime bounds: a record's TTL, clamped. The floor covers clients
// that cache past tiny TTLs; the ceiling bounds how long a since-removed
// name stays reachable. Established connections are unaffected by expiry.
const (
	MinPin = 60 * time.Second
	MaxPin = time.Hour
)

// List is a parsed allowlist (entries from domain.Egress.Normalize).
type List struct {
	names     []string // exact-or-subdomain matches
	wildcards []string // "*.x": subdomains of x only (stored as "x")
	cidrs     []netip.Prefix
}

// Parse builds a List from normalized entries.
func Parse(entries []string) (List, error) {
	var l List
	for _, e := range entries {
		if p, err := netip.ParsePrefix(e); err == nil {
			l.cidrs = append(l.cidrs, p)
			continue
		}
		if e == "" || strings.ContainsAny(e, " /") {
			return List{}, errors.New("dnsallow: invalid entry " + e)
		}
		if w, ok := strings.CutPrefix(e, "*."); ok {
			l.wildcards = append(l.wildcards, w)
		} else {
			l.names = append(l.names, e)
		}
	}
	return l, nil
}

// CIDRs are the address entries.
func (l List) CIDRs() []netip.Prefix { return l.cidrs }

// AllowsName reports whether name (any case, optional trailing dot) is
// allowed: "github.com" covers itself and subdomains, "*.x" covers only
// subdomains of x.
func (l List) AllowsName(name string) bool {
	n := strings.TrimSuffix(strings.ToLower(name), ".")
	for _, a := range l.names {
		if n == a || strings.HasSuffix(n, "."+a) {
			return true
		}
	}
	for _, w := range l.wildcards {
		if strings.HasSuffix(n, "."+w) {
			return true
		}
	}
	return false
}

// AllowsCIDR reports whether a is inside an address entry.
func (l List) AllowsCIDR(a netip.Addr) bool {
	for _, p := range l.cidrs {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Pins are addresses admitted by allowed DNS answers, each with an expiry
// and the names that pinned it. Safe for concurrent use.
type Pins struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[netip.Addr]pin
}

type pin struct {
	until time.Time
	names map[string]bool
}

// NewPins returns an empty pin set (now is time.Now when nil).
func NewPins(now func() time.Time) *Pins {
	if now == nil {
		now = time.Now
	}
	return &Pins{now: now, m: map[netip.Addr]pin{}}
}

// Add pins a for ttl (clamped to [MinPin, MaxPin]) on behalf of name and
// returns the lifetime used.
func (p *Pins) Add(a netip.Addr, name string, ttl time.Duration) time.Duration {
	ttl = min(max(ttl, MinPin), MaxPin)
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.m[a]
	if e.names == nil {
		e.names = map[string]bool{}
	}
	if u := p.now().Add(ttl); u.After(e.until) {
		e.until = u
	}
	e.names[strings.TrimSuffix(strings.ToLower(name), ".")] = true
	p.m[a] = e
	return ttl
}

// Has reports whether a is pinned and not expired.
func (p *Pins) Has(a netip.Addr) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.m[a]
	if ok && !p.now().Before(e.until) {
		delete(p.m, a)
		return false
	}
	return ok
}

// Retain drops pins none of whose names l still allows (after an allowlist
// change) and returns the dropped addresses.
func (p *Pins) Retain(l List) []netip.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	var gone []netip.Addr
	for a, e := range p.m {
		keep := false
		for n := range e.names {
			if l.AllowsName(n) {
				keep = true
				break
			}
		}
		if !keep {
			delete(p.m, a)
			gone = append(gone, a)
		}
	}
	return gone
}

// Answer is one pinned address from an allowed response.
type Answer struct {
	Addr netip.Addr
	Name string
	TTL  time.Duration
}

// Decide inspects a DNS query. It returns ok=true when every question asks
// for an allowed name (forward it); otherwise refusal is an NXDOMAIN
// response for the query. Malformed queries yield ok=false and no refusal
// (drop them).
func (l List) Decide(query []byte) (ok bool, refusal []byte) {
	if !ValidQuery(query) {
		return false, nil
	}
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil || h.Response {
		return false, nil
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) == 0 {
		return false, nil
	}
	for _, q := range qs {
		if !l.AllowsName(q.Name.String()) {
			return false, nxdomain(h, qs)
		}
	}
	return true, nil
}

// ValidQuery reports whether msg is a plain DNS query: opcode QUERY, not a
// response, exactly one question and no answer, authority or additional
// records except one OPT (EDNS). Anything else (UPDATE, NOTIFY, crafted
// payloads) is not forwarded to the host's resolvers (SEC-10).
func ValidQuery(msg []byte) bool {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || h.Response || h.OpCode != 0 {
		return false
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) != 1 {
		return false
	}
	if ans, err := p.AllAnswers(); err != nil || len(ans) != 0 {
		return false
	}
	if auth, err := p.AllAuthorities(); err != nil || len(auth) != 0 {
		return false
	}
	add, err := p.AllAdditionals()
	if err != nil || len(add) > 1 || len(add) == 1 && add[0].Header.Type != dnsmessage.TypeOPT {
		return false
	}
	return true
}

func nxdomain(h dnsmessage.Header, qs []dnsmessage.Question) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, OpCode: h.OpCode,
		RecursionDesired: h.RecursionDesired, RecursionAvailable: true, RCode: dnsmessage.RCodeNameError})
	if b.StartQuestions() != nil {
		return nil
	}
	for _, q := range qs {
		if b.Question(q) != nil {
			return nil
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

// Addresses returns the A records of a DNS response (answer section),
// attributed to the response's question name so pins follow the allowed
// name even through CNAME chains to CDN hostnames.
func Addresses(resp []byte) []Answer {
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil || !h.Response || h.RCode != dnsmessage.RCodeSuccess {
		return nil
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) == 0 {
		return nil
	}
	name := qs[0].Name.String()
	var out []Answer
	for {
		rh, err := p.AnswerHeader()
		if err != nil {
			break
		}
		if rh.Type != dnsmessage.TypeA {
			if p.SkipAnswer() != nil {
				break
			}
			continue
		}
		r, err := p.AResource()
		if err != nil {
			break
		}
		out = append(out, Answer{Addr: netip.AddrFrom4(r.A), Name: name, TTL: time.Duration(rh.TTL) * time.Second})
	}
	return out
}
