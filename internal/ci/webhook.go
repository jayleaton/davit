package ci

import (
	"encoding/json"
	"io"
	"net/http"

	"davit/internal/ci/github"
)

// maxDelivery bounds a webhook body (GitHub caps payloads at 25 MB;
// workflow_job deliveries are a few KB).
const maxDelivery = 25 << 20

// hook is a verified workflow_job delivery waiting for intake.
type hook struct {
	delivery string
	ev       github.WorkflowJobEvent
}

// webhookHandler receives workflow_job deliveries. Only a delivery whose
// X-Hub-Signature-256 matches the secret is read; it is answered at once
// (GitHub waits 10 s at most) and handled in order by handleHooks.
func (s *Service) webhookHandler(secret []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxDelivery+1))
		if err != nil || len(body) > maxDelivery {
			http.Error(w, "unreadable or too large", http.StatusBadRequest)
			return
		}
		if err := github.VerifySignature(secret, body, r.Header.Get(github.HeaderSignature)); err != nil {
			s.cfg.Log.Warn("ci: webhook delivery with a bad signature refused", "remote", r.RemoteAddr, "delivery", r.Header.Get(github.HeaderDelivery))
			http.Error(w, "signature mismatch", http.StatusUnauthorized)
			return
		}
		switch r.Header.Get(github.HeaderEvent) {
		case "ping":
			w.WriteHeader(http.StatusNoContent)
			return
		case "workflow_job":
		default:
			w.WriteHeader(http.StatusAccepted) // subscribed to more than needed: ignore
			return
		}
		var ev github.WorkflowJobEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			http.Error(w, "not a workflow_job payload", http.StatusBadRequest)
			return
		}
		select {
		case s.hooks <- hook{r.Header.Get(github.HeaderDelivery), ev}:
			w.WriteHeader(http.StatusAccepted)
		default:
			// Polling picks up what is dropped here.
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}
	})
}

// handleHooks feeds verified deliveries to the intake, one at a time.
func (s *Service) handleHooks() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case h := <-s.hooks:
			ev := h.ev
			if ev.Repository.FullName == "" {
				continue
			}
			j := ev.WorkflowJob
			if ev.Action != "" {
				j.Status = ev.Action
			}
			if err := s.observe(s.ctx, ev.Installation.ID, ev.Repository.FullName, &ev.Repository, j); err != nil {
				s.cfg.Log.Warn("ci: webhook delivery", "delivery", h.delivery, "repo", ev.Repository.FullName, "githubJob", j.ID, "err", err)
			}
		}
	}
}
