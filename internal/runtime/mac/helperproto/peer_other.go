//go:build !linux && !darwin

package helperproto

import "net"

// PeerUID is unavailable here: helpers run only on macOS (and Linux tests).
func PeerUID(net.Conn) (int, error) {
	return -1, Errorf(CodeUnauthorized, "peer credentials are not supported on this platform")
}
