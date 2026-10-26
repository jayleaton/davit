// Command davit is the single host executable: CLI verbs, `daemon`, and
// the `guest-agent` entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"davit/internal/cli"
	"davit/internal/config"
	"davit/internal/daemon"
	"davit/internal/guestagent"
	"davit/internal/network/nethelper"
)

func main() {
	// Internal: this binary is the rootless network helper (guest egress,
	// allowlist DNS); the network package re-executes it for those.
	nethelper.Serve()
	// Images install this binary as /usr/local/bin/davit-guest-agent and
	// their unit runs it without arguments; images built before the davit
	// rename name it slipway-guest-agent.
	if b := filepath.Base(os.Args[0]); b == "davit-guest-agent" || b == "slipway-guest-agent" {
		os.Exit(guestagent.Main(os.Args[1:], os.Stderr))
	}
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "daemon":
			os.Exit(runDaemon(args[1:]))
		case "guest-agent":
			os.Exit(guestagent.Main(args[1:], os.Stderr))
		}
	}
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), ioctlGetTermios)
	os.Exit(cli.Main(args, cli.Stdio{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Interactive: err == nil}))
}

func runDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration file (JSON); default "+config.DefaultConfigPath()+" if present")
	if err := fs.Parse(args); err != nil {
		return cli.ExitUsage
	}
	cfg, _, err := config.LoadDefault(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return cli.ExitUsage
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, cfg, log); err != nil {
		log.Error("daemon exited", "err", err)
		return cli.ExitGeneral
	}
	return cli.ExitOK
}
