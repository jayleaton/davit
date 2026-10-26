package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/wsock"
)

// DesktopWebSocketPath is the websockify-compatible RFB stream of a desktop
// endpoint (noVNC's default path).
const DesktopWebSocketPath = "/websockify"

// DesktopStats counts what desktop viewers did, for tests and diagnostics.
type DesktopStats struct {
	Sessions     atomic.Int64
	DroppedInput atomic.Int64
}

// desktopHandler serves a desktop route: the viewer page at / and the RFB
// stream at /websockify, both after the same identity and authorization as
// any app endpoint. View rights are view-only: the gateway drops the
// viewer's input messages.
func (s *appServer) desktopHandler(r AppRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		who, t, level, ok := s.authorize(w, req, r)
		if !ok {
			return
		}
		defer s.track(req, who, t)() // unsharing cuts a member's viewer
		viewOnly := levelRank(level) < levelRank(api.TokenExec)
		switch {
		case strings.HasSuffix(req.URL.Path, DesktopWebSocketPath):
			s.desktopStream(w, req, r, who, t, viewOnly)
		case req.URL.Path == "/" || req.URL.Path == "/index.html" || req.URL.Path == "/vnc.html":
			if req.Method != http.MethodGet && req.Method != http.MethodHead {
				appError(w, http.StatusMethodNotAllowed, domain.CodeInvalidArgument, "GET the viewer")
				return
			}
			page := []byte("desktop viewer unavailable")
			if s.g.opts.DesktopPage != nil {
				page = s.g.opts.DesktopPage(viewOnly)
			}
			h := w.Header()
			h.Set("Content-Type", "text/html; charset=utf-8")
			h.Set("Cache-Control", "no-store")
			// Self-contained page: nothing but its own origin's WebSocket.
			h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; "+
				"img-src data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			w.Write(page)
		default:
			appError(w, http.StatusNotFound, domain.CodeNotFound, "the desktop serves its viewer at / and RFB at "+DesktopWebSocketPath)
		}
	})
}

// desktopStream bridges one viewer WebSocket to the guest's VNC server.
func (s *appServer) desktopStream(w http.ResponseWriter, req *http.Request, r AppRoute, who Identity, t Target, viewOnly bool) {
	// The guest side first, so a missing VNC server is a plain 502.
	dctx, cancel := context.WithTimeout(context.WithValue(req.Context(), ctxKeyApp{}, appTarget{t: t, route: r}), 10*time.Second)
	guest, err := s.dial(dctx, "tcp", "")
	cancel()
	if err != nil {
		s.g.opts.Logf("gateway: desktop %s: dial guest VNC port %d: %v", r.ID, r.GuestPort, err)
		appError(w, http.StatusBadGateway, domain.CodeUnavailable, "no VNC server answers on the guest's port; is the desktop running?")
		return
	}
	ws, _, err := wsock.UpgradeProtocol(w, req, "binary")
	if err != nil {
		guest.Close()
		if errors.Is(err, wsock.ErrBadHandshake) {
			appError(w, http.StatusBadRequest, domain.CodeInvalidArgument, "the desktop stream needs a WebSocket upgrade: "+err.Error())
		}
		return
	}
	ws.SetReadLimit(1 << 20)
	stats := s.g.opts.DesktopStats
	if stats != nil {
		stats.Sessions.Add(1)
	}
	onDrop := func() {
		if stats != nil {
			stats.DroppedInput.Add(1)
		}
	}
	if err := ProxyRFB(wsock.NewStream(ws, 30*time.Second), guest, viewOnly, onDrop); err != nil {
		s.g.opts.Logf("gateway: desktop %s (%s): %v", r.ID, who, err)
	}
}
