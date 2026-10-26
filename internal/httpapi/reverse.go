package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/reversefwd"
	"davit/internal/wsock"
)

// SetReverse enables the reverse forward operations (feature
// reverseForward). Without it they answer unsupported_capability.
func (s *Server) SetReverse(r *reversefwd.Service) { s.reverse = r }

type reverseRoute struct {
	pattern string
	need    access
	h       handler
}

// reverseRoutes are owner or admin-token only: a reverse forward reaches
// out of the VM to the caller's machine or a host service.
func (s *Server) reverseRoutes() []reverseRoute {
	b := api.BasePath + "/vms/{vmId}/reverse-forwards"
	admin := onVM(api.TokenAdmin)
	return []reverseRoute{
		{"GET " + b, admin, s.listReverseForwards},
		{"POST " + b, admin, s.createReverseForward},
		{"DELETE " + b + "/{forwardId}", admin, s.revokeReverseForward},
		{"GET " + b + "/{forwardId}/session", admin, s.attachReverseSession},
		{"GET " + b + "/{forwardId}/connections/{connectionId}", admin, s.attachReverseConnection},
	}
}

func (s *Server) listReverseForwards(w http.ResponseWriter, r *http.Request) error {
	items, err := s.reverse.List(r.Context(), principal(r), r.PathValue("vmId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, api.ReverseForwardList{Items: items})
}

func (s *Server) createReverseForward(w http.ResponseWriter, r *http.Request) error {
	var body api.CreateReverseForwardRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	// The grant outlives this request.
	fw, err := s.reverse.Create(context.WithoutCancel(r.Context()), principal(r), r.PathValue("vmId"), body)
	if err != nil {
		return err
	}
	if g := memberOf(r); g != nil && s.sessions != nil {
		// A member's forward goes when its share does.
		p, id := principal(r), fw.ID
		s.sessions.Track(g.Member.UserID, fw.VMID, closerFunc(func() error {
			return s.reverse.Revoke(context.Background(), p, string(fw.VMID), id)
		}))
	}
	w.Header().Set("Location", api.BasePath+"/vms/"+string(fw.VMID)+"/reverse-forwards/"+fw.ID)
	return writeJSON(w, http.StatusCreated, fw)
}

func (s *Server) revokeReverseForward(w http.ResponseWriter, r *http.Request) error {
	if err := s.reverse.Revoke(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("forwardId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func upgradeReverse(w http.ResponseWriter, r *http.Request, op string) (*wsock.Conn, error) {
	ws, err := wsock.Upgrade(w, r)
	if errors.Is(err, wsock.ErrBadHandshake) {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "%s needs a WebSocket upgrade: %v", op, err)
	}
	return ws, err
}

// attachReverseSession sends the session's events as JSON text messages.
// The client sends nothing; its going away revokes the forward.
func (s *Server) attachReverseSession(w http.ResponseWriter, r *http.Request) error {
	p, vm, id := principal(r), r.PathValue("vmId"), r.PathValue("forwardId")
	// Refuse before upgrading when the forward is not attachable.
	if fw, err := s.reverse.Get(r.Context(), p, vm, id); err != nil {
		return err
	} else if fw.Target.Kind != api.ReverseTargetSession || fw.State != api.ReverseAwaitingSession {
		return domain.Errorf(domain.CodeInvalidState, "reverse forward %s is not a session forward awaiting its session", id)
	}
	ws, err := upgradeReverse(w, r, "attachReverseSession")
	if err != nil {
		return err
	}
	defer ws.Close(wsock.CloseNormal, "")
	ws.SetReadLimit(4 << 10)
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	go func() {
		defer cancel()
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()
	send := func(ev api.ReverseSessionEvent) error {
		b, _ := json.Marshal(ev)
		_ = ws.NetConn().SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		return ws.WriteMessage(wsock.TextMessage, b)
	}
	if _, err := s.reverse.RunSession(ctx, p, vm, id, send); err != nil {
		// Already upgraded: the error travels as the revoked event.
		var de *domain.Error
		msg := err.Error()
		if errors.As(err, &de) {
			msg = string(de.Code) + ": " + de.Message
		}
		_ = send(api.ReverseSessionEvent{Type: api.ReverseEventRevoked, Reason: msg})
	}
	return nil
}

// attachReverseConnection carries one announced guest connection.
func (s *Server) attachReverseConnection(w http.ResponseWriter, r *http.Request) error {
	p, vm, id, cid := principal(r), r.PathValue("vmId"), r.PathValue("forwardId"), r.PathValue("connectionId")
	if _, err := s.reverse.Get(r.Context(), p, vm, id); err != nil {
		return err
	}
	ws, err := upgradeReverse(w, r, "attachReverseConnection")
	if err != nil {
		return err
	}
	ws.SetReadLimit(1 << 20)
	st := wsock.NewStream(ws, streamWriteTimeout)
	if err := s.reverse.Connect(r.Context(), p, vm, id, cid, st); err != nil {
		_ = ws.Close(wsock.CloseGoingAway, err.Error())
		return nil
	}
	<-st.Done() // the bridge owns st until it closes it
	return nil
}
