package httpapi

import (
	"net/http"
	"strconv"

	"davit/api"
	"davit/internal/domain"
)

// Environments (G3, owner only).

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request) error {
	p, err := page(r)
	if err != nil {
		return err
	}
	envs, next, err := s.svc.ListEnvironments(r.Context(), principal(r), p)
	if err != nil {
		return err
	}
	if g := memberOf(r); g != nil {
		envs = memberEnvironments(g, envs)
	}
	return writeJSON(w, http.StatusOK, api.EnvironmentList{Items: envs, NextCursor: next})
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) error {
	var body api.CreateEnvironmentRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	env, _, err := s.svc.CreateEnvironment(r.Context(), principal(r), body)
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/environments/"+string(env.ID))
	return writeJSON(w, http.StatusCreated, env)
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) error {
	env, err := s.svc.GetEnvironment(r.Context(), principal(r), r.PathValue("environmentId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, env)
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) error {
	if err := s.svc.DeleteEnvironment(r.Context(), principal(r), r.PathValue("environmentId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listEnvironmentRevisions(w http.ResponseWriter, r *http.Request) error {
	p, err := page(r)
	if err != nil {
		return err
	}
	revs, next, err := s.svc.ListEnvironmentRevisions(r.Context(), principal(r), r.PathValue("environmentId"), p)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, api.EnvironmentRevisionList{Items: revs, NextCursor: next})
}

func (s *Server) createEnvironmentRevision(w http.ResponseWriter, r *http.Request) error {
	req, err := mutation(r) // If-Match: the latest revision number
	if err != nil {
		return err
	}
	var body api.CreateEnvironmentRevisionRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	rev, _, err := s.svc.CreateEnvironmentRevision(r.Context(), req.Principal, r.PathValue("environmentId"), req.IdempotencyKey, req.IfMatch, body.Manifest)
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/environments/"+string(rev.EnvironmentID)+"/revisions/"+strconv.FormatInt(rev.Revision, 10))
	return writeJSON(w, http.StatusCreated, rev)
}

func (s *Server) getEnvironmentRevision(w http.ResponseWriter, r *http.Request) error {
	n, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || n < 1 {
		return domain.Errorf(domain.CodeInvalidArgument, "revision must be a positive integer").WithDetail("field", "revision")
	}
	rev, err := s.svc.GetEnvironmentRevision(r.Context(), principal(r), r.PathValue("environmentId"), n)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, rev)
}
