package gateway

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"davit/internal/domain"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// DefaultGuestUser is the guest account when the SSH username names only a VM.
const DefaultGuestUser = "user"

const guestSSHPort = 22

var (
	vmRefRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	guestUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// ParseSSHUser splits the SSH login name of the shared front door into a VM
// reference and guest user: "devbox" is (devbox, DefaultGuestUser) and
// "devbox+root" is (devbox, root). Both parts are routing parameters that the
// Authorizer and GuestSSH still have to approve.
func ParseSSHUser(login string) (vmRef, guestUser string, err error) {
	vmRef, guestUser = login, DefaultGuestUser
	for i := len(login) - 1; i >= 0; i-- {
		if login[i] == '+' {
			vmRef, guestUser = login[:i], login[i+1:]
			break
		}
	}
	if !vmRefRE.MatchString(vmRef) || !guestUserRE.MatchString(guestUser) {
		return "", "", fmt.Errorf("%w: ssh user %q (want <vm> or <vm>+<guest-user>)", ErrInvalidTarget, login)
	}
	return vmRef, guestUser, nil
}

// SSHFrontDoor is the shared gateway's `ssh <vm>@<host>` endpoint. It
// terminates the client's SSH session (the client sees the gateway's stable
// host key), authenticates the peer by tailnet identity, authorizes the VM
// named in the login, then opens a second SSH session to that VM's sshd over
// GuestDialer with a GuestSSH credential, verifying the guest's host key.
// Channels, requests and port forwards are relayed unchanged.
//
// This deliberately does not use tsnet.ListenSSH/Tailscale SSH: the login
// carries routing, not credentials, and the client's keys are not used.
// Clients that need end-to-end guest host-key verification use a per-VM
// endpoint node (raw byte tunnel) or the HTTP tunnel instead.
type SSHFrontDoor struct {
	Streams *Streams
	Guest   GuestSSH
	HostKey ssh.Signer
	// HandshakeTimeout bounds identification, authorization and both SSH
	// handshakes. Zero means 20s.
	HandshakeTimeout time.Duration
	Logf             func(string, ...any)
}

// Serve accepts SSH clients on ln until it closes or ctx ends.
func (f *SSHFrontDoor) Serve(ctx context.Context, ln net.Listener, w WhoIser) error {
	if f.Streams == nil || f.Guest == nil || f.HostKey == nil {
		return errors.New("gateway: ssh front door not configured")
	}
	return serveConns(ctx, ln, func(ctx context.Context, c net.Conn) { f.handle(ctx, w, c) })
}

func (f *SSHFrontDoor) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
	}
}

// upstream is the authorized guest session opened during client auth.
type upstream struct {
	vm    domain.VMID
	login string
	conn  ssh.Conn
	chans <-chan ssh.NewChannel
	reqs  <-chan *ssh.Request
}

func (f *SSHFrontDoor) handle(ctx context.Context, w WhoIser, c net.Conn) {
	defer c.Close()
	timeout := f.HandshakeTimeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c.SetDeadline(time.Now().Add(timeout))

	id, err := IdentifyConn(hctx, w, c)
	if err != nil {
		f.logf("ssh: reject %v: %v", c.RemoteAddr(), err)
		return
	}

	// Auth callbacks run sequentially on the handshake goroutine. The
	// first decision for a login is cached so clients that try several keys
	// do not re-dial the guest; a changed login is decided afresh.
	var up *upstream
	denied := map[string]error{}
	authorize := func(meta ssh.ConnMetadata) (*ssh.Permissions, error) {
		login := meta.User()
		if up != nil {
			if up.login == login {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("login changed after authorization")
		}
		if err, ok := denied[login]; ok {
			return nil, err
		}
		u, err := f.dialUpstream(hctx, id, login)
		if err != nil {
			f.logf("ssh: %s as %q: %v", id, login, err)
			msg := "davit: VM not found or access denied\r\n"
			switch {
			case errors.Is(err, ErrInvalidTarget):
				msg = "davit: use ssh <vm>@<host> or ssh <vm>+<guest-user>@<host>\r\n"
			case errors.Is(err, ErrNotRunning): // only after authorization
				msg = "davit: VM is not running; resume it first\r\n"
			}
			denied[login] = &ssh.BannerError{Err: err, Message: msg}
			return nil, denied[login]
		}
		up = u
		return &ssh.Permissions{}, nil
	}
	cfg := &ssh.ServerConfig{
		NoClientAuth:         true,
		NoClientAuthCallback: authorize,
		// Any client key is accepted once the tailnet identity is
		// authorized; the key itself grants nothing.
		PublicKeyCallback: func(meta ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			return authorize(meta)
		},
		ServerVersion: "SSH-2.0-davit-gateway",
	}
	cfg.AddHostKey(f.HostKey)

	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		if up != nil {
			up.conn.Close()
		}
		return
	}
	defer sc.Close()
	if up == nil { // cannot happen: auth succeeded only through authorize
		return
	}
	defer up.conn.Close()
	defer f.Streams.Sessions.Track(id.UserID, up.vm, c)()
	c.SetDeadline(time.Time{})
	cancel()
	f.logf("ssh: %s -> %s", id, up.login)
	relayConns(ctx, sc, chans, reqs, up.conn, up.chans, up.reqs)
}

func (f *SSHFrontDoor) dialUpstream(ctx context.Context, id Identity, login string) (*upstream, error) {
	vmRef, guestUser, err := ParseSSHUser(login)
	if err != nil {
		return nil, err
	}
	g, t, err := f.Streams.Open(ctx, id, Request{VMRef: vmRef, Action: ActionSSH, Port: guestSSHPort})
	if err != nil {
		return nil, err
	}
	cfg, err := f.Guest.ClientConfig(ctx, id, t, guestUser)
	if err == nil && (cfg == nil || cfg.HostKeyCallback == nil) {
		err = errors.New("guest ssh config has no host key verification")
	}
	if err != nil {
		g.Close()
		return nil, err
	}
	cfg = copyClientConfig(cfg, guestUser)
	if dl, ok := ctx.Deadline(); ok {
		g.SetDeadline(dl)
	}
	conn, chans, reqs, err := ssh.NewClientConn(g, string(t.VMID), cfg)
	if err != nil {
		g.Close()
		return nil, fmt.Errorf("guest ssh handshake: %w", err)
	}
	g.SetDeadline(time.Time{})
	return &upstream{vm: t.VMID, login: login, conn: conn, chans: chans, reqs: reqs}, nil
}

// copyClientConfig keeps the account GuestSSH chose (it may resolve
// DefaultGuestUser to the image's default account), else the requested one.
func copyClientConfig(c *ssh.ClientConfig, user string) *ssh.ClientConfig {
	cc := *c
	if cc.User == "" {
		cc.User = user
	}
	return &cc
}

// relayConns relays everything between the client-facing and guest-facing
// SSH connections until either closes or ctx ends.
func relayConns(ctx context.Context, a ssh.Conn, aChans <-chan ssh.NewChannel, aReqs <-chan *ssh.Request,
	b ssh.Conn, bChans <-chan ssh.NewChannel, bReqs <-chan *ssh.Request) {
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); relayGlobalRequests(aReqs, b) }()
	go func() { defer wg.Done(); relayGlobalRequests(bReqs, a) }()
	go func() { defer wg.Done(); relayNewChannels(aChans, b) }()
	go func() { defer wg.Done(); relayNewChannels(bChans, a) }()

	done := make(chan struct{}, 2)
	go func() { a.Wait(); done <- struct{}{} }()
	go func() { b.Wait(); done <- struct{}{} }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	a.Close()
	b.Close()
	wg.Wait()
}

func relayGlobalRequests(in <-chan *ssh.Request, dst ssh.Conn) {
	for r := range in {
		ok, payload, err := dst.SendRequest(r.Type, r.WantReply, r.Payload)
		if r.WantReply {
			r.Reply(ok && err == nil, payload)
		}
	}
}

func relayNewChannels(in <-chan ssh.NewChannel, dst ssh.Conn) {
	var wg sync.WaitGroup
	for nc := range in {
		wg.Add(1)
		go func() { defer wg.Done(); relayChannel(nc, dst) }()
	}
	wg.Wait()
}

func relayChannel(nc ssh.NewChannel, dst ssh.Conn) {
	dch, dreqs, err := dst.OpenChannel(nc.ChannelType(), nc.ExtraData())
	if err != nil {
		var oe *ssh.OpenChannelError
		if errors.As(err, &oe) {
			nc.Reject(oe.Reason, oe.Message)
		} else {
			nc.Reject(ssh.ConnectionFailed, "upstream unavailable")
		}
		return
	}
	sch, sreqs, err := nc.Accept()
	if err != nil {
		dch.Close()
		return
	}
	pipeChannels(sch, sreqs, dch, dreqs)
}

// pipeChannels relays data, stderr and requests between two channels. A side
// is closed only after the other side closed, everything it sent has been
// delivered, and every request that side sent has had its reply relayed
// back, so trailing output, exit-status and the reply to a fast command's
// exec are never lost.
func pipeChannels(a ssh.Channel, aReqs <-chan *ssh.Request, b ssh.Channel, bReqs <-chan *ssh.Request) {
	half := func(dst, src ssh.Channel) <-chan struct{} {
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); io.Copy(dst, src) }()
		go func() { defer wg.Done(); io.Copy(dst.Stderr(), src.Stderr()) }()
		// EOF covers both streams: send it only after stderr drained too.
		go func() { wg.Wait(); dst.CloseWrite(); close(done) }()
		return done
	}
	aToB, bToA := half(b, a), half(a, b)
	// Each relayed request holds its origin's read lock until the reply is
	// back; closing a side takes that side's write lock first.
	var aPending, bPending sync.RWMutex
	closeAfter := func(c ssh.Channel, pending *sync.RWMutex) {
		pending.Lock()
		c.Close()
		pending.Unlock()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		relayChannelRequests(aReqs, b, &aPending)
		<-aToB
		closeAfter(b, &bPending)
	}()
	go func() {
		defer wg.Done()
		relayChannelRequests(bReqs, a, &bPending)
		<-bToA
		closeAfter(a, &aPending)
	}()
	wg.Wait()
}

// relayChannelRequests forwards requests from in to dst, holding pending's
// read lock until each reply has been sent back.
func relayChannelRequests(in <-chan *ssh.Request, dst ssh.Channel, pending *sync.RWMutex) {
	for r := range in {
		pending.RLock()
		ok, err := dst.SendRequest(r.Type, r.WantReply, r.Payload)
		if r.WantReply {
			r.Reply(ok && err == nil, nil)
		}
		pending.RUnlock()
	}
}

// LoadOrCreateHostKey returns the ed25519 host key at path, creating it
// (0600, atomically) if absent.
func LoadOrCreateHostKey(path string) (ssh.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "davit gateway host key")
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hostkey-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := pem.Encode(tmp, blk); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) { // lost a race with another creator
			return LoadOrCreateHostKey(path)
		}
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}
