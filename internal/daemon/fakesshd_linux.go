//go:build linux

package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	"golang.org/x/crypto/ssh"
)

// fakeDialTCP is the fake guest's TCPDial: port 22 is an in-process SSH
// server (the fake has no sshd), every other port is the host's loopback,
// which is the fake guest's loopback (its exec runs on the host too).
func fakeDialTCP(root string, log *slog.Logger) func(ctx context.Context, port uint16) (net.Conn, error) {
	return func(ctx context.Context, port uint16) (net.Conn, error) {
		if port == 22 {
			key, err := fakeHostKey(root)
			if err != nil {
				return nil, err
			}
			host, guest := net.Pipe()
			go serveFakeSSH(guest, key, root, log)
			return host, nil
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	}
}

// fakeHostKey is the host key the restore barrier wrote for this fake VM,
// so forks rotate it and known_hosts behaves as with a real guest.
func fakeHostKey(root string) (ssh.Signer, error) {
	b, err := os.ReadFile(filepath.Join(root, "etc", "ssh", "ssh_host_ed25519_key"))
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(b)
}

// serveFakeSSH serves one SSH connection like a guest sshd configured by the
// restore barrier: only user certificates signed by the CA in
// etc/ssh/davit_user_ca.pub, valid now, with the login user as principal,
// authenticate. "exec" and "shell" run /bin/sh on the host without a
// terminal, and direct-tcpip (ssh -L) reaches host loopback only.
func serveFakeSSH(c net.Conn, key ssh.Signer, root string, log *slog.Logger) {
	defer c.Close()
	cas, err := trustedUserCAs(root)
	if err != nil {
		log.Warn("fake sshd: no trusted user CA", "err", err)
	}
	checker := &ssh.CertChecker{IsUserAuthority: func(k ssh.PublicKey) bool {
		for _, ca := range cas {
			if string(ca.Marshal()) == string(k.Marshal()) {
				return true
			}
		}
		return false
	}}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if _, ok := k.(*ssh.Certificate); !ok {
				return nil, errors.New("only certificates are accepted")
			}
			return checker.Authenticate(meta, k)
		},
		ServerVersion: "SSH-2.0-davit-fake-guest",
	}
	cfg.AddHostKey(key)
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		switch nc.ChannelType() {
		case "session":
			go fakeSession(nc, log)
		case "direct-tcpip":
			go fakeDirectTCPIP(nc)
		default:
			nc.Reject(ssh.UnknownChannelType, "not supported by the fake guest")
		}
	}
}

// trustedUserCAs reads the CA keys the barrier installed for this guest.
func trustedUserCAs(root string) ([]ssh.PublicKey, error) {
	b, err := os.ReadFile(filepath.Join(root, "etc", "ssh", "davit_user_ca.pub"))
	if err != nil {
		return nil, err
	}
	var out []ssh.PublicKey
	for len(b) > 0 {
		k, _, _, rest, err := ssh.ParseAuthorizedKey(b)
		if err != nil {
			break
		}
		out = append(out, k)
		b = rest
	}
	return out, nil
}

func fakeSession(nc ssh.NewChannel, log *slog.Logger) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for r := range reqs {
		var argv []string
		switch r.Type {
		case "exec":
			var p struct{ Command string }
			if ssh.Unmarshal(r.Payload, &p) != nil {
				r.Reply(false, nil)
				continue
			}
			argv = []string{"/bin/sh", "-c", p.Command}
		case "shell":
			argv = []string{"/bin/sh"}
		case "env":
			r.Reply(true, nil)
			continue
		default: // pty-req, subsystem (sftp), x11-req, ...
			r.Reply(false, nil)
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
		if err := cmd.Start(); err != nil {
			r.Reply(false, nil)
			continue
		}
		r.Reply(true, nil)
		go ssh.DiscardRequests(reqs)
		status := 0
		if err := cmd.Wait(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				status = ee.ExitCode()
			} else {
				status = 255
				log.Warn("fake sshd command", "err", err)
			}
		}
		_ = ch.CloseWrite()
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
		return
	}
}

func fakeDirectTCPIP(nc ssh.NewChannel) {
	var p struct {
		Host     string
		Port     uint32
		OrigHost string
		OrigPort uint32
	}
	if ssh.Unmarshal(nc.ExtraData(), &p) != nil || p.Port == 0 || p.Port > 65535 {
		nc.Reject(ssh.ConnectionFailed, "bad direct-tcpip request")
		return
	}
	tc, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(p.Port))))
	if err != nil {
		nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	defer tc.Close()
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	go ssh.DiscardRequests(reqs)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(tc, ch)
		if cw, ok := tc.(*net.TCPConn); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, _ = io.Copy(ch, tc)
	_ = ch.CloseWrite()
	wg.Wait()
}
