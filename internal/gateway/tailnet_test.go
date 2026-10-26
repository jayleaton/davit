package gateway

// In-process tailnet tests: Tailscale's testcontrol coordination server plus
// an in-process DERP/STUN relay, with every node a tsnet.Server on loopback.
// Nothing joins a real tailnet or touches the host's tailscaled.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"tailscale.com/ipn"
	"tailscale.com/ipn/store/mem"

	"davit/internal/domain"
	"davit/internal/gateway/gatewaytest"
)

// fakeAuth grants VM names to tailnet user IDs; revocable at runtime.
type fakeAuth struct {
	mu     sync.Mutex
	grants map[int64]map[string]bool
	vms    map[string]Target // name -> target
}

func (a *fakeAuth) set(user int64, vm string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.grants[user] == nil {
		a.grants[user] = map[string]bool{}
	}
	a.grants[user][vm] = ok
}

func (a *fakeAuth) Authorize(_ context.Context, who Identity, req Request) (Target, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for name, t := range a.vms {
		if req.VMRef != name && req.VMRef != string(t.VMID) {
			continue
		}
		if !a.grants[who.UserID][name] {
			return Target{}, ErrDenied
		}
		return t, nil
	}
	return Target{}, ErrNotFound
}

// fakeGuests stands in for vsock: guest port 22 reaches a per-VM SSH server,
// any other port an echo server.
type fakeGuests struct {
	ssh  map[domain.VMID]string // vmID -> sshd addr
	echo string
}

func (f *fakeGuests) DialGuest(ctx context.Context, t Target, port uint16) (net.Conn, error) {
	addr := f.echo
	if port == 22 {
		addr = f.ssh[t.VMID]
	}
	if addr == "" {
		return nil, errors.New("no such guest")
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

type fakeGuestSSH struct {
	key     ssh.Signer
	hostKey ssh.PublicKey
}

func (g fakeGuestSSH) ClientConfig(context.Context, Identity, Target, string) (*ssh.ClientConfig, error) {
	return &ssh.ClientConfig{
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(g.key)},
		HostKeyCallback: ssh.FixedHostKey(g.hostKey),
	}, nil
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// startGuestSSHD runs a minimal sshd for vmName that accepts only clientKey
// and answers every exec with "<vm> <user>: <command>".
func startGuestSSHD(t *testing.T, vmName string, hostKey ssh.Signer, clientKey ssh.PublicKey) string {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(k.Marshal(), clientKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveGuestSSH(c, cfg, vmName)
		}
	}()
	return ln.Addr().String()
}

func serveGuestSSH(c net.Conn, cfg *ssh.ServerConfig, vmName string) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for r := range creqs {
				if r.Type != "exec" {
					r.Reply(false, nil)
					continue
				}
				var p struct{ Command string }
				ssh.Unmarshal(r.Payload, &p)
				r.Reply(true, nil)
				fmt.Fprintf(ch, "%s %s: %s", vmName, sc.User(), p.Command)
				fmt.Fprint(ch.Stderr(), "guest-stderr")
				ch.CloseWrite()
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{7}))
				return
			}
		}()
	}
}

func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// sshRun dials the gateway from p as login and runs cmd, returning stdout,
// stderr and the exit status.
func sshRun(ctx context.Context, p gatewaytest.Peer, addr, login, cmd string, verify ssh.HostKeyCallback, key ssh.Signer) (string, string, int, error) {
	c, err := p.Srv.Dial(ctx, "tcp", addr)
	if err != nil {
		return "", "", 0, err
	}
	c.SetDeadline(time.Now().Add(20 * time.Second))
	conn, chans, reqs, err := ssh.NewClientConn(c, addr, &ssh.ClientConfig{
		User:            login,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: verify,
	})
	if err != nil {
		c.Close()
		return "", "", 0, err
	}
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return "", "", 0, err
	}
	defer sess.Close()
	var out, errOut bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errOut
	err = sess.Run(cmd)
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return out.String(), errOut.String(), ee.ExitStatus(), nil
	}
	return out.String(), errOut.String(), 0, err
}

func TestTailnetGateway(t *testing.T) {
	if testing.Short() {
		t.Skip("starts several in-process tailnet nodes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	controlURL := gatewaytest.StartControl(t)

	gwUserKey := newSigner(t) // the gateway's credential at guest sshd
	guestHostKey := newSigner(t)
	guests := &fakeGuests{
		ssh: map[domain.VMID]string{
			"vm-1": startGuestSSHD(t, "devbox", guestHostKey, gwUserKey.PublicKey()),
			"vm-2": startGuestSSHD(t, "otherbox", guestHostKey, gwUserKey.PublicKey()),
		},
		echo: startEcho(t),
	}
	auth := &fakeAuth{
		grants: map[int64]map[string]bool{},
		vms:    map[string]Target{"devbox": {VMID: "vm-1", Generation: 3}, "otherbox": {VMID: "vm-2", Generation: 1}},
	}

	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFromContext(r.Context())
		if !ok {
			http.Error(w, "no identity", http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(id)
	})
	stateDir := filepath.Join(t.TempDir(), "gw")
	gw, err := New(Config{
		StateDir:   stateDir,
		Hostname:   "davit-host",
		ControlURL: controlURL,
		Ephemeral:  true,
		Store:      new(mem.Store),
		Logf:       gatewaytest.Logf(t),
	}, Options{
		Auth:   auth,
		Dialer: guests,
		Guest:  fakeGuestSSH{key: gwUserKey, hostKey: guestHostKey.PublicKey()},
		API:    api,
		Logf:   t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gw.Close() })
	gatewaytest.WaitDERP(t, ctx, gw.node.srv)
	gwIP := gw.Node().Addrs()[0]
	t.Logf("gateway %s at %v", gw.Node().FQDN(), gwIP)

	alice := gatewaytest.StartPeer(t, ctx, controlURL, "alice-laptop")
	mallory := gatewaytest.StartPeer(t, ctx, controlURL, "mallory-laptop")
	if alice.UserID == mallory.UserID {
		t.Fatal("testcontrol gave both peers the same user; the test needs distinct principals")
	}
	auth.set(alice.UserID, "devbox", true)

	gwHostKey, err := LoadOrCreateHostKey(filepath.Join(stateDir, "ssh_host_ed25519_key"))
	if err != nil {
		t.Fatal(err)
	}
	trustGateway := ssh.FixedHostKey(gwHostKey.PublicKey())
	sshAddr := netip.AddrPortFrom(gwIP, 22).String()
	clientKey := newSigner(t) // arbitrary: identity comes from the tailnet

	t.Run("HTTPIdentityFromWhoIs", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+gwIP.String()+"/whoami", nil)
		// Forged identity headers must not matter.
		req.Header.Set("X-Forwarded-For", "100.64.0.99")
		req.Header.Set("Tailscale-User-Login", "admin@example.com")
		res, err := alice.Srv.HTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var id Identity
		if err := json.NewDecoder(res.Body).Decode(&id); err != nil {
			t.Fatal(err)
		}
		if id.UserID != alice.UserID || !strings.HasPrefix(id.NodeName, "alice-laptop") || id.NodeID == "" || id.LoginName == "admin@example.com" {
			t.Fatalf("handler saw identity %+v, want alice (user %d)", id, alice.UserID)
		}
	})

	t.Run("AuthorizedSSHReachesGuest", func(t *testing.T) {
		out, errOut, code, err := sshRun(ctx, alice, sshAddr, "devbox", "uname -a", trustGateway, clientKey)
		if err != nil {
			t.Fatal(err)
		}
		if out != "devbox user: uname -a" || errOut != "guest-stderr" || code != 7 {
			t.Fatalf("got stdout=%q stderr=%q exit=%d", out, errOut, code)
		}
		out, _, _, err = sshRun(ctx, alice, sshAddr, "devbox+root", "id", trustGateway, clientKey)
		if err != nil || out != "devbox root: id" {
			t.Fatalf("guest user selection: out=%q err=%v", out, err)
		}
	})

	t.Run("UnauthorizedSSHDenied", func(t *testing.T) {
		for _, tc := range []struct {
			who   gatewaytest.Peer
			login string
		}{
			{mallory, "devbox"},    // no grant
			{alice, "otherbox"},    // exists, not granted
			{alice, "nonexist"},    // absent: same answer as denied
			{alice, "devbox;rm"},   // malformed routing parameter
			{mallory, "vm-1+root"}, // raw ID gives nothing extra
		} {
			if _, _, _, err := sshRun(ctx, tc.who, sshAddr, tc.login, "true", trustGateway, clientKey); err == nil ||
				!strings.Contains(err.Error(), "unable to authenticate") {
				t.Errorf("%d as %q: err=%v, want authentication failure", tc.who.UserID, tc.login, err)
			}
		}
	})

	t.Run("RevokedGrantDenied", func(t *testing.T) {
		auth.set(alice.UserID, "devbox", false)
		defer auth.set(alice.UserID, "devbox", true)
		if _, _, _, err := sshRun(ctx, alice, sshAddr, "devbox", "true", trustGateway, clientKey); err == nil {
			t.Fatal("revoked grant still reached the guest")
		}
	})

	t.Run("PortForward", func(t *testing.T) {
		ln, err := gw.Forward(":8022", "devbox", 8080)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		fwd := netip.AddrPortFrom(gwIP, 8022).String()
		echo := func(p gatewaytest.Peer) (string, error) {
			c, err := p.Srv.Dial(ctx, "tcp", fwd)
			if err != nil {
				return "", err
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(15 * time.Second))
			io.WriteString(c, "ping")
			c.(interface{ CloseWrite() error }).CloseWrite()
			b, err := io.ReadAll(c)
			return string(b), err
		}
		if got, err := echo(alice); got != "ping" {
			t.Fatalf("alice forward: got %q err=%v", got, err)
		}
		if got, _ := echo(mallory); got != "" {
			t.Fatalf("mallory forward leaked %q", got)
		}
	})

	t.Run("PerVMEndpointNode", func(t *testing.T) {
		eps, err := NewEndpoints(EndpointsConfig{
			StateDir:   filepath.Join(t.TempDir(), "endpoints"),
			ControlURL: controlURL,
			Ephemeral:  true,
			NewStore:   func(domain.VMID) ipn.StateStore { return new(mem.Store) },
			Logf:       gatewaytest.Logf(t),
		}, gw.Streams())
		if err != nil {
			t.Fatal(err)
		}
		defer eps.Close()
		if _, err := eps.Ensure("vm-1", "devbox"); err != nil {
			t.Fatal(err)
		}
		var info EndpointInfo
		for {
			info, _ = eps.Info("vm-1")
			if info.State != EndpointPending {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("endpoint never enrolled")
			case <-time.After(50 * time.Millisecond):
			}
		}
		if info.State != EndpointReady || len(info.Addrs) == 0 || !strings.HasPrefix(info.FQDN, "devbox") {
			t.Fatalf("endpoint info %+v", info)
		}
		addr := netip.AddrPortFrom(info.Addrs[0], 22).String()
		// End to end: the client verifies the *guest* host key and logs in
		// with the guest credential; the endpoint only moves bytes.
		trustGuest := ssh.FixedHostKey(guestHostKey.PublicKey())
		var out string
		for i := 0; ; i++ { // the new node's first packets may race DERP setup
			out, _, _, err = sshRun(ctx, alice, addr, "user", "hostname", trustGuest, gwUserKey)
			if err == nil || i == 20 {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err != nil || out != "devbox user: hostname" {
			t.Fatalf("endpoint ssh: out=%q err=%v", out, err)
		}
		if _, _, _, err := sshRun(ctx, mallory, addr, "user", "true", trustGuest, gwUserKey); err == nil {
			t.Fatal("mallory reached the guest through its endpoint node")
		}
		if err := eps.Delete(ctx, "vm-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(eps.cfg.StateDir, "vm-1")); !os.IsNotExist(err) {
			t.Fatalf("endpoint identity not erased: %v", err)
		}
	})
}
