package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/harness"
)

// SetPrompts enables the prompt and conversation endpoints. Without it
// they answer 422 unsupported_capability.
func (s *Server) SetPrompts(h *harness.Service) { s.prompts = h }

func (s *Server) promptService() (*harness.Service, error) {
	if s.prompts == nil {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "this runtime has no guest transport").
			WithDetail("capability", "guestTransport")
	}
	return s.prompts, nil
}

func (s *Server) createPrompt(w http.ResponseWriter, r *http.Request) error {
	var body api.PromptRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	if err := s.mayUseDaemonSecrets(r, r.PathValue("vmId"), body.Secrets); err != nil {
		return err
	}
	h, err := s.promptService()
	if err != nil {
		return err
	}
	p, err := h.Prompt(r.Context(), principal(r), r.PathValue("vmId"), r.Header.Get(api.HeaderIdempotencyKey), body)
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/vms/"+string(p.VMID)+"/prompts/"+p.ID)
	return writeJSON(w, http.StatusCreated, p)
}

func (s *Server) getPrompt(w http.ResponseWriter, r *http.Request) error {
	h, err := s.promptService()
	if err != nil {
		return err
	}
	p, err := h.Get(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("promptId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

func (s *Server) cancelPrompt(w http.ResponseWriter, r *http.Request) error {
	h, err := s.promptService()
	if err != nil {
		return err
	}
	p, err := h.Cancel(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("promptId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

func (s *Server) listPromptEvents(w http.ResponseWriter, r *http.Request) error {
	h, err := s.promptService()
	if err != nil {
		return err
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			return domain.Errorf(domain.CodeInvalidArgument, "after must be a non-negative integer").WithDetail("field", "after")
		}
	}
	wait, err := eventWait(r)
	if err != nil {
		return err
	}
	out, err := h.Events(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("promptId"), after, wait)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

// eventWait parses a long-poll's waitMs.
func eventWait(r *http.Request) (time.Duration, error) {
	v := r.URL.Query().Get("waitMs")
	if v == "" {
		return 0, nil
	}
	waitMs, err := strconv.Atoi(v)
	if err != nil || waitMs < 0 || time.Duration(waitMs)*time.Millisecond > MaxEventWait {
		return 0, domain.Errorf(domain.CodeInvalidArgument, "waitMs must be 0..%d", MaxEventWait.Milliseconds()).WithDetail("field", "waitMs")
	}
	return time.Duration(waitMs) * time.Millisecond, nil
}

func (s *Server) listVMPromptEvents(w http.ResponseWriter, r *http.Request) error {
	h, err := s.promptService()
	if err != nil {
		return err
	}
	p, err := page(r)
	if err != nil {
		return err
	}
	wait, err := eventWait(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	out, err := h.Feed(r.Context(), principal(r), r.PathValue("vmId"), q.Get("after"), q.Get("conversationId"), p.Limit, wait)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) interruptPrompts(w http.ResponseWriter, r *http.Request) error {
	h, err := s.promptService()
	if err != nil {
		return err
	}
	var body api.InterruptRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	out, err := h.Interrupt(r.Context(), principal(r), r.PathValue("vmId"), body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) steerPrompt(w http.ResponseWriter, r *http.Request) error {
	h, err := s.promptService()
	if err != nil {
		return err
	}
	var body api.SteerRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	// A restart steer replays the prompt's daemon secrets into a new turn.
	if names, err := h.PromptSecretNames(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("promptId")); err != nil {
		return err
	} else if err := s.mayUseDaemonSecrets(r, r.PathValue("vmId"), names); err != nil {
		return err
	}
	out, err := h.Steer(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("promptId"), r.Header.Get(api.HeaderIdempotencyKey), body.Text)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) error {
	h, err := s.promptService()
	if err != nil {
		return err
	}
	p, err := page(r)
	if err != nil {
		return err
	}
	items, next, err := h.Conversations(r.Context(), principal(r), r.PathValue("vmId"), p.Limit, p.Cursor)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, api.ConversationList{Items: items, NextCursor: next})
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) error {
	h, err := s.promptService()
	if err != nil {
		return err
	}
	c, err := h.Conversation(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("conversationId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, c)
}
