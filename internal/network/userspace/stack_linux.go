//go:build linux

package userspace

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"davit/internal/network/dnsallow"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	nicID       = 1
	dialTimeout = 10 * time.Second
	udpIdle     = 60 * time.Second
	dnsIdle     = 10 * time.Second
	maxSYNs     = 1024 // half-open guest connections being dialed
)

// Stats counts flows (exported to tests and logs).
type Stats struct {
	TCP, UDP, DNS, Denied atomic.Int64
}

// Stack is a running egress stack.
type Stack struct {
	cfg       Config
	log       *slog.Logger
	s         *stack.Stack
	resolvers []string
	Stats     Stats
	closed    chan struct{}
	once      sync.Once

	// mu orders policy swaps against DNS answers pinning under the
	// policy they were decided with.
	mu    sync.RWMutex
	pol   atomic.Pointer[modeState]
	flows flows
	ctl   *net.UnixListener
}

// modeState is the live mode; replaced as a whole on every change.
type modeState struct {
	mode string
	list dnsallow.List
	pins *dnsallow.Pins // allowlist mode only
}

func newModeState(mode string, allow []string) (*modeState, error) {
	switch mode {
	case "":
		mode = ModeFiltered
	case ModeOff, ModeFiltered, ModeAllowlist:
	default:
		return nil, fmt.Errorf("userspace: unknown egress mode %q", mode)
	}
	ms := &modeState{mode: mode}
	if mode == ModeAllowlist {
		l, err := dnsallow.Parse(allow)
		if err != nil {
			return nil, err
		}
		ms.list, ms.pins = l, dnsallow.NewPins(nil)
	}
	return ms, nil
}

// allowed reports whether a guest flow over proto may reach dst under ms:
// never in off mode; the base policy always; in allowlist mode also an
// allowlisted CIDR or an address pinned by an allowed DNS answer (so a LAN
// service needs both the operator's permission and the VM's allowlist).
func (st *Stack) allowed(ms *modeState, proto string, dst netip.AddrPort) bool {
	if ms.mode == ModeOff || !st.cfg.Policy.Allows(proto, dst) {
		return false
	}
	a := dst.Addr().Unmap()
	return ms.mode != ModeAllowlist || ms.list.AllowsCIDR(a) || ms.pins.Has(a)
}

// apply switches to mode/allow, drops pins the new allowlist no longer
// covers and closes guest flows the new policy denies. Flows judged by
// pins are judged now: one whose pin has expired is closed too (fail
// closed; the guest re-resolves).
func (st *Stack) apply(mode string, allow []string) error {
	ms, err := newModeState(mode, allow)
	if err != nil {
		return err
	}
	st.mu.Lock()
	if old := st.pol.Load(); old.mode == ModeAllowlist && ms.mode == ModeAllowlist {
		old.pins.Retain(ms.list)
		ms.pins = old.pins
	}
	st.pol.Store(ms)
	st.mu.Unlock()
	n := st.flows.sweep(func(f *flow) bool { return st.allowed(ms, f.proto, f.dst) })
	st.log.Info("egress policy changed", "mode", ms.mode, "allow", len(allow), "closedFlows", n)
	return nil
}

// flow is an active guest TCP splice or UDP relay.
type flow struct {
	proto string // "tcp" or "udp"
	dst   netip.AddrPort
	close func()
}

// flows registers active flows so policy changes can close them.
type flows struct {
	mu sync.Mutex
	m  map[*flow]struct{}
}

func (fs *flows) add(f *flow) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.m == nil {
		fs.m = map[*flow]struct{}{}
	}
	fs.m[f] = struct{}{}
}

func (fs *flows) remove(f *flow) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.m, f)
}

// sweep closes (and forgets) the flows keep rejects and returns how many.
func (fs *flows) sweep(keep func(*flow) bool) int {
	fs.mu.Lock()
	var gone []*flow
	for f := range fs.m {
		if !keep(f) {
			gone = append(gone, f)
			delete(fs.m, f)
		}
	}
	fs.mu.Unlock()
	for _, f := range gone {
		f.close()
	}
	return len(gone)
}

// track registers a flow, then rechecks it against the current policy so
// a change racing with its setup cannot leave it open. It reports false
// (flow already closed) when denied.
func (st *Stack) track(f *flow) bool {
	st.flows.add(f)
	if !st.allowed(st.pol.Load(), f.proto, f.dst) {
		st.flows.remove(f)
		f.close()
		return false
	}
	return true
}

// New builds the stack on tapFD (an IFF_TAP|IFF_NO_PI file descriptor). The
// stack starts serving immediately; Done is closed when the TAP goes away.
func New(cfg Config, tapFD int, log *slog.Logger) (*Stack, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.MTU <= 0 {
		cfg.MTU = 1500
	}
	if !cfg.Gateway.Addr().Is4() || !cfg.Peer.Is4() {
		return nil, errors.New("userspace: gateway and peer must be IPv4")
	}
	st := &Stack{cfg: cfg, log: log, closed: make(chan struct{}), resolvers: cfg.Resolvers}
	ms, err := newModeState(cfg.Mode, cfg.Allow)
	if err != nil {
		return nil, err
	}
	st.pol.Store(ms)
	if len(st.resolvers) == 0 {
		st.resolvers = hostResolvers("/etc/resolv.conf")
	}
	if len(st.resolvers) == 0 {
		return nil, errors.New("userspace: no host DNS resolvers")
	}
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		HandleLocal:        false,
	})
	st.s = s
	ep, err := fdbased.New(&fdbased.Options{
		FDs: []int{tapFD}, MTU: uint32(cfg.MTU), EthernetHeader: true,
		Address:    tcpip.LinkAddress("\x02\x53\x4c\x57\x00\x02"), // locally administered
		ClosedFunc: func(tcpip.Error) { st.close() },
	})
	if err != nil {
		return nil, fmt.Errorf("userspace: tap endpoint: %v", err)
	}
	if e := s.CreateNIC(nicID, ep); e != nil {
		return nil, fmt.Errorf("userspace: create nic: %v", e)
	}
	gw := cfg.Gateway.Addr().As4()
	if e := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(gw), PrefixLen: cfg.Gateway.Bits()}}, stack.AddressProperties{}); e != nil {
		return nil, fmt.Errorf("userspace: address: %v", e)
	}
	// Accept packets for any destination and answer from it (we are the
	// guest's whole internet).
	if e := s.SetPromiscuousMode(nicID, true); e != nil {
		return nil, fmt.Errorf("userspace: promiscuous: %v", e)
	}
	if e := s.SetSpoofing(nicID, true); e != nil {
		return nil, fmt.Errorf("userspace: spoofing: %v", e)
	}
	subnet, _ := tcpip.NewSubnet(tcpip.AddrFrom4(cfg.Gateway.Masked().Addr().As4()),
		tcpip.MaskFromBytes(net.CIDRMask(cfg.Gateway.Bits(), 32)))
	s.SetRouteTable([]tcpip.Route{
		{Destination: subnet, NIC: nicID},
		// Replies to un-masqueraded guest addresses go back via the netns.
		{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(cfg.Peer.As4()), NIC: nicID},
	})
	tf := tcp.NewForwarder(s, 0, maxSYNs, st.tcp)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tf.HandlePacket)
	uf := udp.NewForwarder(s, st.udp)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, uf.HandlePacket)
	if cfg.Control != "" {
		if err := st.listenControl(cfg.Control); err != nil {
			s.Close()
			return nil, err
		}
	}
	return st, nil
}

// Done is closed when the TAP is gone (namespace destroyed) or Close ran.
func (st *Stack) Done() <-chan struct{} { return st.closed }

// Close stops the stack.
func (st *Stack) Close() {
	st.close()
	if st.ctl != nil {
		st.ctl.Close()
	}
	st.s.Close()
}

func (st *Stack) close() { st.once.Do(func() { close(st.closed) }) }

func toAddr(a tcpip.Address) netip.Addr {
	if a.Len() == 4 {
		return netip.AddrFrom4(a.As4())
	}
	return netip.Addr{}
}

func (st *Stack) tcp(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := toAddr(id.LocalAddress)
	if id.LocalPort == 53 {
		// DNS is answered here whatever server the guest asked.
		if st.pol.Load().mode == ModeOff {
			st.Stats.Denied.Add(1)
			r.Complete(true)
			return
		}
		var wq waiter.Queue
		ep, e := r.CreateEndpoint(&wq)
		if e != nil {
			r.Complete(true)
			return
		}
		r.Complete(false)
		st.Stats.DNS.Add(1)
		go st.dnsTCP(gonet.NewTCPConn(&wq, ep))
		return
	}
	dstPort := netip.AddrPortFrom(dst, id.LocalPort)
	if !st.allowed(st.pol.Load(), "tcp", dstPort) {
		st.Stats.Denied.Add(1)
		r.Complete(true) // RST: the guest sees "connection refused"
		return
	}
	addr := dstPort.String()
	// Dial first so a refused or unreachable host surfaces to the guest as
	// a reset instead of a connection that later dies.
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	host, err := (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp4", addr)
	cancel()
	if err != nil {
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, e := r.CreateEndpoint(&wq)
	if e != nil {
		host.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)
	ep.SocketOptions().SetKeepAlive(true)
	f := &flow{proto: "tcp", dst: dstPort, close: func() { ep.Abort(); host.Close() }}
	if !st.track(f) {
		st.Stats.Denied.Add(1)
		return
	}
	st.Stats.TCP.Add(1)
	go func() {
		splice(gonet.NewTCPConn(&wq, ep), host.(*net.TCPConn))
		st.flows.remove(f)
	}()
}

// splice copies both ways with half-close, then closes both.
func splice(guest *gonet.TCPConn, host *net.TCPConn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(host, guest)
		_ = host.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(guest, host)
		_ = guest.CloseWrite()
	}()
	wg.Wait()
	guest.Close()
	host.Close()
}

func (st *Stack) udp(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := toAddr(id.LocalAddress)
	dns := id.LocalPort == 53
	dstPort := netip.AddrPortFrom(dst, id.LocalPort)
	ms := st.pol.Load()
	if dns && ms.mode == ModeOff || !dns && !st.allowed(ms, "udp", dstPort) {
		st.Stats.Denied.Add(1)
		return true // dropped (UDP has no refusal)
	}
	var wq waiter.Queue
	ep, e := r.CreateEndpoint(&wq)
	if e != nil {
		return true
	}
	guest := gonet.NewUDPConn(&wq, ep)
	if dns {
		st.Stats.DNS.Add(1)
		go st.dnsUDP(guest)
		return true
	}
	host, err := net.Dial("udp4", dstPort.String())
	if err != nil {
		guest.Close()
		return true
	}
	f := &flow{proto: "udp", dst: dstPort, close: func() { guest.Close(); host.Close() }}
	if !st.track(f) {
		st.Stats.Denied.Add(1)
		return true
	}
	st.Stats.UDP.Add(1)
	go func() {
		relayUDP(guest, host, udpIdle)
		st.flows.remove(f)
	}()
	return true
}

// relayUDP shuttles datagrams of one flow until it is idle.
func relayUDP(guest *gonet.UDPConn, host net.Conn, idle time.Duration) {
	defer guest.Close()
	defer host.Close()
	done := make(chan struct{}, 2)
	pump := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 65535)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
			n, err := src.Read(buf)
			if err != nil {
				return
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
	}
	go pump(host, guest)
	go pump(guest, host)
	<-done
}

// hostResolvers reads nameserver lines (IPv4 only: the stack dials IPv4).
func hostResolvers(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 2 && fs[0] == "nameserver" {
			if a, err := netip.ParseAddr(fs[1]); err == nil && a.Is4() {
				out = append(out, net.JoinHostPort(a.String(), "53"))
			}
		}
	}
	return out
}
