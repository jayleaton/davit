package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"davit/internal/buildinfo"
)

// The client config ($XDG_CONFIG_HOME/davit/client.json) holds this
// machine's defaults for the CLI as a client, so `davit box ...` works
// from any directory with no flags or environment: today the default host
// (a tailnet name, an http(s):// URL or ssh://USER@HOST; `davit connect`
// sets it up and checks it). --host,
// --api-url, $DAVIT_HOST and $DAVIT_API_URL all take precedence.
// It is separate from the daemon's config.json on the same machine.

// clientConfig is client.json.
type clientConfig struct {
	Host string `json:"host,omitempty"`
}

func clientConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, buildinfo.Name, "client.json"), nil
}

// loadClientConfig reads client.json; a missing file is the zero config.
func loadClientConfig() (clientConfig, string, error) {
	var c clientConfig
	p, err := clientConfigPath()
	if err != nil {
		return c, "", err
	}
	migrateClientConfig(p)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, p, nil
	}
	if err != nil {
		return c, p, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, p, usageErr("%s: %v", p, err)
	}
	return c, p, nil
}

func clientConfigFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.clientHost, "set-host", "", "set the default host (USER@HOST or ssh://USER@HOST, a tailnet NAME, or an http(s):// URL); `davit connect` also checks it")
	fs.BoolVar(&o.clientClear, "clear", false, "remove the default host")
}

// runClientConfig shows or changes client.json.
func runClientConfig(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) > 0 {
		return usageErr("client-config takes no arguments")
	}
	c, p, err := loadClientConfig()
	if err != nil {
		return err
	}
	if o.clientHost != "" || o.clientClear {
		if o.clientHost != "" {
			u, err := hostURL(o.clientHost)
			if err != nil {
				return err
			}
			if strings.HasPrefix(u, "ssh://") {
				o.clientHost = u // USER@HOST is saved as the ssh:// URL it means
			}
		}
		c.Host = o.clientHost
		if err := saveClientConfig(c, p); err != nil {
			return err
		}
	}
	s.out.result("", map[string]any{"path": p, "host": c.Host})
	if c.Host == "" {
		s.out.human("%s: no default host (the local daemon socket is used)", p)
	} else {
		s.out.human("%s: default host %s", p, c.Host)
	}
	return nil
}

// saveClientConfig writes c to p (0600).
func saveClientConfig(c clientConfig, p string) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(p, append(b, '\n'), 0o600)
}

// migrateClientConfig moves the config dir of a CLI from before the
// slipway -> davit rename (a "slipway" dir beside p's, on every OS: the
// client config, the SSH key and certificates) into p's dir, entry by entry
// so nothing davit already wrote is replaced.
func migrateClientConfig(p string) {
	dir := filepath.Dir(p)
	old := filepath.Join(filepath.Dir(dir), "slipway")
	ents, err := os.ReadDir(old)
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	for _, e := range ents {
		if _, err := os.Lstat(filepath.Join(dir, e.Name())); errors.Is(err, fs.ErrNotExist) {
			_ = os.Rename(filepath.Join(old, e.Name()), filepath.Join(dir, e.Name()))
		}
	}
	os.Remove(old) // only when nothing is left there
}
