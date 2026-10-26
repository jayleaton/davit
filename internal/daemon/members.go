package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"davit/internal/config"
	"davit/internal/credproxy"
	"davit/internal/domain"
	"davit/internal/gateway"
	"davit/internal/httpapi"
	"davit/internal/lifecycle"
	"davit/internal/members"
)

// membersFile is the members store in the tailnet state dir, next to the
// owner pins: membership is gateway state (see package members).
const membersFile = "members.json"

// newMembers is the members service (G13), nil without the tailnet
// gateway. Pausing a VM's credential grants cuts its proxied requests.
func newMembers(cfg config.Daemon, svc *lifecycle.Service, sessions *gateway.Sessions, creds *credproxy.Control,
	host *credproxy.Host, log *slog.Logger) (*members.Service, error) {
	if !cfg.Tailnet.Enabled {
		return nil, nil
	}
	return members.New(members.Config{
		Path: filepath.Join(cfg.TailnetStateDir(), membersFile), Owner: httpapi.LocalPrincipal(os.Getuid()),
		Resources: svc, DefaultVisibility: cfg.Tailnet.DefaultVisibility, Sessions: sessions,
		OnCredentialsPaused: func(vm domain.VMID) {
			l, err := creds.ListGrants(context.Background(), vm)
			if err != nil {
				return
			}
			ids := make([]string, 0, len(l.Items))
			for _, g := range l.Items {
				ids = append(ids, g.ID)
			}
			host.Cut(ids...)
		},
		Log: log.With("component", "members"),
	})
}
