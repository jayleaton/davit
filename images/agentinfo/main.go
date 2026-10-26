//go:build linux

// Command agentinfo prints the guest-agent protocol of this checkout as one
// line of JSON, for images/scripts/build-agent.sh to record in image
// manifests (guest.agent.protocol). The build runs it from the same commit
// as the agent binary and cross-checks the version range against the
// binary's own `guest-agent -version`; smoke checks the live handshake.
package main

import (
	"encoding/json"
	"os"
	"slices"

	"davit/internal/guestagent"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
)

func main() {
	features := slices.Clone(guestagent.Features)
	slices.Sort(features)
	_ = json.NewEncoder(os.Stdout).Encode(guestclient.AgentProtocol{
		Min: guestproto.MinProtocolVersion, Max: guestproto.ProtocolVersion, Features: features,
	})
}
