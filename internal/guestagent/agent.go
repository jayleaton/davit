//go:build linux

package guestagent

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"davit/internal/guestproto"
)

// Config configures an Agent.
type Config struct {
	// Root prefixes the system identity files the barrier rewrites
	// (etc/machine-id, etc/hostname, etc/hosts, etc/ssh, var/lib/davit).
	// "/" in production; tests point it at a temp dir. Exec and file RPCs
	// always use real guest paths.
	Root string
	// RunDir holds the per-generation secret across agent restarts. It must
	// be on tmpfs so the secret never lands in a disk snapshot. Default
	// /run/davit-agent.
	RunDir string
	// System performs clock, hostname, service and power operations.
	System System
	// DefaultUser runs Exec requests with an empty User. Empty means the
	// agent's own user.
	DefaultUser string
	// SSHUnit is restarted after host keys or sshd config change. Default
	// "ssh.service" (Ubuntu); "-" disables.
	SSHUnit string
	// MachineIDUnits are restarted after machine-id rotation. Default
	// systemd-journald.service.
	MachineIDUnits []string
	// RetainOutput bounds retained output per process for ProcessAttach;
	// default 8 MiB.
	RetainOutput int
	// DialTCP connects DialTCP requests to a guest-loopback port; default
	// 127.0.0.1:port. Test and fake guests point it at in-process services.
	DialTCP func(ctx context.Context, port uint16) (net.Conn, error)
	// Listen opens Listen requests' listeners; default a TCP listener on
	// 127.0.0.1:port (port 0 picks one).
	Listen func(ctx context.Context, port int) (net.Listener, error)
	// ListenPending bounds a listener's connections waiting for the host's
	// ReverseConnect (default 32); ListenClaimWindow is how long one waits
	// (default 30s).
	ListenPending     int
	ListenClaimWindow time.Duration
	// ProcRoot is where ProcessListRequest reads processes; default /proc.
	ProcRoot string
	Log      *slog.Logger
}

// Agent is the in-guest agent. One Agent serves any number of sessions;
// process and identity state outlive sessions.
type Agent struct {
	cfg   Config
	procs procTable

	barrier   sync.Mutex // serialises SetIdentity
	persistMu sync.Mutex
	// jobsMu orders jobs-file writers; jobLines counts its lines.
	jobsMu   sync.Mutex
	jobLines int

	mu   sync.Mutex
	held heldSecret
	// sshd freshness (see restartAsync): guarded by mu.
	sshEpoch, sshDone uint64
	sshFailed         bool
	sessions          map[*session]struct{}

	lmu       sync.Mutex
	listeners map[string]*listener
	workspace sync.Mutex // held while a WorkspacePrepare runs
}

// heldSecret is the generation the agent is bound to and its secret.
type heldSecret struct {
	VMID       string `json:"vmId"`
	Generation uint64 `json:"generation"`
	Secret     []byte `json:"secret"`
	// Ready is true once the barrier completed for this generation.
	Ready bool `json:"ready"`
}

const secretFile = "secret.json"

// New returns an agent, loading a secret persisted by a previous agent
// process in this boot.
func New(cfg Config) (*Agent, error) {
	if cfg.Root == "" {
		cfg.Root = "/"
	}
	if cfg.RunDir == "" {
		cfg.RunDir = "/run/davit-agent"
	}
	if cfg.System == nil {
		return nil, errors.New("guestagent: System is required")
	}
	if cfg.SSHUnit == "" {
		cfg.SSHUnit = "ssh.service"
	}
	if cfg.MachineIDUnits == nil {
		cfg.MachineIDUnits = []string{"systemd-journald.service"}
	}
	if cfg.RetainOutput == 0 {
		cfg.RetainOutput = 8 << 20
	}
	if cfg.Listen == nil {
		cfg.Listen = listenLoopback
	}
	if cfg.ListenPending <= 0 {
		cfg.ListenPending = defaultListenPending
	}
	if cfg.ListenClaimWindow <= 0 {
		cfg.ListenClaimWindow = defaultListenClaimWindow
	}
	if cfg.ProcRoot == "" {
		cfg.ProcRoot = "/proc"
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	a := &Agent{cfg: cfg, sessions: map[*session]struct{}{}, listeners: map[string]*listener{}}
	a.procs.init()
	raw, err := os.ReadFile(filepath.Join(cfg.RunDir, secretFile))
	switch {
	case err == nil:
		if json.Unmarshal(raw, &a.held) != nil {
			a.held = heldSecret{}
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	if err := a.loadJobs(); err != nil {
		return nil, err
	}
	return a, nil
}

// Serve accepts sessions on l until ctx ends or l fails.
func (a *Agent) Serve(ctx context.Context, l net.Listener) error {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.ServeConn(ctx, c)
		}()
	}
}

// ServeConn runs one control session on c and closes it.
func (a *Agent) ServeConn(ctx context.Context, c net.Conn) {
	s := newSession(a, c)
	stop := context.AfterFunc(ctx, s.close)
	defer stop()
	s.run()
}

// authenticate applies the handshake's secret rules and returns the secret
// the proof must use. Rules:
//   - A Hello for an older generation of the bound VM is refused.
//   - BootstrapSecret is accepted unless this exact (vmId, generation)
//     already holds a different secret: the first session of a generation
//     (cold boot, golden/fork restore, which resumes RAM bound to another
//     VM or generation) provisions it; re-sending the same bootstrap is
//     idempotent.
//   - Without a bootstrap, the agent must hold this generation's secret.
//
// The bootstrap is only as strong as the vsock link: only the host can
// reach the agent's port (peer CID is checked), and guests cannot reach
// each other's vsock.
func (a *Agent) authenticate(h guestproto.Hello) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	same := a.held.VMID == h.VMID && a.held.Generation == h.Generation && a.held.Secret != nil
	if a.held.VMID == h.VMID && h.Generation < a.held.Generation {
		return nil, guestproto.Errorf(guestproto.CodeUnauthenticated, "generation %d is older than bound generation %d", h.Generation, a.held.Generation)
	}
	if h.BootstrapSecret == nil {
		if !same {
			return nil, guestproto.Errorf(guestproto.CodeUnauthenticated, "no secret provisioned for this generation")
		}
		return bytes.Clone(a.held.Secret), nil
	}
	if same && subtle.ConstantTimeCompare(a.held.Secret, h.BootstrapSecret) != 1 {
		return nil, guestproto.Errorf(guestproto.CodeUnauthenticated, "generation already provisioned")
	}
	return bytes.Clone(h.BootstrapSecret), nil
}

// bind records the generation a verified bootstrap session provisioned.
// Moving to a new generation discards every other session (SPEC §5 step 1)
// and resets readiness until the barrier runs again.
func (a *Agent) bind(s *session, h guestproto.Hello, secret []byte) error {
	a.mu.Lock()
	newGen := a.held.VMID != h.VMID || a.held.Generation != h.Generation
	a.held = heldSecret{VMID: h.VMID, Generation: h.Generation, Secret: bytes.Clone(secret), Ready: a.held.Ready && !newGen}
	var stale []*session
	if newGen {
		for o := range a.sessions {
			if o != s {
				stale = append(stale, o)
			}
		}
	}
	a.mu.Unlock()
	for _, o := range stale {
		o.close()
	}
	return a.persist()
}

// rotate replaces the held secret after SetIdentity and marks the barrier
// done for gen.
func (a *Agent) rotate(vmID string, gen uint64, secret []byte) error {
	a.mu.Lock()
	if a.held.VMID != vmID || a.held.Generation != gen {
		a.mu.Unlock()
		return guestproto.Errorf(guestproto.CodeUnauthenticated, "session generation is no longer bound")
	}
	if secret != nil {
		a.held.Secret = bytes.Clone(secret)
	}
	a.held.Ready = true
	a.mu.Unlock()
	return a.persist()
}

// barrierDone reports whether the barrier completed for this generation.
func (a *Agent) barrierDone(vmID string, gen uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held.Ready && a.held.VMID == vmID && a.held.Generation == gen
}

// persist writes the current held secret; persistMu orders writers so the
// file always ends with the latest state.
func (a *Agent) persist() error {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.mu.Lock()
	h := a.held
	a.mu.Unlock()
	if err := os.MkdirAll(a.cfg.RunDir, 0o700); err != nil {
		return err
	}
	raw, _ := json.Marshal(h)
	return writeAtomic(filepath.Join(a.cfg.RunDir, secretFile), raw, 0o600)
}

func (a *Agent) addSession(s *session) {
	a.mu.Lock()
	a.sessions[s] = struct{}{}
	a.mu.Unlock()
}

func (a *Agent) removeSession(s *session) {
	a.mu.Lock()
	delete(a.sessions, s)
	a.mu.Unlock()
}

// writeAtomic writes data to path via a synced temp file and rename.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".davit-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeNoSync replaces path via a temp file and rename without fsync; the
// caller makes it durable (syncFS) before relying on it.
func writeNoSync(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".davit-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// syncFS flushes the filesystem holding path (data, metadata and renames).
func syncFS(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}
