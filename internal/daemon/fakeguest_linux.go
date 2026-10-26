//go:build linux

package daemon

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"davit/internal/domain"
	"davit/internal/guestagent"
	"davit/internal/guestproto"
	"davit/internal/runtime"
)

// fakeGuests runs one in-process guest agent per fake VM instance, so the
// full barrier, exec and file paths work without real VMs. Identity files
// live under dir/<vm>/root (persisting across generations like a disk);
// the secret's tmpfs stand-in is dir/<vm>/run-<generation>. Commands run on
// the host as the daemon user.
type fakeGuests struct {
	dir string
	log *slog.Logger

	mu     sync.Mutex
	agents map[domain.VMID]*fakeGuest
}

type fakeGuest struct {
	gen    int64
	agent  *guestagent.Agent
	ctx    context.Context
	cancel context.CancelFunc
}

func newFakeGuests(dir string, log *slog.Logger) func(context.Context, runtime.Handle) (runtime.Dialer, error) {
	g := &fakeGuests{dir: dir, log: log, agents: map[domain.VMID]*fakeGuest{}}
	return g.transport
}

func (g *fakeGuests) transport(_ context.Context, h runtime.Handle) (runtime.Dialer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	cur := g.agents[h.VMID]
	if cur != nil && cur.gen == h.Generation {
		return cur, nil
	}
	if cur != nil {
		cur.cancel() // a new generation replaces the old guest
	}
	vmDir := filepath.Join(g.dir, string(h.VMID))
	root := filepath.Join(vmDir, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	a, err := guestagent.New(guestagent.Config{
		Root: root, RunDir: filepath.Join(vmDir, "run-"+strconv.FormatInt(h.Generation, 10)),
		System: hostlessSystem{}, SSHUnit: "-", MachineIDUnits: []string{},
		DialTCP: fakeDialTCP(root, g.log),
		Log:     g.log.With("vm", h.VMID, "generation", h.Generation, "component", "fake-guest-agent"),
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cur = &fakeGuest{gen: h.Generation, agent: a, ctx: ctx, cancel: cancel}
	g.agents[h.VMID] = cur
	return cur, nil
}

// DialGuest connects to the in-process agent over a Unix socketpair, which
// (like Firecracker's hybrid vsock socket) supports half-close.
func (f *fakeGuest) DialGuest(ctx context.Context, port uint32) (net.Conn, error) {
	if port != guestagent.Port {
		return nil, domain.Errorf(domain.CodeUnavailable, "fake guest: nothing listens on vsock port %d", port)
	}
	if err := f.ctx.Err(); err != nil {
		return nil, domain.Errorf(domain.CodeUnavailable, "fake guest: instance replaced")
	}
	host, guest, err := socketPair()
	if err != nil {
		return nil, err
	}
	go f.agent.ServeConn(f.ctx, guest)
	return host, nil
}

// socketPair returns the two ends of a connected Unix stream socketpair.
func socketPair() (net.Conn, net.Conn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	conn := func(fd int, name string) (net.Conn, error) {
		f := os.NewFile(uintptr(fd), name)
		defer f.Close() // FileConn dups the descriptor
		return net.FileConn(f)
	}
	a, err := conn(fds[0], "fake-vsock-host")
	if err != nil {
		syscall.Close(fds[1])
		return nil, nil, err
	}
	b, err := conn(fds[1], "fake-vsock-guest")
	if err != nil {
		a.Close()
		return nil, nil, err
	}
	return a, b, nil
}

// hostlessSystem is the fake guest's OS: it must not change the host's
// clock, hostname or services, and it has no sshd (TCPDial to port 22 gets
// an in-process SSH server instead, see fakeDialTCP). Readiness reports SSH
// false and systemd unknown rather than claiming what it did not verify.
type hostlessSystem struct{}

func (hostlessSystem) SetClock(time.Time) error                     { return nil }
func (hostlessSystem) SetHostname(string) error                     { return nil }
func (hostlessSystem) RestartUnits(context.Context, []string) error { return nil }
func (hostlessSystem) SystemState(context.Context) guestproto.ServiceState {
	return guestproto.ServiceUnknown
}
func (hostlessSystem) UnitState(context.Context, string) guestproto.ServiceState {
	return guestproto.ServiceUnknown
}
func (hostlessSystem) SSHReady(context.Context) bool  { return false }
func (hostlessSystem) PowerOff(context.Context) error { return nil }
