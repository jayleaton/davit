package network

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"davit/internal/network/nethelper"
	"davit/internal/network/userspace"
)

// selfExe is the davit binary that serves the re-executed helper
// commands (userspace egress, netns DNS). Tests that cannot re-exec
// themselves leave it failing.
var selfExe = helperExe

// helperExe is os.Executable, but only for an executable that declared it
// serves the helper commands (nethelper.Serve): re-executing any other
// binary would run whatever it does by default inside the VM's namespaces.
func helperExe() (string, error) {
	if !nethelper.Served() {
		return "", errors.New("this executable does not serve the network helper commands (its main must call nethelper.Serve)")
	}
	return os.Executable()
}

// userspaceResolvers overrides the stack's host DNS resolvers (host:port)
// in tests; empty uses the host's /etc/resolv.conf.
var userspaceResolvers []string

// startUserspace adds the uplink TAP (Config.Uplink) with the netns side of
// the userspace subnet and a default route through the stack, obtains the
// TAP's fd from inside the namespaces, and starts the egress process in the
// host network namespace in its own session (it outlives the daemon and
// exits when the namespace goes away). It enforces h.Policy and listens
// for live changes (userspace.SetPolicy) on h.EgressControl, set here.
func (r *Rootless) startUserspace(ctx context.Context, h *Handle) (*Process, error) {
	exe, err := selfExe()
	if err != nil {
		return nil, fmt.Errorf("locate davit binary: %w", err)
	}
	peer, gw := userspace.DefaultPeer, userspace.DefaultGateway
	for _, argv := range [][]string{
		{"ip", "tuntap", "add", "dev", r.cfg.Uplink, "mode", "tap"},
		{"ip", "addr", "add", peer.String(), "dev", r.cfg.Uplink},
		{"ip", "link", "set", "dev", r.cfg.Uplink, "mtu", strconv.Itoa(r.cfg.MTU), "up"},
		{"ip", "route", "add", "default", "via", gw.Addr().String(), "dev", r.cfg.Uplink},
	} {
		if err := r.inNS(ctx, *h, argv, ""); err != nil {
			return nil, err
		}
	}
	tap, err := fetchTap(ctx, *h, exe, r.cfg.Uplink)
	if err != nil {
		return nil, fmt.Errorf("open uplink tap: %w", err)
	}
	defer tap.Close()

	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	control := "davit-egress/" + string(h.VMID) + "/" + strconv.FormatInt(h.Generation, 10) + "/" + hex.EncodeToString(nonce[:])
	cfg, _ := json.Marshal(userspace.Config{
		Gateway: gw, Peer: peer.Addr(), MTU: r.cfg.MTU,
		Policy:    stackPolicy(r.cfg.Egress),
		Resolvers: userspaceResolvers,
		Mode:      string(h.Policy.Mode), Allow: h.Policy.Allow, Control: control,
		Holder: userspace.Holder{PID: h.Holder.PID, StartTime: h.Holder.StartTime},
	})
	rd, wr, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	cmd := exec.Command(exe, userspace.CmdEgress, string(cfg))
	cmd.ExtraFiles = []*os.File{tap, wr} // fd 3, fd 4
	// stdout/stderr stay /dev/null: the process outlives the daemon, and a
	// write to a closed pipe on fd 2 would kill it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	wr.Close()
	if err != nil {
		return nil, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	_ = rd.SetReadDeadline(time.Now().Add(readyTimeout))
	buf := make([]byte, 1)
	if _, err := io.ReadFull(rd, buf); err != nil || buf[0] != '1' {
		_ = cmd.Process.Kill()
		<-exited
		return nil, fmt.Errorf("egress stack not ready: %v", err)
	}
	h.EgressControl = control
	return identify(cmd.Process.Pid)
}

// fetchTap runs `davit netns-tapfd` inside h's namespaces (where it has
// CAP_NET_ADMIN) and receives the TAP fd over a socketpair.
func fetchTap(ctx context.Context, h Handle, exe, ifname string) (*os.File, error) {
	return fetchFD(ctx, h, exe, userspace.CmdTapFD, ifname)
}

// fetchFD runs `exe args...` inside h's namespaces with one end of a
// socketpair as fd 3, waits (at most readyTimeout) for it to exit and
// receives the one fd it sent over the socket with SCM_RIGHTS.
func fetchFD(ctx context.Context, h Handle, exe string, args ...string) (*os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parent, child := os.NewFile(uintptr(fds[0]), "fd-parent"), os.NewFile(uintptr(fds[1]), "fd-child")
	defer parent.Close()
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	argv := append(append(EnterArgv(h), exe), args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.ExtraFiles = []*os.File{child} // fd 3
	out, err := cmd.CombinedOutput()
	child.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, out)
	}
	c, err := net.FileConn(parent)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil, fmt.Errorf("unexpected socket type %T", c)
	}
	_ = uc.SetReadDeadline(time.Now().Add(readyTimeout))
	return userspace.RecvFD(uc)
}

// stackPolicy is p in the userspace stack's terms.
func stackPolicy(p EgressPolicy) userspace.Policy {
	svcs := make([]userspace.Service, len(p.Services))
	for i, s := range p.Services {
		svcs[i] = userspace.Service{Proto: s.Proto, Addr: s.Addr, Port: s.Port}
	}
	return userspace.Policy{Deny: p.Deny, Allow: p.Allow, HostAddrs: p.HostAddrs, Services: svcs}
}
