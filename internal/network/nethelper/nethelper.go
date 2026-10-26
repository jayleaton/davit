// Package nethelper is the single entry point for the internal commands the
// rootless network provider runs by re-executing the current binary: the
// userspace egress stack (userspace.CmdEgress, userspace.CmdTapFD), the
// allowlist DNS resolver (netnsdns.Cmd) and the TAP-service listener
// (netnslisten.Cmd).
package nethelper

import (
	"os"
	"sync/atomic"

	"davit/internal/network/netnsdns"
	"davit/internal/network/netnslisten"
	"davit/internal/network/userspace"
)

var served atomic.Bool

// Serve makes this executable a network helper: when os.Args[1] names one
// of the internal commands it runs it and exits; otherwise it records that
// the executable serves them and returns. Call it first in main, and in
// TestMain (or init) of tests that create rootless networks.
//
// The network package re-executes only an executable that has called
// Serve. Any other binary would misread the command and do something else
// entirely: a Go test binary, for one, runs its whole test suite, here
// inside a VM's namespaces and its own session, where it boots VMs that
// outlive the test.
func Serve() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case userspace.CmdEgress, userspace.CmdTapFD:
			os.Exit(userspace.Main(os.Args[1:], os.Stderr))
		case netnsdns.Cmd:
			os.Exit(netnsdns.Main(os.Args[1:], os.Stderr))
		case netnslisten.Cmd:
			os.Exit(netnslisten.Main(os.Args[1:], os.Stderr))
		}
	}
	served.Store(true)
}

// Served reports whether this executable called Serve.
func Served() bool { return served.Load() }
