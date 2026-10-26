package guestclient

import (
	"fmt"
	"slices"
	"strings"

	"davit/internal/guestproto"
)

// AgentProtocol is what an image manifest records about its baked guest
// agent (images/ manifest guest.agent.protocol): the protocol versions and
// features the agent speaks.
type AgentProtocol struct {
	Min      uint16   `json:"min"`
	Max      uint16   `json:"max"`
	Features []string `json:"features"`
}

// Compat grades a baked agent against this host before anything boots it.
type Compat int

const (
	// CompatOK: the protocol ranges overlap and the agent has every feature this
	// host offers.
	CompatOK Compat = iota
	// CompatDegraded ("minor" skew): the handshake will succeed, but the agent
	// lacks features this host offers (or the manifest predates protocol
	// tracking); the affected operations fail at use.
	CompatDegraded
	// CompatIncompatible ("major" skew): no common protocol version, so the
	// handshake and with it every boot's restore barrier would fail.
	CompatIncompatible
)

// CheckAgent compares a manifest's agent protocol with this host's
// (guestproto.SupportedVersions, Features). p nil means the manifest records
// none. The message says what to do about anything but CompatOK.
func CheckAgent(p *AgentProtocol) (Compat, string) {
	host := guestproto.SupportedVersions
	if p == nil {
		return CompatDegraded, "image manifest records no guest-agent protocol (built before protocol tracking); " +
			"compatibility is only known at the first handshake: rebuild the image (images/build.sh)"
	}
	if p.Min < 1 || p.Min > p.Max {
		return CompatIncompatible, fmt.Sprintf("image manifest records an invalid guest-agent protocol range %d-%d", p.Min, p.Max)
	}
	if p.Max < host.Min || p.Min > host.Max {
		return CompatIncompatible, fmt.Sprintf("baked guest agent speaks protocol %d-%d, this host speaks %d-%d: "+
			"rebuild the image with this davit version (images/build.sh) or run a davit that matches the image",
			p.Min, p.Max, host.Min, host.Max)
	}
	var missing []string
	for _, f := range Features {
		if !slices.Contains(p.Features, f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return CompatDegraded, fmt.Sprintf("baked guest agent lacks features this host uses (%s); those operations fail on its VMs: "+
			"rebuild the image with this davit version (images/build.sh)", strings.Join(missing, ", "))
	}
	return CompatOK, ""
}
