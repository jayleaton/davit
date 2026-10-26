package guestssh

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime"
)

// sshdGuest answers DialTCP(22) with an SSH server presenting host.
type sshdGuest struct {
	host ssh.Signer
	key  runtime.Key
}

func (g sshdGuest) DialTCP(context.Context, domain.PrincipalID, string, int) (net.Conn, runtime.Key, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, runtime.Key{}, err
	}
	go func() {
		defer ln.Close()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		cfg := &ssh.ServerConfig{NoClientAuth: true}
		cfg.AddHostKey(g.host)
		ssh.NewServerConn(c, cfg)
		c.Close()
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	return c, g.key, err
}

// trusts reports whether a guest sshd trusting exactly caLine for login user
// would accept cert.
func trusts(t *testing.T, caLine, user string, cert *ssh.Certificate) bool {
	t.Helper()
	ca, _, _, _, err := ssh.ParseAuthorizedKey([]byte(caLine))
	if err != nil {
		t.Fatal(err)
	}
	checker := &ssh.CertChecker{IsUserAuthority: func(k ssh.PublicKey) bool { return string(k.Marshal()) == string(ca.Marshal()) }}
	_, err = checker.Authenticate(meta{user}, cert)
	return err == nil
}

type meta struct{ user string }

func (m meta) User() string        { return m.user }
func (meta) SessionID() []byte     { return nil }
func (meta) ClientVersion() []byte { return nil }
func (meta) ServerVersion() []byte { return nil }
func (meta) RemoteAddr() net.Addr  { return nil }
func (meta) LocalAddr() net.Addr   { return nil }

func TestIssuedCertificateIsScopedToItsVMGeneration(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := newHostKey(t)
	key := runtime.Key{VMID: "vm-a", Generation: 2}
	record(t, a, key.VMID, key.Generation, host.PublicKey())
	iss := &Issuer{CA: a, Guest: sshdGuest{host: host, key: key}}
	client := newHostKey(t)
	req := api.IssueSSHCredentialRequest{PublicKey: string(ssh.MarshalAuthorizedKey(client.PublicKey()))}

	cred, err := iss.Issue(context.Background(), "local:1000", "vm-a", req)
	if err != nil {
		t.Fatal(err)
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cred.Certificate))
	if err != nil {
		t.Fatal(err)
	}
	cert := pk.(*ssh.Certificate)
	hk, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(cred.HostKeys[0]))
	if cred.GuestUser != "user" || cred.Generation != 2 || ssh.FingerprintSHA256(hk) != ssh.FingerprintSHA256(host.PublicKey()) ||
		time.Until(cred.ExpiresAt) > CertLifetime+time.Second {
		t.Fatalf("credential %+v", cred)
	}
	if _, ok := cert.Permissions.Extensions["permit-agent-forwarding"]; ok {
		t.Fatal("issued certificates must not permit agent forwarding")
	}

	own, _ := a.CAPublicKeys(key)
	otherVM, _ := a.CAPublicKeys(runtime.Key{VMID: "vm-b", Generation: 2})
	nextGen, _ := a.CAPublicKeys(runtime.Key{VMID: "vm-a", Generation: 3})
	if !trusts(t, own[0], "user", cert) {
		t.Fatal("own VM generation rejects its certificate")
	}
	if trusts(t, otherVM[0], "user", cert) || trusts(t, nextGen[0], "user", cert) || trusts(t, own[0], "root", cert) {
		t.Fatal("certificate accepted outside its VM, generation or user")
	}

	// A host key the barrier did not report is never handed out to pin.
	iss.Guest = sshdGuest{host: newHostKey(t), key: key}
	if _, err := iss.Issue(context.Background(), "local:1000", "vm-a", req); err == nil {
		t.Fatal("issued with an unregistered guest host key")
	}
	// No registry entry for the generation at all.
	iss.Guest = sshdGuest{host: host, key: runtime.Key{VMID: "vm-a", Generation: 9}}
	if _, err := iss.Issue(context.Background(), "local:1000", "vm-a", req); !errors.Is(err, &domain.Error{Code: domain.CodeInvalidState}) {
		t.Fatalf("unregistered generation: %v", err)
	}
	iss.Guest = sshdGuest{host: host, key: key}
	for name, r := range map[string]api.IssueSSHCredentialRequest{
		"garbage key": {PublicKey: "not a key"},
		"bad user":    {PublicKey: req.PublicKey, GuestUser: "a b"},
		"long ttl":    {PublicKey: req.PublicKey, TTLSeconds: 601},
		"certificate": {PublicKey: strings.TrimSpace(cred.Certificate)},
	} {
		if _, err := iss.Issue(context.Background(), "local:1000", "vm-a", r); !errors.Is(err, &domain.Error{Code: domain.CodeInvalidArgument}) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
