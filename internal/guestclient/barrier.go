package guestclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"davit/internal/domain"
	"davit/internal/guestproto"
	"davit/internal/runtime"
)

// Plan is what the restore barrier assigns to one boot of an instance.
type Plan struct {
	Hostname string
	// RotateIdentity requests a new machine-id and SSH host keys: new VMs and
	// forks, not resume of the same disk VM (SPEC §5). The agent also rotates
	// on its own when the disk's identity stamp names another VM.
	RotateIdentity bool
	SSHUserCAKeys  []string
	// WaitSSH holds readiness until sshd answers with an SSH banner.
	WaitSSH bool
	// Services must all report ready (e.g. "docker") before readiness.
	Services []string
}

// DefaultPlan derives a plan from the runtime key alone: hostname from the
// VM ID, rotation on generation 1 (create and fork start a new VM ID at
// generation 1; resume increments it), and SSH required.
func DefaultPlan(key runtime.Key) Plan {
	return Plan{Hostname: HostnameFor(string(key.VMID)), RotateIdentity: key.Generation == 1, WaitSSH: true}
}

// HostnameFor maps a VM name or ID to a valid RFC 1123 label.
func HostnameFor(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	h := strings.Trim(b.String(), "-")
	if len(h) > 63 {
		h = strings.TrimRight(h[:63], "-")
	}
	if h == "" {
		h = "vm"
	}
	return h
}

// Prober is the lifecycle ReadinessProber backed by the guest agent. Each
// Probe runs the full restore barrier (SPEC §5 steps 1-5) on a fresh
// session: authenticate, set clock and identity, rotate the session secret,
// verify the guest's report, then wait for readiness.
type Prober struct {
	// Secrets holds per-generation secrets. Required.
	Secrets SecretStore
	// Port is the agent's vsock port; default DefaultPort.
	Port uint32
	// Plan supplies hostname/rotation policy; default DefaultPlan.
	Plan func(ctx context.Context, key runtime.Key) (Plan, error)
	// OnIdentity runs after the guest's identity report verified, before
	// readiness is published (SSH host-key registration).
	OnIdentity func(ctx context.Context, key runtime.Key, id guestproto.IdentityResult) error
	// Now is the trusted host clock sent to the guest; default time.Now.
	Now func() time.Time
	// Poll is the retry interval for dialing and readiness; default 20ms.
	Poll time.Duration
	// Log, when set, receives a debug line per barrier with phase timings.
	Log *slog.Logger
}

// Probe implements lifecycle.ReadinessProber.
func (p *Prober) Probe(ctx context.Context, rt runtime.Runtime, h runtime.Handle) (domain.Readiness, error) {
	if p.Secrets == nil {
		return domain.Readiness{}, errors.New("guestclient: Prober.Secrets is required")
	}
	d, err := rt.GuestTransport(ctx, h)
	if err != nil {
		return domain.Readiness{}, err
	}
	port := p.Port
	if port == 0 {
		port = DefaultPort
	}
	dial := func(ctx context.Context) (net.Conn, error) { return d.DialGuest(ctx, port) }
	alive := func(ctx context.Context) error {
		st, err := rt.Inspect(ctx, h)
		if err != nil {
			return err
		}
		if st.Phase == runtime.PhaseExited || st.Phase == runtime.PhaseGone {
			return domain.Errorf(domain.CodeUnavailable, "instance exited during boot (%s)", st.ExitReason)
		}
		return nil
	}
	return p.Barrier(ctx, h.Key, dial, alive)
}

// Barrier runs the restore barrier against the instance key over dial.
// alive, when set, is checked between connection attempts so a dead
// instance fails fast instead of waiting out ctx.
func (p *Prober) Barrier(ctx context.Context, key runtime.Key, dial DialFunc, alive func(context.Context) error) (domain.Readiness, error) {
	plan := DefaultPlan(key)
	if p.Plan != nil {
		var err error
		if plan, err = p.Plan(ctx, key); err != nil {
			return domain.Readiness{}, err
		}
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	poll := p.Poll
	if poll == 0 {
		poll = 20 * time.Millisecond
	}

	t0 := time.Now()
	var tConnect, tIdentity time.Duration
	polls := 0
	defer func() {
		if p.Log != nil {
			p.Log.Debug("restore barrier timings", "vm", key.VMID, "generation", key.Generation,
				"connectMs", ms(tConnect), "identityMs", ms(tIdentity), "readyMs", ms(time.Since(t0)-tConnect-tIdentity),
				"readinessPolls", polls, "totalMs", ms(time.Since(t0)))
		}
	}()

	// Step 1: fresh authenticated session.
	next := newSecret() // the secret SetIdentity will rotate to
	c, used, saved, err := p.connect(ctx, key, dial, alive, poll, next)
	tConnect = time.Since(t0)
	if err != nil {
		return domain.Readiness{}, err
	}
	defer c.Close()

	// Steps 2-4: clock, identity, credentials. Persist {new, old} first so a
	// crash after the guest switched secrets can still authenticate it.
	if !saved {
		if err := p.Secrets.Save(ctx, key, [][]byte{next, used}); err != nil {
			return domain.Readiness{}, err
		}
	}
	id, err := c.SetIdentity(ctx, guestproto.SetIdentity{
		VMID: string(key.VMID), Generation: uint64(key.Generation), Hostname: plan.Hostname,
		WallClockUnixNano: now().UnixNano(),
		RotateMachineID:   plan.RotateIdentity, RotateSSHHostKeys: plan.RotateIdentity,
		NewAgentSecret: next, SSHUserCAKeys: plan.SSHUserCAKeys,
	})
	tIdentity = time.Since(t0) - tConnect
	if err != nil {
		return domain.Readiness{}, fmt.Errorf("guestclient: restore barrier: %w", err)
	}

	// Step 5: verify the report against the host's records.
	if err := verifyIdentity(key, plan, id); err != nil {
		return domain.Readiness{}, err
	}
	if p.OnIdentity != nil {
		if err := p.OnIdentity(ctx, key, id); err != nil {
			return domain.Readiness{}, err
		}
	}
	return waitReady(ctx, c, plan, poll, &polls)
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func verifyIdentity(key runtime.Key, plan Plan, id guestproto.IdentityResult) error {
	bad := func(what string) error {
		return domain.Errorf(domain.CodeInternal, "restore barrier: guest reported wrong %s", what)
	}
	switch {
	case id.VMID != string(key.VMID):
		return bad("vmId")
	case id.Generation != uint64(key.Generation):
		return bad("generation")
	case id.Hostname != plan.Hostname:
		return bad("hostname")
	case !validMachineID(id.MachineID):
		return bad("machineId")
	case plan.WaitSSH && len(id.SSHHostKeyFingerprints) == 0:
		return bad("SSH host keys (none)")
	}
	return nil
}

func validMachineID(s string) bool {
	if len(s) != 32 || strings.Trim(s, "0") == "" {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// connect establishes the authenticated session, retrying while the agent
// is not yet listening. It returns the secret the guest proved and whether
// next was already saved alongside it (a bootstrap saves both at once).
func (p *Prober) connect(ctx context.Context, key runtime.Key, dial DialFunc, alive func(context.Context) error, poll time.Duration, next []byte) (*Client, []byte, bool, error) {
	cands, err := p.Secrets.Load(ctx, key)
	if err != nil {
		return nil, nil, false, err
	}
	s := Session{VMID: string(key.VMID), Generation: uint64(key.Generation), Secrets: cands}
	bootstrapped := false
	if len(cands) == 0 {
		if s, err = p.bootstrap(ctx, key, s, next); err != nil {
			return nil, nil, false, err
		}
		bootstrapped = true
	}
	for {
		c, err := Connect(ctx, dial, s)
		if err == nil {
			return c, c.secret, bootstrapped, nil
		}
		switch {
		case ctx.Err() != nil:
			return nil, nil, false, ctx.Err()
		case errors.Is(err, ErrRejected) && !bootstrapped:
			// The guest holds no secret for this generation (e.g. the host
			// recorded one but crashed before Hello). Offer a bootstrap; a
			// guest that does hold one refuses it.
			if s, err = p.bootstrap(ctx, key, s, next); err != nil {
				return nil, nil, false, err
			}
			bootstrapped = true
			continue
		case errors.Is(err, ErrRejected), isProtoError(err):
			return nil, nil, false, domain.Errorf(domain.CodeUnavailable, "guest agent handshake failed: %v", err)
		}
		if alive != nil {
			if aerr := alive(ctx); aerr != nil {
				return nil, nil, false, aerr
			}
		}
		select {
		case <-time.After(poll):
		case <-ctx.Done():
			return nil, nil, false, ctx.Err()
		}
	}
}

// bootstrap provisions a fresh secret and, in the same durable write, the
// secret the barrier will rotate to, so a crash at any later point leaves a
// candidate the guest holds.
func (p *Prober) bootstrap(ctx context.Context, key runtime.Key, s Session, next []byte) (Session, error) {
	s.Bootstrap = newSecret()
	if err := p.Secrets.Save(ctx, key, append([][]byte{next, s.Bootstrap}, s.Secrets...)); err != nil {
		return s, err
	}
	return s, nil
}

func isProtoError(err error) bool {
	var pe *guestproto.Error
	return errors.As(err, &pe)
}

func newSecret() []byte {
	b := make([]byte, guestproto.MinSecretSize)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// waitReady polls the agent until it, sshd and the plan's services are up.
func waitReady(ctx context.Context, c *Client, plan Plan, poll time.Duration, polls *int) (domain.Readiness, error) {
	for {
		*polls++
		r, err := c.Readiness(ctx, plan.Services)
		if err != nil {
			return domain.Readiness{}, err
		}
		out := domain.Readiness{Agent: r.Agent, SSH: r.SSH, Services: map[string]bool{}}
		ok := r.Agent && (r.SSH || !plan.WaitSSH)
		if plan.WaitSSH && r.Services[guestproto.ServiceSSHRestart] == guestproto.ServiceFailed {
			return domain.Readiness{}, domain.Errorf(domain.CodeUnavailable, "guest could not restart sshd with its new host keys")
		}
		for name, st := range r.Services {
			out.Services[name] = st == guestproto.ServiceReady
		}
		for _, name := range plan.Services {
			switch r.Services[name] {
			case guestproto.ServiceReady:
			case guestproto.ServiceFailed:
				return domain.Readiness{}, domain.Errorf(domain.CodeUnavailable, "guest service %s failed", name)
			default:
				ok = false
			}
		}
		if ok {
			return out, nil
		}
		select {
		case <-time.After(poll):
		case <-ctx.Done():
			return domain.Readiness{}, ctx.Err()
		}
	}
}
