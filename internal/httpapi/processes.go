package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestexec"
	"davit/internal/wsock"
)

// SetProcesses enables the process endpoints. Without it they answer 422
// unsupported_capability (the runtime has no guest transport).
func (s *Server) SetProcesses(m *guestexec.Manager) { s.procs = m }

func (s *Server) processes() (*guestexec.Manager, error) {
	if s.procs == nil {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "this runtime has no guest transport").
			WithDetail("capability", "guestTransport")
	}
	return s.procs, nil
}

func (s *Server) listProcesses(w http.ResponseWriter, r *http.Request) error {
	m, err := s.processes()
	if err != nil {
		return err
	}
	p, err := page(r)
	if err != nil {
		return err
	}
	items, next, err := m.List(r.Context(), principal(r), r.PathValue("vmId"), p.Limit, p.Cursor)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, api.ProcessList{Items: items, NextCursor: next})
}

func (s *Server) execProcess(w http.ResponseWriter, r *http.Request) error {
	m, err := s.processes()
	if err != nil {
		return err
	}
	var body api.ExecRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	p, err := m.Exec(r.Context(), principal(r), r.PathValue("vmId"), r.Header.Get(api.HeaderIdempotencyKey), body)
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/vms/"+string(p.VMID)+"/processes/"+p.ID)
	return writeJSON(w, http.StatusCreated, p)
}

func (s *Server) getProcess(w http.ResponseWriter, r *http.Request) error {
	m, err := s.processes()
	if err != nil {
		return err
	}
	p, err := m.Get(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("processId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

func (s *Server) killProcess(w http.ResponseWriter, r *http.Request) error {
	m, err := s.processes()
	if err != nil {
		return err
	}
	sig := r.URL.Query().Get("signal")
	if sig == "" {
		sig = "SIGTERM"
	}
	p, err := m.Signal(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("processId"), sig)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

// streamWriteTimeout bounds one message write to a slow client.
const streamWriteTimeout = 30 * time.Second

// attachProcess upgrades to the WebSocket stream described in
// api/openapi.yaml. Authorization and process lookup happen before the
// upgrade so failures are ordinary JSON errors.
func (s *Server) attachProcess(w http.ResponseWriter, r *http.Request) error {
	m, err := s.processes()
	if err != nil {
		return err
	}
	var after int64
	if v := r.URL.Query().Get("outputCursor"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			return domain.Errorf(domain.CodeInvalidArgument, "outputCursor must be a non-negative integer").WithDetail("field", "outputCursor")
		}
	}
	who, vm, id := principal(r), r.PathValue("vmId"), r.PathValue("processId")
	if _, err := m.Get(r.Context(), who, vm, id); err != nil {
		return err
	}
	ws, err := wsock.Upgrade(w, r)
	if err != nil {
		if errors.Is(err, wsock.ErrBadHandshake) {
			return domain.Errorf(domain.CodeInvalidArgument, "attachProcess needs a WebSocket upgrade: %v", err)
		}
		return err
	}
	defer ws.Close(wsock.CloseNormal, "")
	ws.SetReadLimit(1 << 20)

	// A hijacked request's context does not end when the client goes away;
	// the read loop cancels ctx when the stream closes.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	send := func(msg api.ProcessStreamMessage) error {
		b, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		_ = ws.NetConn().SetWriteDeadline(time.Now().Add(streamWriteTimeout))
		return ws.WriteMessage(wsock.TextMessage, b)
	}
	err = m.Attach(ctx, who, vm, id, after, send, func(st guestexec.Stream) {
		defer cancel()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var msg api.ProcessStreamMessage
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			s.streamInput(ctx, st, msg)
		}
	})
	if err != nil {
		de := domain.AsError(err)
		_ = send(api.ProcessStreamMessage{Type: api.StreamError, Error: &api.Error{Code: de.Code, Message: de.Message, Retryable: de.Retryable}})
	}
	return nil
}

// streamInput applies one client message. Input for a stream that does not
// own stdin is reported and dropped; unknown types are ignored.
func (s *Server) streamInput(ctx context.Context, st guestexec.Stream, msg api.ProcessStreamMessage) {
	fail := func(code domain.Code, text string) {
		_ = st.Send(api.ProcessStreamMessage{Type: api.StreamError, Error: &api.Error{Code: code, Message: text}})
	}
	switch msg.Type {
	case api.StreamStdin, api.StreamStdinClose:
		if st.Stdin == nil {
			fail(domain.CodeInvalidState, "this stream does not own the process's stdin")
			return
		}
		if msg.Type == api.StreamStdinClose {
			_ = st.Stdin.Close()
			return
		}
		data, err := base64.StdEncoding.DecodeString(msg.Data)
		if err != nil {
			fail(domain.CodeInvalidArgument, "stdin data must be base64")
			return
		}
		if _, err := st.Stdin.Write(data); err != nil {
			fail(domain.CodeUnavailable, "stdin: "+err.Error())
		}
	case api.StreamSignal:
		if err := st.Signal(ctx, msg.Signal); err != nil {
			de := domain.AsError(err)
			fail(de.Code, de.Message)
		}
	}
}
