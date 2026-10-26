package daemon

import (
	"log/slog"
	"path/filepath"

	"davit/internal/appendpoint"
	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/lifecycle"
)

// appEndpointsFile is the app endpoint store in the tailnet state dir: the
// URLs are gateway state and survive daemon restarts.
const appEndpointsFile = "app-endpoints.json"

// appEndpointStatus is feature appEndpoints' status: available with the
// tailnet gateway.
func appEndpointStatus(cfg config.Daemon) domain.FeatureStatus {
	if cfg.Tailnet.Enabled {
		return domain.FeatureAvailable
	}
	return domain.FeatureUnsupported
}

// newAppEndpoints is the app endpoint service; startTailnet attaches the
// gateway that serves it.
func newAppEndpoints(cfg config.Daemon, svc *lifecycle.Service, log *slog.Logger) (*appendpoint.Service, error) {
	ports, err := cfg.Tailnet.AppPortRange()
	if err != nil {
		return nil, err
	}
	return appendpoint.New(appendpoint.Config{
		Path: filepath.Join(cfg.TailnetStateDir(), appEndpointsFile), VMs: svc, Ports: ports,
		Log: log.With("component", "app-endpoints"),
	})
}
