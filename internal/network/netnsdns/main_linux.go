//go:build linux

package netnsdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Main runs `davit netns-dns CONFIG_JSON` (inside the VM namespaces) and
// returns the exit status. It writes "1" to fd FDReady once listening (or
// the startup error, then exits) and exits when the holder exits, on SIGTERM/SIGINT, or when a listener fails.
func Main(args []string, stderr io.Writer) int {
	ready := os.NewFile(FDReady, "ready")
	fail := func(code int, err error) int {
		fmt.Fprintln(stderr, "netns-dns:", err)
		if ready != nil {
			_, _ = ready.Write([]byte(err.Error()))
			ready.Close()
		}
		return code
	}
	if len(args) != 2 || args[0] != Cmd {
		return fail(2, errors.New("usage: davit netns-dns CONFIG_JSON"))
	}
	var cfg Config
	if err := json.Unmarshal([]byte(args[1]), &cfg); err != nil {
		return fail(2, fmt.Errorf("config: %w", err))
	}
	s, err := New(cfg)
	if err != nil {
		return fail(2, err)
	}
	gone := make(<-chan struct{})
	if cfg.Holder.PID != 0 {
		if gone, err = watchHolder(cfg.Holder); err != nil {
			return fail(1, err)
		}
	}
	addr := net.UDPAddrFromAddrPort(cfg.Listen)
	udp, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return fail(1, err)
	}
	tcp, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: addr.IP, Port: addr.Port})
	if err != nil {
		udp.Close()
		return fail(1, err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, udp, tcp) }()
	if ready != nil {
		_, _ = ready.Write([]byte("1"))
		ready.Close()
	}
	select {
	case err := <-served:
		fmt.Fprintln(stderr, "netns-dns:", err)
		return 1
	case <-gone:
	case <-sig:
	}
	return 0
}

// watchHolder returns a channel closed when the holder exits. The pidfd
// pins the process; the start time check after opening it rules out a
// reused PID.
func watchHolder(h Holder) (<-chan struct{}, error) {
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

// startTime is field 22 of /proc/<pid>/stat (after the last ')').
func startTime(pid int) (uint64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, errors.New("malformed stat")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return 0, errors.New("short stat")
	}
	return strconv.ParseUint(f[19], 10, 64)
}
