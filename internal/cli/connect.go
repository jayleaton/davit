package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// `davit connect [USER@]HOST` takes a fresh client machine to a working
// default host over the ssh:// transport (sshtransport.go): an ed25519 key
// (found, or created; never overwritten), key auth tested with BatchMode
// (printing how to authorize the key when the host refuses it, then
// waiting), davit and its daemon on the host, and client.json. --check
// re-verifies the saved host and changes nothing.

// connectOpts are connect's flags.
type connectOpts struct {
	via   string
	key   string
	wait  optDuration
	check bool
}

func connectFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.connect.via, "via", "", "reach the host through a jump box ([USER@]JUMP[:PORT], OpenSSH ProxyJump)")
	fs.StringVar(&o.connect.key, "key", "", "SSH private key to use or create (default ~/.ssh/id_ed25519)")
	fs.Var(&o.connect.wait, "wait", "how long to wait for the key to be authorized on the host (default 10m in a terminal, else 0s)")
	fs.BoolVar(&o.connect.check, "check", false, "re-verify the saved (or --host) host; changes nothing")
}

const connectUsage = "[USER@]HOST[:PORT] [--via JUMP] [--key FILE] [--wait D] | --check"

// connectRetry is how often a denied key is retried while waiting.
var connectRetry = 3 * time.Second

// sshProblem is why an ssh session to the host failed.
type sshProblem int

const (
	sshOther sshProblem = iota
	sshDenied
	sshUnreachable
	sshHostKey
	sshNoDavit
	sshNoDaemon
)

// classifySSH reads an ssh session's stderr and exit status. ssh itself
// exits 255; the remote shell exits 127 for a missing command.
func classifySSH(stderr string, exit int) sshProblem {
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(stderr, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("REMOTE HOST IDENTIFICATION HAS CHANGED", "Host key verification failed"):
		return sshHostKey
	case has("Permission denied", "Too many authentication failures", "No more authentication methods"):
		return sshDenied
	case has("Could not resolve hostname", "Connection refused", "Connection timed out", "Operation timed out",
		"No route to host", "Network is unreachable", "Connection closed by", "Connection reset", "kex_exchange_identification",
		"Name or service not known", "nodename nor servname"):
		return sshUnreachable
	case has("daemon on this host is not reachable"):
		return sshNoDaemon
	case exit == 127 || has("command not found", ": not found"):
		return sshNoDavit
	}
	return sshOther
}

// sshFailure is the error for a failed session to login (USER@HOST),
// saying what to do; raw is ssh's own message.
func sshFailure(p sshProblem, login, raw string) *cliError {
	n := buildinfo.Name
	var e *cliError
	switch p {
	case sshDenied:
		e = clientErr(domain.CodeUnauthenticated, "%s does not accept this machine's SSH key: run `%s connect %s`, which "+
			"prints the line that authorizes it", login, n, login)
	case sshUnreachable:
		e = clientErr(domain.CodeUnavailable, "cannot reach %s over ssh: %s (check the address, that the host is on, and that "+
			"`ssh %s true` works; behind a jump box: `%s connect --via JUMP %s`)", login, lastLine(raw), login, n, login)
	case sshHostKey:
		e = clientErr(domain.CodeUnauthenticated, "the host key of %s changed or is unknown: if the host was reinstalled, verify "+
			"its new key, then `ssh-keygen -R %s` and `%s connect %s`", login, hostOf(login), n, login)
	case sshNoDavit:
		e = clientErr(domain.CodeUnsupportedCapability, "%s is not installed on %s (or not on its PATH): install it there "+
			"(docs/install-host.md), then `%s connect --check`", n, login, n)
	case sshNoDaemon:
		e = clientErr(domain.CodeUnavailable, "%s is installed on %s but its daemon is not running: on the host run "+
			"`systemctl --user start %s` (or `%s doctor`), then `%s connect --check`", n, login, n, n, n)
	default:
		e = clientErr(domain.CodeUnavailable, "ssh to %s: %s (`%s connect --check` diagnoses the connection)", login, raw, n)
	}
	e.Retryable = p == sshUnreachable || p == sshNoDaemon || p == sshOther
	e.Details = map[string]any{"ssh": raw}
	return e
}

// lastLine is the last non-empty line of s (ssh's reason, after any banner).
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func hostOf(login string) string {
	if i := strings.LastIndex(login, "@"); i >= 0 {
		return login[i+1:]
	}
	return login
}

// connectStep is one checked step, printed as it completes.
type connectStep struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func (s *session) step(id string, ok bool, format string, args ...any) {
	st := connectStep{ID: id, OK: ok, Detail: fmt.Sprintf(format, args...)}
	s.out.emit("item", "", 0, st, nil)
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	s.out.human("%s %-7s %s", mark, id, st.Detail)
}

// clientSSHKey is the key connect uses: its path and authorized_keys line.
type clientSSHKey struct {
	path, pub string
	created   bool
}

// defaultKeyPath is ~/.ssh/id_ed25519 ($HOME, as the CLI resolves it).
func defaultKeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "id_ed25519"), nil
}

// loadSSHKey reads path's public key (path.pub, else derived from an
// unencrypted private key). ok is false when path does not exist.
func loadSSHKey(path string) (k clientSSHKey, ok bool, err error) {
	k.path = path
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return k, false, nil
	} else if err != nil {
		return k, false, err
	}
	if b, err := os.ReadFile(path + ".pub"); err == nil {
		k.pub = strings.TrimSpace(string(b))
		return k, true, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return k, true, err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return k, true, usageErr("%s: %v; write its public half with `ssh-keygen -y -f %s > %s.pub`", path, err, path, path)
	}
	k.pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	return k, true, nil
}

// createSSHKey writes a new unencrypted ed25519 key at path (0600) and
// path.pub. It refuses to replace an existing file.
func createSSHKey(path string) (clientSSHKey, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return clientSSHKey{}, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return clientSSHKey{}, err
	}
	comment := buildinfo.Name
	if h, err := os.Hostname(); err == nil && h != "" {
		comment += "@" + h
	}
	blk, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return clientSSHKey{}, err
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		return clientSSHKey{}, err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + comment
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{{path, pem.EncodeToMemory(blk), 0o600}, {path + ".pub", []byte(line + "\n"), 0o644}} {
		w, err := os.OpenFile(f.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, f.mode)
		if err != nil {
			return clientSSHKey{}, err // O_EXCL: an existing key is never replaced
		}
		_, err = w.Write(f.data)
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return clientSSHKey{}, err
		}
	}
	return clientSSHKey{path: path, pub: line, created: true}, nil
}

// probeSSH runs `davit version --json` on the host in a fresh session (no
// control master, so it tests the current key) and returns the host's
// davit version. A host key seen for the first time is accepted and
// recorded, as ssh-copy-id does.
func probeSSH(ctx context.Context, t sshTarget) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	bin, err := exec.LookPath(sshBinary())
	if err != nil {
		return "", clientErr(domain.CodeUnsupportedCapability, "%s connect needs the OpenSSH client (%s not found in PATH)", buildinfo.Name, sshBinary())
	}
	args := append(t.args(), "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", "-o", "ControlPath=none",
		t.host, "--", remoteCommand("version", "--json"))
	var stdout bytes.Buffer
	stderr := &limitedBuffer{max: 8 << 10}
	c := exec.CommandContext(ctx, bin, args...)
	c.Stdout, c.Stderr = &stdout, stderr
	err = c.Run()
	if err == nil {
		var env struct {
			Data struct{ Version string } `json:"data"`
		}
		for _, l := range strings.Split(stdout.String(), "\n") {
			if json.Unmarshal([]byte(l), &env) == nil && env.Data.Version != "" {
				return env.Data.Version, nil
			}
		}
		return "", sshFailure(sshNoDavit, t.login(), "unexpected output from `davit version --json`: "+lastLine(stdout.String()))
	}
	if ctx.Err() != nil {
		return "", sshFailure(sshUnreachable, t.login(), "no answer within 30s")
	}
	exit := -1
	var xe *exec.ExitError
	if errors.As(err, &xe) {
		exit = xe.ExitCode()
	}
	msg := strings.TrimSpace(stderr.String())
	if msg == "" {
		msg = err.Error()
	}
	return "", sshFailure(classifySSH(msg, exit), t.login(), msg)
}

func isProblem(err error, code domain.Code) bool {
	var ce *cliError
	return errors.As(err, &ce) && ce.Code == code
}

// authorizeHelp is how to put key on the host for t.
func authorizeHelp(t sshTarget, k clientSSHKey) (copyID, paste string) {
	copyID = "ssh-copy-id -i " + sshRemoteQuote(k.path+".pub")
	if t.port != "" {
		copyID += " -p " + t.port
	}
	if t.jump != "" {
		copyID += " -o ProxyJump=" + sshRemoteQuote(t.jump)
	}
	copyID += " " + sshRemoteQuote(t.login())
	paste = "mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo " + sshRemoteQuote(k.pub) +
		" >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"
	return copyID, paste
}

// connectTarget turns connect's argument into a host URL: ssh:// unless it
// is an http(s):// gateway URL.
func connectTarget(arg string) (string, error) {
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		return hostURL(arg)
	}
	if !strings.HasPrefix(arg, "ssh://") {
		if strings.Contains(arg, "://") || arg == "" || strings.ContainsAny(arg, "/ ") {
			return "", usageErr("usage: %s connect %s", buildinfo.Name, connectUsage)
		}
		arg = "ssh://" + arg
	}
	return arg, nil
}

func runConnect(ctx context.Context, s *session, o *opts, args []string) error {
	co := o.connect
	if err := maxArgs(args, 1, "connect "+connectUsage); err != nil {
		return err
	}
	var target string
	switch {
	case len(args) == 1:
		target = args[0]
	case co.check && s.g.host != "":
		target = s.g.host
	case co.check:
		c, p, err := loadClientConfig()
		if err != nil {
			return err
		}
		if c.Host == "" {
			return clientErr(domain.CodeNotFound, "no default host in %s: run `%s connect USER@HOST`", p, buildinfo.Name)
		}
		target = c.Host
	default:
		return usageErr("usage: %s connect %s", buildinfo.Name, connectUsage)
	}
	raw, err := connectTarget(target)
	if err != nil {
		return err
	}
	if co.check && (co.via != "" || co.key != "" || co.wait.set) {
		return usageErr("--check re-verifies a saved host; --via, --key and --wait set one up")
	}
	hostURLStr, login := raw, raw
	var version string
	if strings.HasPrefix(raw, "ssh://") {
		u, err := url.Parse(raw)
		if err != nil {
			return usageErr("invalid host %q", target)
		}
		t, err := parseSSHTarget(u)
		if err != nil {
			return err
		}
		if co.via != "" {
			t.jump = co.via
		}
		if co.key != "" {
			t.key = co.key
		}
		login = t.login()
		if t, version, err = connectSSH(ctx, s, co, t); err != nil {
			return err
		}
		hostURLStr = t.String()
	} else if co.via != "" || co.key != "" {
		return usageErr("--via and --key apply to ssh hosts (USER@HOST)")
	}

	// The daemon, through the same transport every later command uses.
	cl, err := newClient(hostURLStr, s.out.requestID)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var caps api.HostCapabilities
	if err := cl.get(cctx, "/capabilities", nil, &caps); err != nil {
		s.step("daemon", false, "%v", err)
		return err
	}
	s.step("daemon", true, "%s %s answers (runtime %s)", buildinfo.Name, caps.Version, caps.Runtime.Engine)
	if caps.Version != "" && caps.Version != buildinfo.Version {
		s.out.diag("note: versions differ (this CLI %s, the host %s); if a command misbehaves, update the older one", buildinfo.Version, caps.Version)
	}

	res := map[string]any{"host": hostURLStr, "version": caps.Version, "runtime": caps.Runtime.Engine}
	if version != "" {
		res["remoteBinary"] = version
	}
	if !co.check {
		c, p, err := loadClientConfig()
		if err != nil {
			return err
		}
		c.Host = hostURLStr
		if err := saveClientConfig(c, p); err != nil {
			return err
		}
		res["config"] = p
		s.step("saved", true, "default host %s in %s", hostURLStr, p)
	}
	s.out.result("", res)
	s.out.human("\n✓ connected to %s (%s %s, runtime %s)", login, buildinfo.Name, caps.Version, caps.Runtime.Engine)
	if !co.check {
		s.out.human("next: cd your-repo && %s box run -- make test", buildinfo.Name)
	}
	return nil
}

// connectSSH runs the key, ssh and davit steps for t and returns t with
// the key it settled on.
func connectSSH(ctx context.Context, s *session, co connectOpts, t sshTarget) (sshTarget, string, error) {
	keyPath := t.key
	if keyPath == "" {
		p, err := defaultKeyPath()
		if err != nil {
			return t, "", err
		}
		keyPath = p
	}
	k, haveKey, err := loadSSHKey(keyPath)
	if err != nil {
		return t, "", err
	}
	if haveKey {
		t.key = keyPath
		s.step("key", true, "%s", keyPath)
	}
	version, err := probeSSH(ctx, t)
	if !haveKey && err == nil {
		s.step("key", true, "none needed: your ssh setup already logs in")
	}
	if !haveKey && isProblem(err, domain.CodeUnauthenticated) && !co.check {
		if k, err = createSSHKey(keyPath); err != nil {
			return t, "", err
		}
		t.key = keyPath
		s.step("key", true, "%s (created)", keyPath)
		version, err = probeSSH(ctx, t)
	}
	var ce *cliError
	if err != nil && errors.As(err, &ce) && ce.Code == domain.CodeUnauthenticated && classifySSH(fmt.Sprint(ce.Details["ssh"]), -1) == sshDenied && t.key != "" {
		copyID, paste := authorizeHelp(t, k)
		wait := co.wait.d
		if !co.wait.set && s.io.Interactive && !s.g.json && !co.check {
			wait = 10 * time.Minute
		}
		s.step("ssh", false, "%s does not accept %s yet", t.login(), k.path)
		ce.Details["authorize"] = map[string]string{"sshCopyId": copyID, "onHost": paste, "publicKey": k.pub}
		ce.Message = fmt.Sprintf("%s does not accept this machine's SSH key %s: authorize it with `%s` "+
			"(if password login works), or run this on the host as %s: %s", t.login(), k.path, copyID, userOr(t.user), paste)
		then := "then this finishes by itself"
		if wait <= 0 {
			then = "then run `" + buildinfo.Name + " connect " + t.login() + "` again"
			if co.check {
				then = "then `" + buildinfo.Name + " connect --check` again"
			}
		}
		s.out.human("\nAuthorize this key on the host, %s:\n\n    %s\n\nor, logged in on the host as %s, paste:\n\n    %s\n",
			then, copyID, userOr(t.user), paste)
		if wait <= 0 {
			return t, "", err
		}
		s.out.human("waiting up to %s for the key (Ctrl-C to stop)...", wait)
		deadline := time.Now().Add(wait)
		for err != nil && isProblem(err, domain.CodeUnauthenticated) && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return t, "", ctx.Err()
			case <-time.After(connectRetry):
			}
			version, err = probeSSH(ctx, t)
		}
		if err != nil {
			if isProblem(err, domain.CodeUnauthenticated) {
				ce.Message = fmt.Sprintf("gave up after %s: %s", wait, ce.Message)
				return t, "", ce
			}
		}
	}
	if err != nil {
		var pe *cliError
		id := "ssh"
		if errors.As(err, &pe) && (pe.Code == domain.CodeUnsupportedCapability) {
			s.step("ssh", true, "%s logs in", t.login())
			id = buildinfo.Name
		}
		s.step(id, false, "%v", err)
		return t, "", err
	}
	s.step("ssh", true, "%s logs in with key auth", t.login())
	s.step(buildinfo.Name, true, "%s %s installed on the host", buildinfo.Name, version)
	return t, version, nil
}

func userOr(u string) string {
	if u == "" {
		return "your user"
	}
	return u
}
