package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"davit/internal/buildinfo"
	"davit/internal/config"
	"davit/internal/domain"
)

// The ssh:// transport reaches a host's daemon socket over OpenSSH, for a
// host without a tailnet: --host ssh://[USER@]HOST[:PORT][/SOCKET][?jump=J&key=K]
// (or DAVIT_HOST, or client.json from `davit connect`). Every API
// connection is one ssh session running `davit dial-stdio` on the host,
// which splices it to the daemon's unix socket; the daemon sees a local
// owner connection (the ssh user's), so the remote side needs no listener,
// token or tailnet. A shared OpenSSH control master makes every session
// after the first one cheap, and ProxyCommand (ssh into a VM) goes through
// the same path. jump is an OpenSSH ProxyJump ([USER@]HOST[:PORT], for a
// host behind a jump box) and key an identity file tried besides the
// user's own ssh setup.
//
// DAVIT_SSH names the ssh client (default "ssh") and DAVIT_REMOTE_BIN
// the davit binary on the host (default "davit" on its PATH).

var (
	sshClientEnv = buildinfo.Env("SSH")
	remoteBinEnv = buildinfo.Env("REMOTE_BIN")
)

// sshTarget is a parsed ssh:// host URL.
type sshTarget struct {
	user, host, port, socket string
	jump, key                string
}

func parseSSHTarget(u *url.URL) (sshTarget, error) {
	const form = "use ssh://[USER@]HOST[:PORT][/SOCKET][?jump=[USER@]HOST[:PORT]&key=FILE]"
	if u.Hostname() == "" {
		return sshTarget{}, usageErr("ssh:// URL %q has no host", u.String())
	}
	if u.Fragment != "" {
		return sshTarget{}, usageErr("ssh:// URL %q: %s", u.String(), form)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return sshTarget{}, usageErr("ssh:// URL %q: %s", u.String(), form)
	}
	t := sshTarget{host: u.Hostname(), port: u.Port(), jump: q.Get("jump"), key: q.Get("key")}
	for k := range q {
		if k != "jump" && k != "key" {
			return sshTarget{}, usageErr("ssh:// URL %q: unknown option %q; %s", u.String(), k, form)
		}
	}
	if u.User != nil {
		t.user = u.User.Username()
	}
	if u.Path != "/" {
		t.socket = u.Path
	}
	if strings.HasPrefix(t.jump, "-") || strings.HasPrefix(t.host, "-") || strings.HasPrefix(t.user, "-") {
		return sshTarget{}, usageErr("ssh:// URL %q: names cannot start with '-'", u.String())
	}
	return t, nil
}

// String is t as an ssh:// URL.
func (t sshTarget) String() string {
	u := url.URL{Scheme: "ssh", Host: t.host, Path: t.socket}
	if t.port != "" {
		u.Host = net.JoinHostPort(t.host, t.port)
	}
	if t.user != "" {
		u.User = url.User(t.user)
	}
	// Paths and USER@HOST stay readable: only what would end or split the
	// value is escaped.
	esc := strings.NewReplacer("%", "%25", "&", "%26", "=", "%3D", "#", "%23", "+", "%2B", " ", "%20")
	var q []string
	if t.jump != "" {
		q = append(q, "jump="+esc.Replace(t.jump))
	}
	if t.key != "" {
		q = append(q, "key="+esc.Replace(t.key))
	}
	u.RawQuery = strings.Join(q, "&")
	return u.String()
}

// login is [USER@]HOST, as people type it to ssh.
func (t sshTarget) login() string {
	if t.user != "" {
		return t.user + "@" + t.host
	}
	return t.host
}

// args are the ssh options every session to t uses (before the host name).
func (t sshTarget) args() []string {
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3"}
	if t.port != "" {
		args = append(args, "-p", t.port)
	}
	if t.user != "" {
		args = append(args, "-l", t.user)
	}
	if t.jump != "" {
		args = append(args, "-J", t.jump)
	}
	if t.key != "" {
		args = append(args, "-i", t.key)
	}
	return args
}

// remoteCommand runs davit with args on the host. A non-interactive ssh
// command gets the system PATH only; the install locations
// (scripts/host-install.sh: ~/.local/bin, or a --bin-dir such as
// ~/.svm/davit/bin) are searched first.
func remoteCommand(args ...string) string {
	remote := os.Getenv(remoteBinEnv)
	if remote == "" {
		remote = buildinfo.Name
	}
	cmd := `PATH="$HOME/.local/bin:$HOME/.svm/davit/bin:$PATH" exec ` + sshRemoteQuote(remote)
	for _, a := range args {
		cmd += " " + sshRemoteQuote(a)
	}
	return cmd
}

func sshBinary() string {
	if bin := os.Getenv(sshClientEnv); bin != "" {
		return bin
	}
	return "ssh"
}

// sshDialer returns the dial function for an ssh:// API URL.
func sshDialer(u *url.URL) (func(ctx context.Context) (net.Conn, error), error) {
	t, err := parseSSHTarget(u)
	if err != nil {
		return nil, err
	}
	args := t.args()
	if dir, err := controlDir(); err == nil {
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+filepath.Join(dir, "%C"), "-o", "ControlPersist=120")
	}
	remote := []string{"dial-stdio"}
	if t.socket != "" {
		remote = append(remote, "--socket", t.socket)
	}
	args = append(args, t.host, "--", remoteCommand(remote...))
	bin := sshBinary()
	return func(ctx context.Context) (net.Conn, error) { return dialSSH(ctx, bin, args, t.login()) }, nil
}

// controlDir is the directory for OpenSSH control sockets (kept short:
// unix socket paths are limited to ~104 bytes).
func controlDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, buildinfo.Name, "ssh")
	if len(dir) > 60 {
		return "", errors.New("cache dir too long for control sockets")
	}
	return dir, os.MkdirAll(dir, 0o700)
}

// sshRemoteQuote quotes s for a POSIX shell (the remote login shell, or a
// command printed for people to paste).
func sshRemoteQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-@:=+") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshConn is one ssh session's stdin/stdout as a net.Conn.
type sshConn struct {
	cmd    *exec.Cmd
	r      *os.File // the session's stdout
	w      *os.File // the session's stdin
	host   string
	stderr *limitedBuffer
	exited chan struct{}
	waitMu sync.Mutex
	err    error       // the session's exit, once exited is closed
	read   atomic.Bool // some data arrived
	close  sync.Once
}

func dialSSH(ctx context.Context, bin string, args []string, host string) (net.Conn, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, clientErr(domain.CodeUnsupportedCapability, "the ssh:// transport needs the OpenSSH client (%s not found in PATH)", bin)
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	c := &sshConn{cmd: exec.Command(path, args...), r: outR, w: inW, host: host, stderr: &limitedBuffer{max: 8 << 10}, exited: make(chan struct{})}
	c.cmd.Stdin, c.cmd.Stdout, c.cmd.Stderr = inR, outW, c.stderr
	if err := c.cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	inR.Close() // the child's ends
	outW.Close()
	go func() {
		err := c.cmd.Wait()
		c.waitMu.Lock()
		c.err = err
		c.waitMu.Unlock()
		close(c.exited)
	}()
	if ctx.Err() != nil {
		c.Close()
		return nil, ctx.Err()
	}
	return c, nil
}

func (c *sshConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.read.Store(true)
	}
	if err != nil && !c.read.Load() && !errors.Is(err, os.ErrDeadlineExceeded) {
		return n, c.failure(err)
	}
	return n, err
}

// failure explains a session that ended before any data: ssh could not
// connect or authenticate, or davit on the host could not reach the
// daemon. Its stderr and exit status say which.
func (c *sshConn) failure(err error) error {
	select {
	case <-c.exited:
	case <-time.After(2 * time.Second):
	}
	msg := strings.TrimSpace(c.stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	exit := -1
	c.waitMu.Lock()
	var xe *exec.ExitError
	if errors.As(c.err, &xe) {
		exit = xe.ExitCode()
	}
	c.waitMu.Unlock()
	return sshFailure(classifySSH(msg, exit), c.host, msg)
}

// Write sends to the session. One that already ended before any data (the
// remote exited first: no davit there, or ssh failed) is explained like a
// failed read, not reported as a bare broken pipe.
func (c *sshConn) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err != nil && !c.read.Load() && !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, os.ErrClosed) {
		return n, c.failure(err)
	}
	return n, err
}

// CloseWrite ends the session's stdin (the host side half-closes the
// daemon connection in turn).
func (c *sshConn) CloseWrite() error { return c.w.Close() }

func (c *sshConn) Close() error {
	c.close.Do(func() {
		c.w.Close()
		c.r.Close()
		go func() {
			select {
			case <-c.exited:
			case <-time.After(2 * time.Second):
				_ = c.cmd.Process.Kill()
			}
		}()
	})
	return nil
}

type sshAddr string

func (a sshAddr) Network() string { return "ssh" }
func (a sshAddr) String() string  { return string(a) }

func (c *sshConn) LocalAddr() net.Addr  { return sshAddr("local") }
func (c *sshConn) RemoteAddr() net.Addr { return sshAddr(c.host) }
func (c *sshConn) SetDeadline(t time.Time) error {
	return errors.Join(c.r.SetReadDeadline(t), c.w.SetWriteDeadline(t))
}
func (c *sshConn) SetReadDeadline(t time.Time) error  { return c.r.SetReadDeadline(t) }
func (c *sshConn) SetWriteDeadline(t time.Time) error { return c.w.SetWriteDeadline(t) }

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hostSocket is the daemon socket dial-stdio uses without --socket: the
// first that exists of $DAVIT_API_URL's unix path, the default config's
// socket, the host-install data dir's (~/.svm/davit/davit.sock, whatever
// config the unit runs) and the built-in default.
func hostSocket() string {
	var cands []string
	if u, err := url.Parse(os.Getenv(buildinfo.Env("API_URL"))); err == nil && u.Scheme == "unix" {
		cands = append(cands, u.Path)
	}
	if cfg, path, err := config.LoadDefault(""); err == nil && path != "" && cfg.Socket != "" {
		cands = append(cands, cfg.Socket)
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".svm", buildinfo.Name, buildinfo.Name+".sock"))
	}
	cands = append(cands, config.DefaultSocket())
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return c
		}
	}
	return cands[len(cands)-1]
}

// runDialStdio is the host side of the ssh:// transport: it splices its
// stdin/stdout to the daemon's socket (--socket, else the configured or
// default one) until either side ends. It writes nothing else to stdout.
func runDialStdio(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) > 0 {
		return usageErr("dial-stdio takes no arguments")
	}
	sock := o.socket
	if sock == "" {
		sock = hostSocket()
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return clientErr(domain.CodeUnavailable, "the daemon on this host is not reachable at %s: %v", sock, err)
	}
	uc := conn.(*net.UnixConn)
	go func() {
		_, _ = io.Copy(uc, s.io.Stdin)
		_ = uc.CloseWrite()
	}()
	if _, err := io.Copy(s.io.Stdout, uc); err != nil {
		return fmt.Errorf("dial-stdio: %w", err)
	}
	uc.Close()
	return nil
}
