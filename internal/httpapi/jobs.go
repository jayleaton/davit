package httpapi

import (
	"net/http"
	"strconv"

	"davit/internal/domain"
	"davit/internal/jobs"
)

// Job history and retained output (G7). They read the host's records, so
// they work while the VM is stopped.

// SetJobs enables listProcessHistory and readProcessOutput.
func (s *Server) SetJobs(j *jobs.Service) { s.jobs = j }

func (s *Server) jobsService(op string) (*jobs.Service, error) {
	if s.jobs == nil {
		return nil, domain.NotImplemented(domain.FeatureProcessHistory, op)
	}
	return s.jobs, nil
}

func (s *Server) listProcessHistory(w http.ResponseWriter, r *http.Request) error {
	j, err := s.jobsService("listProcessHistory")
	if err != nil {
		return err
	}
	p, err := page(r)
	if err != nil {
		return err
	}
	h, err := j.History(r.Context(), principal(r), r.PathValue("vmId"), p.Limit, p.Cursor)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, h)
}

func (s *Server) readProcessOutput(w http.ResponseWriter, r *http.Request) error {
	j, err := s.jobsService("readProcessOutput")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			return domain.Errorf(domain.CodeInvalidArgument, "limit must be a positive integer").WithDetail("field", "limit")
		}
	}
	out, err := j.Output(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("processId"), q.Get("after"), q.Get("stream"), limit)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, out)
}

// listGuestProcesses samples every guest process; a VM that is not running
// answers with its last sample (stale).
func (s *Server) listGuestProcesses(w http.ResponseWriter, r *http.Request) error {
	j, err := s.jobsService("listGuestProcesses")
	if err != nil {
		return err
	}
	l, err := j.PS(r.Context(), principal(r), r.PathValue("vmId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}
