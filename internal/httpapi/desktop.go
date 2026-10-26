package httpapi

import (
	"net/http"

	"davit/api"
)

// desktopRoutes (G15): the desktop is the VM's app endpoint of kind
// desktop. Reading it needs view; opening or closing it publishes or
// withdraws the VM's display, so owner or admin.
func (s *Server) desktopRoutes() []reverseRoute {
	b := api.BasePath + "/vms/{vmId}/desktop"
	return []reverseRoute{
		{"GET " + b, onVM(api.TokenView), s.getDesktop},
		{"POST " + b, onVM(api.TokenAdmin), s.openDesktop},
		{"DELETE " + b, onVM(api.TokenAdmin), s.closeDesktop},
	}
}

func (s *Server) getDesktop(w http.ResponseWriter, r *http.Request) error {
	a, err := s.appsService()
	if err != nil {
		return err
	}
	ep, err := a.GetDesktop(r.Context(), principal(r), r.PathValue("vmId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, ep)
}

func (s *Server) openDesktop(w http.ResponseWriter, r *http.Request) error {
	a, err := s.appsService()
	if err != nil {
		return err
	}
	var body api.OpenDesktopRequest
	if err := decode(r, &body, true); err != nil {
		return err
	}
	ep, created, err := a.OpenDesktop(r.Context(), principal(r), r.PathValue("vmId"), body.Port)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		w.Header().Set("Location", api.BasePath+"/vms/"+string(ep.VMID)+"/desktop")
	}
	return writeJSON(w, status, ep)
}

func (s *Server) closeDesktop(w http.ResponseWriter, r *http.Request) error {
	a, err := s.appsService()
	if err != nil {
		return err
	}
	if err := a.CloseDesktop(r.Context(), principal(r), r.PathValue("vmId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
