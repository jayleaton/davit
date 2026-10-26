package httpapi

import (
	"errors"
	"io"
	"net/http"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/wsock"
)

// tunnelChunk is the largest binary message the server sends.
const tunnelChunk = 32 << 10

func (s *Server) openTunnel(w http.ResponseWriter, r *http.Request) error {
	m, err := s.processes()
	if err != nil {
		return err
	}
	var body api.OpenTunnelRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	t, err := m.OpenTunnel(r.Context(), principal(r), r.PathValue("vmId"), body.Port, r.Header.Get(api.HeaderIdempotencyKey))
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/tunnels/"+t.ID)
	return writeJSON(w, http.StatusCreated, t)
}

func (s *Server) closeTunnel(w http.ResponseWriter, r *http.Request) error {
	m, err := s.processes()
	if err != nil {
		return err
	}
	if err := m.CloseTunnel(principal(r), r.PathValue("tunnelId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// attachTunnel claims the tunnel and relays its bytes as binary WebSocket
// messages. An empty binary message from the client half-closes the guest
// side; the WebSocket closes when the guest side ends.
func (s *Server) attachTunnel(w http.ResponseWriter, r *http.Request) error {
	m, err := s.processes()
	if err != nil {
		return err
	}
	conn, _, err := m.AttachTunnel(principal(r), r.PathValue("tunnelId"))
	if err != nil {
		return err
	}
	defer conn.Close()
	ws, err := wsock.Upgrade(w, r)
	if err != nil {
		if errors.Is(err, wsock.ErrBadHandshake) {
			return domain.Errorf(domain.CodeInvalidArgument, "attachTunnel needs a WebSocket upgrade: %v", err)
		}
		return err
	}
	ws.SetReadLimit(1 << 20)

	go func() {
		defer conn.Close() // the client went away: end the guest side too
		for {
			typ, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if typ != wsock.BinaryMessage {
				continue
			}
			if len(data) == 0 {
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
				continue
			}
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}()
	buf := make([]byte, tunnelChunk)
	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			_ = ws.NetConn().SetWriteDeadline(time.Now().Add(streamWriteTimeout))
			if err := ws.WriteMessage(wsock.BinaryMessage, buf[:n]); err != nil {
				return nil
			}
		}
		if rerr != nil {
			code := wsock.CloseNormal
			if !errors.Is(rerr, io.EOF) {
				code = wsock.CloseGoingAway
			}
			_ = ws.Close(code, "")
			return nil
		}
	}
}
