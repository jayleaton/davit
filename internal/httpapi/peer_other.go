//go:build !linux && !darwin

package httpapi

import (
	"errors"
	"net"
)

var errNoPeerCred = errors.New("httpapi: peer credentials not supported on this OS")

// peerUID fails closed where peer credentials are unavailable.
func peerUID(*net.UnixConn) (int, error) { return 0, errNoPeerCred }
