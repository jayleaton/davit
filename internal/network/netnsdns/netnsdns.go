// Package netnsdns is the allowlist DNS resolver of the nft-only egress
// backends (pasta, slirp4netns, privileged). It runs inside the VM network
// namespace as `davit netns-dns CONFIG_JSON`, listens on the TAP address
// port 53 (UDP and TCP; the guest ruleset DNATs all guest DNS to it),
// answers NXDOMAIN for names the allowlist does not cover (dnsallow),
// forwards the others to the host's upstream resolvers, and pins every A
// address of an allowed answer into the guest table's pinned4 set, with the
// record's TTL clamped to [dnsallow.MinPin, dnsallow.MaxPin], before the
// guest sees the answer. A failed pin turns the answer into SERVFAIL.
package netnsdns

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"davit/internal/network/dnsallow"
)

// Cmd is the davit subcommand that runs the resolver.
const Cmd = "netns-dns"

// FDReady is the fd the resolver writes "1" to once it is listening.
const FDReady = 3

// Fallback is the upstream used when the host has no usable resolver.
var Fallback = netip.MustParseAddrPort("1.1.1.1:53")

// Config is the resolver's command-line configuration.
type Config struct {
	Listen    netip.AddrPort   `json:"listen"`    // TAP address, port 53
	Upstreams []netip.AddrPort `json:"upstreams"` // tried in order
	Allow     []string         `json:"allow"`     // normalized domain.Egress.Allow
	Table     string           `json:"table"`     // nft table (family inet)
	Set       string           `json:"set"`       // timeout set for pins
	// Holder is the namespace holder; the resolver exits when it does. A
	// zero PID (privileged helper, which supervises it) disables the watch.
	Holder Holder `json:"holder"`
}

// Holder identifies a process by PID and start time (/proc/<pid>/stat
// field 22), so a reused PID is not mistaken for it.
type Holder struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"startTime"`
}

var nftName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

func (c Config) validate() error {
	if !c.Listen.IsValid() || len(c.Upstreams) == 0 {
		return errors.New("netns-dns: listen address and upstreams required")
	}
	if !nftName.MatchString(c.Table) || !nftName.MatchString(c.Set) {
		return errors.New("netns-dns: invalid nft table or set name")
	}
	return nil
}

// Upstreams returns the non-loopback IPv4 nameservers of a resolv.conf
// (port 53), or Fallback when there are none (e.g. systemd-resolved's
// 127.0.0.53, which the VM netns cannot reach).
func Upstreams(resolvConf []byte) []netip.AddrPort {
	var out []netip.AddrPort
	sc := bufio.NewScanner(bytes.NewReader(resolvConf))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		a, err := netip.ParseAddr(f[1])
		if err != nil || !a.Is4() || a.IsLoopback() || a.IsUnspecified() {
			continue
		}
		out = append(out, netip.AddrPortFrom(a, 53))
	}
	if len(out) == 0 {
		return []netip.AddrPort{Fallback}
	}
	return out
}

// Limits on concurrent work, so a guest cannot exhaust the resolver.
const (
	maxInflight  = 256
	maxTCPConns  = 64
	queryTimeout = 3 * time.Second
	tcpIdle      = 10 * time.Second
)

// Server answers guest DNS queries.
type Server struct {
	cfg  Config
	list dnsallow.List
	// pin installs pins (nft by default; tests replace it).
	pin func(ctx context.Context, batch string) error

	mu    sync.Mutex // serializes pin batches and guards until
	until map[netip.Addr]time.Time
	slots chan struct{}
}

// New validates cfg and returns a server that pins through nft.
func New(cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	l, err := dnsallow.Parse(cfg.Allow)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, list: l, pin: runNft, until: map[netip.Addr]time.Time{}, slots: make(chan struct{}, maxInflight)}, nil
}

func runNft(ctx context.Context, batch string) error {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(batch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Serve answers queries on udp and tcp until ctx ends or a listener fails.
// It closes both.
func (s *Server) Serve(ctx context.Context, udp net.PacketConn, tcp net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); udp.Close(); tcp.Close() }()
	errc := make(chan error, 2)
	go func() { errc <- s.serveUDP(ctx, udp) }()
	go func() { errc <- s.serveTCP(ctx, tcp) }()
	err := <-errc
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (s *Server) serveUDP(ctx context.Context, c net.PacketConn) error {
	for {
		buf := make([]byte, 65535)
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return err
		}
		select {
		case s.slots <- struct{}{}:
		default:
			continue // overloaded: drop, the client retries
		}
		go func() {
			defer func() { <-s.slots }()
			if resp := s.Handle(ctx, buf[:n], false); resp != nil {
				_, _ = c.WriteTo(resp, from)
			}
		}()
	}
}

func (s *Server) serveTCP(ctx context.Context, l net.Listener) error {
	conns := make(chan struct{}, maxTCPConns)
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		select {
		case conns <- struct{}{}:
		default:
			c.Close()
			continue
		}
		go func() {
			defer func() { <-conns }()
			defer c.Close()
			for {
				_ = c.SetDeadline(time.Now().Add(tcpIdle))
				q, err := readTCPMsg(c)
				if err != nil {
					return
				}
				resp := s.Handle(ctx, q, true)
				if resp == nil || writeTCPMsg(c, resp) != nil {
					return
				}
			}
		}()
	}
}

func readTCPMsg(r io.Reader) ([]byte, error) {
	var n uint16
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return nil, err
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

func writeTCPMsg(w io.Writer, m []byte) error {
	if len(m) > 0xffff {
		return errors.New("dns message too long")
	}
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(m))), m...))
	return err
}

// Handle answers one query: NXDOMAIN for disallowed names, nil (drop) for
// malformed ones, otherwise the upstream answer after its A addresses are
// pinned, or SERVFAIL when forwarding or pinning failed.
func (s *Server) Handle(ctx context.Context, q []byte, tcp bool) []byte {
	ok, refusal := s.list.Decide(q)
	if !ok {
		return refusal
	}
	resp, err := s.forward(ctx, q, tcp)
	if err != nil {
		return servfail(q)
	}
	if as := dnsallow.Addresses(resp); len(as) > 0 {
		if err := s.pinAll(ctx, as); err != nil {
			return servfail(q)
		}
	}
	return resp
}

func (s *Server) forward(ctx context.Context, q []byte, tcp bool) ([]byte, error) {
	var last error
	for _, up := range s.cfg.Upstreams {
		resp, err := exchange(ctx, up, q, tcp)
		if err == nil {
			return resp, nil
		}
		last = err
	}
	return nil, last
}

// exchange sends q to up and returns the first response that matches its
// ID and question.
func exchange(ctx context.Context, up netip.AddrPort, q []byte, tcp bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	network := "udp"
	if tcp {
		network = "tcp"
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, network, up.String())
	if err != nil {
		return nil, err
	}
	defer c.Close()
	dl, _ := ctx.Deadline()
	_ = c.SetDeadline(dl)
	if tcp {
		if err := writeTCPMsg(c, q); err != nil {
			return nil, err
		}
		resp, err := readTCPMsg(c)
		if err != nil {
			return nil, err
		}
		if !matches(q, resp) {
			return nil, errors.New("mismatched upstream response")
		}
		return resp, nil
	}
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return nil, err
		}
		if matches(q, buf[:n]) {
			return buf[:n], nil
		}
	}
}

// matches reports whether resp answers q (same ID and first question).
func matches(q, resp []byte) bool {
	var pq, pr dnsmessage.Parser
	hq, err1 := pq.Start(q)
	hr, err2 := pr.Start(resp)
	if err1 != nil || err2 != nil || !hr.Response || hr.ID != hq.ID {
		return false
	}
	qq, err1 := pq.Question()
	qr, err2 := pr.Question()
	return err1 == nil && err2 == nil && qq.Type == qr.Type &&
		strings.EqualFold(qq.Name.String(), qr.Name.String())
}

func servfail(q []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(q)
	if err != nil {
		return nil
	}
	qs, err := p.AllQuestions()
	if err != nil {
		return nil
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, OpCode: h.OpCode,
		RecursionDesired: h.RecursionDesired, RecursionAvailable: true, RCode: dnsmessage.RCodeServerFailure})
	if b.StartQuestions() != nil {
		return nil
	}
	for _, qq := range qs {
		if b.Question(qq) != nil {
			return nil
		}
	}
	out, _ := b.Finish()
	return out
}

// pinAll pins as in one nft transaction. A pin never shrinks: its timeout
// covers the longest TTL handed out for the address since the resolver
// started. Each element is added, deleted and added again so an existing
// element's timeout is refreshed (a plain add keeps the old one).
func (s *Server) pinAll(ctx context.Context, as []dnsallow.Answer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for a, u := range s.until {
		if !u.After(now) {
			delete(s.until, a)
		}
	}
	var b strings.Builder
	for _, a := range as {
		u := now.Add(min(max(a.TTL, dnsallow.MinPin), dnsallow.MaxPin))
		if old := s.until[a.Addr]; old.After(u) {
			u = old
		}
		s.until[a.Addr] = u
		secs := int((u.Sub(now) + time.Second - 1) / time.Second)
		b.WriteString(PinBatch(s.cfg.Table, s.cfg.Set, a.Addr, secs, a.Name))
	}
	return s.pin(ctx, b.String())
}

// PinBatch is the nft input that (re)pins addr in table/set for secs
// seconds with name as the element comment.
func PinBatch(table, set string, addr netip.Addr, secs int, name string) string {
	elem := fmt.Sprintf("%s timeout %ds", addr, secs)
	if c := CommentName(name); c != "" {
		elem += fmt.Sprintf(" comment %q", c)
	}
	return fmt.Sprintf("add element inet %[1]s %[2]s { %[3]s }\ndelete element inet %[1]s %[2]s { %[4]s }\nadd element inet %[1]s %[2]s { %[3]s }\n",
		table, set, elem, addr)
}

// CommentName reduces a DNS name to a safe nft element comment: lowercase,
// no trailing dot, only [a-z0-9._-], at most 120 bytes; "" if anything else
// appears (DNS labels may hold arbitrary bytes).
func CommentName(name string) string {
	n := strings.TrimSuffix(strings.ToLower(name), ".")
	if n == "" || len(n) > 120 {
		return ""
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_') {
			return ""
		}
	}
	return n
}
