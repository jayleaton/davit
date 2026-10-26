package guestssh

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/gateway"
	"davit/internal/runtime"
)

// MaxCredentialTTL bounds a certificate issued to an API client; the
// default is CertLifetime.
const MaxCredentialTTL = 10 * time.Minute

// GuestTCP dials a guest-loopback port as principal and reports the instance
// reached (guestexec.Manager).
type GuestTCP interface {
	DialTCP(ctx context.Context, principal domain.PrincipalID, ref string, port int) (net.Conn, runtime.Key, error)
}

// Issuer implements issueSSHCredential on top of an Authority.
type Issuer struct {
	CA    *Authority
	Guest GuestTCP
}

// Issue signs req.PublicKey for ref's current generation and the guest user,
// and returns the guest host keys to pin: read from the guest sshd and kept
// only when their fingerprints are registered for exactly that generation.
func (s *Issuer) Issue(ctx context.Context, principal domain.PrincipalID, ref string, req api.IssueSSHCredentialRequest) (api.SSHCredential, error) {
	invalid := func(field, format string, args ...any) error {
		return domain.Errorf(domain.CodeInvalidArgument, format, args...).WithDetail("field", field)
	}
	if err := checkUser(cmp.Or(req.GuestUser, gateway.DefaultGuestUser)); err != nil {
		return api.SSHCredential{}, invalid("guestUser", "guestUser %q is not a valid login name", req.GuestUser)
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if req.TTLSeconds < 0 || ttl > MaxCredentialTTL {
		return api.SSHCredential{}, invalid("ttlSeconds", "ttlSeconds must be 1..%d", int(MaxCredentialTTL/time.Second))
	}
	if ttl == 0 {
		ttl = CertLifetime
	}
	pub, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(req.PublicKey))
	if err != nil || strings.TrimSpace(string(rest)) != "" {
		return api.SSHCredential{}, invalid("publicKey", "publicKey must be one OpenSSH public key (authorized_keys format)")
	}
	if _, isCert := pub.(*ssh.Certificate); isCert {
		return api.SSHCredential{}, invalid("publicKey", "publicKey must be a plain key, not a certificate")
	}

	conn, key, err := s.Guest.DialTCP(ctx, principal, ref, 22)
	if err != nil {
		return api.SSHCredential{}, err
	}
	// No user: the VM image's login user, as for the front door.
	user := s.CA.resolveUser(key.VMID, cmp.Or(req.GuestUser, gateway.DefaultGuestUser))
	fps, err := s.CA.hostKeys(key.VMID, key.Generation)
	if err != nil {
		conn.Close()
		return api.SSHCredential{}, domain.Errorf(domain.CodeInvalidState,
			"vm %s has no SSH host keys registered for generation %d; stop and resume it", key.VMID, key.Generation)
	}
	hostKey, err := scanHostKey(conn, s.CA.now().Add(dialTimeout))
	if err != nil {
		return api.SSHCredential{}, domain.Errorf(domain.CodeUnavailable, "guest sshd on vm %s did not answer: %v", key.VMID, err)
	}
	if err := pinHostKey(gateway.Target{VMID: key.VMID, Generation: key.Generation}, fps)("", nil, hostKey); err != nil {
		return api.SSHCredential{}, domain.Errorf(domain.CodeInternal, "%v", err)
	}

	ca, err := s.CA.signer(key.VMID, key.Generation)
	if err != nil {
		return api.SSHCredential{}, err
	}
	var serial [8]byte
	if _, err := rand.Read(serial[:]); err != nil {
		return api.SSHCredential{}, err
	}
	now := s.CA.now()
	cert := &ssh.Certificate{
		Key:             pub,
		Serial:          binary.BigEndian.Uint64(serial[:]),
		CertType:        ssh.UserCert,
		KeyId:           fmt.Sprintf("davit vm=%s gen=%d user=%s principal=%q via=api", key.VMID, key.Generation, user, principal),
		ValidPrincipals: []string{user},
		ValidAfter:      uint64(now.Add(-certBackdate).Unix()),
		ValidBefore:     uint64(now.Add(ttl).Unix()),
		Permissions:     ssh.Permissions{Extensions: maps.Clone(certExtensions)},
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		return api.SSHCredential{}, fmt.Errorf("guestssh: sign user certificate: %w", err)
	}
	return api.SSHCredential{
		Certificate: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert))),
		GuestUser:   user,
		Generation:  key.Generation,
		HostKeys:    []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))},
		SSHTarget:   user + "@" + buildinfo.Name + "-" + string(key.VMID),
		ExpiresAt:   time.Unix(int64(cert.ValidBefore), 0).UTC(),
	}, nil
}

// errScanned aborts a handshake once the host key has been seen.
var errScanned = errors.New("guestssh: host key captured")

// scanHostKey reads the ed25519 host key the SSH server on c presents (the
// one the agent manages and reports), aborting the handshake right after it.
// c is closed.
func scanHostKey(c net.Conn, deadline time.Time) (ssh.PublicKey, error) {
	defer c.Close()
	_ = c.SetDeadline(deadline)
	var key ssh.PublicKey
	_, _, _, err := ssh.NewClientConn(c, "guest", &ssh.ClientConfig{
		User:              "davit-keyscan",
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errScanned
		},
	})
	if key != nil {
		return key, nil
	}
	if err == nil {
		err = errors.New("server presented no host key")
	}
	return nil, err
}
