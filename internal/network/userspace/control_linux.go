//go:build linux

package userspace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// dialControl connects to the abstract unix socket name.
func dialControl(ctx context.Context, name string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", "@"+name)
}

// listenControl serves policy changes on the abstract unix socket name
// (it lives in the host network namespace, like the stack process).
func (st *Stack) listenControl(name string) error {
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + name, Net: "unix"})
	if err != nil {
		return fmt.Errorf("userspace: control socket: %w", err)
	}
	st.ctl = l
	uid := os.Getuid()
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			go st.serveControl(c, uid)
		}
	}()
	return nil
}

// serveControl handles one control connection: one request, one reply.
// Abstract sockets have no file permissions, so the peer must be uid.
func (st *Stack) serveControl(c *net.UnixConn, uid int) {
	defer c.Close()
	if err := checkPeer(c, uid); err != nil {
		st.log.Warn("egress control: rejected peer", "err", err)
		return
	}
	_ = c.SetDeadline(time.Now().Add(controlTimeout))
	var req controlRequest
	rep := controlReply{OK: true}
	err := json.NewDecoder(io.LimitReader(c, 1<<20)).Decode(&req)
	if err == nil {
		err = st.apply(req.Mode, req.Allow)
	}
	if err != nil {
		rep = controlReply{Error: err.Error()}
	}
	_ = json.NewEncoder(c).Encode(rep)
}

// checkPeer reports an error unless c's peer process runs as uid
// (SO_PEERCRED).
func checkPeer(c *net.UnixConn, uid int) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Ucred
	var cerr error
	if err := rc.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if cerr != nil {
		return fmt.Errorf("SO_PEERCRED: %w", cerr)
	}
	if int(cred.Uid) != uid {
		return fmt.Errorf("peer pid %d uid %d, want uid %d", cred.Pid, cred.Uid, uid)
	}
	return nil
}
