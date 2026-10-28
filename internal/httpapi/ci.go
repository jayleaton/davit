package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"davit/api"
	"davit/internal/domain"
)

// CIRunners is the CI runner controller (internal/ci.Service).
type CIRunners interface {
	Status(ctx context.Context, recent int) (api.CIStatus, error)
}

// SetCI enables getCIStatus's job lists. Without it the host reports CI
// runners off.
func (s *Server) SetCI(c CIRunners) { s.ci = c }

// getCIStatus reports queued, running and recent CI jobs (owner only: it
// names repositories and jobs).
func (s *Server) getCIStatus(w http.ResponseWriter, r *http.Request) error {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		var err error
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > api.MaxListLimit {
			return domain.Errorf(domain.CodeInvalidArgument, "limit must be 1..%d", api.MaxListLimit).WithDetail("field", "limit")
		}
	}
	if s.ci == nil {
		return writeJSON(w, http.StatusOK, api.CIStatus{Queued: []api.CIJob{}, Running: []api.CIJob{}, Recent: []api.CIJob{}})
	}
	st, err := s.ci.Status(r.Context(), limit)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, st)
}
