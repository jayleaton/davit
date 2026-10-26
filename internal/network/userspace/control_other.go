//go:build !linux

package userspace

import (
	"context"
	"errors"
	"net"
)

// dialControl fails: control sockets are Linux abstract unix sockets.
func dialControl(context.Context, string) (net.Conn, error) {
	return nil, errors.New("userspace egress runs only on Linux")
}
