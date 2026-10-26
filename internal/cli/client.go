package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/wsock"
)

// client calls API v1 over the local socket (or, later, the tailnet).
type client struct {
	http      *http.Client
	base      string
	requestID string
	// dial opens a raw connection to the daemon (WebSocket streams).
	dial func(ctx context.Context) (net.Conn, error)
	// token is a service-token secret sent as a bearer to remote (tailnet)
	// API URLs; the local socket is owner-only and never gets one.
	token string
	// host is the remote API host (WebSocket Host header); "" for the socket.
	host string
	// noHost: no host was configured, so this is the default local socket.
	noHost bool
}

// unreachable is the error for a request that never reached the daemon.
// With no host configured and no local daemon socket, that is a client
// machine that has not run `davit connect` yet.
func (c *client) unreachable(err error) *cliError {
	if c.noHost && transportRetryable(err) {
		n := buildinfo.Name
		return clientErr(domain.CodeUnavailable, "no %s host configured: run `%s connect user@host` (on the host itself: "+
			"start the daemon with `systemctl --user start %s`, or `%s doctor`)", n, n, n, n)
	}
	var ce *cliError
	if errors.As(err, &ce) {
		return ce // the ssh transport's own diagnosis (sshFailure)
	}
	e := clientErr(domain.CodeUnavailable, "cannot reach the daemon: %v", err)
	e.Retryable = true
	return e
}

// CAFileEnv names a PEM bundle of extra CAs trusted for a remote https API
// (an operator certificate); system roots stay trusted.
var CAFileEnv = buildinfo.Env("CA_FILE")

// remoteTLS is the TLS configuration for a remote https API: system roots
// plus $DAVIT_CA_FILE, verifying serverName.
func remoteTLS(serverName string) (*tls.Config, error) {
	conf := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	file := os.Getenv(CAFileEnv)
	if file == "" {
		return conf, nil
	}
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, usageErr("%s: %v", CAFileEnv, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, usageErr("%s=%s holds no PEM certificates", CAFileEnv, file)
	}
	conf.RootCAs = pool
	return conf, nil
}

// hostPort is u's host with def as the port when u has none.
func hostPort(u *url.URL, def string) string {
	if u.Port() != "" {
		return u.Host
	}
	return net.JoinHostPort(u.Hostname(), def)
}

// authorize adds the service-token bearer, if any.
func (c *client) authorize(h http.Header) {
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
}

func newClient(apiURL, requestID string) (*client, error) {
	u, err := url.Parse(apiURL)
	if err != nil {
		return nil, usageErr("invalid --api-url %q", apiURL)
	}
	switch u.Scheme {
	case "ssh":
		dial, err := sshDialer(u)
		if err != nil {
			return nil, err
		}
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
			IdleConnTimeout: 30 * time.Second}
		return &client{http: &http.Client{Transport: tr}, base: "http://local" + api.BasePath, requestID: requestID, dial: dial}, nil
	case "unix":
		sock := u.Path
		if sock == "" {
			sock = u.Opaque
		}
		dial := func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}
		tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }}
		return &client{http: &http.Client{Transport: tr}, base: "http://local" + api.BasePath, requestID: requestID, dial: dial}, nil
	case "http", "https":
		c := &client{http: &http.Client{}, base: strings.TrimSuffix(apiURL, "/") + api.BasePath, requestID: requestID,
			token: strings.TrimSpace(os.Getenv(buildinfo.Env("TOKEN"))), host: u.Host}
		if u.Scheme == "http" {
			c.dial = func(ctx context.Context) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", hostPort(u, "80"))
			}
			return c, nil
		}
		conf, err := remoteTLS(u.Hostname())
		if err != nil {
			return nil, err
		}
		c.http.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: conf, ForceAttemptHTTP2: true}
		c.dial = func(ctx context.Context) (net.Conn, error) {
			d := tls.Dialer{Config: conf.Clone()}
			d.Config.NextProtos = []string{"http/1.1"} // WebSocket upgrades are HTTP/1.1
			return d.DialContext(ctx, "tcp", hostPort(u, "443"))
		}
		return c, nil
	}
	return nil, usageErr("--api-url must be unix:///path/to.sock, http(s)://host or ssh://[user@]host")
}

// transportRetryable reports whether a request certainly did not reach the
// server, so retrying with the same idempotency key is safe and cheap.
func transportRetryable(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// do sends a request and decodes a 2xx body into out. Mutations carry an
// idempotency key generated once by the caller and reused on retry.
func (c *client) do(ctx context.Context, method, path string, q url.Values, body any, hdr map[string]string, out any) error {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return err
		}
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set(api.HeaderRequestID, c.requestID)
		c.authorize(req.Header)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err = c.http.Do(req)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return clientErr(domain.CodeTimeout, "timed out waiting for the daemon")
		}
		if attempt >= 3 || !transportRetryable(err) {
			return c.unreachable(err)
		}
		time.Sleep(time.Duration(100<<attempt) * time.Millisecond)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return clientErr(domain.CodeUnavailable, "reading response: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		var er api.ErrorResponse
		if json.Unmarshal(data, &er) != nil || er.Error.Code == "" {
			return &cliError{ErrorBody{Code: domain.CodeInternal, Origin: "server",
				Message: fmt.Sprintf("unexpected HTTP %d from daemon", resp.StatusCode), RequestID: resp.Header.Get(api.HeaderRequestID)}}
		}
		return &cliError{ErrorBody{Code: er.Error.Code, Message: er.Error.Message, Retryable: er.Error.Retryable,
			Details: er.Error.Details, Origin: "server", RequestID: er.Error.RequestID, OperationID: er.Error.OperationID}}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return clientErr(domain.CodeInternal, "decoding response: %v", err)
		}
	}
	return nil
}

func (c *client) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, q, nil, nil, out)
}

func (c *client) mutate(ctx context.Context, method, path string, body any, key string) (*api.OperationAccepted, error) {
	var out api.OperationAccepted
	return &out, c.do(ctx, method, path, nil, body, map[string]string{api.HeaderIdempotencyKey: key}, &out)
}

func (c *client) events(ctx context.Context, op domain.OperationID, after int64, wait time.Duration) (*api.OperationEventList, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "waitMs": {strconv.FormatInt(wait.Milliseconds(), 10)}}
	var out api.OperationEventList
	return &out, c.get(ctx, "/operations/"+url.PathEscape(string(op))+"/events", q, &out)
}

// websocket opens the WebSocket at path (BasePath-relative path already
// prefixed by the caller). A non-101 answer is decoded as an API error.
func (c *client) websocket(ctx context.Context, path string) (*wsock.Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, c.unreachable(err)
	}
	hdr := http.Header{api.HeaderRequestID: {c.requestID}}
	c.authorize(hdr)
	host := c.host
	if host == "" {
		host = "local"
	}
	ws, resp, err := wsock.Client(ctx, conn, host, path, hdr)
	if err == nil {
		return ws, nil
	}
	if resp != nil {
		defer resp.Body.Close()
		var er api.ErrorResponse
		if data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)); json.Unmarshal(data, &er) == nil && er.Error.Code != "" {
			return nil, &cliError{ErrorBody{Code: er.Error.Code, Message: er.Error.Message, Retryable: er.Error.Retryable,
				Details: er.Error.Details, Origin: "server", RequestID: er.Error.RequestID}}
		}
	}
	return nil, clientErr(domain.CodeUnavailable, "opening the process stream: %v", err)
}
