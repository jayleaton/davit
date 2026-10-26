//go:build !linux

package firecracker

import (
	"context"
	"net"
)

func dialUnix(ctx context.Context, path string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", path)
}
