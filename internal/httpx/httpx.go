// Package httpx holds the limits every daemon HTTP server applies (SEC-11).
package httpx

import (
	"net/http"
	"time"
)

// Limits of Harden. Long-lived streams (WebSockets, SSE, long polls) are
// hijacked or written continuously, so no ReadTimeout or WriteTimeout is
// set; request bodies are bounded by each handler's MaxBytesReader.
const (
	ReadHeaderTimeout = 10 * time.Second
	IdleTimeout       = 2 * time.Minute
	MaxHeaderBytes    = 64 << 10
)

// Harden bounds header read time, idle keep-alive connections and header
// size of srv, keeping any stricter value already set.
func Harden(srv *http.Server) *http.Server {
	if srv.ReadHeaderTimeout == 0 || srv.ReadHeaderTimeout > ReadHeaderTimeout {
		srv.ReadHeaderTimeout = ReadHeaderTimeout
	}
	if srv.IdleTimeout == 0 || srv.IdleTimeout > IdleTimeout {
		srv.IdleTimeout = IdleTimeout
	}
	if srv.MaxHeaderBytes == 0 || srv.MaxHeaderBytes > MaxHeaderBytes {
		srv.MaxHeaderBytes = MaxHeaderBytes
	}
	return srv
}
