package credproxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"davit/internal/domain"
	"davit/internal/httpx"
	"davit/internal/network"
	"davit/internal/network/netguard"
	"davit/internal/state"
)

// GuestPort is the TCP port of the proxy on the netns side of every VM's
// TAP (network.Config.TapIP, 172.16.0.1). Each VM's listener lives in that
// VM's own network namespace, so a guest reaches its own proxy only, and
// the proxy knows the VM from the listener, never from the request.
const GuestPort uint16 = 15080

// GuestBaseURL is the base URL a guest uses for the proxy (point the
// provider SDK's base URL here; the placeholder goes where the key would).
func GuestBaseURL(cfg network.Config) string {
	return "http://" + net.JoinHostPort(cfg.TapIP.String(), strconv.Itoa(int(GuestPort)))
}

// Default upstreams of the providers whose API base URL is well known; any
// other upstream comes from the credential's BaseURL.
var providerUpstreams = map[string]string{
	"anthropic": "https://api.anthropic.com",
	"openai":    "https://api.openai.com",
}

// UpstreamFor returns the upstream of a credential: its BaseURL, else its
// provider's well-known API.
func UpstreamFor(c domain.Credential) (string, error) {
	if c.BaseURL != "" {
		return ValidUpstream(c.BaseURL)
	}
	if u, ok := providerUpstreams[c.Provider]; ok {
		return u, nil
	}
	return "", domain.Errorf(domain.CodeInvalidArgument, "credential %s has no baseUrl and provider %q has no known API (known: anthropic, openai)", c.Name, c.Provider).WithDetail("field", "baseUrl")
}

// ValidUpstream checks and canonicalizes an upstream base URL: http or
// https, a host, no credentials, query or fragment; no trailing slash.
func ValidUpstream(s string) (string, error) {
	bad := func(why string) (string, error) {
		return "", domain.Errorf(domain.CodeInvalidArgument, "upstream %q: %s", s, why).WithDetail("field", "baseUrl")
	}
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return bad("not a URL")
	case u.Scheme != "https" && u.Scheme != "http":
		return bad("scheme must be https (or http)")
	case u.Host == "" || u.Hostname() == "":
		return bad("no host")
	case u.User != nil:
		return bad("must not carry credentials")
	case u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return bad("must not have a query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

var envVarRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// ValidEnvVar checks a guest environment variable name.
func ValidEnvVar(s string) error {
	if !envVarRe.MatchString(s) {
		return domain.Errorf(domain.CodeInvalidArgument, "envVar %q must be a shell variable name", s).WithDetail("field", "envVar")
	}
	return nil
}

// placeholderPrefix starts every new placeholder; legacyPlaceholderPrefix
// those of grants made before the slipway -> davit rename, which guests may
// still hold. placeholderRe finds either in guest request headers.
const (
	placeholderPrefix       = "davit-placeholder-"
	legacyPlaceholderPrefix = "slipway-placeholder-"
)

var placeholderRe = regexp.MustCompile(`(?:davit|slipway)-placeholder-[0-9a-f]{48}`)

func newPlaceholder() string { return placeholderPrefix + randomHex(24) }

// Host is the real Proxy: grants in the state database, secrets in a
// SecretStore, and one guest-facing HTTP listener per running VM (inside
// the VM's network namespace) that swaps the grant's placeholder for the
// secret on requests to the grant's upstream, streaming both ways.
type Host struct {
	DB      *state.DB
	Secrets SecretStore
	Now     func() time.Time
	Log     *slog.Logger
	// Upstream sends proxied requests (default: a netguard transport).
	Upstream http.RoundTripper
	// AllowPrivateUpstreams lets credentials point at loopback, private
	// and tailnet addresses, e.g. a model server on the LAN
	// (network.allowPrivateUpstreams). Link-local and metadata addresses
	// are refused either way.
	AllowPrivateUpstreams bool
	// Listen opens the guest-facing listener of a VM network
	// (network.TapListener.ListenTCP on GuestPort). Nil: no listeners (the
	// proxy then serves only through Handler, e.g. in tests).
	Listen func(ctx context.Context, h network.Handle) (net.Listener, error)
	// Usable, if set, may refuse a VM's grants for now (G13: a VM shared
	// with exec or admin without credentials pauses them).
	Usable func(vm domain.VMID) error

	mu             sync.Mutex
	guarded        *http.Transport // upstream()
	guardedPrivate bool
	streams        map[string]map[*stream]struct{} // by grant ID: requests in flight
	servers        map[domain.VMID]*http.Server
}

type stream struct{ cancel context.CancelFunc }

func (h *Host) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

func (h *Host) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.New(slog.DiscardHandler)
}

// newGrant validates spec against the VM record and builds the grant.
func (h *Host) newGrant(ctx context.Context, tx *state.Tx, spec GrantSpec) (*domain.CredentialGrant, domain.PrincipalID, error) {
	rec, err := tx.GetVM(spec.VM)
	if err != nil {
		return nil, "", err
	}
	if rec.VM.State == domain.StateDeleted || rec.VM.State == domain.StateDeleting {
		return nil, "", domain.Errorf(domain.CodeNotFound, "vm %s not found", spec.VM)
	}
	if _, err := h.Secrets.Secret(ctx, spec.ProfileID, spec.Credential); err != nil {
		return nil, "", err
	}
	if err := ValidEnvVar(spec.EnvVar); err != nil {
		return nil, "", err
	}
	up, err := ValidUpstream(spec.Upstream)
	if err != nil {
		return nil, "", err
	}
	return &domain.CredentialGrant{
		ID: domain.NewID("grant"), VMID: spec.VM, ProfileID: spec.ProfileID, Credential: spec.Credential,
		State: domain.GrantActive, Placeholder: newPlaceholder(), EnvVar: spec.EnvVar, Upstream: up,
		CreatedAt: h.now(),
	}, rec.VM.Owner, nil
}

// Grant implements Proxy.
func (h *Host) Grant(ctx context.Context, spec GrantSpec) (domain.CredentialGrant, error) {
	var out domain.CredentialGrant
	err := h.DB.Tx(ctx, func(tx *state.Tx) error {
		g, owner, err := h.newGrant(ctx, tx, spec)
		if err != nil {
			return err
		}
		out = *g
		return tx.PutGrantKeyed(owner, g, "", "")
	})
	return out, err
}

// Revoke implements Proxy: the placeholder stops working at once and the
// grant's streams in flight are cut. A revoked grant keeps its first reason.
func (h *Host) Revoke(ctx context.Context, id string, reason domain.RevokeReason) (domain.CredentialGrant, error) {
	var out domain.CredentialGrant
	err := h.DB.Tx(ctx, func(tx *state.Tx) error {
		if _, _, err := tx.GetGrant(id); err != nil {
			return err
		}
		if _, err := tx.RevokeGrantsWhere(state.GrantFilter{ID: id}, reason, h.now()); err != nil {
			return err
		}
		g, _, err := tx.GetGrant(id)
		if err == nil {
			out = *g
		}
		return err
	})
	if err == nil {
		h.Cut(id)
	}
	return out, err
}

// Lifecycle implements Proxy (domain.GrantAfter).
func (h *Host) Lifecycle(ctx context.Context, vm domain.VMID, e domain.GrantEvent) error {
	keep, _, reason := domain.GrantAfter(e)
	if keep {
		return nil
	}
	var ids []string
	err := h.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		ids, err = tx.RevokeGrantsWhere(state.GrantFilter{VM: vm}, reason, h.now())
		return err
	})
	h.Cut(ids...)
	if len(ids) > 0 {
		h.log().Info("credential grants revoked", "vm", vm, "event", e, "count", len(ids))
	}
	return err
}

// errDenied is the one answer for unknown, foreign and revoked
// placeholders: the guest learns nothing about other grants.
var errDenied = domain.Errorf(domain.CodePermissionDenied, "credential placeholder not valid for this VM")

// Resolve implements Proxy.
func (h *Host) Resolve(ctx context.Context, vm domain.VMID, placeholder string) (Resolved, error) {
	var g *domain.CredentialGrant
	err := h.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		g, _, err = tx.GrantByPlaceholder(placeholder)
		return err
	})
	if domain.IsCode(err, domain.CodeNotFound) || err == nil && (g.VMID != vm || g.State != domain.GrantActive) {
		return Resolved{}, errDenied
	}
	if err != nil {
		return Resolved{}, err
	}
	if h.Usable != nil {
		if err := h.Usable(vm); err != nil {
			return Resolved{}, err
		}
	}
	secret, err := h.Secrets.Secret(ctx, g.ProfileID, g.Credential)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{Grant: *g, Secret: secret}, nil
}

// Cut ends the requests in flight of the given grants.
func (h *Host) Cut(ids ...string) {
	h.mu.Lock()
	var cancels []context.CancelFunc
	for _, id := range ids {
		for s := range h.streams[id] {
			cancels = append(cancels, s.cancel)
		}
	}
	h.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

func (h *Host) track(id string, s *stream) func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams == nil {
		h.streams = map[string]map[*stream]struct{}{}
	}
	if h.streams[id] == nil {
		h.streams[id] = map[*stream]struct{}{}
	}
	h.streams[id][s] = struct{}{}
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.streams[id], s)
		if len(h.streams[id]) == 0 {
			delete(h.streams, id)
		}
	}
}

// Handler serves vm's guest. A request must carry exactly one placeholder
// (in any header, e.g. x-api-key or Authorization: Bearer) of an active
// grant of vm. It is sent to that grant's upstream only, whatever URL or
// Host the guest names, with the placeholder replaced by the secret in
// every header; responses (SSE included) stream back as they arrive, with
// any echo of the secret scrubbed from response headers.
func (h *Host) Handler(vm domain.VMID) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			guestError(w, http.StatusMethodNotAllowed, "the credential proxy forwards plain HTTP API requests only")
			return
		}
		ph, ok := placeholderOf(r.Header)
		if !ok {
			guestError(w, http.StatusUnauthorized, "no credential placeholder in the request (put the grant's placeholder where the API key goes)")
			return
		}
		// SEC-5: the secret goes only where APIs read keys; a placeholder
		// anywhere else could be echoed back by the upstream.
		if name, misplaced := placeholderOutsideAuth(r.Header); misplaced {
			guestError(w, http.StatusBadRequest, "the credential placeholder may only appear in an API key header (Authorization, X-Api-Key, Api-Key, X-Goog-Api-Key), not in "+name)
			return
		}
		// SEC-7: the guest's path is appended to the grant's base path;
		// dot segments could climb out of it.
		if dotSegments(r.URL.EscapedPath()) {
			guestError(w, http.StatusBadRequest, "the request path must not contain dot segments or encoded slashes")
			return
		}
		res, err := h.Resolve(r.Context(), vm, ph)
		if err != nil {
			code := http.StatusForbidden
			if !domain.IsCode(err, domain.CodePermissionDenied) {
				code = http.StatusBadGateway
				h.log().Warn("credential proxy resolve", "vm", vm, "err", err)
			}
			guestError(w, code, "credential grant not valid for this VM")
			return
		}
		target, err := url.Parse(res.Grant.Upstream)
		if err != nil {
			guestError(w, http.StatusBadGateway, "grant upstream is invalid")
			return
		}
		// SEC-6: the secret never crosses the network in cleartext unless
		// the operator allowed private upstreams (a model server on the LAN).
		if target.Scheme != "https" && !h.AllowPrivateUpstreams {
			guestError(w, http.StatusBadGateway, "the credential's upstream is plain http; the host allows that only with network.allowPrivateUpstreams")
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer h.track(res.Grant.ID, &stream{cancel: cancel})()
		secret := string(res.Secret)
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = target.Host
				for _, name := range authHeaders {
					vs := pr.Out.Header.Values(name)
					for i, v := range vs {
						vs[i] = strings.ReplaceAll(v, ph, secret)
					}
				}
			},
			Transport:     h.upstream(),
			FlushInterval: -1, // stream SSE and chunked answers as they come
			ModifyResponse: func(resp *http.Response) error {
				scrubHeader(resp.Header, secret)
				// An upstream may quote the key back in an error body or a
				// trailer (SEC-5): scrub those as they stream.
				resp.Body = &redactingBody{r: resp.Body, secret: []byte(secret), resp: resp}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				if ctx.Err() == nil {
					h.log().Warn("credential proxy upstream", "vm", vm, "grant", res.Grant.ID, "upstream", res.Grant.Upstream, "err", redact(err, secret))
				}
				guestError(w, http.StatusBadGateway, "upstream request failed")
			},
		}
		rp.ServeHTTP(w, r.WithContext(ctx))
	})
}

// upstream is h.Upstream, else a netguard transport: the address actually
// dialed (after DNS) is checked, so a baseUrl that resolves or rebinds to
// loopback, private, tailnet or metadata addresses is refused unless
// AllowPrivateUpstreams, and environment proxies are not used (SEC-6).
func (h *Host) upstream() http.RoundTripper {
	if h.Upstream != nil {
		return h.Upstream
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.guarded == nil || h.guardedPrivate != h.AllowPrivateUpstreams {
		h.guarded = netguard.Policy{AllowPrivate: h.AllowPrivateUpstreams}.Transport()
		h.guardedPrivate = h.AllowPrivateUpstreams
	}
	return h.guarded
}

// placeholderOf finds the one placeholder in hdr; none or two different
// ones is no placeholder.
func placeholderOf(hdr http.Header) (string, bool) {
	var found string
	for _, vs := range hdr {
		for _, v := range vs {
			for _, m := range placeholderRe.FindAllString(v, -1) {
				if found != "" && m != found {
					return "", false
				}
				found = m
			}
		}
	}
	return found, found != ""
}

func redact(err error, secret string) string {
	return strings.ReplaceAll(err.Error(), secret, "[redacted]")
}

func guestError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "davit_credential_proxy", "message": msg}})
}

// AttachNetwork starts serving vm's guest on its network (at boot, and for
// running VMs at daemon start); a previous listener of vm is replaced.
// Without a Listen function or on a network without a guest NIC it does
// nothing. Failures are logged: the VM runs, its grants just cannot be
// used until the next boot.
func (h *Host) AttachNetwork(ctx context.Context, vm domain.VMID, nh network.Handle) {
	if h.Listen == nil || nh.Mode == network.ModeNone || nh.Mode == "" {
		return
	}
	ln, err := h.Listen(ctx, nh)
	if err != nil {
		h.log().Warn("credential proxy: cannot listen on the VM network", "vm", vm, "generation", nh.Generation, "err", err)
		return
	}
	srv := httpx.Harden(&http.Server{Handler: h.Handler(vm), ErrorLog: slogStd(h.log())})
	h.mu.Lock()
	if h.servers == nil {
		h.servers = map[domain.VMID]*http.Server{}
	}
	old := h.servers[vm]
	h.servers[vm] = srv
	h.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			h.log().Warn("credential proxy listener ended", "vm", vm, "err", err)
		}
	}()
}

// DetachNetwork stops serving vm's guest and closes its connections.
func (h *Host) DetachNetwork(vm domain.VMID) {
	h.mu.Lock()
	srv := h.servers[vm]
	delete(h.servers, vm)
	h.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// Close stops every listener.
func (h *Host) Close() {
	h.mu.Lock()
	srvs := h.servers
	h.servers = nil
	h.mu.Unlock()
	for _, s := range srvs {
		_ = s.Close()
	}
}

// slogStd routes net/http server errors (bad requests from a guest) to
// debug logs.
func slogStd(l *slog.Logger) *log.Logger {
	return slog.NewLogLogger(l.Handler(), slog.LevelDebug)
}

var _ Proxy = (*Host)(nil)
