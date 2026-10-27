package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"davit/api"
	"davit/internal/domain"
)

// g9Routes registers the shipped G9 operations (recovery and capacity).
func (s *Server) g9Routes(route func(string, access, handler)) {
	b := api.BasePath
	route("POST "+b+"/vms/{vmId}/recover", onVM(api.TokenAdmin), s.recoverVM)
	route("POST "+b+"/vms/batch", allVMs(api.TokenAdmin), s.createVMs)
	route("POST "+b+"/vms/reclaim", allVMs(api.TokenAdmin), s.reclaimVMs)
	route("POST "+b+"/images/prune", ownerOnly, s.pruneImages)
}

// pruneImages is POST /images/prune: remove the image blobs nothing uses
// (lifecycle.PruneImages); host owner only.
func (s *Server) pruneImages(w http.ResponseWriter, r *http.Request) error {
	var body api.PruneImagesRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	res, err := s.svc.PruneImages(r.Context(), principal(r), body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, res)
}

// reclaimVMs is POST /vms/reclaim (feature capacityReclaim): stop the
// caller's idle VMs (lifecycle.Reclaim). It takes no Idempotency-Key: each
// stop is keyed by its VM's generation and revision, so a repeat stops
// nothing twice.
func (s *Server) reclaimVMs(w http.ResponseWriter, r *http.Request) error {
	var body api.ReclaimVMsRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	res, err := s.svc.Reclaim(r.Context(), principal(r), body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, res)
}

// capacityWait reads createVM/resumeVM's waitSeconds query parameter: how
// long the request may queue for host capacity (0: not at all).
func capacityWait(r *http.Request) (time.Duration, error) {
	v := r.URL.Query().Get("waitSeconds")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > domain.MaxCapacityWaitSeconds {
		return 0, domain.Errorf(domain.CodeInvalidArgument, "waitSeconds must be an integer 0..%d", domain.MaxCapacityWaitSeconds).WithDetail("field", "waitSeconds")
	}
	return time.Duration(n) * time.Second, nil
}

// recoverVM power-cycles a VM on its current disk (lifecycle.Recover).
func (s *Server) recoverVM(w http.ResponseWriter, r *http.Request) error {
	req, err := mutation(r)
	if err != nil {
		return err
	}
	var body api.RecoverVMRequest
	if err := decode(r, &body, true); err != nil {
		return err
	}
	a, err := s.svc.Recover(r.Context(), req, r.PathValue("vmId"), body)
	if err != nil {
		return err
	}
	return accepted(w, a)
}

// createVMs is POST /vms/batch (G9, feature batchCreate): 200 with one
// result per item, accepted or refused. Errors that concern the whole
// request (body, template, Idempotency-Key) are ordinary error responses.
func (s *Server) createVMs(w http.ResponseWriter, r *http.Request) error {
	var body api.CreateVMsRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	req, err := mutation(r)
	if err != nil {
		return err
	}
	res, err := s.svc.CreateVMs(r.Context(), req, body)
	if err != nil {
		return err
	}
	rid, _ := r.Context().Value(keyRequestID).(string)
	for i := range res.Items {
		if e := res.Items[i].Error; e != nil {
			e.RequestID = rid
		}
	}
	return writeJSON(w, http.StatusOK, res)
}
