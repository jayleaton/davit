package gateway

import (
	"context"
	"davit/internal/httpx"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Options wires the gateway to the rest of the daemon.
type Options struct {
	Auth Authorizer
	// Dialer reaches guest ports; nil makes every guest stream fail with
	// ErrNoGuestTransport (the HTTP API still works).
	Dialer GuestDialer
	// Guest enables the SSH front door; nil disables it.
	Guest GuestSSH
	// API is served behind RequireIdentity; nil disables the HTTP listener.
	API http.Handler
	// APIAddr defaults to ":443" with APITLS, else ":80". Traffic is
	// WireGuard-encrypted on the tailnet either way; APITLS needs tailnet
	// HTTPS certificates enabled.
	APIAddr string
	APITLS  bool
	// TLSCertFile and TLSKeyFile serve the API over TLS with an operator
	// certificate (e.g. from a private CA, or for a tailnet without HTTPS
	// certificates) instead of the tailnet's own; reloaded on change.
	TLSCertFile, TLSKeyFile string
	// OnStatus receives enrollment and endpoint reports.
	OnStatus func(Status)
	// Apps authorizes app endpoint requests (SetApps); nil serves none.
	Apps AppAuthorizer
	// Sessions tracks users' live streams for revocation (may be nil).
	Sessions *Sessions
	// DesktopPage renders the desktop viewer (desktopui.Page).
	DesktopPage func(viewOnly bool) []byte
	// DesktopStats, if set, counts desktop sessions and dropped input.
	DesktopStats *DesktopStats
	// SSHAddr defaults to ":22".
	SSHAddr string
	Limits  Limits
	Logf    func(string, ...any)
}

// Gateway is the shared host tailnet node with its listeners.
type Gateway struct {
	hostname string
	certs    *fileCert
	node     *Node
	opts     Options
	streams  *Streams
	ssh      *SSHFrontDoor
	apps     *appServer
	who      WhoIser

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	errs   []error

	closeOnce sync.Once
	closeErr  error
}

// New prepares the gateway node and its SSH host key; Start brings it up.
func New(cfg Config, opts Options) (*Gateway, error) {
	if opts.Auth == nil {
		return nil, errors.New("gateway: Auth is required")
	}
	if opts.Dialer == nil {
		opts.Dialer = noGuestTransport{}
	}
	node, err := NewNode(cfg)
	if err != nil {
		return nil, err
	}
	var certs *fileCert
	if (opts.TLSCertFile == "") != (opts.TLSKeyFile == "") {
		return nil, errors.New("gateway: TLSCertFile and TLSKeyFile go together")
	}
	if opts.TLSCertFile != "" {
		if certs, err = newFileCert(opts.TLSCertFile, opts.TLSKeyFile); err != nil {
			return nil, err
		}
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	g := &Gateway{
		hostname: cfg.Hostname,
		certs:    certs,
		node:     node,
		opts:     opts,
		streams:  &Streams{Auth: opts.Auth, Dialer: opts.Dialer, Limits: opts.Limits, Sessions: opts.Sessions},
	}
	if opts.Apps != nil {
		g.apps = newAppServer(g, opts.Apps)
	}
	if opts.Guest != nil {
		key, err := LoadOrCreateHostKey(filepath.Join(cfg.StateDir, "ssh_host_ed25519_key"))
		if err != nil {
			return nil, fmt.Errorf("gateway: ssh host key: %w", err)
		}
		g.ssh = &SSHFrontDoor{Streams: g.streams, Guest: opts.Guest, HostKey: key, Logf: opts.Logf}
	}
	return g, nil
}

// TLSMode is how the API listener serves TLS.
func (g *Gateway) TLSMode() string {
	switch {
	case g.certs != nil:
		return TLSFiles
	case g.opts.APITLS:
		return TLSTailscale
	}
	return TLSOff
}

func (g *Gateway) report(s Status) {
	if g.opts.OnStatus == nil {
		return
	}
	s.Hostname, s.TLS, s.PID, s.UpdatedAt = g.hostname, g.TLSMode(), os.Getpid(), time.Now().UTC()
	g.opts.OnStatus(s)
}

// Start joins the tailnet (blocking until up or ctx ends) and starts the
// configured listeners. Listener failures after Start are reported by Close.
func (g *Gateway) Start(ctx context.Context) error {
	g.report(Status{State: EnrollStarting})
	err := g.node.UpWithProgress(ctx, func(state, authURL string) {
		switch state {
		case "NeedsLogin":
			g.report(Status{State: EnrollNeedsLogin, LoginURL: authURL})
		case "NeedsMachineAuth":
			g.report(Status{State: EnrollNeedsApproval})
		}
	})
	if err != nil {
		if ctx.Err() == nil {
			g.report(Status{State: EnrollError, Error: err.Error()})
		}
		return err
	}
	who, err := g.node.whoIser()
	if err != nil {
		return err
	}
	g.who = who
	g.ctx, g.cancel = context.WithCancel(context.Background())

	if g.opts.API != nil {
		addr := g.opts.APIAddr
		if addr == "" {
			addr = ":443"
			if g.TLSMode() == TLSOff {
				addr = ":80"
			}
		}
		ln, err := g.listen(addr)
		if err != nil {
			g.report(Status{State: EnrollError, Error: "api listen: " + err.Error()})
			g.Close()
			return fmt.Errorf("gateway: api listen: %w", err)
		}
		srv := httpx.Harden(&http.Server{
			Handler:     RequireIdentity(who, g.opts.API),
			BaseContext: func(net.Listener) context.Context { return g.ctx },
		})
		g.run("api", func() error {
			go func() { <-g.ctx.Done(); srv.Close() }()
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		})
	}
	if g.ssh != nil {
		addr := g.opts.SSHAddr
		if addr == "" {
			addr = ":22"
		}
		ln, err := g.node.Listen(addr)
		if err != nil {
			g.Close()
			return fmt.Errorf("gateway: ssh listen: %w", err)
		}
		g.run("ssh", func() error { return g.ssh.Serve(g.ctx, ln, who) })
	}
	if g.apps != nil {
		g.apps.start()
	}
	var addrs []string
	for _, a := range g.node.Addrs() {
		addrs = append(addrs, a.String())
	}
	g.report(Status{State: EnrollUp, FQDN: g.node.FQDN(), Addrs: addrs, APIURL: apiURL(g.node.FQDN(), g.TLSMode())})
	return nil
}

// listen opens an HTTP listener on the node in the gateway's TLS mode.
func (g *Gateway) listen(addr string) (net.Listener, error) {
	switch g.TLSMode() {
	case TLSFiles:
		ln, err := g.node.Listen(addr)
		if err != nil {
			return nil, err
		}
		return g.certs.listen(ln), nil
	case TLSTailscale:
		return g.node.ListenTLS(addr)
	}
	return g.node.Listen(addr)
}

func (g *Gateway) run(name string, f func() error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := f(); err != nil {
			g.opts.Logf("gateway: %s: %v", name, err)
			g.mu.Lock()
			g.errs = append(g.errs, fmt.Errorf("%s: %w", name, err))
			g.mu.Unlock()
		}
	}()
}

// Forward serves an allocated gateway port (e.g. ":8022") that tunnels each
// authorized connection to guestPort on vmRef. It runs until Close.
func (g *Gateway) Forward(addr, vmRef string, guestPort uint16) (net.Listener, error) {
	if g.ctx == nil {
		return nil, errors.New("gateway: not started")
	}
	ln, err := g.node.Listen(addr)
	if err != nil {
		return nil, err
	}
	g.run("forward "+addr, func() error {
		return g.streams.ServeForward(g.ctx, ln, g.who, vmRef, guestPort, g.opts.Logf)
	})
	return ln, nil
}

// Streams is the authorized guest-stream primitive for the HTTP tunnel
// endpoints (identity comes from IdentityFromContext).
func (g *Gateway) Streams() *Streams { return g.streams }

// Node is the underlying tailnet node.
func (g *Gateway) Node() *Node { return g.node }

// Close stops the listeners and the node, returning listener errors. It is
// idempotent but must not race Start; callers wait for Start to return.
func (g *Gateway) Close() error {
	g.closeOnce.Do(func() {
		if g.cancel != nil {
			g.cancel()
		}
		if g.apps != nil {
			g.apps.close()
		}
		g.wg.Wait()
		err := g.node.Close()
		g.mu.Lock()
		defer g.mu.Unlock()
		g.closeErr = errors.Join(append(g.errs, err)...)
	})
	return g.closeErr
}
