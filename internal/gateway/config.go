package gateway

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"tailscale.com/ipn"
)

// DefaultAuthKeyEnv is read when Config.AuthKeyEnv is empty.
const DefaultAuthKeyEnv = "DAVIT_TS_AUTHKEY"

// Config configures the host gateway node.
type Config struct {
	// StateDir holds the node's persistent tailnet identity and the SSH
	// front door host key. Created 0700; it must not be group/world
	// accessible.
	StateDir string
	// Hostname is the requested tailnet hostname. The registered name may
	// differ on conflict; use Node.FQDN for the actual one.
	Hostname string
	// ControlURL selects the coordination server; empty means Tailscale's.
	// Set it for Headscale (compatibility is tested separately, not assumed).
	ControlURL string
	// AuthKeyFile, then the AuthKeyEnv variable (default DefaultAuthKeyEnv),
	// supply a one-time enrollment key. Neither is needed once StateDir holds
	// a logged-in node. Keys are never logged.
	AuthKeyFile string
	AuthKeyEnv  string
	// Ephemeral registers a node that the control server deletes when it goes
	// offline. Only tests and throwaway endpoints should set it.
	Ephemeral bool
	// AdvertiseTags requests ACL tags for the node (must be permitted by the
	// tailnet policy and the auth key).
	AdvertiseTags []string

	// Store overrides the on-disk tailnet state store (tests use mem.Store).
	Store ipn.StateStore
	// Logf receives tsnet backend logs; nil discards them.
	Logf func(format string, args ...any)
	// UserLogf receives the few user-facing messages, notably the login URL
	// when no auth key is configured; nil uses Logf.
	UserLogf func(format string, args ...any)
}

var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidHostname reports whether s is a usable tailnet hostname label.
func ValidHostname(s string) bool { return hostnameRE.MatchString(s) }

func (c Config) validate() error {
	if c.StateDir == "" {
		return errors.New("gateway: StateDir is required")
	}
	if !ValidHostname(c.Hostname) {
		return fmt.Errorf("gateway: invalid hostname %q (want a lowercase DNS label)", c.Hostname)
	}
	return nil
}

// authKey resolves the enrollment key. An empty result is fine for a node
// whose state is already logged in.
func (c Config) authKey() (string, error) {
	if c.AuthKeyFile != "" {
		fi, err := os.Stat(c.AuthKeyFile)
		if err != nil {
			return "", fmt.Errorf("gateway: auth key file: %w", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("gateway: auth key file %s must not be group/world accessible (mode %v)", c.AuthKeyFile, fi.Mode().Perm())
		}
		b, err := os.ReadFile(c.AuthKeyFile)
		if err != nil {
			return "", fmt.Errorf("gateway: auth key file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	env := c.AuthKeyEnv
	if env == "" {
		env = DefaultAuthKeyEnv
	}
	return strings.TrimSpace(os.Getenv(env)), nil
}

// ensurePrivateDir creates dir 0700, or verifies an existing one is not
// group/world accessible.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("gateway: %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("gateway: state dir %s must be 0700 (mode %v)", dir, fi.Mode().Perm())
	}
	return nil
}
