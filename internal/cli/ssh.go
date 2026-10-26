package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/crypto/ssh"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/wsock"
)

// sshHostPrefix names VMs as SSH hosts: davit-<vm>.
var sshHostPrefix = buildinfo.Name + "-"

// sshOpts are the flags of ssh, scp and forward.
type sshOpts struct {
	user          string
	proxy         string
	prepare       string
	recursive     bool
	preserve      bool
	local, remote int
	bind          string
	reverse       bool
	// forward --reverse (reverse.go)
	hostService   bool
	reverseList   bool
	reverseRevoke string
}

func sshFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.ssh.user, "user", "", "guest user (default: the VM image's login user)")
	fs.StringVar(&o.ssh.proxy, "proxy", "", "stdio mode for OpenSSH ProxyCommand: relay the VM's sshd on stdin/stdout")
	fs.StringVar(&o.ssh.prepare, "prepare", "", "write a fresh certificate and pinned host keys for SSH host HOST (ssh-config's Match exec)")
}

func scpFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.ssh.user, "user", "", "guest user (default: the VM image's login user)")
	fs.BoolVar(&o.ssh.recursive, "r", false, "copy directories recursively")
	fs.BoolVar(&o.ssh.recursive, "recursive", false, "copy directories recursively")
	fs.BoolVar(&o.ssh.preserve, "preserve", false, "preserve modes and times")
}

func forwardFlags(fs *flag.FlagSet, o *opts) {
	fs.IntVar(&o.ssh.local, "local", -1, "local port (default: the remote port; 0 picks a free one)")
	fs.IntVar(&o.ssh.remote, "remote", 0, "guest port")
	fs.StringVar(&o.ssh.bind, "bind", "127.0.0.1", "local address to listen on")
	fs.BoolVar(&o.ssh.reverse, "reverse", false, "guest -> this machine: [ID] GUESTPORT:HOST:PORT makes HOST:PORT (reachable from here) reachable at the guest's 127.0.0.1:GUESTPORT until Ctrl-C")
	fs.BoolVar(&o.ssh.hostService, "host-service", false, "with --reverse: target an operator-listed host service (network.reverseHostServices) instead; persists until revoked or the VM stops")
	fs.BoolVar(&o.ssh.reverseList, "list", false, "with --reverse: list the VM's reverse forwards")
	fs.StringVar(&o.ssh.reverseRevoke, "revoke", "", "with --reverse: revoke reverse forward `FORWARD_ID`")
}

// tunnelStream is one attached tunnel as a byte stream: binary WebSocket
// messages carry the bytes, an empty one is a half-close.
type tunnelStream struct {
	ws  *wsock.Conn
	buf []byte
	wmu sync.Mutex
}

func (t *tunnelStream) Read(p []byte) (int, error) {
	for len(t.buf) == 0 {
		typ, data, err := t.ws.ReadMessage()
		if err != nil {
			if errors.Is(err, wsock.ErrClosed) {
				return 0, io.EOF
			}
			return 0, err
		}
		if typ == wsock.BinaryMessage {
			t.buf = data
		}
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

func (t *tunnelStream) Write(p []byte) (int, error) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	for off := 0; off < len(p); off += tunnelChunk {
		end := min(off+tunnelChunk, len(p))
		if err := t.ws.WriteMessage(wsock.BinaryMessage, p[off:end]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// CloseWrite sends EOF to the guest port.
func (t *tunnelStream) CloseWrite() error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	return t.ws.WriteMessage(wsock.BinaryMessage, nil)
}

func (t *tunnelStream) Close() error { return t.ws.Close(wsock.CloseNormal, "") }

// tunnelChunk is the largest binary message the client sends.
const tunnelChunk = 32 << 10

// openTunnel connects to port on vm's guest loopback (openTunnel, then
// attachTunnel).
func (s *session) openTunnel(ctx context.Context, vm string, port int) (*tunnelStream, api.Tunnel, error) {
	var t api.Tunnel
	if err := s.cl.do(ctx, http.MethodPost, vmPath(vm)+"/tunnels", nil, api.OpenTunnelRequest{Port: port}, nil, &t); err != nil {
		return nil, t, err
	}
	ws, err := s.cl.websocket(ctx, api.BasePath+"/tunnels/"+url.PathEscape(t.ID)+"/stream")
	if err != nil {
		return nil, t, err
	}
	return &tunnelStream{ws: ws}, t, nil
}

func (s *session) getVM(ctx context.Context, ref string) (api.VM, error) {
	var vm api.VM
	err := s.cl.get(ctx, vmPath(ref), nil, &vm)
	if err == nil {
		autoStopNotice(s.out, &vm)
	}
	return vm, err
}

// sshDir holds the CLI's SSH client key and, per SSH host name, its current
// certificate and pinned host keys.
func sshDir() (string, error) {
	dir := filepath.Join(config.DefaultConfigDir(), "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// sshFiles are what OpenSSH needs to reach one SSH host name.
type sshFiles struct {
	key, cert, knownHosts string
}

func filesFor(dir, host string) sshFiles {
	return sshFiles{key: filepath.Join(dir, "id_ed25519"), cert: filepath.Join(dir, host+"-cert.pub"),
		knownHosts: filepath.Join(dir, host+".known_hosts")}
}

// clientKey loads the CLI's ed25519 SSH key, creating it (0600) on first
// use. It never leaves this machine; the daemon only certifies its public
// half.
func clientKey(path string) (ssh.PublicKey, error) {
	if b, err := os.ReadFile(path); err == nil {
		s, err := ssh.ParsePrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return s.PublicKey(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(priv, buildinfo.Name+" client key")
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		return nil, err
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return nil, err
	}
	return pub, writeFileAtomic(path+".pub", ssh.MarshalAuthorizedKey(pub), 0o644)
}

// prepareSSH asks the daemon for a certificate for vm and user, and writes
// it plus the guest host keys (as the restore barrier reported them) for
// SSH host name host. OpenSSH then pins exactly those keys: no trust on
// first use, and a key the barrier did not report is refused.
//
// An empty user asks for the VM image's login user; the user the certificate
// was issued for is returned.
func (s *session) prepareSSH(ctx context.Context, vm, user, host string) (sshFiles, string, error) {
	dir, err := sshDir()
	if err != nil {
		return sshFiles{}, "", err
	}
	f := filesFor(dir, host)
	pub, err := clientKey(f.key)
	if err != nil {
		return sshFiles{}, "", err
	}
	var cred api.SSHCredential
	req := api.IssueSSHCredentialRequest{PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), GuestUser: user}
	if err := s.cl.do(ctx, http.MethodPost, vmPath(vm)+"/ssh-credentials", nil, req, nil, &cred); err != nil {
		return sshFiles{}, "", err
	}
	if len(cred.HostKeys) == 0 {
		return sshFiles{}, "", clientErr(domain.CodeInternal, "the daemon returned no guest host keys to pin")
	}
	var kh strings.Builder
	for _, k := range cred.HostKeys {
		fmt.Fprintf(&kh, "%s %s\n", host, strings.TrimSpace(k))
	}
	if err := writeFileAtomic(f.knownHosts, []byte(kh.String()), 0o600); err != nil {
		return sshFiles{}, "", err
	}
	return f, cred.GuestUser, writeFileAtomic(f.cert, []byte(cred.Certificate+"\n"), 0o600)
}

// writeFileAtomic replaces path with b (mode) via a temp file and rename.
func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	err = tmp.Chmod(mode)
	if err == nil {
		_, err = tmp.Write(b)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

var shellSafeRE = regexp.MustCompile(`^[A-Za-z0-9_./:@=+-]+$`)

// shellQuote quotes s for /bin/sh, which runs OpenSSH's ProxyCommand; '%'
// is doubled because ssh expands %-tokens in it.
func shellQuote(s string) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if shellSafeRE.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// proxyCommand is the ProxyCommand that relays target through this CLI and
// the same API endpoint (target may contain ssh %-tokens such as %n).
func (s *session) proxyCommand(target string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	cmd := shellQuote(exe)
	if s.g.apiURLSet {
		cmd += " --api-url " + shellQuote(s.g.apiURL)
	}
	return cmd + " ssh --proxy " + target, nil
}

// openSSHOptions prepares a certificate and pinned host keys for vm and
// returns the OpenSSH arguments that reach it as SSH host davit-<vm-id>.
func (s *session) openSSHOptions(ctx context.Context, vmID domain.VMID, user string) ([]string, string, error) {
	host := sshHostPrefix + string(vmID)
	f, user, err := s.prepareSSH(ctx, string(vmID), user, host)
	if err != nil {
		return nil, "", err
	}
	pc, err := s.proxyCommand(shellQuote(string(vmID)))
	if err != nil {
		return nil, "", err
	}
	return []string{"-o", "ProxyCommand=" + pc,
		"-o", "UserKnownHostsFile=" + f.knownHosts, "-o", "GlobalKnownHostsFile=/dev/null", "-o", "StrictHostKeyChecking=yes",
		"-o", "IdentityFile=" + f.key, "-o", "CertificateFile=" + f.cert, "-o", "IdentitiesOnly=yes",
		"-o", "User=" + user}, host, nil
}

// runOpenSSH runs an OpenSSH client with this terminal and returns its exit
// status as an *exitStatus.
func (s *session) runOpenSSH(ctx context.Context, tool string, args []string) error {
	bin, err := exec.LookPath(tool)
	if err != nil {
		return clientErr(domain.CodeUnsupportedCapability, "%s needs the OpenSSH client (%s not found in PATH)", buildinfo.Name, tool)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.io.Stdin, s.io.Stdout, s.io.Stderr
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee):
		return &exitStatus{code: ee.ExitCode()}
	}
	return err
}

func runSSH(ctx context.Context, s *session, o *opts, args []string) error {
	if o.ssh.proxy != "" {
		if len(args) > 0 || len(o.exec.argv) > 0 {
			return usageErr("ssh --proxy VM takes no other arguments")
		}
		return s.sshProxy(ctx, strings.TrimPrefix(o.ssh.proxy, sshHostPrefix))
	}
	if o.ssh.prepare != "" {
		// OpenSSH runs this from ssh-config's Match exec; stdout must stay
		// empty and failure (exit != 0) makes the Match not apply.
		if len(args) > 0 {
			return usageErr("ssh --prepare HOST takes no other arguments")
		}
		_, _, err := s.prepareSSH(ctx, strings.TrimPrefix(o.ssh.prepare, sshHostPrefix), o.ssh.user, o.ssh.prepare)
		return err
	}
	if s.g.json {
		return usageErr("ssh is interactive and has no --json mode; use `%s exec --stream --json`", buildinfo.Name)
	}
	if err := maxArgs(args, 1, "ssh [ID] [-- COMMAND...]"); err != nil {
		return err
	}
	ref, err := s.currentID(args)
	if err != nil {
		return err
	}
	// Local socket or remote (tailnet) API alike: the API tunnel to the
	// guest sshd, a certificate for this VM generation and the pinned host
	// key, authenticated like every other request (tailnet identity or
	// DAVIT_TOKEN), so CI with a token can ssh too.
	vm, err := s.getVM(ctx, ref)
	if err != nil {
		return err
	}
	sshArgs, host, err := s.openSSHOptions(ctx, vm.ID, o.ssh.user)
	if err != nil {
		return err
	}
	sshArgs = append(sshArgs, host)
	if len(o.exec.argv) > 0 {
		sshArgs = append(append(sshArgs, "--"), o.exec.argv...)
	}
	return s.runOpenSSH(ctx, "ssh", sshArgs)
}

// sshProxy relays the VM's sshd (guest port 22) on stdin/stdout for
// OpenSSH's ProxyCommand. It never writes anything else to stdout.
func (s *session) sshProxy(ctx context.Context, ref string) error {
	t, _, err := s.openTunnel(ctx, ref, 22)
	if err != nil {
		return err
	}
	defer t.Close()
	go func() {
		_, _ = io.Copy(t, s.io.Stdin)
		_ = t.CloseWrite()
	}()
	_, err = io.Copy(s.io.Stdout, t)
	return err
}

func runSSHConfig(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 0, "ssh-config"); err != nil {
		return err
	}
	pc, err := s.proxyCommand("%n")
	if err != nil {
		return err
	}
	prep := strings.TrimSuffix(pc, " ssh --proxy %n") + " ssh --prepare %n --user %r"
	dir, err := sshDir()
	if err != nil {
		return err
	}
	f := filesFor(sshConfigPath(dir), "%n")
	userLine := s.sshConfigUser(ctx)
	block := fmt.Sprintf(`# %[1]s VMs as SSH hosts: ssh %[2]s<vm>, scp/rsync/sftp too (%[1]s ssh-config >> ~/.ssh/config).
# Each connection first fetches a 5-minute certificate for the VM and the
# guest host keys its restore barrier reported; those keys are pinned.
Host %[2]s*
    %[3]s
Match host %[2]s* exec "%[4]s"
    ProxyCommand %[5]s
    IdentityFile %[6]s
    CertificateFile %[7]s
    UserKnownHostsFile %[8]s
    GlobalKnownHostsFile /dev/null
    StrictHostKeyChecking yes
    IdentitiesOnly yes
`, buildinfo.Name, sshHostPrefix, userLine, prep, pc, sshConfigQuote(f.key), sshConfigQuote(f.cert), sshConfigQuote(f.knownHosts))
	s.out.result("", map[string]string{"config": block})
	if !s.g.json {
		fmt.Fprint(s.io.Stdout, block)
	}
	return nil
}

// sshConfigPath escapes '%' in a literal path used where ssh_config expands
// %-tokens.
func sshConfigPath(p string) string { return strings.ReplaceAll(p, "%", "%%") }

// sshConfigQuote double-quotes an ssh_config argument containing spaces.
func sshConfigQuote(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// remotePathRE matches the VM side of an scp argument: VM:path.
var remotePathRE = regexp.MustCompile(`^([a-z0-9][a-z0-9_-]*):(.*)$`)

func runSCP(ctx context.Context, s *session, o *opts, args []string) error {
	if s.g.json {
		return usageErr("scp has no --json mode; use `%s cp`", buildinfo.Name)
	}
	if len(args) != 2 {
		return usageErr("usage: %s scp [-r] [--preserve] SRC DST (one side VM:/path)", buildinfo.Name)
	}
	side := -1
	for i, a := range args {
		if m := remotePathRE.FindStringSubmatch(a); m != nil && !strings.HasPrefix(a, "/") && !strings.HasPrefix(a, ".") {
			if side >= 0 {
				return usageErr("scp: exactly one side may be VM:/path")
			}
			side = i
		}
	}
	if side < 0 {
		return usageErr("scp: one side must be VM:/path")
	}
	m := remotePathRE.FindStringSubmatch(args[side])
	ref, path := m[1], m[2]
	vm, err := s.getVM(ctx, ref)
	if err != nil {
		return err
	}
	scpArgs, host, err := s.openSSHOptions(ctx, vm.ID, o.ssh.user)
	if err != nil {
		return err
	}
	if o.ssh.recursive {
		scpArgs = append(scpArgs, "-r")
	}
	if o.ssh.preserve {
		scpArgs = append(scpArgs, "-p")
	}
	files := slices.Clone(args)
	files[side] = host + ":" + path
	return s.runOpenSSH(ctx, "scp", append(append(scpArgs, "--"), files...))
}

// forwardSpecRE matches [LOCAL:]REMOTE.
var forwardSpecRE = regexp.MustCompile(`^(?:(\d{1,5}):)?(\d{1,5})$`)

func runForward(ctx context.Context, s *session, o *opts, args []string) error {
	f := &o.ssh
	if f.reverse {
		return runReverse(ctx, s, o, args)
	}
	if f.hostService || f.reverseList || f.reverseRevoke != "" {
		return usageErr("--host-service, --list and --revoke go with --reverse")
	}
	if len(args) > 0 {
		if m := forwardSpecRE.FindStringSubmatch(args[len(args)-1]); m != nil {
			if f.remote != 0 || f.local >= 0 {
				return usageErr("give the ports either as [LOCAL:]REMOTE or with --local/--remote, not both")
			}
			f.remote, _ = strconv.Atoi(m[2])
			if m[1] != "" {
				f.local, _ = strconv.Atoi(m[1])
			}
			args = args[:len(args)-1]
		}
	}
	if err := maxArgs(args, 1, "forward [ID] [LOCAL:]REMOTE"); err != nil {
		return err
	}
	if f.local < 0 {
		f.local = f.remote
	}
	if f.remote < 1 || f.remote > 65535 || f.local > 65535 {
		return usageErr("forward needs a guest port 1..65535: %s forward [ID] [LOCAL:]REMOTE", buildinfo.Name)
	}
	ref, err := s.currentID(args)
	if err != nil {
		return err
	}
	vm, err := s.getVM(ctx, ref)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(f.bind, strconv.Itoa(f.local)))
	if err != nil {
		return clientErr(domain.CodeUnavailable, "listen on %s:%d: %v", f.bind, f.local, err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); ln.Close() }()

	type listening struct {
		VMID   domain.VMID `json:"vmId"`
		Local  string      `json:"local"`
		Remote int         `json:"remote"`
	}
	addr := ln.Addr().String()
	var seq atomic.Int64
	s.out.emit("forward.listening", "", seq.Add(1), listening{vm.ID, addr, f.remote}, nil)
	s.out.human("forwarding %s -> %s:%d (Ctrl-C to stop)", addr, vm.Name, f.remote)

	type connEvent struct {
		From     string `json:"from"`
		BytesIn  int64  `json:"bytesIn,omitempty"`  // guest -> local client
		BytesOut int64  `json:"bytesOut,omitempty"` // local client -> guest
	}
	var wg sync.WaitGroup
	var conns atomic.Int64
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}
		conns.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			from := c.RemoteAddr().String()
			t, _, err := s.openTunnel(ctx, string(vm.ID), f.remote)
			if err != nil {
				e := asErrorBody(err)
				s.out.emit("forward.error", "", seq.Add(1), connEvent{From: from}, &e)
				s.out.diag("forward %s: %s", from, e.Message)
				return
			}
			defer t.Close()
			s.out.emit("forward.connection", "", seq.Add(1), connEvent{From: from}, nil)
			in, out := splice(ctx, c, t)
			s.out.emit("forward.closed", "", seq.Add(1), connEvent{From: from, BytesIn: in, BytesOut: out}, nil)
		}()
	}
	wg.Wait()
	s.out.result("", map[string]any{"vmId": vm.ID, "local": addr, "remote": f.remote, "connections": conns.Load()})
	return nil
}

// splice copies client <-> guest and returns the bytes moved each way. A
// client EOF is passed on as a half-close; when the guest side ends the
// server has closed the stream, so both connections are closed.
func splice(ctx context.Context, client net.Conn, guest *tunnelStream) (in, out int64) {
	done := make(chan struct{})
	go func() {
		out, _ = io.Copy(guest, client)
		_ = guest.CloseWrite()
		close(done)
	}()
	stop := context.AfterFunc(ctx, func() { client.Close(); guest.Close() })
	defer stop()
	in, _ = io.Copy(client, guest)
	client.Close()
	guest.Close()
	<-done
	return in, out
}

// asErrorBody converts err to the JSON error body.
func asErrorBody(err error) ErrorBody {
	var ce *cliError
	if errors.As(err, &ce) {
		return ce.ErrorBody
	}
	return ErrorBody{Code: domain.CodeUnavailable, Message: err.Error(), Origin: "client"}
}

// sshConfigUser is the ssh-config User line: the login user of the daemon's
// images (OpenSSH needs a static user; a certificate is issued for it).
func (s *session) sshConfigUser(ctx context.Context) string {
	var l api.ImageList
	if err := s.cl.get(ctx, "/images", nil, &l); err != nil {
		return "# User <guest user>   (the daemon was unreachable: set your image's login user)"
	}
	var users []string
	for _, img := range l.Items {
		if img.DefaultUser != "" && !slices.Contains(users, img.DefaultUser) {
			users = append(users, img.DefaultUser)
		}
	}
	slices.Sort(users)
	switch len(users) {
	case 0:
		return "# User <guest user>   (the images declare no login user: set it here)"
	case 1:
		return "User " + users[0]
	}
	return fmt.Sprintf("User %s   # images use different login users (%s): ssh <user>@%s<vm> for the others",
		users[0], strings.Join(users, ", "), sshHostPrefix)
}
