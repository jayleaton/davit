package guestexec

import (
	"context"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
	"davit/internal/lifecycle"
	"davit/internal/state"
)

// SampleStore keeps a VM's latest whole-guest process sample, so a stopped
// VM can still answer with it (stale). A Store that implements it gets every
// GuestProcesses sample recorded.
type SampleStore interface {
	PutGuestSample(ctx context.Context, vm domain.VMID, s *api.GuestProcessList) error
}

// maxHandleLookups bounds the ProcessLookups one sample spends mapping
// managed-job handles to process IDs.
const maxHandleLookups = 256

// GuestProcesses samples every process in ref's guest (feature ps). Managed
// jobs carry their process ID (their handle is looked up). The sample is
// also recorded when the Store implements SampleStore and the VM's privacy
// policy keeps samples.
func (m *Manager) GuestProcesses(ctx context.Context, principal domain.PrincipalID, ref string) (api.GuestProcessList, error) {
	t, err := m.cfg.Targets.GuestTarget(ctx, principal, ref)
	if err != nil {
		return api.GuestProcessList{}, err
	}
	c, err := m.session(ctx, t)
	if err != nil {
		return api.GuestProcessList{}, err
	}
	l, err := c.ProcessList(ctx)
	if err != nil {
		return api.GuestProcessList{}, protoErr(err)
	}
	out := api.GuestProcessList{SampledAt: time.Unix(0, l.SampledAtUnixNano).UTC(), Truncated: l.Truncated,
		Items: make([]api.GuestProcess, 0, len(l.Processes))}
	ids := map[string]string{} // handle -> process ID
	lookups := 0
	for _, g := range l.Processes {
		item := api.GuestProcess{PID: g.PID, PPID: g.PPID, User: g.User, Args: g.Args, State: g.State,
			CPUTimeMillis: g.CPUTimeMillis, RSSBytes: g.RSSBytes}
		if item.Args == nil {
			item.Args = []string{}
		}
		if g.StartedAtUnixNano > 0 {
			item.StartedAt = time.Unix(0, g.StartedAtUnixNano).UTC()
		}
		if g.Handle != "" {
			id, ok := ids[g.Handle]
			if !ok && lookups < maxHandleLookups && c.Has(guestproto.FeatureJobs) {
				lookups++
				if st, err := c.Lookup(ctx, g.Handle); err == nil {
					id = st.ProcessID
				}
				ids[g.Handle] = id
			}
			item.ProcessID = id
		}
		out.Items = append(out.Items, item)
	}
	// A noProcessSamples VM still answers live; the store would drop the
	// sample anyway (it enforces the policy), so don't offer it.
	if ss, ok := m.cfg.Store.(SampleStore); ok && t.Privacy.RetainsSamples() {
		if err := ss.PutGuestSample(context.WithoutCancel(ctx), t.Key.VMID, &out); err != nil {
			m.cfg.Log.Warn("record guest process sample", "vm", t.Key.VMID, "err", err)
		}
	}
	return out, nil
}

// PutGuestSample implements SampleStore.
func (s DBStore) PutGuestSample(ctx context.Context, vm domain.VMID, l *api.GuestProcessList) error {
	return s.DB.Tx(ctx, func(tx *state.Tx) error { return tx.PutGuestSample(vm, l) })
}

// ListenGuest opens a listener on guest 127.0.0.1:port of t's instance
// (feature listen) for reverse connections; the result implements
// net.Listener (and the reverse-connection GuestListener: Accept, Close).
// It refuses a target that is no longer generation, and the listener ends
// when that instance's agent session does (stop, delete, new generation).
func (m *Manager) ListenGuest(ctx context.Context, t lifecycle.GuestTarget, generation int64, port uint16) (*guestclient.Listener, error) {
	if int64(t.Key.Generation) != generation {
		return nil, domain.Errorf(domain.CodeInvalidState, "vm %s is at generation %d, not %d", t.Key.VMID, t.Key.Generation, generation)
	}
	dial, sess, err := m.route(ctx, t)
	if err != nil {
		return nil, err
	}
	l, err := guestclient.Listen(ctx, dial, sess, int(port))
	if err != nil {
		return nil, protoErr(err)
	}
	return l, nil
}

// PrepareWorkspace runs req in ref's guest (feature workspace), reporting
// each step to progress (if non-nil). A failed step is a result with State
// failed; an error means the outcome is unknown. It runs on a session of its
// own, closed when it returns, so cancelling ctx also kills a running clone
// or setup step (and frees the agent for the next preparation).
func (m *Manager) PrepareWorkspace(ctx context.Context, principal domain.PrincipalID, ref string, req guestproto.WorkspacePrepare,
	progress func(guestproto.WorkspaceProgress)) (guestproto.WorkspaceResult, error) {
	t, err := m.cfg.Targets.GuestTarget(ctx, principal, ref)
	if err != nil {
		return guestproto.WorkspaceResult{}, err
	}
	c, err := m.connect(ctx, t)
	if err != nil {
		return guestproto.WorkspaceResult{}, err
	}
	defer c.Close()
	res, err := c.PrepareWorkspace(ctx, req, progress)
	if err != nil {
		return res, protoErr(err)
	}
	return res, nil
}
