package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EnrollState is where the gateway's tailnet enrollment stands.
type EnrollState string

// Enrollment states, in the order a healthy start passes through them.
const (
	EnrollStarting EnrollState = "starting"
	// EnrollNeedsLogin: no usable auth key and no saved login; LoginURL
	// is the interactive sign-in link.
	EnrollNeedsLogin EnrollState = "needs-login"
	// EnrollNeedsApproval: logged in, waiting for an admin to approve the
	// device (tailnet device approval).
	EnrollNeedsApproval EnrollState = "needs-approval"
	EnrollUp            EnrollState = "up"
	EnrollError         EnrollState = "error"
)

// TLS modes of the API listener.
const (
	TLSOff       = "off"       // plain HTTP on :80 (WireGuard-encrypted on the tailnet)
	TLSTailscale = "tailscale" // the tailnet's own certificate for the MagicDNS name
	TLSFiles     = "files"     // an operator-provided certificate and key
)

// Status is the gateway's enrollment and endpoint report, written by the
// daemon for `doctor` and `status` (StatusFile in the tailnet state dir).
type Status struct {
	State    EnrollState `json:"state"`
	Hostname string      `json:"hostname"`
	// FQDN and Addrs are what the control server registered (state up).
	FQDN  string   `json:"fqdn,omitempty"`
	Addrs []string `json:"addrs,omitempty"`
	// APIURL is the URL remote clients use (DAVIT_API_URL / --host).
	APIURL   string `json:"apiUrl,omitempty"`
	TLS      string `json:"tls"`
	LoginURL string `json:"loginUrl,omitempty"`
	Error    string `json:"error,omitempty"`
	// PID is the daemon that wrote this report; a report from a process that
	// is gone is stale.
	PID       int       `json:"pid"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// StatusFile is the status report's name in the tailnet state dir.
const StatusFile = "gateway-status.json"

// WriteStatus atomically replaces the report at path (0600).
func WriteStatus(path string, s Status) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gateway-status-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = tmp.Chmod(0o600)
	if err == nil {
		_, err = tmp.Write(append(b, '\n'))
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadStatus reads a report written by WriteStatus.
func ReadStatus(path string) (Status, error) {
	var s Status
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("gateway status %s: %w", path, err)
	}
	if s.State == "" {
		return s, errors.New("gateway status: no state")
	}
	return s, nil
}

// apiURL is the remote endpoint for fqdn under a TLS mode.
func apiURL(fqdn, tlsMode string) string {
	if fqdn == "" {
		return ""
	}
	if tlsMode == TLSOff {
		return "http://" + fqdn
	}
	return "https://" + fqdn
}
