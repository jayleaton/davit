package daemon

import (
	"context"
	"log/slog"

	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/guestexec"
	"davit/internal/lifecycle"
	"davit/internal/network/reverse"
	"davit/internal/reversefwd"
)

// agentListen opens reverse-forward listeners through the guest agent
// (guest protocol feature "listen", guestexec ListenGuest) on the VM's
// current instance; a stale generation is refused and the listener ends with
// that instance's agent session. The fake runtime's in-process agents listen
// on the host's loopback, which is their guest loopback. A guest agent
// without the feature answers unsupported_capability for its VM.
type agentListen struct {
	svc       *lifecycle.Service
	procs     *guestexec.Manager
	principal domain.PrincipalID
}

func (a agentListen) ListenGuest(ctx context.Context, vm domain.VMID, generation int64, port uint16) (reverse.GuestListener, error) {
	t, err := a.svc.GuestTarget(ctx, a.principal, string(vm))
	if err != nil {
		return nil, err
	}
	return a.procs.ListenGuest(ctx, t, generation, port)
}

// reverseStatus is feature reverseForward's status: available whenever the
// runtime has a guest transport (guest agents listen).
func reverseStatus(guest bool) domain.FeatureStatus {
	if !guest {
		return domain.FeatureUnsupported
	}
	return domain.FeatureAvailable
}

// newReverse is the reverse forward service; procs nil (no guest transport)
// leaves every operation unsupported_capability.
func newReverse(cfg config.Daemon, svc *lifecycle.Service, procs *guestexec.Manager, principal domain.PrincipalID, log *slog.Logger) (*reversefwd.Service, error) {
	policy, err := cfg.Network.ReversePolicy()
	if err != nil {
		return nil, err
	}
	var listen reverse.GuestListen
	if procs != nil {
		listen = agentListen{svc: svc, procs: procs, principal: principal}
	}
	return reversefwd.New(reversefwd.Config{Listen: listen, Policy: policy, VMs: svc, Log: log.With("component", "reverse-forward")}), nil
}
