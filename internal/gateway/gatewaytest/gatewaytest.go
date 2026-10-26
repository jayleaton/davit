// Package gatewaytest runs an in-process tailnet for tests: Tailscale's
// testcontrol coordination server, an in-process DERP/STUN relay and tsnet
// peers, all on loopback. Nothing joins a real tailnet or touches the host's
// tailscaled. Import it from tests only.
package gatewaytest

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

// Logf discards tsnet's very chatty logs unless DAVIT_TEST_VERBOSE is set.
func Logf(t testing.TB) logger.Logf {
	if os.Getenv("DAVIT_TEST_VERBOSE") != "" {
		return t.Logf
	}
	return logger.Discard
}

// StartControl starts a coordination server and returns its URL. testcontrol
// gives every node its own user unless configured otherwise.
func StartControl(t testing.TB) string {
	t.Helper()
	return StartControlServer(t).HTTPTestServer.URL
}

// StartControlServer is StartControl returning the server itself, so a test
// can change the tailnet (e.g. ExpireNode).
func StartControlServer(t testing.TB) *testcontrol.Server {
	t.Helper()
	// As in tsnet's own tests: no SO_MARK/bind-to-interface on loopback.
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	derpMap := integration.RunDERPAndSTUN(t, Logf(t), "127.0.0.1")
	control := &testcontrol.Server{
		DERPMap:        derpMap,
		DNSConfig:      &tailcfg.DNSConfig{Proxied: true},
		MagicDNSDomain: "tail-scale.ts.net",
		Logf:           Logf(t),
	}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)
	return control
}

// ExpireNode expires p's node key on the control server, as an admin
// revoking the device does: the rest of the tailnet drops it as a peer.
func ExpireNode(t testing.TB, ctx context.Context, control *testcontrol.Server, p Peer) {
	t.Helper()
	lc, err := p.Srv.LocalClient()
	if err != nil {
		t.Fatal(err)
	}
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil || st.Self == nil {
		t.Fatalf("ExpireNode: status: %v", err)
	}
	n := control.Node(st.Self.PublicKey)
	if n == nil {
		t.Fatalf("ExpireNode: control has no node %v", st.Self.PublicKey)
	}
	n.KeyExpiry = time.Now().Add(-time.Minute)
	n.Expired = true
	control.UpdateNode(n)
}

// WaitDERP blocks until s has a home DERP connection; before that, peers'
// first packets can be dropped (mirrors tsnet's waitForHomeDERPConnected).
func WaitDERP(t testing.TB, ctx context.Context, s *tsnet.Server) {
	t.Helper()
	h := s.Sys().HealthTracker.Get()
	ms := s.Sys().MagicSock.Get()
	for {
		if r := ms.GetLastNetcheckReport(ctx); r != nil && r.PreferredDERP != 0 &&
			!h.GetDERPRegionReceivedTime(r.PreferredDERP).IsZero() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("WaitDERP: %v", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Peer is a client node on the test tailnet.
type Peer struct {
	Srv       *tsnet.Server
	UserID    int64
	LoginName string
}

// StartPeer brings up an ephemeral client node named hostname.
func StartPeer(t testing.TB, ctx context.Context, controlURL, hostname string) Peer {
	t.Helper()
	s := &tsnet.Server{
		Dir:        filepath.Join(t.TempDir(), hostname),
		ControlURL: controlURL,
		Hostname:   hostname,
		Store:      new(mem.Store),
		Ephemeral:  true,
		Logf:       Logf(t),
	}
	t.Cleanup(func() { s.Close() })
	st, err := s.Up(ctx)
	if err != nil {
		t.Fatal(err)
	}
	WaitDERP(t, ctx, s)
	p := Peer{Srv: s, UserID: int64(st.Self.UserID)}
	if u, ok := st.User[st.Self.UserID]; ok {
		p.LoginName = u.LoginName
	}
	return p
}

// PeerAddr waits until p sees a peer whose hostname is host and returns its
// first tailnet address.
func PeerAddr(t testing.TB, ctx context.Context, p Peer, host string) netip.Addr {
	t.Helper()
	lc, err := p.Srv.LocalClient()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if st, err := lc.Status(ctx); err == nil {
			for _, ps := range st.Peer {
				if ps.HostName == host && len(ps.TailscaleIPs) > 0 {
					return ps.TailscaleIPs[0]
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("peer %q never appeared: %v", host, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
