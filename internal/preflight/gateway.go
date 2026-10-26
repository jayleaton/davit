package preflight

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"davit/internal/buildinfo"
	"davit/internal/gateway"
)

// stuckAfter is how long enrollment may sit in "starting" before doctor
// calls it stuck.
const stuckAfter = 2 * time.Minute

// GatewayChecks report the tailnet gateway's enrollment (from the daemon's
// status report) and its TLS setup, each failure with the fix that applies.
// `doctor` runs them on the host; `davit status` shows them for the local
// daemon.
func GatewayChecks(o Options, now time.Time) []Check {
	return []Check{gatewayEnrollCheck(o, now), gatewayTLSCheck(o, now)}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// enrollHint names the fix for an enrollment error message.
func enrollHint(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "auth key") || strings.Contains(m, "authkey") || strings.Contains(m, "invalid key") || strings.Contains(m, "expired"):
		return fmt.Sprintf("the auth key was rejected (expired, already used, or for another tailnet): create a new one and run `%s init --auth-key-stdin`, then restart the daemon", buildinfo.Name)
	case strings.Contains(m, "listen"):
		return "another process on this tailnet node uses the API or SSH port; check tailnet.hostname is not shared with another davit daemon"
	case strings.Contains(m, "certificate") || strings.Contains(m, "tls") || strings.Contains(m, "https"):
		return "enable HTTPS certificates in the tailnet admin console (DNS → HTTPS), or set tailnet.tlsCertFile/tlsKeyFile"
	case strings.Contains(m, "control") || strings.Contains(m, "dial") || strings.Contains(m, "connection"):
		return "the coordination server is unreachable from this host: check network access and tailnet.controlURL"
	}
	return "see the daemon log (component=tailnet) for the cause"
}

func gatewayEnrollCheck(o Options, now time.Time) Check {
	c := Check{ID: "tailnet_gateway", Title: "Tailnet gateway"}
	if !o.TailnetEnabled {
		c.Status, c.Detail = Skip, tailnetOff
		return c
	}
	s, err := gateway.ReadStatus(filepath.Join(o.TailnetStateDir, gateway.StatusFile))
	if errors.Is(err, os.ErrNotExist) {
		c.Status, c.Detail = Warn, "no enrollment report yet: the daemon has not started the gateway"
		c.Hint = fmt.Sprintf("start the daemon (`%s daemon` or the systemd unit) and run doctor again", buildinfo.Name)
		return c
	}
	if err != nil {
		c.Status, c.Detail, c.Hint = Warn, err.Error(), "restart the daemon to rewrite the report"
		return c
	}
	stale := ""
	if !pidAlive(s.PID) {
		stale = fmt.Sprintf(" (reported by daemon pid %d, which is not running; start the daemon to refresh)", s.PID)
	}
	switch s.State {
	case gateway.EnrollUp:
		c.Status = Pass
		c.Detail = fmt.Sprintf("up as %s (%s), TLS %s; remote clients use %s=%s", s.FQDN, strings.Join(s.Addrs, ", "),
			s.TLS, buildinfo.Env("API_URL"), s.APIURL)
		if stale != "" {
			c.Status, c.Detail = Warn, "was up as "+s.FQDN+stale
		}
	case gateway.EnrollNeedsLogin:
		c.Status, c.Detail = Fail, "this host is not enrolled in the tailnet yet"+stale
		c.Hint = fmt.Sprintf("open %s to sign it in, or store an auth key (`%s init --auth-key-stdin`) and restart the daemon", s.LoginURL, buildinfo.Name)
		if s.LoginURL == "" {
			c.Hint = fmt.Sprintf("store an auth key (`%s init --auth-key-stdin`) and restart the daemon", buildinfo.Name)
		}
	case gateway.EnrollNeedsApproval:
		c.Status, c.Detail = Fail, "signed in, waiting for a tailnet admin to approve this device"+stale
		c.Hint = "approve " + s.Hostname + " in the tailnet admin console (Machines), or use a pre-approved auth key"
	case gateway.EnrollStarting:
		c.Status, c.Detail = Warn, "enrolling since "+s.UpdatedAt.Format(time.RFC3339)+stale
		c.Hint = "wait a few seconds and run doctor again"
		if now.Sub(s.UpdatedAt) > stuckAfter && stale == "" {
			c.Status, c.Detail = Fail, "enrollment has not finished after "+now.Sub(s.UpdatedAt).Round(time.Second).String()
			c.Hint = enrollHint("control")
		}
	default:
		c.Status, c.Detail = Fail, "enrollment failed: "+s.Error+stale
		c.Hint = enrollHint(s.Error)
	}
	return c
}

func gatewayTLSCheck(o Options, now time.Time) Check {
	c := Check{ID: "tailnet_tls", Title: "Tailnet HTTPS"}
	if !o.TailnetEnabled {
		c.Status, c.Detail = Skip, tailnetOff
		return c
	}
	sdkHint := "set tailnet.https (the tailnet's own certificate; enable HTTPS in the tailnet admin console) or tailnet.tlsCertFile/tlsKeyFile"
	switch {
	case o.TLSCertFile != "":
		cert, err := gateway.LoadCertificate(o.TLSCertFile, o.TLSKeyFile)
		if err != nil {
			c.Status, c.Detail, c.Hint = Fail, err.Error(), "install a current certificate and key for the gateway's name"
			return c
		}
		leaf := cert.Leaf
		c.Status = Pass
		c.Detail = fmt.Sprintf("operator certificate %q valid until %s; clients trust its CA with %s", leaf.Subject.CommonName,
			leaf.NotAfter.Format("2006-01-02"), buildinfo.Env("CA_FILE"))
		if s, err := gateway.ReadStatus(filepath.Join(o.TailnetStateDir, gateway.StatusFile)); err == nil && s.FQDN != "" {
			if err := leaf.VerifyHostname(s.FQDN); err != nil {
				c.Status = Fail
				c.Detail = fmt.Sprintf("the certificate does not cover the registered name %s: %v", s.FQDN, err)
				c.Hint = "reissue it with " + s.FQDN + " as a DNS name (SAN)"
				return c
			}
		}
		if left := leaf.NotAfter.Sub(now); left < 14*24*time.Hour {
			c.Status, c.Hint = Warn, "renew it soon; the gateway reloads the files without a restart"
			c.Detail += fmt.Sprintf(" (%d days left)", int(left.Hours()/24))
		}
	case o.TailnetHTTPS:
		c.Status, c.Detail = Pass, "the tailnet's own certificate for the MagicDNS name (needs HTTPS certificates enabled in the tailnet)"
	default:
		c.Status = Warn
		c.Detail = "plain HTTP on :80 (WireGuard-encrypted): the CLI works, the SDKs refuse bearer tokens over plain http"
		c.Hint = sdkHint
	}
	return c
}
