package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/events"
	"davit/internal/webhooks"
)

// G16: the host event stream and webhooks.

// SetEvents enables listEvents.
func (s *Server) SetEvents(e *events.Service) { s.events = e }

// SetWebhooks enables the webhook operations.
func (s *Server) SetWebhooks(w *webhooks.Service) { s.webhooks = w }

func (s *Server) eventsRoutes(route func(string, access, handler)) {
	b := api.BasePath
	route("GET "+b+"/events", allVMs(api.TokenView), s.listEvents)
	route("GET "+b+"/webhooks", ownerOnly, s.listWebhooks)
	route("POST "+b+"/webhooks", ownerOnly, s.createWebhook)
	route("GET "+b+"/webhooks/{webhookId}", ownerOnly, s.getWebhook)
	route("PATCH "+b+"/webhooks/{webhookId}", ownerOnly, s.updateWebhook)
	route("DELETE "+b+"/webhooks/{webhookId}", ownerOnly, s.deleteWebhook)
	route("POST "+b+"/webhooks/{webhookId}/rotate-secret", ownerOnly, s.rotateWebhookSecret)
	route("GET "+b+"/webhooks/{webhookId}/deliveries", ownerOnly, s.listWebhookDeliveries)
	route("POST "+b+"/webhooks/{webhookId}/deliveries/{deliveryId}/retry", ownerOnly, s.retryWebhookDelivery)
}

func queryLimit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 200 {
		return 0, domain.Errorf(domain.CodeInvalidArgument, "limit must be 1..200").WithDetail("field", "limit")
	}
	return n, nil
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) error {
	if s.events == nil {
		return domain.NotImplemented(domain.FeatureEvents, "listEvents")
	}
	q := r.URL.Query()
	limit, err := queryLimit(r)
	if err != nil {
		return err
	}
	types, err := events.ParseTypes(q.Get("types"))
	if err != nil {
		return err
	}
	eq := events.Query{Cursor: q.Get("cursor"), Limit: limit, Types: types, VMID: domain.VMID(q.Get("vmId")), Caller: pollCaller(r)}
	if ref := q.Get("vmId"); ref != "" {
		// A name resolves to its VM; anything else (a deleted VM's ID)
		// filters as given.
		if vm, err := s.svc.GetVM(r.Context(), principal(r), ref); err == nil {
			eq.VMID = vm.ID
		}
	}
	switch q.Get("start") {
	case "", "oldest":
	case "latest":
		eq.Latest = true
	default:
		return domain.Errorf(domain.CodeInvalidArgument, "start must be oldest or latest").WithDetail("field", "start")
	}
	if v := q.Get("waitMs"); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ms < 0 || ms > events.MaxWait.Milliseconds() {
			return domain.Errorf(domain.CodeInvalidArgument, "waitMs must be 0..%d", events.MaxWait.Milliseconds()).WithDetail("field", "waitMs")
		}
		eq.Wait = time.Duration(ms) * time.Millisecond
	}
	l, err := s.events.List(r.Context(), principal(r), eq)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}

func (s *Server) webhookService(op string) (*webhooks.Service, error) {
	if s.webhooks == nil {
		return nil, domain.NotImplemented(domain.FeatureWebhooks, op)
	}
	return s.webhooks, nil
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) error {
	ws, err := s.webhookService("listWebhooks")
	if err != nil {
		return err
	}
	limit, err := queryLimit(r)
	if err != nil {
		return err
	}
	l, err := ws.List(r.Context(), principal(r), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) error {
	ws, err := s.webhookService("createWebhook")
	if err != nil {
		return err
	}
	var body api.CreateWebhookRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	out, err := ws.Create(r.Context(), principal(r), r.Header.Get(api.HeaderIdempotencyKey), body)
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/webhooks/"+out.Webhook.ID)
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusCreated, out)
}

func (s *Server) getWebhook(w http.ResponseWriter, r *http.Request) error {
	ws, err := s.webhookService("getWebhook")
	if err != nil {
		return err
	}
	out, err := ws.Get(r.Context(), principal(r), r.PathValue("webhookId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) error {
	ws, err := s.webhookService("updateWebhook")
	if err != nil {
		return err
	}
	var body api.UpdateWebhookRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	out, err := ws.Update(r.Context(), principal(r), r.PathValue("webhookId"), body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) error {
	ws, err := s.webhookService("deleteWebhook")
	if err != nil {
		return err
	}
	if err := ws.Delete(r.Context(), principal(r), r.PathValue("webhookId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) error {
	ws, err := s.webhookService("rotateWebhookSecret")
	if err != nil {
		return err
	}
	var body api.RotateWebhookSecretRequest
	if err := decode(r, &body, true); err != nil {
		return err
	}
	out, err := ws.Rotate(r.Context(), principal(r), r.PathValue("webhookId"), r.Header.Get(api.HeaderIdempotencyKey), body)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return writeJSON(w, http.StatusOK, out)
}

func (s *Server) listWebhookDeliveries(w http.ResponseWriter, r *http.Request) error {
	ws, err := s.webhookService("listWebhookDeliveries")
	if err != nil {
		return err
	}
	limit, err := queryLimit(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	l, err := ws.Deliveries(r.Context(), principal(r), r.PathValue("webhookId"), q.Get("state"), limit, q.Get("cursor"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}

func (s *Server) retryWebhookDelivery(w http.ResponseWriter, r *http.Request) error {
	ws, err := s.webhookService("retryWebhookDelivery")
	if err != nil {
		return err
	}
	out, err := ws.Retry(r.Context(), principal(r), r.PathValue("webhookId"), r.PathValue("deliveryId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}
