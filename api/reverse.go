package api

import (
	"time"

	"davit/internal/domain"
)

// G10 reverse forwards (feature reverseForward): a guest loopback port whose
// connections reach a far end the owner fixed when creating the forward,
// never one the guest chooses.

// ReverseTargetKind is the far end of a reverse forward.
type ReverseTargetKind string

const (
	// ReverseTargetSession: the client that created the forward (a laptop
	// or CI runner) holds attachReverseSession open and carries each guest
	// connection to a service it can reach (attachReverseConnection).
	ReverseTargetSession ReverseTargetKind = "session"
	// ReverseTargetHost: a host service the operator listed in the daemon
	// config (network.reverseHostServices); the daemon dials it.
	ReverseTargetHost ReverseTargetKind = "host"
)

// ReverseForwardState says whether a forward carries connections.
type ReverseForwardState string

const (
	// ReverseAwaitingSession: a session forward whose session is not
	// attached yet (until sessionExpiresAt, then it is revoked).
	ReverseAwaitingSession ReverseForwardState = "awaiting_session"
	// ReverseActive: guest connections are bridged.
	ReverseActive ReverseForwardState = "active"
)

// ReverseTarget is a reverse forward's far end.
type ReverseTarget struct {
	Kind ReverseTargetKind `json:"kind"`
	// Address is the host service (ip:port, exactly as listed) for kind
	// host. For kind session it is an optional label of where the client
	// forwards, shown in lists; the daemon never dials it.
	Address string `json:"address,omitempty"`
}

// CreateReverseForwardRequest is the body of createReverseForward.
type CreateReverseForwardRequest struct {
	GuestPort int           `json:"guestPort"`
	Target    ReverseTarget `json:"target"`
}

// ReverseForward is one open reverse forward of a VM generation. Stopping or
// deleting the VM, or a new generation, revokes it.
type ReverseForward struct {
	ID         string              `json:"id"`
	VMID       domain.VMID         `json:"vmId"`
	Generation int64               `json:"generation"`
	GuestPort  int                 `json:"guestPort"`
	Target     ReverseTarget       `json:"target"`
	State      ReverseForwardState `json:"state"`
	// Active is the number of guest connections bridged right now.
	Active    int       `json:"active"`
	CreatedAt time.Time `json:"createdAt"`
	// SessionExpiresAt is when an unattached session forward is revoked.
	SessionExpiresAt *time.Time `json:"sessionExpiresAt,omitempty"`
}

// ReverseForwardList is a VM's open reverse forwards.
type ReverseForwardList struct {
	Items []ReverseForward `json:"items"`
}

// ReverseSessionEventType is the type of a session stream message.
type ReverseSessionEventType string

const (
	// ReverseEventConnection: a guest process connected; claim it with
	// attachReverseConnection(connectionId) within the dial window.
	ReverseEventConnection ReverseSessionEventType = "connection"
	// ReverseEventPing keeps an idle session alive (every 20 s).
	ReverseEventPing ReverseSessionEventType = "ping"
	// ReverseEventRevoked: the forward ended (reason says why); the server
	// closes the stream next.
	ReverseEventRevoked ReverseSessionEventType = "revoked"
)

// ReverseSessionEvent is one JSON text message on attachReverseSession.
type ReverseSessionEvent struct {
	Type         ReverseSessionEventType `json:"type"`
	ConnectionID string                  `json:"connectionId,omitempty"`
	Reason       string                  `json:"reason,omitempty"`
}
