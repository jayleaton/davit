//go:build linux

package main_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/wsock"
)

// call makes an API request as the client's tailnet identity.
func (c *appClient) call(method, path string, body any) (int, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+net.JoinHostPort(c.gwIP, "443")+api.BasePath+path, rd)
	req.Host = c.fqdn
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(api.HeaderIdempotencyKey, "k-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	}
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b
}

// apiWebSocket opens an API WebSocket (e.g. a tunnel stream) as the client.
func (c *appClient) apiWebSocket(path string) (*wsock.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	raw, err := c.p.Srv.Dial(ctx, "tcp", net.JoinHostPort(c.gwIP, "443"))
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, &tls.Config{RootCAs: c.roots, ServerName: c.fqdn, NextProtos: []string{"http/1.1"}})
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	ws, _, err := wsock.Client(ctx, tc, c.fqdn, api.BasePath+path, nil)
	if err != nil {
		tc.Close()
	}
	return ws, err
}

// TestMembersOverTailnet: a second tailnet user is refused until it is a
// member, then sees and does only what is shared with it, by role; never the
// VM's credential grants, never a fork or a new VM; a shared snapshot is
// readable without its VM; unsharing cuts its live tunnel stream at once.
func TestMembersOverTailnet(t *testing.T) {
	if testing.Short() {
		t.Skip("starts in-process tailnet nodes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a := startAppHost(t, ctx)
	h := a.h
	h.waited(h.cli("new", "box"), domain.OpCreate)
	h.waited(h.cli("new", "other"), domain.OpCreate)
	h.waited(h.cli("stop", "other"), domain.OpStop)
	if r := h.cli("snapshot", "create", "other", "golden"); r.code != 0 {
		t.Fatalf("snapshot: %s", r.raw)
	}

	bob := newAppClient(t, a.mallory, a.gwIP, a.fqdn, a.caFile)
	var code int

	eventually(t, "tailnet API", func() bool {
		req, _ := http.NewRequest("GET", "https://"+net.JoinHostPort(a.gwIP, "443")+api.BasePath+"/vms", nil)
		req.Host = a.fqdn
		res, err := bob.http.Do(req)
		if err == nil {
			res.Body.Close()
			code = res.StatusCode
		}
		return err == nil
	})
	if code != http.StatusForbidden {
		t.Fatalf("non-member list: HTTP %d, want 403", code)
	}

	// A member sees nothing until something is shared (default private).
	r := h.cli("members", "add", a.mallory.LoginName, "--role", "viewer")
	if r.code != 0 {
		t.Fatalf("members add: %s", r.raw)
	}
	listNames := func() []string {
		t.Helper()
		code, body := bob.call("GET", "/vms", nil)
		var l api.VMList
		json.Unmarshal(body, &l)
		if code != 200 {
			t.Fatalf("member list: %d %s", code, body)
		}
		var names []string
		for _, vm := range l.Items {
			names = append(names, vm.Name)
		}
		return names
	}
	if names := listNames(); len(names) != 0 {
		t.Fatalf("member sees %v before any share", names)
	}
	if r := h.cli("members"); r.code != 0 || !strings.Contains(string(r.raw[0]), `"userId"`) {
		t.Fatalf("member not pinned to its user ID after connecting: %s", r.raw)
	}

	// viewer: reads only.
	if r := h.cli("share", "box", a.mallory.LoginName, "--role", "viewer"); r.code != 0 {
		t.Fatalf("share viewer: %s", r.raw)
	}
	if names := listNames(); len(names) != 1 || names[0] != "box" {
		t.Fatalf("viewer list %v, want [box]", names)
	}
	expect := func(method, path string, body any, want int) []byte {
		t.Helper()
		code, b := bob.call(method, path, body)
		if code != want {
			t.Fatalf("%s %s: HTTP %d %s, want %d", method, path, code, b, want)
		}
		return b
	}
	expect("GET", "/vms/box", nil, 200)
	expect("GET", "/vms/other", nil, 404) // not shared: invisible, not just forbidden
	expect("POST", "/vms/box/processes", map[string]any{"argv": []string{"true"}}, 403)
	expect("POST", "/vms/box/stop", map[string]any{}, 403)
	expect("GET", "/vms/box/credential-grants", nil, 403)

	// exec: runs code, but still no credential grants and no admin.
	if r := h.cli("share", "box", a.mallory.LoginName, "--role", "exec"); r.code != 0 {
		t.Fatalf("share exec: %s", r.raw)
	}
	expect("POST", "/vms/box/processes", map[string]any{"argv": []string{"true"}}, 201)
	expect("GET", "/vms/box/credential-grants", nil, 403)
	ttl := map[string]any{"ttlSeconds": 7200}
	expect("PATCH", "/vms/box", ttl, 403)
	// The privacy policy is readable at every role and changes only with
	// admin, like every VM setting.
	privacy := map[string]any{"privacy": map[string]any{"noOutputRetention": true}}
	expect("PATCH", "/vms/box", privacy, 403)

	// admin: manages the VM, but forks and new VMs stay the owner's, and so
	// do tokens, members and shares.
	if r := h.cli("share", "box", a.mallory.LoginName, "--role", "admin"); r.code != 0 {
		t.Fatalf("share admin: %s", r.raw)
	}
	expect("PATCH", "/vms/box", ttl, 200)
	var vm api.VM
	json.Unmarshal(expect("PATCH", "/vms/box", privacy, 200), &vm)
	if !vm.Privacy.NoOutputRetention {
		t.Fatalf("admin privacy update: %+v", vm.Privacy)
	}
	expect("POST", "/vms/box/fork", map[string]any{"name": "mine"}, 403)
	expect("POST", "/vms", map[string]any{"name": "mine"}, 403)
	expect("GET", "/vms/box/credential-grants", nil, 403)
	expect("GET", "/tokens", nil, 403)
	expect("GET", "/members", nil, 403)
	// Events and webhooks are host-wide: owner only for members.
	expect("GET", "/events", nil, 403)
	expect("GET", "/webhooks", nil, 403)
	expect("POST", "/webhooks", map[string]any{"url": "https://example.com/hook"}, 403)
	expect("POST", "/shares", map[string]any{"kind": "vm", "resource": "other", "member": "*", "role": "admin"}, 403)

	// A shared snapshot: readable without its VM.
	expect("GET", "/snapshots/golden", nil, 404)
	if r := h.cli("share", "--snapshot", "golden", a.mallory.LoginName, "--role", "viewer"); r.code != 0 {
		t.Fatalf("share snapshot: %s", r.raw)
	}
	var snap api.Snapshot
	json.Unmarshal(expect("GET", "/snapshots/golden", nil, 200), &snap)
	expect("GET", "/vms/other", nil, 404)
	var snaps api.SnapshotList
	json.Unmarshal(expect("GET", "/snapshots", nil, 200), &snaps)
	if len(snaps.Items) != 1 || snaps.Items[0].ID != snap.ID {
		t.Fatalf("member snapshot list: %+v", snaps.Items)
	}

	// Unsharing cuts a live stream: an exec tunnel to a guest port.
	echo := echoServer(t)
	var tn api.Tunnel
	json.Unmarshal(expect("POST", "/vms/box/tunnels", map[string]any{"port": echo}, 201), &tn)
	ws, err := bob.apiWebSocket("/tunnels/" + tn.ID + "/stream")
	if err != nil {
		t.Fatalf("tunnel stream: %v", err)
	}
	ws.WriteMessage(wsock.BinaryMessage, []byte("ping"))
	if _, msg, err := ws.ReadMessage(); err != nil || string(msg) != "ping" {
		t.Fatalf("tunnel echo %q %v", msg, err)
	}
	r = h.cli("unshare", "box", a.mallory.LoginName)
	if r.code != 0 {
		t.Fatalf("unshare: %s", r.raw)
	}
	ws.NetConn().SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("unshare left the member's tunnel stream open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("unshare did not cut the member's tunnel stream")
	}
	expect("GET", "/vms/box", nil, 404)
	if names := listNames(); len(names) != 0 {
		t.Fatalf("after unshare the member sees %v", names)
	}

	// Removing the member: refused again, its snapshot share gone too.
	if r := h.cli("members", "remove", a.mallory.LoginName); r.code != 0 {
		t.Fatalf("members remove: %s", r.raw)
	}
	expect("GET", "/snapshots/golden", nil, 403)
	if r := h.cli("shares"); r.code != 0 || !strings.Contains(string(r.raw[len(r.raw)-1]), `"count":0`) {
		t.Fatalf("shares after removing the member: %s", r.raw)
	}
}
