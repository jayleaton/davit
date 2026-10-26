// Package guestssh is the host side of guest SSH (docs/network.md "SSH: two
// paths"): a persistent user CA, a durable registry of the host-key
// fingerprints each guest reports from its restore barrier, and the
// gateway.GuestSSH implementation and issueSSHCredential that combine both.
//
// Certificates are scoped to one VM generation by key, not just by label:
// each (VM, generation) signs with its own ed25519 key derived from the
// persistent CA key, and each boot's barrier installs that generation's
// public key as the guest sshd's TrustedUserCAKeys. A certificate therefore
// logs in only to the VM generation it was issued for, which matters once a
// certificate leaves the daemon (issueSSHCredential). The shared SSH front
// door logs in to a guest with a fresh, minutes-long user certificate and
// accepts only the host key registered for exactly that VM and generation.
package guestssh

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"davit/internal/domain"
	"davit/internal/gateway"
	"davit/internal/guestproto"
	"davit/internal/runtime"
)

const (
	// CertLifetime is how long a guest user certificate is valid. Each
	// front-door connection gets its own certificate and uses it once, for
	// the handshake that follows immediately, so minutes are plenty.
	CertLifetime = 5 * time.Minute
	// certBackdate tolerates a guest clock slightly behind the host's.
	certBackdate = 60 * time.Second
	// dialTimeout bounds the guest SSH handshake.
	dialTimeout = 15 * time.Second

	caKeyFile   = "user_ca_ed25519"
	hostKeysDir = "hostkeys"
)

// certExtensions are the permissions granted to the front door's guest
// sessions. The gateway relays channels and requests unchanged, so agent and
// X11 forwarding work only when the end client asks for them (ssh -A / -X).
// Agent and X11 forwarding are left out: the guest is untrusted, and
// handing it the user's agent should be an explicit later decision.
// permit-user-rc is deliberately left out.
var certExtensions = map[string]string{
	"permit-pty":             "",
	"permit-port-forwarding": "",
}

var (
	// ErrUnknownHostKey: no host key is registered for the VM generation,
	// or the guest presented a key that is not registered.
	ErrUnknownHostKey = errors.New("guestssh: guest host key not registered")

	vmIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	fpRE   = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

var _ gateway.GuestSSH = (*Authority)(nil)

// Authority issues short-lived guest SSH user certificates and verifies guest
// host keys. It is safe for concurrent use.
type Authority struct {
	dir string
	// root is the persistent CA key; signing keys are derived from it per
	// VM generation (signer).
	root ed25519.PrivateKey
	// now is the clock certificates are dated with; tests override it.
	now func() time.Time

	mu          sync.RWMutex
	defaultUser func(domain.VMID) string
}

// hostRecord is one VM generation's registered host keys.
type hostRecord struct {
	VMID         domain.VMID `json:"vmId"`
	Generation   int64       `json:"generation"`
	Fingerprints []string    `json:"fingerprints"`
	RecordedAt   time.Time   `json:"recordedAt"`
}

// Open loads the user CA key under dir, creating it (0700 dir, 0600 key) if
// absent, and opens the host-key registry under dir/hostkeys.
func Open(dir string) (*Authority, error) {
	if dir == "" {
		return nil, errors.New("guestssh: empty state dir")
	}
	if err := os.MkdirAll(filepath.Join(dir, hostKeysDir), 0o700); err != nil {
		return nil, fmt.Errorf("guestssh: %w", err)
	}
	root, err := loadOrCreateCA(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, fmt.Errorf("guestssh: user CA: %w", err)
	}
	return &Authority{dir: dir, root: root, now: time.Now}, nil
}

func loadOrCreateCA(path string) (ed25519.PrivateKey, error) {
	s, err := loadCA(path)
	if !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "davit guest user CA")
	if err != nil {
		return nil, err
	}
	tmp, err := writeTemp(filepath.Dir(path), pem.EncodeToMemory(pemBlock), 0o600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp)
	// Link, not rename: a concurrent Open that won the race keeps its key.
	if err := os.Link(tmp, path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return loadCA(path)
}

func loadCA(path string) (ed25519.PrivateKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is accessible by group or others (mode %v)", path, fi.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := ssh.ParseRawPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	priv, ok := k.(*ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an ed25519 key", path)
	}
	return *priv, nil
}

// signer is the CA key of one VM generation: ed25519 from
// HMAC-SHA256(root seed, vm, generation). Deterministic, so it needs no
// storage and survives daemon restarts.
func (a *Authority) signer(vm domain.VMID, gen int64) (ssh.Signer, error) {
	if err := checkVMID(vm); err != nil {
		return nil, err
	}
	if gen < 1 {
		return nil, fmt.Errorf("guestssh: invalid generation %d", gen)
	}
	m := hmac.New(sha256.New, a.root.Seed())
	m.Write([]byte("davit guest user ca v1\x00" + string(vm) + "\x00" + strconv.FormatInt(gen, 10)))
	return ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(m.Sum(nil)))
}

// CAPublicKeys returns the authorized_keys-format CA public keys the guest
// sshd of exactly this VM generation must trust
// (guestclient.Plan.SSHUserCAKeys).
func (a *Authority) CAPublicKeys(key runtime.Key) ([]string, error) {
	s, err := a.signer(key.VMID, key.Generation)
	if err != nil {
		return nil, err
	}
	return []string{fmt.Sprintf("%s davit-user-ca:%s:%d", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))),
		key.VMID, key.Generation)}, nil
}

// SSHHostKeyFingerprints implements lifecycle.GuestIdentity: the
// fingerprints registered for exactly this VM generation, or none.
func (a *Authority) SSHHostKeyFingerprints(key runtime.Key) []string {
	fps, err := a.hostKeys(key.VMID, key.Generation)
	if err != nil {
		return nil
	}
	return fps
}

// SetDefaultUser installs the mapping from a VM to its image's default guest
// account. When the front door asks for gateway.DefaultGuestUser and f
// returns a non-empty name, that name is the certificate principal and login
// user instead. A nil f removes the mapping.
func (a *Authority) SetDefaultUser(f func(vm domain.VMID) string) {
	a.mu.Lock()
	a.defaultUser = f
	a.mu.Unlock()
}

// RecordIdentity is the guestclient.Prober.OnIdentity hook: it durably
// registers the host-key fingerprints the guest reported for (key.VMID,
// key.Generation) and drops records of older generations of that VM.
func (a *Authority) RecordIdentity(ctx context.Context, key runtime.Key, id guestproto.IdentityResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkVMID(key.VMID); err != nil {
		return err
	}
	if key.Generation < 1 {
		return fmt.Errorf("guestssh: invalid generation %d", key.Generation)
	}
	if id.VMID != string(key.VMID) || id.Generation != uint64(key.Generation) {
		return fmt.Errorf("guestssh: identity report is for %s/%d, not %s/%d", id.VMID, id.Generation, key.VMID, key.Generation)
	}
	if len(id.SSHHostKeyFingerprints) == 0 {
		return fmt.Errorf("guestssh: %s/%d reported no ssh host keys", key.VMID, key.Generation)
	}
	fps := slices.Clone(id.SSHHostKeyFingerprints)
	for _, fp := range fps {
		if !fpRE.MatchString(fp) {
			return fmt.Errorf("guestssh: %s/%d reported malformed host key fingerprint %q", key.VMID, key.Generation, fp)
		}
	}
	slices.Sort(fps)
	fps = slices.Compact(fps)
	raw, err := json.Marshal(hostRecord{VMID: key.VMID, Generation: key.Generation, Fingerprints: fps, RecordedAt: a.now().UTC()})
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	vmDir := a.vmDir(key.VMID)
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		return fmt.Errorf("guestssh: %w", err)
	}
	if err := writeAtomic(filepath.Join(vmDir, genFile(key.Generation)), raw, 0o600); err != nil {
		return fmt.Errorf("guestssh: record host keys: %w", err)
	}
	// Older generations can never be dialed again; best-effort cleanup.
	ents, _ := os.ReadDir(vmDir)
	for _, e := range ents {
		if g, ok := parseGenFile(e.Name()); ok && g < key.Generation {
			_ = os.Remove(filepath.Join(vmDir, e.Name()))
		}
	}
	return nil
}

// Forget drops the host-key records of every generation of vmID. Call it
// when the VM is deleted.
func (a *Authority) Forget(vmID domain.VMID) error {
	if err := checkVMID(vmID); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.RemoveAll(a.vmDir(vmID)); err != nil {
		return fmt.Errorf("guestssh: %w", err)
	}
	return syncDir(filepath.Join(a.dir, hostKeysDir))
}

// hostKeys returns the registered fingerprints for exactly (vm, gen).
func (a *Authority) hostKeys(vm domain.VMID, gen int64) ([]string, error) {
	if err := checkVMID(vm); err != nil {
		return nil, err
	}
	a.mu.RLock()
	raw, err := os.ReadFile(filepath.Join(a.vmDir(vm), genFile(gen)))
	a.mu.RUnlock()
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w for %s generation %d", ErrUnknownHostKey, vm, gen)
	}
	if err != nil {
		return nil, fmt.Errorf("guestssh: %w", err)
	}
	var rec hostRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("guestssh: host key record for %s/%d: %w", vm, gen, err)
	}
	if rec.VMID != vm || rec.Generation != gen || len(rec.Fingerprints) == 0 {
		return nil, fmt.Errorf("%w for %s generation %d (corrupt record)", ErrUnknownHostKey, vm, gen)
	}
	return rec.Fingerprints, nil
}

// ClientConfig implements gateway.GuestSSH. Every call mints a fresh
// ephemeral key and a user certificate for guestUser (or the VM's mapped
// default account) valid for CertLifetime, and pins the guest host key to the
// fingerprints registered for exactly t.VMID and t.Generation.
func (a *Authority) ClientConfig(ctx context.Context, who gateway.Identity, t gateway.Target, guestUser string) (*ssh.ClientConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fps, err := a.hostKeys(t.VMID, t.Generation)
	if err != nil {
		return nil, err
	}
	user := a.resolveUser(t.VMID, guestUser)
	if err := checkUser(user); err != nil {
		return nil, err
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	var serial [8]byte
	if _, err := rand.Read(serial[:]); err != nil {
		return nil, err
	}
	now := a.now()
	cert := &ssh.Certificate{
		Key:             signer.PublicKey(),
		Serial:          binary.BigEndian.Uint64(serial[:]),
		CertType:        ssh.UserCert,
		KeyId:           keyID(who, t, user),
		ValidPrincipals: []string{user},
		ValidAfter:      uint64(now.Add(-certBackdate).Unix()),
		ValidBefore:     uint64(now.Add(CertLifetime).Unix()),
		Permissions:     ssh.Permissions{Extensions: maps.Clone(certExtensions)},
	}
	ca, err := a.signer(t.VMID, t.Generation)
	if err != nil {
		return nil, err
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		return nil, fmt.Errorf("guestssh: sign user certificate: %w", err)
	}
	certSigner, err := ssh.NewCertSigner(cert, signer)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(certSigner)},
		HostKeyCallback: pinHostKey(t, fps),
		// The agent manages (and reports) exactly one ed25519 host key;
		// images may serve others it does not rotate, so never negotiate them.
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           dialTimeout,
	}, nil
}

func (a *Authority) resolveUser(vm domain.VMID, guestUser string) string {
	if guestUser != gateway.DefaultGuestUser {
		return guestUser
	}
	a.mu.RLock()
	f := a.defaultUser
	a.mu.RUnlock()
	if f != nil {
		if u := f(vm); u != "" {
			return u
		}
	}
	return guestUser
}

// pinHostKey accepts only a host key whose SHA256 fingerprint is registered
// for the target generation. A host certificate is judged by its key.
func pinHostKey(t gateway.Target, fps []string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if c, ok := key.(*ssh.Certificate); ok {
			key = c.Key
		}
		fp := ssh.FingerprintSHA256(key)
		if slices.Contains(fps, fp) {
			return nil
		}
		return fmt.Errorf("%w: %s generation %d presented %s %s", ErrUnknownHostKey, t.VMID, t.Generation, key.Type(), fp)
	}
}

// keyID is the certificate key ID guest sshd logs on login: the VM, its
// generation, the guest user and the tailnet identity behind the session.
func keyID(who gateway.Identity, t gateway.Target, user string) string {
	return fmt.Sprintf("davit vm=%s gen=%d user=%s login=%s uid=%d node=%s",
		t.VMID, t.Generation, user, strconv.Quote(who.LoginName), who.UserID, strconv.Quote(who.NodeID))
}

func checkVMID(vm domain.VMID) error {
	if !vmIDRE.MatchString(string(vm)) || vm == "." || vm == ".." {
		return fmt.Errorf("guestssh: invalid vm id %q", vm)
	}
	return nil
}

func checkUser(u string) error {
	if u == "" || len(u) > 64 || strings.ContainsFunc(u, func(r rune) bool {
		return r <= ' ' || r == ',' || r == 0x7f || r > 0x7e
	}) {
		return fmt.Errorf("guestssh: invalid guest user %q", u)
	}
	return nil
}

func (a *Authority) vmDir(vm domain.VMID) string {
	return filepath.Join(a.dir, hostKeysDir, string(vm))
}

func genFile(gen int64) string { return strconv.FormatInt(gen, 10) + ".json" }

func parseGenFile(name string) (int64, bool) {
	s, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return 0, false
	}
	g, err := strconv.ParseInt(s, 10, 64)
	return g, err == nil
}
