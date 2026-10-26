package firecracker

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// VsockDialer reaches guest vsock ports through Firecracker's hybrid vsock:
// connect to the VMM's Unix socket, send "CONNECT <port>\n", expect
// "OK <host port>\n", then the stream is the guest connection. It satisfies
// runtime.Dialer.
type VsockDialer struct{ Path string }

// DialGuest opens a stream to guest port.
func (d VsockDialer) DialGuest(ctx context.Context, port uint32) (net.Conn, error) {
	c, err := dialUnix(ctx, d.Path)
	if err != nil {
		return nil, err
	}
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > 10*time.Second {
		dl = time.Now().Add(10 * time.Second)
	}
	c.SetDeadline(dl)
	// Cancellation interrupts the handshake too, not only the deadline.
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if _, err := fmt.Fprintf(c, "CONNECT %d\n", port); err != nil {
		c.Close()
		return nil, err
	}
	// Read the reply a byte at a time so no guest bytes are consumed.
	var line []byte
	b := make([]byte, 1)
	for len(line) < 64 {
		if _, err := c.Read(b); err != nil {
			c.Close()
			return nil, fmt.Errorf("vsock port %d: %w", port, err)
		}
		if b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	if !strings.HasPrefix(string(line), "OK ") {
		c.Close()
		return nil, fmt.Errorf("vsock port %d: unexpected reply %q", port, line)
	}
	if !stop() {
		c.Close()
		return nil, ctx.Err()
	}
	c.SetDeadline(time.Time{})
	return c, nil
}
