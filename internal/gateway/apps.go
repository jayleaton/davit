package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/httpx"
	"davit/internal/tokens"
)

// App endpoints (G11): each route is a gateway port on the shared tailnet
// node whose HTTP requests (streaming and WebSocket upgrades included) are
// proxied to an app on a guest loopback port. The URL is port-based
// (https://<gateway fqdn>:<port>/): Tailscale issues certificates and
// MagicDNS names only for the node itself, so sub-names of the gateway are
// not possible on the shared node, and a path prefix would break apps that
// use root-relative URLs and share the API's origin. A port gives each app
// its own origin with the node's certificate.

// AppRoute is one app endpoint the gateway serves.
type AppRoute struct {
	ID          string
	VMID        domain.VMID
	GuestPort   uint16
	GatewayPort int
	// ForwardIdentity sends the caller's tailnet login to the app
	// (Tailscale-User-Login); off, the app learns nothing of it.
	ForwardIdentity bool
	// Desktop serves the viewer page and an RFB WebSocket (/websockify)
	// to a VNC server on GuestPort instead of proxying HTTP (G15).
	Desktop bool
}

// AppAuthorizer decides one app request: the tailnet identity, and the
// service-token secret when the request carried one (never forwarded to
// the app). It returns the VM's current instance, ErrDenied/ErrNotFound
// (403), ErrNotRunning (503) or ErrUnauthenticated (401).
type AppAuthorizer interface {
	// The returned level is the caller's right on the VM as a token
	// action (view, exec or admin): HTTP endpoints need exec, a desktop
	// view (view-only) or exec (input).
	AuthorizeApp(ctx context.Context, who Identity, bearer string, vm domain.VMID) (Target, string, error)
}

// levelRank orders token actions.
func levelRank(level string) int {
	switch level {
	case api.TokenAdmin:
		return 3
	case api.TokenExec:
		return 2
	case api.TokenView:
		return 1
	}
	return 0
}

// ErrUnauthenticated: the request's credential (a bearer) is invalid.
var ErrUnauthenticated = errors.New("gateway: invalid credential")

// identityHeaders are set by the gateway alone; client values are dropped.
var identityHeaders = []string{"Tailscale-User-Login", "Tailscale-User-Name", "Tailscale-User-Profile-Pic", "Tailscale-App-Capabilities"}

type appServer struct {
	g     *Gateway
	auth  AppAuthorizer
	trans *http.Transport

	mu      sync.Mutex
	started bool
	want    map[int]AppRoute
	serving map[int]*appListener
	conns   map[*appConn]struct{}
}

type appListener struct {
	route AppRoute
	srv   *http.Server
}

type ctxKeyApp struct{}

// connIdentity caches the WhoIs of one accepted connection: a TCP
// connection's peer cannot change, so its requests share one lookup.
// Authorization still runs on every request.
type connIdentity struct {
	conn net.Conn
	once sync.Once
	who  Identity
	err  error
}

type ctxKeyConn struct{}

type appTarget struct {
	t     Target
	route AppRoute
}

// appConn is a guest connection carrying one route's requests.
type appConn struct {
	net.Conn
	s     *appServer
	vm    domain.VMID
	gen   int64
	route string
	once  sync.Once
}

func (c *appConn) Close() error {
	c.once.Do(func() {
		c.s.mu.Lock()
		delete(c.s.conns, c)
		c.s.mu.Unlock()
	})
	return c.Conn.Close()
}

func newAppServer(g *Gateway, auth AppAuthorizer) *appServer {
	s := &appServer{g: g, auth: auth, want: map[int]AppRoute{}, serving: map[int]*appListener{}, conns: map[*appConn]struct{}{}}
	s.trans = &http.Transport{
		DialContext:           s.dial,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 0, // streaming apps answer when they like
		ForceAttemptHTTP2:     false,
	}
	return s
}

// dial opens a guest connection for the request's authorized target.
func (s *appServer) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	at, ok := ctx.Value(ctxKeyApp{}).(appTarget)
	if !ok {
		return nil, errors.New("gateway: app dial without a target")
	}
	c, err := s.g.opts.Dialer.DialGuest(ctx, at.t, at.route.GuestPort)
	if err != nil {
		return nil, err
	}
	ac := &appConn{Conn: c, s: s, vm: at.t.VMID, gen: at.t.Generation, route: at.route.ID}
	s.mu.Lock()
	s.conns[ac] = struct{}{}
	s.mu.Unlock()
	return ac, nil
}

// SetApps replaces the served app routes (by gateway port). Routes no
// longer wanted, or changed, close their port and cut their connections.
// Before Start the routes are kept and served once the node is up.
func (g *Gateway) SetApps(routes []AppRoute) {
	if g.apps == nil {
		return
	}
	s := g.apps
	s.mu.Lock()
	s.want = map[int]AppRoute{}
	for _, r := range routes {
		s.want[r.GatewayPort] = r
	}
	started := s.started
	s.mu.Unlock()
	if started {
		s.reconcile()
	}
}

// CutApps closes app connections to vm's instances other than
// keepGeneration (0: all of them), for a stop, delete or new generation.
func (g *Gateway) CutApps(vm domain.VMID, keepGeneration int64) {
	if g.apps == nil {
		return
	}
	g.apps.cut(func(c *appConn) bool { return c.vm == vm && c.gen != keepGeneration })
}

func (s *appServer) cut(match func(*appConn) bool) {
	s.mu.Lock()
	var victims []*appConn
	for c := range s.conns {
		if match(c) {
			victims = append(victims, c)
		}
	}
	s.mu.Unlock()
	for _, c := range victims {
		c.Close()
	}
	s.trans.CloseIdleConnections()
}

func (s *appServer) start() {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	s.reconcile()
}

func (s *appServer) close() {
	s.mu.Lock()
	s.started = false
	ls := s.serving
	s.serving = map[int]*appListener{}
	s.mu.Unlock()
	for _, l := range ls {
		l.srv.Close()
	}
	s.cut(func(*appConn) bool { return true })
}

func (s *appServer) reconcile() {
	s.mu.Lock()
	var stop []*appListener
	var start []AppRoute
	for port, l := range s.serving {
		if w, ok := s.want[port]; !ok || w != l.route {
			stop = append(stop, l)
			delete(s.serving, port)
		}
	}
	for port, r := range s.want {
		if _, ok := s.serving[port]; !ok {
			start = append(start, r)
		}
	}
	s.mu.Unlock()
	for _, l := range stop {
		l.srv.Close()
		id := l.route.ID
		s.cut(func(c *appConn) bool { return c.route == id })
	}
	for _, r := range start {
		ln, err := s.g.listen(":" + strconv.Itoa(r.GatewayPort))
		if err != nil {
			s.g.opts.Logf("gateway: app endpoint %s: listen on :%d: %v", r.ID, r.GatewayPort, err)
			continue
		}
		srv := httpx.Harden(&http.Server{
			Handler:     s.handler(r),
			BaseContext: func(net.Listener) context.Context { return s.g.ctx },
			ConnContext: func(ctx context.Context, c net.Conn) context.Context {
				return context.WithValue(ctx, ctxKeyConn{}, &connIdentity{conn: c})
			},
		})
		s.mu.Lock()
		if !s.started || s.want[r.GatewayPort] != r {
			s.mu.Unlock()
			ln.Close()
			continue
		}
		s.serving[r.GatewayPort] = &appListener{route: r, srv: srv}
		s.mu.Unlock()
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.g.opts.Logf("gateway: app endpoint %s: %v", r.ID, err)
			}
		}()
	}
}

func appError(w http.ResponseWriter, status int, code domain.Code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(api.ErrorResponse{Error: api.Error{Code: code, Message: msg}})
}

// handler serves one route: identify, authorize, proxy.
// track records the request's client connection for revocation (G13:
// unsharing cuts a member's connection) until the returned func runs.
func (s *appServer) track(req *http.Request, who Identity, t Target) func() {
	ci, _ := req.Context().Value(ctxKeyConn{}).(*connIdentity)
	if ci == nil || ci.conn == nil {
		return func() {}
	}
	return s.g.streams.Sessions.Track(who.UserID, t.VMID, ci.conn)
}

// authorize identifies and authorizes one request; on failure it has
// answered the request.
func (s *appServer) authorize(w http.ResponseWriter, req *http.Request, r AppRoute) (Identity, Target, string, bool) {
	ci, _ := req.Context().Value(ctxKeyConn{}).(*connIdentity)
	if ci == nil {
		ci = &connIdentity{}
	}
	ci.once.Do(func() { ci.who, ci.err = Identify(req.Context(), s.g.who, req.RemoteAddr) })
	who, err := ci.who, ci.err
	if err != nil {
		appError(w, http.StatusUnauthorized, domain.CodeUnauthenticated, "tailnet identity required")
		return who, Target{}, "", false
	}
	bearer := ""
	if h := req.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer slw_") {
		bearer = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		req.Header.Del("Authorization")
	}
	t, level, err := s.auth.AuthorizeApp(req.Context(), who, bearer, r.VMID)
	need := api.TokenExec
	if r.Desktop {
		need = api.TokenView
	}
	switch {
	case errors.Is(err, ErrUnauthenticated):
		appError(w, http.StatusUnauthorized, domain.CodeUnauthenticated, "the service token is invalid, expired or revoked")
	case errors.Is(err, ErrNotRunning):
		appError(w, http.StatusServiceUnavailable, domain.CodeUnavailable, "the VM is not running; resume it to serve this endpoint again")
	case err != nil || levelRank(level) < levelRank(need):
		appError(w, http.StatusForbidden, domain.CodePermissionDenied, "this tailnet identity may not use this endpoint")
	default:
		return who, t, level, true
	}
	return who, Target{}, "", false
}

func (s *appServer) handler(r AppRoute) http.Handler {
	if r.Desktop {
		return s.desktopHandler(r)
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			// The pool key: one host per VM instance and port, so kept-alive
			// guest connections never cross instances.
			at := pr.In.Context().Value(ctxKeyApp{}).(appTarget)
			pr.Out.URL.Host = fmt.Sprintf("%s.%d.%d.guest", strings.ReplaceAll(string(at.t.VMID), "_", "-"), at.t.Generation, r.GuestPort)
			pr.SetXForwarded()
			pr.Out.Host = pr.In.Host // the app sees the browser's Host (and Origin)
		},
		Transport:     s.trans,
		FlushInterval: -1, // stream (SSE, chunked) as the app writes
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			s.g.opts.Logf("gateway: app endpoint %s: %v", r.ID, err)
			appError(w, http.StatusBadGateway, domain.CodeUnavailable, "the app on guest port "+strconv.Itoa(int(r.GuestPort))+" did not answer")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		who, t, _, ok := s.authorize(w, req, r)
		if !ok {
			return
		}
		defer s.track(req, who, t)()
		for _, h := range identityHeaders {
			req.Header.Del(h)
		}
		if r.ForwardIdentity && len(who.Tags) == 0 && who.LoginName != "" {
			req.Header.Set("Tailscale-User-Login", who.LoginName)
		}
		rp.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), ctxKeyApp{}, appTarget{t: t, route: r})))
	})
}

// OwnerAppAuthorizer is the v0 AppAuthorizer: the owner, or a service token
// whose grant has exec on the VM (as for tunnels), on a running VM.
type OwnerAppAuthorizer struct {
	Owners    OwnerMatcher
	Principal domain.PrincipalID
	Tokens    TokenVerifier
	VMs       VMLookup
	// Members may use endpoints of VMs shared with them with exec.
	Members MemberDirectory
}

// grantLevel is the highest action g grants on vm ("" for none).
func grantLevel(g *tokens.Grant, vm domain.VMID) string {
	for _, a := range []string{api.TokenAdmin, api.TokenExec, api.TokenView} {
		if g.Allows(a, vm) {
			return a
		}
	}
	return ""
}

// AuthorizeApp implements AppAuthorizer.
func (a OwnerAppAuthorizer) AuthorizeApp(ctx context.Context, who Identity, bearer string, vmID domain.VMID) (Target, string, error) {
	p, level := a.Principal, api.TokenAdmin
	switch {
	case bearer != "":
		if a.Tokens == nil {
			return Target{}, "", ErrUnauthenticated
		}
		tp, g, err := a.Tokens.Verify(ctx, bearer)
		if err != nil {
			return Target{}, "", ErrUnauthenticated
		}
		if g == nil {
			return Target{}, "", ErrDenied
		}
		if level = grantLevel(g, vmID); level == "" {
			return Target{}, "", ErrDenied
		}
		p = tp
	case a.Owners != nil && a.Owners.IsOwner(who):
	default:
		// A G13 member: its role on the VM is its level (a viewer gets a
		// view-only desktop and no HTTP endpoints).
		g, ok := (*tokens.Grant)(nil), false
		if a.Members != nil {
			g, ok = a.Members.MemberGrant(who)
		}
		if !ok {
			return Target{}, "", ErrDenied
		}
		if level = grantLevel(g, vmID); level == "" {
			return Target{}, "", ErrDenied
		}
	}
	if p == "" || a.VMs == nil {
		return Target{}, "", ErrDenied
	}
	vm, err := a.VMs.GetVM(ctx, p, string(vmID))
	if err != nil {
		return Target{}, "", ErrNotFound
	}
	switch vm.State {
	case domain.StateReady, domain.StateRunning:
		return Target{VMID: vm.ID, Generation: vm.InstanceGeneration}, level, nil
	case domain.StateDeleting, domain.StateDeleted:
		return Target{}, "", ErrNotFound
	}
	return Target{}, "", fmt.Errorf("%w: vm is %s", ErrNotRunning, vm.State)
}

// AppBase is the scheme and host app endpoint URLs start with
// (https://<fqdn>, or http:// with TLS off); "" until the node is up.
func (g *Gateway) AppBase() string {
	if g.apps == nil {
		return ""
	}
	g.apps.mu.Lock()
	started := g.apps.started
	g.apps.mu.Unlock()
	if !started {
		return ""
	}
	return apiURL(g.node.FQDN(), g.TLSMode())
}
