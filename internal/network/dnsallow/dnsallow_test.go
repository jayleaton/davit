package dnsallow

import (
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"davit/internal/domain"
)

func list(t *testing.T, entries ...string) List {
	t.Helper()
	e, err := domain.Egress{Mode: domain.EgressAllowlist, Allow: entries}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	l, err := Parse(e.Allow)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestMatching(t *testing.T) {
	l := list(t, "GitHub.com.", "*.npmjs.org", "140.82.112.0/20", "1.2.3.4")
	for name, want := range map[string]bool{
		"github.com": true, "api.github.com.": true, "GITHUB.COM": true,
		"notgithub.com": false, "github.com.evil.io": false,
		"registry.npmjs.org": true, "npmjs.org": false, "example.com": false,
	} {
		if got := l.AllowsName(name); got != want {
			t.Errorf("AllowsName(%q) = %v, want %v", name, got, want)
		}
	}
	for addr, want := range map[string]bool{"140.82.121.4": true, "1.2.3.4": true, "1.2.3.5": false} {
		if got := l.AllowsCIDR(netip.MustParseAddr(addr)); got != want {
			t.Errorf("AllowsCIDR(%s) = %v, want %v", addr, got, want)
		}
	}
}

func query(t *testing.T, name string) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// rawMessage builds a DNS message with the given opcode, question count
// and an answer section, for the shapes a plain query never has.
func rawMessage(t *testing.T, op dnsmessage.OpCode, questions int, answer bool) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 9, OpCode: op})
	b.StartQuestions()
	for range questions {
		b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("api.github.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	}
	if answer {
		b.StartAnswers()
		b.AResource(dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("api.github.com."), Class: dnsmessage.ClassINET},
			dnsmessage.AResource{A: [4]byte{10, 0, 0, 1}})
	}
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestOnlyPlainQueries: only standard queries with exactly one question
// and no records are forwarded; UPDATE, NOTIFY and odd shapes carry
// payloads to resolvers the guest could not otherwise reach (SEC-10).
func TestOnlyPlainQueries(t *testing.T) {
	l := list(t, "github.com")
	if !ValidQuery(rawMessage(t, 0, 1, false)) {
		t.Fatal("a plain query was refused")
	}
	for name, q := range map[string][]byte{
		"update":    rawMessage(t, 5, 1, false),
		"notify":    rawMessage(t, 4, 1, false),
		"two qs":    rawMessage(t, 0, 2, false),
		"no qs":     rawMessage(t, 0, 0, false),
		"answers":   rawMessage(t, 0, 1, true),
		"too short": []byte{0, 1, 2},
	} {
		if ValidQuery(q) {
			t.Errorf("%s accepted as a plain query", name)
		}
		if ok, _ := l.Decide(q); ok {
			t.Errorf("%s forwarded by an allowlist", name)
		}
	}
}

func TestDecideAndAddresses(t *testing.T) {
	l := list(t, "github.com")
	if ok, ref := l.Decide(query(t, "api.github.com.")); !ok || ref != nil {
		t.Fatalf("allowed name: ok=%v ref=%v", ok, ref)
	}
	ok, ref := l.Decide(query(t, "example.com."))
	var p dnsmessage.Parser
	h, err := p.Start(ref)
	if ok || err != nil || h.RCode != dnsmessage.RCodeNameError || h.ID != 7 || !h.Response {
		t.Fatalf("refused name: ok=%v header=%+v err=%v", ok, h, err)
	}
	if ok, ref := l.Decide([]byte("junk")); ok || ref != nil {
		t.Fatal("malformed query not dropped")
	}

	// A response with a CNAME to a CDN name, then A records: pins carry the
	// question name.
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, Response: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("api.github.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	b.StartAnswers()
	b.CNAMEResource(dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("api.github.com."), Class: dnsmessage.ClassINET, TTL: 30},
		dnsmessage.CNAMEResource{CNAME: dnsmessage.MustNewName("cdn.example.net.")})
	b.AResource(dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("cdn.example.net."), Class: dnsmessage.ClassINET, TTL: 300},
		dnsmessage.AResource{A: [4]byte{140, 82, 121, 4}})
	resp, _ := b.Finish()
	as := Addresses(resp)
	if len(as) != 1 || as[0].Addr != netip.MustParseAddr("140.82.121.4") || as[0].Name != "api.github.com." || as[0].TTL != 300*time.Second {
		t.Fatalf("addresses: %+v", as)
	}
}

func TestPins(t *testing.T) {
	now := time.Unix(1000, 0)
	p := NewPins(func() time.Time { return now })
	a, b := netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("2.2.2.2")
	if got := p.Add(a, "x.github.com", 5*time.Second); got != MinPin {
		t.Fatalf("ttl clamp low: %v", got)
	}
	if got := p.Add(b, "npmjs.org", 48*time.Hour); got != MaxPin {
		t.Fatalf("ttl clamp high: %v", got)
	}
	if !p.Has(a) || !p.Has(b) {
		t.Fatal("pins missing")
	}
	now = now.Add(MinPin)
	if p.Has(a) || !p.Has(b) {
		t.Fatal("expiry wrong")
	}
	if gone := p.Retain(list(t, "github.com")); len(gone) != 1 || gone[0] != b || p.Has(b) {
		t.Fatalf("retain dropped %v", gone)
	}
}
