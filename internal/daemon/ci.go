package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"davit/internal/ci"
	"davit/internal/ci/github"
	"davit/internal/config"
	"davit/internal/guestexec"
	"davit/internal/httpapi"
	"davit/internal/lifecycle"
	"davit/internal/state"
)

// startCI starts the GitHub Actions runner controller when ci.github is
// enabled (nil otherwise). Runner boxes are the host owner's; the runner
// runs through the guest process API, so it needs a guest transport.
func startCI(ctx context.Context, cfg config.Daemon, db *state.DB, svc *lifecycle.Service, procs *guestexec.Manager, log *slog.Logger) (*ci.Service, error) {
	g := cfg.CI.GitHub
	if !g.Enabled {
		return nil, nil
	}
	if procs == nil {
		return nil, fmt.Errorf("ci.github needs a runtime with a guest transport (the runner runs through the guest agent)")
	}
	key, err := github.LoadPrivateKey(g.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("ci.github.privateKeyFile: %w", err)
	}
	host, _ := os.Hostname()
	c, err := ci.New(ci.Config{GitHub: g, App: &github.App{BaseURL: g.APIURL, AppID: g.AppID, Key: key},
		DB: db, VMs: svc, Procs: procs, Owner: httpapi.LocalPrincipal(os.Getuid()), Hostname: host,
		Log: log.With("component", "ci")})
	if err != nil {
		return nil, err
	}
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	return c, nil
}
