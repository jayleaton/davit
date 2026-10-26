//go:build linux

package netnslisten

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"

	"golang.org/x/sys/unix"
)

// Main runs `davit netns-listenfd ADDR:PORT` and returns the exit status.
func Main(args []string, stderr io.Writer) int {
	if len(args) != 2 || args[0] != Cmd {
		fmt.Fprintln(stderr, "usage: davit netns-listenfd ADDR:PORT")
		return 2
	}
	ap, err := netip.ParseAddrPort(args[1])
	if err != nil || !ap.Addr().Is4() || ap.Port() == 0 {
		fmt.Fprintf(stderr, "netns-listenfd: %q is not an IPv4 ADDR:PORT with a nonzero port\n", args[1])
		return 2
	}
	if err := sendListener(ap); err != nil {
		fmt.Fprintln(stderr, "netns-listenfd:", err)
		return 1
	}
	return 0
}

// sendListener binds ap in the current network namespace and sends the
// listening socket over FDSock. Closing this process's copies afterwards
// leaves the socket open in the receiver.
func sendListener(ap netip.AddrPort) error {
	l, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(ap))
	if err != nil {
		return err
	}
	defer l.Close()
	f, err := l.File()
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Sendmsg(FDSock, []byte{'l'}, unix.UnixRights(int(f.Fd())), nil, 0); err != nil {
		return errors.Join(errors.New("send listener fd"), err)
	}
	return nil
}
