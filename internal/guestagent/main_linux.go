//go:build linux

package guestagent

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"davit/internal/buildinfo"
	"davit/internal/guestproto"
)

// Main runs `davit guest-agent [flags]` and returns the exit status.
func Main(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("guest-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.Bool("version", false, "print agent and protocol version")
	listen := fs.String("listen", "vsock:"+strconv.Itoa(int(Port)), "vsock:<port> or unix:<path>")
	root := fs.String("root", "/", "root for identity files (machine-id, hostname, ssh)")
	runDir := fs.String("run-dir", "/run/davit-agent", "tmpfs directory for the per-generation secret")
	user := fs.String("default-user", "", "user for exec requests that name none (default: the agent's user)")
	sshUnit := fs.String("ssh-unit", "ssh.service", `unit restarted after host-key changes ("-" disables)`)
	midUnits := fs.String("machine-id-units", "systemd-journald.service", "comma-separated units restarted after machine-id rotation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *version {
		fmt.Fprintf(os.Stdout, "%s guest-agent %s protocol %d-%d\n", buildinfo.Name, buildinfo.Version, guestproto.MinProtocolVersion, guestproto.ProtocolVersion)
		return 0
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	units := []string{}
	for _, u := range strings.Split(*midUnits, ",") {
		if u = strings.TrimSpace(u); u != "" {
			units = append(units, u)
		}
	}
	a, err := New(Config{Root: *root, RunDir: *runDir, System: OSSystem{}, DefaultUser: *user,
		SSHUnit: *sshUnit, MachineIDUnits: units, Log: log})
	if err != nil {
		log.Error("start", "err", err)
		return 1
	}
	l, err := listenOn(*listen)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("guest agent listening", "addr", l.Addr().String(), "version", buildinfo.Version)
	if err := a.Serve(ctx, l); err != nil {
		log.Error("serve", "err", err)
		return 1
	}
	return 0
}

func listenOn(spec string) (net.Listener, error) {
	kind, arg, ok := strings.Cut(spec, ":")
	switch {
	case ok && kind == "vsock":
		p, err := strconv.ParseUint(arg, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bad vsock port %q", arg)
		}
		return ListenVsock(uint32(p))
	case ok && kind == "unix":
		_ = os.Remove(arg)
		return net.Listen("unix", arg)
	}
	return nil, fmt.Errorf("listen address %q must be vsock:<port> or unix:<path>", spec)
}
