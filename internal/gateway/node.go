package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

// Node is one embedded tsnet node: the shared host gateway or a per-VM
// endpoint. It owns no authorization policy.
type Node struct {
	srv *tsnet.Server
	lc  *local.Client

	mu   sync.Mutex
	fqdn string
	ips  []netip.Addr
}

// NewNode prepares a node; nothing touches the network until Up.
func NewNode(c Config) (*Node, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(c.StateDir); err != nil {
		return nil, err
	}
	// tsnet keeps tailscaled.state directly in StateDir, where `doctor`
	// looks for an enrolled node.
	dir := c.StateDir
	key, err := c.authKey()
	if err != nil {
		return nil, err
	}
	logf := c.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	userLogf := c.UserLogf
	if userLogf == nil {
		userLogf = logf
	}
	srv := &tsnet.Server{
		Dir:           dir,
		Store:         c.Store,
		Hostname:      c.Hostname,
		ControlURL:    c.ControlURL,
		AuthKey:       key,
		Ephemeral:     c.Ephemeral,
		AdvertiseTags: c.AdvertiseTags,
		Logf:          logf,
		UserLogf:      userLogf,
	}
	return &Node{srv: srv}, nil
}

// Up starts the node and blocks until it is logged in and has addresses, or
// ctx ends. Interactive login (no auth key, no saved state) does not complete
// here; it surfaces as a timeout with the backend's AuthURL in the logs.
func (n *Node) Up(ctx context.Context) error { return n.UpWithProgress(ctx, nil) }

// UpWithProgress is Up that reports the backend's state while it waits
// (e.g. NeedsLogin with the sign-in URL, NeedsMachineAuth while an admin
// must approve the device), so the operator can be told what to do.
func (n *Node) UpWithProgress(ctx context.Context, progress func(backendState, authURL string)) error {
	if progress != nil {
		watch, stop := context.WithCancel(ctx)
		defer stop()
		go n.watchBackend(watch, progress)
	}
	st, err := n.srv.Up(ctx)
	if err != nil {
		return fmt.Errorf("gateway: tsnet up: %w", err)
	}
	lc, err := n.srv.LocalClient()
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lc = lc
	n.ips = append([]netip.Addr(nil), st.TailscaleIPs...)
	if st.Self != nil {
		n.fqdn = strings.TrimSuffix(st.Self.DNSName, ".")
	}
	return nil
}

// FQDN is the name the control server actually registered (empty before Up).
func (n *Node) FQDN() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.fqdn
}

// Addrs are the node's tailnet addresses (empty before Up).
func (n *Node) Addrs() []netip.Addr {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]netip.Addr(nil), n.ips...)
}

// whoIser returns the node's local backend client for WhoIs lookups.
func (n *Node) whoIser() (WhoIser, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lc == nil {
		return nil, errors.New("gateway: node is not up")
	}
	return n.lc, nil
}

// Listen listens on the tailnet only (e.g. ":22"); it never binds a host
// interface.
func (n *Node) Listen(addr string) (net.Listener, error) { return n.srv.Listen("tcp", addr) }

// ListenTLS listens with a tailnet HTTPS certificate; the tailnet must have
// HTTPS certificates enabled.
func (n *Node) ListenTLS(addr string) (net.Listener, error) { return n.srv.ListenTLS("tcp", addr) }

// Dial dials a tailnet peer from this node.
func (n *Node) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return n.srv.Dial(ctx, network, addr)
}

// Logout removes the node from the tailnet (used when its VM is deleted).
func (n *Node) Logout(ctx context.Context) error {
	n.mu.Lock()
	lc := n.lc
	n.mu.Unlock()
	if lc == nil {
		return nil
	}
	return lc.Logout(ctx)
}

// Close stops the node, keeping its persistent identity in StateDir.
func (n *Node) Close() error { return n.srv.Close() }

// watchBackend reports backend state changes until ctx ends.
func (n *Node) watchBackend(ctx context.Context, progress func(string, string)) {
	lc, err := n.srv.LocalClient()
	if err != nil {
		return
	}
	var last string
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		if st, err := lc.StatusWithoutPeers(ctx); err == nil {
			if key := st.BackendState + "\x00" + st.AuthURL; key != last {
				last = key
				progress(st.BackendState, st.AuthURL)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
