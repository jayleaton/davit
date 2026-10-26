//go:build linux

package userspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Main runs a userspace-egress subcommand of the davit binary and returns
// the exit status:
//
//	netstack-egress CONFIG_JSON   serve the TAP on fd 3; write "1" to fd 4 when ready
//	netns-tapfd IFNAME            (inside the VM netns) open TAP IFNAME and send its fd over the unix socket on fd 3
func Main(args []string, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: davit netstack-egress CONFIG_JSON | davit netns-tapfd IFNAME")
		return 2
	}
	switch args[0] {
	case CmdTapFD:
		if err := sendTapFD(args[1]); err != nil {
			fmt.Fprintln(stderr, "netns-tapfd:", err)
			return 1
		}
		return 0
	case CmdEgress:
		var cfg Config
		if err := json.Unmarshal([]byte(args[1]), &cfg); err != nil {
			fmt.Fprintln(stderr, "netstack-egress: config:", err)
			return 2
		}
		log := slog.New(slog.NewTextHandler(stderr, nil))
		holderGone, err := watchHolder(cfg.Holder)
		if err != nil {
			log.Error("netstack-egress: namespace holder", "err", err)
			return 1
		}
		st, err := New(cfg, FDTap, log)
		if err != nil {
			log.Error("netstack-egress", "err", err)
			return 1
		}
		ready := os.NewFile(FDReady, "ready")
		_, _ = ready.Write([]byte("1"))
		ready.Close()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-st.Done():
		case <-holderGone:
		case <-sig:
		}
		st.Close()
		return 0
	}
	fmt.Fprintf(stderr, "unknown userspace subcommand %q\n", args[0])
	return 2
}

// OpenTap attaches to (or creates) TAP ifname in the current network
// namespace, IFF_TAP|IFF_NO_PI, and returns its fd.
func OpenTap(ifname string) (int, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var req [unix.IFNAMSIZ + 64]byte
	copy(req[:unix.IFNAMSIZ-1], ifname)
	*(*uint16)(unsafe.Pointer(&req[unix.IFNAMSIZ])) = unix.IFF_TAP | unix.IFF_NO_PI
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&req[0]))); errno != 0 {
		unix.Close(fd)
		return -1, fmt.Errorf("TUNSETIFF %s: %w", ifname, errno)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func sendTapFD(ifname string) error {
	fd, err := OpenTap(ifname)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.Sendmsg(FDSock, []byte{'t'}, unix.UnixRights(fd), nil, 0)
}

// RecvFD receives one file descriptor sent with SCM_RIGHTS on c.
func RecvFD(c *net.UnixConn) (*os.File, error) {
	buf := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, err
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) != 1 {
		return nil, fmt.Errorf("no fd received (%v)", err)
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		return nil, fmt.Errorf("no fd received (%v)", err)
	}
	return os.NewFile(uintptr(fds[0]), "tap"), nil
}

// watchHolder returns a channel closed when the holder process exits. The
// pidfd pins the process, and the start time check (after opening it)
// rules out a reused PID.
func watchHolder(h Holder) (<-chan struct{}, error) {
	if h.PID <= 0 {
		return nil, errors.New("no holder PID")
	}
	pidfd, err := unix.PidfdOpen(h.PID, 0)
	if err != nil {
		return nil, fmt.Errorf("pidfd_open %d: %w", h.PID, err)
	}
	if start, err := startTime(h.PID); err != nil || start != h.StartTime {
		unix.Close(pidfd)
		return nil, fmt.Errorf("holder %d is gone or not the recorded process", h.PID)
	}
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		defer unix.Close(pidfd)
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		for {
			if _, err := unix.Poll(fds, -1); err != unix.EINTR {
				return // readable: exited (or poll failed: treat as gone)
			}
		}
	}()
	return gone, nil
}

func startTime(pid int) (uint64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 20 {
		return 0, fmt.Errorf("short /proc/%d/stat", pid)
	}
	return strconv.ParseUint(f[19], 10, 64)
}
