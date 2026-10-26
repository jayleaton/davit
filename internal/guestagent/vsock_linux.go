//go:build linux

package guestagent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ListenVsock listens on AF_VSOCK port (any local CID) and accepts only
// connections from the host (CID 2). Guest processes reaching the port over
// vsock loopback (CID 1) or any other CID are refused: the bootstrap secret
// is only as private as this check.
func ListenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 64); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	return &vsockListener{f: os.NewFile(uintptr(fd), "vsock-listener"), port: port}, nil
}

type vsockListener struct {
	f    *os.File
	port uint32
}

func (l *vsockListener) Accept() (net.Conn, error) {
	rc, err := l.f.SyscallConn()
	if err != nil {
		return nil, err
	}
	for {
		var nfd int
		var sa unix.Sockaddr
		var aerr error
		err := rc.Read(func(fd uintptr) bool {
			nfd, sa, aerr = unix.Accept4(int(fd), unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
			return !errors.Is(aerr, unix.EAGAIN)
		})
		if err != nil {
			return nil, err
		}
		if aerr != nil {
			if errors.Is(aerr, unix.ECONNABORTED) || errors.Is(aerr, unix.EINTR) {
				continue
			}
			return nil, aerr
		}
		peer, _ := sa.(*unix.SockaddrVM)
		if peer == nil || peer.CID != unix.VMADDR_CID_HOST {
			unix.Close(nfd)
			continue
		}
		return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock"), local: vsockAddr{unix.VMADDR_CID_ANY, l.port},
			remote: vsockAddr{peer.CID, peer.Port}}, nil
	}
}

func (l *vsockListener) Close() error   { return l.f.Close() }
func (l *vsockListener) Addr() net.Addr { return vsockAddr{unix.VMADDR_CID_ANY, l.port} }

type vsockAddr struct{ cid, port uint32 }

func (vsockAddr) Network() string  { return "vsock" }
func (a vsockAddr) String() string { return fmt.Sprintf("vsock:%d:%d", a.cid, a.port) }

// vsockConn is a net.Conn over a non-blocking vsock fd; *os.File provides
// poller-backed Read/Write and deadlines.
type vsockConn struct {
	*os.File
	local, remote vsockAddr
}

func (c *vsockConn) LocalAddr() net.Addr                { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr               { return c.remote }
func (c *vsockConn) SetDeadline(t time.Time) error      { return c.File.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.File.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.File.SetWriteDeadline(t) }

// CloseWrite shuts down the write half so a spliced TCP dial can half-close.
func (c *vsockConn) CloseWrite() error {
	rc, err := c.File.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return serr
}
