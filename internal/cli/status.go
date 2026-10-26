package cli

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/preflight"
)

// runStatus diagnoses the endpoint this CLI is configured for (the local
// socket, or --host / DAVIT_API_URL), naming the fix for each failure. It
// uses exactly the configuration the CLI, `davit ssh` and the SDKs share:
// DAVIT_API_URL, DAVIT_TOKEN and DAVIT_CA_FILE.
func runStatus(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 0, "status"); err != nil {
		return err
	}
	remote := !strings.HasPrefix(s.g.apiURL, "unix:")
	checks := []preflight.Check{endpointCheck(s.g.apiURL, s.cl.token)}
	if remote && !strings.HasPrefix(s.g.apiURL, "ssh:") { // over ssh the key is the credential
		checks = append(checks, credentialCheck(s.cl.token))
		if c, ok := caFileCheck(); ok {
			checks = append(checks, c)
		}
	}
	checks = append(checks, s.apiCheck(ctx, remote))
	if !remote {
		// On the host: how remote clients reach this daemon.
		if cfg, _, err := config.LoadDefault(o.config); err == nil {
			checks = append(checks, preflight.GatewayChecks(preflight.FromConfig(cfg), time.Now())...)
		}
	}

	var failed []string
	for _, c := range checks {
		if c.Status == preflight.Fail {
			failed = append(failed, c.ID)
		}
		s.out.emit("item", "", 0, c, nil)
		s.out.human("%-5s %-15s %s", strings.ToUpper(string(c.Status)), c.ID, c.Detail)
		if c.Hint != "" && c.Status != preflight.Pass {
			s.out.human("      %-15s fix: %s", "", c.Hint)
		}
	}
	if len(failed) > 0 {
		e := clientErr(domain.CodeUnavailable, "%d status check(s) failed: %s", len(failed), strings.Join(failed, ", "))
		e.Details = map[string]any{"failed": failed}
		return e
	}
	s.out.result("", map[string]any{"ok": true, "endpoint": redactURL(s.g.apiURL), "remote": remote})
	return nil
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func endpointCheck(apiURL, token string) preflight.Check {
	c := preflight.Check{ID: "endpoint", Title: "API endpoint", Status: preflight.Pass}
	u, err := url.Parse(apiURL)
	if err != nil {
		c.Status, c.Detail, c.Hint = preflight.Fail, err.Error(), "set "+buildinfo.Env("API_URL")+"=https://<host> or pass --host"
		return c
	}
	switch u.Scheme {
	case "unix":
		c.Detail = "local daemon socket " + strings.TrimPrefix(apiURL, "unix://")
	case "ssh":
		if t, err := parseSSHTarget(u); err != nil {
			c.Status, c.Detail, c.Hint = preflight.Fail, err.Error(), buildinfo.Name+" connect USER@HOST"
		} else {
			c.Detail = "remote " + t.login() + " over ssh (the daemon socket through `" + buildinfo.Name + " dial-stdio`)"
		}
	case "https":
		c.Detail = "remote " + redactURL(apiURL)
	case "http":
		c.Detail = "remote " + redactURL(apiURL) + " over plain http (WireGuard-encrypted on the tailnet)"
		if loopbackHost(u.Hostname()) {
			break
		}
		c.Status = preflight.Warn
		c.Hint = "use the https:// URL (host: tailnet.https or tailnet.tlsCertFile); the SDKs refuse bearer tokens over plain http"
		if token != "" {
			c.Status = preflight.Fail
			c.Detail += ", with a service token"
		}
	default:
		c.Status, c.Detail = preflight.Fail, "unsupported scheme "+u.Scheme
		c.Hint = "use unix:///path.sock or https://host"
	}
	return c
}

func loopbackHost(h string) bool {
	return h == "localhost" || strings.HasPrefix(h, "127.") || h == "::1"
}

func credentialCheck(token string) preflight.Check {
	c := preflight.Check{ID: "credential", Title: "Credential", Status: preflight.Pass}
	switch {
	case token == "":
		c.Detail = "this machine's tailnet identity (no " + buildinfo.Env("TOKEN") + " set)"
	case !strings.HasPrefix(token, "slw_"):
		c.Status, c.Detail = preflight.Fail, buildinfo.Env("TOKEN")+" is not a "+buildinfo.Name+" token (want slw_…)"
		c.Hint = "copy the secret printed by `" + buildinfo.Name + " token create` on the host"
	default:
		c.Detail = "service token from " + buildinfo.Env("TOKEN") + " (secret not shown)"
	}
	return c
}

func caFileCheck() (preflight.Check, bool) {
	file := os.Getenv(CAFileEnv)
	if file == "" {
		return preflight.Check{}, false
	}
	c := preflight.Check{ID: "ca_file", Title: "Trusted CA", Status: preflight.Pass}
	b, err := os.ReadFile(file)
	if err != nil {
		c.Status, c.Detail, c.Hint = preflight.Fail, err.Error(), "point "+CAFileEnv+" at the operator CA's PEM file"
		return c, true
	}
	n := 0
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if _, err := x509.ParseCertificate(blk.Bytes); err == nil && blk.Type == "CERTIFICATE" {
			n++
		}
	}
	if n == 0 {
		c.Status, c.Detail, c.Hint = preflight.Fail, file+" holds no PEM certificates", "point "+CAFileEnv+" at the operator CA's PEM file"
		return c, true
	}
	c.Detail = fmt.Sprintf("%s: %d certificate(s), trusted in addition to the system roots", file, n)
	return c, true
}

// apiCheck makes one authenticated request and turns its failure into the
// fix that applies.
func (s *session) apiCheck(ctx context.Context, remote bool) preflight.Check {
	c := preflight.Check{ID: "api", Title: "Authenticated API request"}
	t0 := time.Now()
	var caps api.HostCapabilities
	err := s.cl.get(ctx, "/capabilities", nil, &caps)
	if err == nil {
		c.Status = preflight.Pass
		c.Detail = fmt.Sprintf("%s answered in %s", strings.TrimSpace(caps.Runtime.Engine+" "+caps.Runtime.EngineVersion),
			time.Since(t0).Round(time.Millisecond))
		return c
	}
	c.Status = preflight.Fail
	c.Detail, c.Hint = remoteFailure(err, s.cl.token != "", remote)
	return c
}

// remoteFailure explains a failed request and names its fix.
func remoteFailure(err error, token, remote bool) (detail, hint string) {
	var ce *cliError
	code, msg := domain.CodeInternal, err.Error()
	if errors.As(err, &ce) {
		code, msg = ce.Code, ce.Message
	}
	m := strings.ToLower(msg)
	switch {
	case code == domain.CodeUnauthenticated && token:
		return "the service token was rejected: " + msg,
			"it is invalid, expired or revoked: on the host run `" + buildinfo.Name + " token create NAME --action view` and update " + buildinfo.Env("TOKEN")
	case code == domain.CodeUnauthenticated:
		return "the gateway saw no tailnet identity: " + msg,
			"connect over the tailnet (MagicDNS name or 100.x address from `tailscale status`), not through a proxy"
	case code == domain.CodePermissionDenied && token:
		return "the token does not grant this: " + msg,
			"create a token whose --action covers it (view < exec < admin), without --vm for host-wide reads"
	case code == domain.CodePermissionDenied:
		return "this tailnet identity is not the host's owner: " + msg,
			"on the host add your login to tailnet.ownerLogins (or your node's tag to tailnet.ownerTags), or use a service token (" + buildinfo.Env("TOKEN") + ")"
	case !remote:
		return msg, "start the daemon (`" + buildinfo.Name + " daemon` or the systemd unit); `" + buildinfo.Name + " doctor` checks the host"
	case strings.Contains(m, "unknown authority") || strings.Contains(m, "certificate signed by unknown"):
		return "the gateway's certificate is not trusted: " + msg,
			"set " + CAFileEnv + " to the operator CA (tailnet.tlsCertFile), or use the tailnet's own certificate (tailnet.https)"
	case strings.Contains(m, "certificate is valid for") || strings.Contains(m, "doesn't contain any ip sans") || strings.Contains(m, "not valid for"):
		return "the certificate does not match this name: " + msg,
			"use the name the certificate covers: `" + buildinfo.Name + " doctor` on the host prints the URL (tailnet_gateway)"
	case strings.Contains(m, "no such host") || strings.Contains(m, "server misbehaving"):
		return "the host name does not resolve: " + msg,
			"enable MagicDNS on this machine, or use the host's 100.x address"
	case strings.Contains(m, "connection refused"):
		return "nothing answers on that port: " + msg,
			"on the host run `" + buildinfo.Name + " doctor` (tailnet_gateway, tailnet_tls): the gateway may be down, or serve https on :443 while you used http (or the reverse)"
	case strings.Contains(m, "timeout") || strings.Contains(m, "no route") || strings.Contains(m, "unreachable"):
		return "the host is unreachable: " + msg,
			"check that this machine is on the same tailnet (`tailscale status`) and that the tailnet ACL lets it reach the host"
	}
	return msg, "run `" + buildinfo.Name + " doctor` on the host"
}
