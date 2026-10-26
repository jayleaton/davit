// Package gateway is the host's tailnet front door (SPEC §4 "Authentication
// and authorization", §5 "Tailnet gateway and per-VM names").
//
// One persistent tsnet node serves the HTTP API, an SSH front door and TCP
// forwards. Every accepted connection is identified with LocalClient.WhoIs on
// the actual peer address and fails closed; VM names, SSH usernames and
// headers are routing parameters, never identity.
//
// The package defines the narrow interfaces it consumes (Authorizer,
// GuestDialer, GuestSSH) so it does not depend on lifecycle, runtime or state
// internals; the daemon wires them to those services.
package gateway

import (
	"context"
	"errors"
	"net"

	"golang.org/x/crypto/ssh"

	"davit/internal/domain"
)

// Action is what a principal wants to do with a VM.
type Action string

// Actions the gateway authorizes. The HTTP API authorizes its own actions
// downstream; the gateway only authenticates those requests.
const (
	ActionSSH     Action = "ssh"
	ActionForward Action = "forward"
)

// Target is an authorized, resolved VM. Generation pins the instance the
// grant was issued for, so a stale grant cannot reach a replacement VM.
type Target struct {
	VMID       domain.VMID
	Generation int64
}

// Request is one authorization question.
type Request struct {
	// VMRef is the user-supplied VM name or ID. It is a routing parameter
	// only; the Authorizer resolves it within the principal's scope.
	VMRef  string
	Action Action
	// Port is the guest TCP port for ActionForward (22 for ActionSSH).
	Port uint16
}

// Authorizer maps a tailnet identity to the VMs it may use. Implementations
// must return ErrDenied or ErrNotFound (wrapped is fine) rather than leaking
// whether an invisible VM exists, and must re-check on every call: grants can
// be revoked between connections.
type Authorizer interface {
	Authorize(ctx context.Context, who Identity, req Request) (Target, error)
}

// GuestDialer opens a stream to a TCP port on the guest's loopback, carried
// over the VM's vsock/guest-agent transport, never a routable guest address.
// It must refuse a Target whose generation is no longer current. As with
// net.Dialer.DialContext, ctx bounds only the dial, not the stream.
type GuestDialer interface {
	DialGuest(ctx context.Context, t Target, port uint16) (net.Conn, error)
}

// GuestSSH supplies how the shared SSH front door logs in to a guest's sshd:
// a short-lived credential scoped to the VM, generation and guest user, and a
// HostKeyCallback that verifies the guest host key against the VM's
// registered identity record. Never return ssh.InsecureIgnoreHostKey.
// ClientConfig.User is the guest account to log in as; empty means guestUser.
// Implementations may resolve DefaultGuestUser to the image's account.
type GuestSSH interface {
	ClientConfig(ctx context.Context, who Identity, t Target, guestUser string) (*ssh.ClientConfig, error)
}

// Errors returned by Authorizer implementations and surfaced by the gateway.
var (
	ErrDenied        = errors.New("gateway: access denied")
	ErrNotFound      = errors.New("gateway: vm not found")
	ErrUnidentified  = errors.New("gateway: peer has no tailnet identity")
	ErrInvalidTarget = errors.New("gateway: invalid target")
	// ErrNotRunning: authorized, but the VM is not ready/running.
	ErrNotRunning = errors.New("gateway: vm is not running")
	// ErrNoGuestTransport: the runtime provides no guest dialer yet.
	ErrNoGuestTransport = errors.New("gateway: guest transport unavailable")
)

// noGuestTransport is the GuestDialer used when none is injected: every
// authorized stream fails cleanly instead of the gateway refusing to start.
type noGuestTransport struct{}

func (noGuestTransport) DialGuest(context.Context, Target, uint16) (net.Conn, error) {
	return nil, ErrNoGuestTransport
}
