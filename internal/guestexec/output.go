package guestexec

import (
	"context"
	"errors"

	"davit/api"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
	"davit/internal/lifecycle"
	"davit/internal/state"
)

// OutputStore retains managed processes' output (the process_output table)
// so it stays readable while the VM is stopped and after a daemon restart.
// When Config.Store also implements it, the manager follows every running
// process it knows (a collector per process, on the VM's agent session)
// and appends each chunk with its guest seq. A collector resumes after the
// last stored seq, so a daemon restart neither duplicates nor skips output
// the agent still retains. At most Config.RetainOutput bytes are kept per
// process: the newest (agents care about the tail); the oldest chunks are
// dropped and the drop is recorded.
//
// Privacy (G17): the store guarantees that a VM whose policy keeps no
// output never gets any stored (state.Tx.AppendOutput drops the chunk,
// whichever writer offers it, including a collector a previous daemon run
// or a policy change left behind). Here it is only an optimization: no
// collector is started while the VM's session target (refreshed on every
// guest access) says the policy keeps no output.
type OutputStore interface {
	AppendOutput(ctx context.Context, process string, c state.OutputChunk) error
	// OutputExtent returns the last stored seq and the bytes stored.
	OutputExtent(ctx context.Context, process string) (lastSeq, size int64, err error)
	// TrimOutput drops the oldest output beyond keep bytes, recording the
	// drop, and returns the bytes left.
	TrimOutput(ctx context.Context, process string, keep int64) (int64, error)
}

// collect starts p's output collector unless one runs, recording ended, or
// p has exited. c is the VM session p was seen on; the collector runs on a
// session of its own (closed when it stops, which also ends the agent's
// follower, as the protocol has no detach), so recording output never
// queues behind, or stalls, the VM's other requests.
func (m *Manager) collect(p *proc, c *guestclient.Client) {
	if m.out == nil || c == nil || p.exited() {
		return
	}
	p.mu.Lock()
	id, vm, done := p.p.ID, p.p.VMID, p.collected
	p.mu.Unlock()
	m.mu.Lock()
	vs := m.sessions[vm]
	if done || m.collecting[id] || m.ctx.Err() != nil || vs == nil || vs.c != c || !vs.t.Privacy.RetainsOutput() {
		m.mu.Unlock()
		return
	}
	t := vs.t
	m.collecting[id] = true
	m.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.collecting, id)
			m.mu.Unlock()
		}()
		final, exit, err := m.collectOutput(id, t)
		if final {
			p.mu.Lock()
			p.collected = true
			p.mu.Unlock()
		}
		if exit != nil && exit.Status != "" && exit.Status != guestproto.StatusRunning && !p.exited() {
			// The exit nobody else observes (a detached process with no
			// stream): record it now, so the VM stops counting as busy.
			p.observe(m.cfg.Now(), exit.Status, exit.ExitCode, exit.Signal, exit.TimedOut, 0)
			m.persist(m.ctx, p)
			m.active(vm)
		}
		if err != nil && m.ctx.Err() == nil {
			m.cfg.Log.Warn("collect process output", "process", id, "err", err)
		}
	}()
}

// collectOutput records id's output until it exits, keeping the newest
// RetainOutput bytes; final means recording ended for good (exit, output
// evicted), and exit is how it ended when the agent said.
func (m *Manager) collectOutput(id string, t lifecycle.GuestTarget) (final bool, exit *guestproto.ProcessExited, err error) {
	last, size, err := m.out.OutputExtent(m.ctx, id)
	if err != nil {
		return false, nil, err
	}
	c, err := m.connect(m.ctx, t)
	if err != nil {
		return false, nil, err
	}
	defer c.Close()
	ex, err := c.AttachFunc(m.ctx, id, uint64(last), func(seq uint64, isErr bool, data []byte) error {
		stream := api.StreamStdout
		if isErr {
			stream = api.StreamStderr
		}
		if err := m.out.AppendOutput(m.ctx, id, state.OutputChunk{Seq: int64(seq), Stream: stream, Data: data}); err != nil {
			return err
		}
		if size += int64(len(data)); size > int64(m.cfg.RetainOutput) {
			left, err := m.out.TrimOutput(m.ctx, id, int64(m.cfg.RetainOutput))
			if err != nil {
				return err
			}
			size = left
		}
		return nil
	})
	switch {
	case err == nil:
		return true, &ex, nil
	case errors.Is(err, guestproto.ErrInvalid):
		// The agent no longer retains output after our cursor (the daemon
		// was down while it was evicted): that output is lost.
		m.cfg.Log.Warn("process output evicted by the guest before it was recorded", "process", id, "afterSeq", last)
		return true, nil, nil
	}
	return false, nil, err
}

// AppendOutput implements OutputStore.
func (s DBStore) AppendOutput(ctx context.Context, process string, c state.OutputChunk) error {
	return s.DB.Tx(ctx, func(tx *state.Tx) error { return tx.AppendOutput(process, c) })
}

// TrimOutput implements OutputStore.
func (s DBStore) TrimOutput(ctx context.Context, process string, keep int64) (left int64, err error) {
	err = s.DB.Tx(ctx, func(tx *state.Tx) error {
		left, err = tx.TrimOutput(process, keep)
		return err
	})
	return left, err
}

// OutputExtent implements OutputStore.
func (s DBStore) OutputExtent(ctx context.Context, process string) (lastSeq, size int64, err error) {
	const page = 1024
	for {
		var chunks []state.OutputChunk
		if err := s.DB.Tx(ctx, func(tx *state.Tx) (err error) {
			chunks, err = tx.ReadOutput(process, lastSeq, page)
			return err
		}); err != nil {
			return 0, 0, err
		}
		for _, c := range chunks {
			lastSeq, size = c.Seq, size+int64(len(c.Data))
		}
		if len(chunks) < page {
			return lastSeq, size, nil
		}
	}
}
