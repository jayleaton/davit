package guestssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"davit/internal/domain"
	"davit/internal/gateway"
	"davit/internal/guestproto"
	"davit/internal/runtime"
)

var who = gateway.Identity{UserID: 7, LoginName: "alice@example.com", NodeID: "n123"}

func newHostKey(t *testing.T) ssh.Signer {
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

func record(t *testing.T, a *Authority, vm domain.VMID, gen int64, keys ...ssh.PublicKey) {
	t.Helper()
	id := guestproto.IdentityResult{VMID: string(vm), Generation: uint64(gen)}
	for _, k := range keys {
		id.SSHHostKeyFingerprints = append(id.SSHHostKeyFingerprints, ssh.FingerprintSHA256(k))
	}
	if err := a.RecordIdentity(context.Background(), runtime.Key{VMID: vm, Generation: gen}, id); err != nil {
		t.Fatal(err)
	}
}

// login runs a guest sshd stand-in that trusts a's CA, connects with
// ClientConfig and runs one exec request. It returns the principal and key ID
// the server authenticated, and the command output.
func login(t *testing.T, a *Authority, host ssh.Signer, tgt gateway.Target, user string) (srvUser, keyID string, err error) {
	t.Helper()
	cfg, err := a.ClientConfig(context.Background(), who, tgt, user)
	if err != nil {
		return "", "", err
	}
	cas, err := a.CAPublicKeys(runtime.Key{VMID: tgt.VMID, Generation: tgt.Generation})
	if err != nil {
		t.Fatal(err)
	}
	caPub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cas[0]))
	if err != nil {
		t.Fatal(err)
	}
	checker := &ssh.CertChecker{IsUserAuthority: func(k ssh.PublicKey) bool {
		return bytes.Equal(k.Marshal(), caPub.Marshal())
	}}
	srvCfg := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		p, err := checker.Authenticate(c, k)
		if err == nil {
			if _, ok := p.Extensions["permit-pty"]; !ok {
				return nil, errors.New("no permit-pty")
			}
			srvUser, keyID = c.User(), k.(*ssh.Certificate).KeyId
		}
		return p, err
	}}
	srvCfg.AddHostKey(host)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		sc, err := ln.Accept()
		if err != nil {
			return
		}
		defer sc.Close()
		_, chans, reqs, err := ssh.NewServerConn(sc, srvCfg)
		if err != nil {
			return
		}
		go ssh.DiscardRequests(reqs)
		for nc := range chans {
			ch, creqs, err := nc.Accept()
			if err != nil {
				return
			}
			for r := range creqs {
				r.Reply(r.Type == "exec", nil)
				if r.Type == "exec" {
					ch.Write([]byte("hello\n"))
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{0}))
					ch.Close()
				}
			}
		}
	}()
	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	conn, chans, reqs, err := ssh.NewClientConn(cc, string(tgt.VMID), cfg)
	if err != nil {
		return "", "", err
	}
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("true")
	if err != nil || string(out) != "hello\n" {
		t.Fatalf("exec: %q, %v", out, err)
	}
	return srvUser, keyID, nil
}

func TestClientConfigLogsIn(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := newHostKey(t)
	record(t, a, "vm-a", 3, newHostKey(t).PublicKey(), host.PublicKey())

	u, kid, err := login(t, a, host, gateway.Target{VMID: "vm-a", Generation: 3}, "root")
	if err != nil {
		t.Fatal(err)
	}
	if u != "root" || !strings.Contains(kid, "vm=vm-a gen=3 user=root") || !strings.Contains(kid, `login="alice@example.com"`) || !strings.Contains(kid, `node="n123"`) {
		t.Fatalf("user %q key id %q", u, kid)
	}

	// The default user maps to the image's account.
	a.SetDefaultUser(func(vm domain.VMID) string { return map[domain.VMID]string{"vm-a": "ubuntu"}[vm] })
	if u, _, err = login(t, a, host, gateway.Target{VMID: "vm-a", Generation: 3}, gateway.DefaultGuestUser); err != nil || u != "ubuntu" {
		t.Fatalf("mapped default user: %q, %v", u, err)
	}
	if u, _, err = login(t, a, host, gateway.Target{VMID: "vm-a", Generation: 3}, "dev"); err != nil || u != "dev" {
		t.Fatalf("explicit user: %q, %v", u, err)
	}
}

func TestHostKeyVerification(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := newHostKey(t)
	record(t, a, "vm-a", 2, host.PublicKey())

	if _, err := a.ClientConfig(context.Background(), who, gateway.Target{VMID: "vm-a", Generation: 1}, "user"); !errors.Is(err, ErrUnknownHostKey) {
		t.Fatalf("other generation: %v", err)
	}
	if _, err := a.ClientConfig(context.Background(), who, gateway.Target{VMID: "vm-b", Generation: 2}, "user"); !errors.Is(err, ErrUnknownHostKey) {
		t.Fatalf("unrecorded vm: %v", err)
	}
	if _, _, err := login(t, a, newHostKey(t), gateway.Target{VMID: "vm-a", Generation: 2}, "user"); !errors.Is(err, ErrUnknownHostKey) {
		t.Fatalf("mismatched host key: %v", err)
	}
	// A newer generation supersedes the old record.
	record(t, a, "vm-a", 3, host.PublicKey())
	if _, err := a.ClientConfig(context.Background(), who, gateway.Target{VMID: "vm-a", Generation: 2}, "user"); !errors.Is(err, ErrUnknownHostKey) {
		t.Fatalf("superseded generation: %v", err)
	}
}

func TestExpiredCertRejected(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := newHostKey(t)
	record(t, a, "vm-a", 1, host.PublicKey())
	a.now = func() time.Time { return time.Now().Add(-CertLifetime - time.Minute) }
	if _, _, err := login(t, a, host, gateway.Target{VMID: "vm-a", Generation: 1}, "user"); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("expired cert: %v", err)
	}
}

func TestRecordIdentityRejectsBadReports(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := runtime.Key{VMID: "vm-a", Generation: 1}
	fp := ssh.FingerprintSHA256(newHostKey(t).PublicKey())
	for name, id := range map[string]guestproto.IdentityResult{
		"empty":     {VMID: "vm-a", Generation: 1},
		"malformed": {VMID: "vm-a", Generation: 1, SSHHostKeyFingerprints: []string{"MD5:aa"}},
		"wrong vm":  {VMID: "vm-b", Generation: 1, SSHHostKeyFingerprints: []string{fp}},
		"wrong gen": {VMID: "vm-a", Generation: 2, SSHHostKeyFingerprints: []string{fp}},
	} {
		if err := a.RecordIdentity(context.Background(), key, id); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := a.RecordIdentity(context.Background(), runtime.Key{VMID: "../x", Generation: 1},
		guestproto.IdentityResult{VMID: "../x", Generation: 1, SSHHostKeyFingerprints: []string{fp}}); err == nil {
		t.Error("path-like vm id accepted")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, caKeyFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("ca key: %v, %v", fi, err)
	}
	host := newHostKey(t)
	record(t, a, "vm-a", 1, host.PublicKey())
	record(t, a, "vm-b", 1, host.PublicKey())

	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	k := runtime.Key{VMID: "vm-a", Generation: 1}
	ka, _ := a.CAPublicKeys(k)
	kb, _ := b.CAPublicKeys(k)
	if len(ka) != 1 || ka[0] != kb[0] || !strings.HasPrefix(kb[0], "ssh-ed25519 ") {
		t.Fatalf("CA changed across Open: %q vs %q", ka, kb)
	}
	if _, _, err := login(t, b, host, gateway.Target{VMID: "vm-a", Generation: 1}, "user"); err != nil {
		t.Fatalf("record lost across reopen: %v", err)
	}

	if err := b.Forget("vm-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ClientConfig(context.Background(), who, gateway.Target{VMID: "vm-a", Generation: 1}, "user"); !errors.Is(err, ErrUnknownHostKey) {
		t.Fatalf("after Forget: %v", err)
	}
	if _, err := b.ClientConfig(context.Background(), who, gateway.Target{VMID: "vm-b", Generation: 1}, "user"); err != nil {
		t.Fatalf("Forget removed another vm: %v", err)
	}

	// A group-readable CA key is refused.
	if err := os.Chmod(filepath.Join(dir, caKeyFile), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("Open accepted a group-readable CA key")
	}
}
