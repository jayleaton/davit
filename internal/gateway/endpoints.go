package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"tailscale.com/ipn"

	"davit/internal/domain"
)

// EndpointState is the enrollment state of a per-VM node, reported
// separately from VM readiness.
type EndpointState string

// Endpoint states.
const (
	EndpointPending EndpointState = "pending"
	EndpointReady   EndpointState = "ready"
	EndpointError   EndpointState = "error"
)

// EndpointInfo describes one per-VM node. FQDN and Addrs are what the control
// server actually registered, which can differ from the requested Hostname.
type EndpointInfo struct {
	VMID     domain.VMID
	Hostname string
	FQDN     string
	Addrs    []netip.Addr
	State    EndpointState
	Err      string
}

// EndpointsConfig configures optional per-VM nodes (endpoint.mode=node).
type EndpointsConfig struct {
	// StateDir holds one subdirectory of persistent node identity per VM.
	StateDir   string
	ControlURL string
	// AuthKeyFile/AuthKeyEnv supply an enrollment key for new nodes; it must
	// be reusable (or an OAuth-minted key per node) since every VM enrolls.
	AuthKeyFile   string
	AuthKeyEnv    string
	AdvertiseTags []string
	Ephemeral     bool
	// NewStore overrides each node's state store (tests use mem.Store).
	NewStore func(vmID domain.VMID) ipn.StateStore
	Logf     func(string, ...any)
}

// Endpoints runs one host-resident tsnet node per VM. Each node's :22 is a
// raw byte tunnel to that VM's sshd, so OpenSSH verifies the guest host key
// end to end; nothing runs or is stored inside the guest.
type Endpoints struct {
	cfg     EndpointsConfig
	streams *Streams

	mu sync.Mutex
	m  map[domain.VMID]*endpoint
}

type endpoint struct {
	node   *Node
	cancel context.CancelFunc
	done   chan struct{}
	info   EndpointInfo
}

var vmIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NewEndpoints returns a manager whose nodes authorize through streams.
func NewEndpoints(cfg EndpointsConfig, streams *Streams) (*Endpoints, error) {
	if cfg.StateDir == "" || streams == nil {
		return nil, errors.New("gateway: endpoints need StateDir and Streams")
	}
	if err := ensurePrivateDir(cfg.StateDir); err != nil {
		return nil, err
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Endpoints{cfg: cfg, streams: streams, m: map[domain.VMID]*endpoint{}}, nil
}

// Ensure starts vmID's node if it is not running and returns its current
// info. Enrollment continues in the background; poll Info for ready/error.
// A fork is a new VM ID and therefore always gets a new identity.
func (e *Endpoints) Ensure(vmID domain.VMID, hostname string) (EndpointInfo, error) {
	if !vmIDRE.MatchString(string(vmID)) {
		return EndpointInfo{}, fmt.Errorf("%w: vm id %q", ErrInvalidTarget, vmID)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if ep, ok := e.m[vmID]; ok {
		return ep.info, nil
	}
	c := Config{
		StateDir:      filepath.Join(e.cfg.StateDir, string(vmID)),
		Hostname:      hostname,
		ControlURL:    e.cfg.ControlURL,
		AuthKeyFile:   e.cfg.AuthKeyFile,
		AuthKeyEnv:    e.cfg.AuthKeyEnv,
		Ephemeral:     e.cfg.Ephemeral,
		AdvertiseTags: e.cfg.AdvertiseTags,
		Logf:          e.cfg.Logf,
	}
	if e.cfg.NewStore != nil {
		c.Store = e.cfg.NewStore(vmID)
	}
	node, err := NewNode(c)
	if err != nil {
		return EndpointInfo{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	ep := &endpoint{
		node:   node,
		cancel: cancel,
		done:   make(chan struct{}),
		info:   EndpointInfo{VMID: vmID, Hostname: hostname, State: EndpointPending},
	}
	e.m[vmID] = ep
	go e.run(ctx, ep)
	return ep.info, nil
}

func (e *Endpoints) run(ctx context.Context, ep *endpoint) {
	defer close(ep.done)
	fail := func(err error) {
		e.mu.Lock()
		ep.info.State, ep.info.Err = EndpointError, err.Error()
		e.mu.Unlock()
		e.cfg.Logf("endpoint %s: %v", ep.info.VMID, err)
	}
	if err := ep.node.Up(ctx); err != nil {
		fail(err)
		return
	}
	who, err := ep.node.whoIser()
	if err != nil {
		fail(err)
		return
	}
	ln, err := ep.node.Listen(fmt.Sprintf(":%d", guestSSHPort))
	if err != nil {
		fail(err)
		return
	}
	e.mu.Lock()
	ep.info.State, ep.info.FQDN, ep.info.Addrs = EndpointReady, ep.node.FQDN(), ep.node.Addrs()
	e.mu.Unlock()
	vmID := ep.info.VMID
	err = serveConns(ctx, ln, func(ctx context.Context, c net.Conn) {
		id, err := IdentifyConn(ctx, who, c)
		if err != nil {
			c.Close()
			return
		}
		req := Request{VMRef: string(vmID), Action: ActionSSH, Port: guestSSHPort}
		if err := e.streams.Tunnel(ctx, id, req, c); err != nil && !errors.Is(err, net.ErrClosed) {
			e.cfg.Logf("endpoint %s: %s: %v", vmID, id, err)
		}
	})
	if err != nil {
		fail(err)
	}
}

// Info returns vmID's endpoint, if running.
func (e *Endpoints) Info(vmID domain.VMID) (EndpointInfo, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ep, ok := e.m[vmID]
	if !ok {
		return EndpointInfo{}, false
	}
	info := ep.info
	info.Addrs = append([]netip.Addr(nil), info.Addrs...)
	return info, true
}

// Detach stops vmID's node and closes its sessions but keeps its identity,
// so a stopped VM resumes under the same name.
func (e *Endpoints) Detach(vmID domain.VMID) error {
	e.mu.Lock()
	ep, ok := e.m[vmID]
	delete(e.m, vmID)
	e.mu.Unlock()
	if !ok {
		return nil
	}
	ep.cancel()
	<-ep.done
	return ep.node.Close()
}

// Delete removes vmID's node from the tailnet and erases its identity. A
// running node is logged out first; a detached one only loses local state
// (the control server expires it per tailnet policy).
func (e *Endpoints) Delete(ctx context.Context, vmID domain.VMID) error {
	if !vmIDRE.MatchString(string(vmID)) {
		return fmt.Errorf("%w: vm id %q", ErrInvalidTarget, vmID)
	}
	e.mu.Lock()
	ep, ok := e.m[vmID]
	e.mu.Unlock()
	var errs []error
	if ok {
		errs = append(errs, ep.node.Logout(ctx))
	}
	errs = append(errs, e.Detach(vmID), os.RemoveAll(filepath.Join(e.cfg.StateDir, string(vmID))))
	return errors.Join(errs...)
}

// Close detaches every endpoint.
func (e *Endpoints) Close() error {
	e.mu.Lock()
	ids := make([]domain.VMID, 0, len(e.m))
	for id := range e.m {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	var errs []error
	for _, id := range ids {
		errs = append(errs, e.Detach(id))
	}
	return errors.Join(errs...)
}
