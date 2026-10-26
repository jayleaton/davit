package lifecycle

import (
	"context"
	"encoding/json"
	"time"

	"davit/internal/domain"
	"davit/internal/network"
	"davit/internal/runtime"
	"davit/internal/state"
)

// provisionNetwork returns the network for vm's instance generation gen,
// reusing the recorded one when it belongs to gen and still exists (a
// replayed boot that adopts a live VMM must keep its namespace), else
// releasing any previous one and creating a new one. The handle is
// persisted before it is returned, so a crash never leaks it unrecorded
// past this point.
func (s *Service) provisionNetwork(ctx context.Context, vm domain.VMID, gen int64) (runtime.NetworkHandle, error) {
	rec, err := s.loadVM(ctx, vm)
	if err != nil {
		return runtime.NetworkHandle{}, err
	}
	if h := decodeNetwork(rec.Network); h != nil {
		if h.Generation == gen {
			// A replay: the VMM may already run in it, so never swap it.
			enter, err := s.cfg.Network.Enter(*h)
			if err != nil {
				e := domain.Errorf(domain.CodeUnavailable, "network of generation %d is gone: %v", gen, err)
				e.Retryable = true
				return runtime.NetworkHandle{}, e
			}
			s.attachCredentials(ctx, vm, *h)
			return RuntimeNetwork(*h, enter), nil
		}
		// Never overwrite a record whose namespace could not be released.
		if err := s.releaseNetwork(ctx, vm); err != nil {
			return runtime.NetworkHandle{}, err
		}
	}
	h, err := s.cfg.Network.Create(ctx, vm, gen, rec.VM.Egress)
	if err != nil {
		return runtime.NetworkHandle{}, err
	}
	raw, _ := json.Marshal(h)
	if err := s.updateVM(ctx, vm, func(r *state.VMRecord, _ time.Time) error { r.Network = raw; return nil }); err != nil {
		s.cfg.Network.Destroy(context.WithoutCancel(ctx), h)
		return runtime.NetworkHandle{}, err
	}
	if !h.Egress.Enabled && h.Mode != network.ModeNone {
		s.cfg.Log.Info("guest egress disabled", "vm", vm, "generation", gen, "reason", h.Egress.Reason)
	}
	enter, err := s.cfg.Network.Enter(h)
	if err != nil {
		return runtime.NetworkHandle{}, err
	}
	s.attachCredentials(ctx, vm, h)
	return RuntimeNetwork(h, enter), nil
}

// CredentialHooks is the credential proxy's seam into the lifecycle (G4).
// AttachNetwork serves a VM's guest on its network once it is provisioned
// (and for running VMs after a daemon restart); DetachNetwork stops that
// before the network is released; Lifecycle applies domain.GrantAfter.
type CredentialHooks interface {
	AttachNetwork(ctx context.Context, vm domain.VMID, h network.Handle)
	DetachNetwork(vm domain.VMID)
	Lifecycle(ctx context.Context, vm domain.VMID, e domain.GrantEvent) error
}

func (s *Service) attachCredentials(ctx context.Context, vm domain.VMID, h network.Handle) {
	if s.cfg.Credentials != nil {
		s.cfg.Credentials.AttachNetwork(ctx, vm, h)
	}
}

// grantEvent applies a succeeded operation's domain.GrantAfter event: stop,
// resume and delete to the VM itself, fork to its source (a fork child
// inherits no grants).
func (s *Service) grantEvent(ctx context.Context, op *domain.Operation) {
	if s.cfg.Credentials == nil {
		return
	}
	vm, e := op.VMID, domain.GrantEvent("")
	switch op.Kind {
	case domain.OpStop:
		e = domain.GrantOnStop
		if op.Params.Delete { // an ephemeral VM's stop deletes it
			e = domain.GrantOnDelete
		}
	case domain.OpResume, domain.OpRecover: // a power cycle keeps grants, as resume does
		e = domain.GrantOnResume
	case domain.OpDelete:
		e = domain.GrantOnDelete
	case domain.OpFork:
		vm, e = op.Params.SourceVMID, domain.GrantOnFork
	default:
		return
	}
	if err := s.cfg.Credentials.Lifecycle(ctx, vm, e); err != nil {
		s.cfg.Log.Warn("credential grants after "+string(e), "vm", vm, "err", err)
	}
}

// releaseNetwork destroys vm's recorded network, if any, and forgets it.
// Call it only once the VMM using it is gone. On failure the record is kept
// (a later reconcile or retry releases it) and a retryable error returned.
func (s *Service) releaseNetwork(ctx context.Context, vm domain.VMID) error {
	rec, err := s.loadVM(ctx, vm)
	if err != nil {
		return err
	}
	h := decodeNetwork(rec.Network)
	if h == nil {
		return nil
	}
	if s.cfg.Credentials != nil {
		// Its listener lives in the namespace and would keep it alive.
		s.cfg.Credentials.DetachNetwork(vm)
	}
	if err := s.cfg.Network.Destroy(ctx, *h); err != nil {
		s.cfg.Log.Warn("release network", "vm", vm, "generation", h.Generation, "err", err)
		e := domain.Errorf(domain.CodeUnavailable, "release network of %s: %v", vm, err)
		e.Retryable = true
		return e
	}
	return s.updateVM(ctx, vm, func(r *state.VMRecord, _ time.Time) error { r.Network = nil; return nil })
}

func decodeNetwork(raw json.RawMessage) *network.Handle {
	if len(raw) == 0 {
		return nil
	}
	var h network.Handle
	if json.Unmarshal(raw, &h) != nil {
		return nil
	}
	return &h
}

// RuntimeNetwork converts a provider handle and its Enter argv to the
// runtime's view.
func RuntimeNetwork(h network.Handle, enter []string) runtime.NetworkHandle {
	return runtime.NetworkHandle{Namespace: h.NetNSPath, TapName: h.TapName, UserNamespace: h.UserNSPath,
		GuestMAC: h.GuestMAC, EnterArgv: enter}
}
