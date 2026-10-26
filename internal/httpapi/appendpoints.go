package httpapi

import (
	"net/http"

	"davit/api"
	"davit/internal/appendpoint"
	"davit/internal/domain"
)

// SetAppEndpoints enables the app endpoint operations (feature
// appEndpoints). Without it they answer unsupported_capability.
func (s *Server) SetAppEndpoints(a *appendpoint.Service) { s.apps = a }

// appEndpointRoutes: listing needs view on the VM; creating, renaming,
// hiding and deleting publish or withdraw a guest port, so owner or admin.
func (s *Server) appEndpointRoutes() []reverseRoute {
	b := api.BasePath + "/vms/{vmId}/app-endpoints"
	return []reverseRoute{
		{"GET " + b, onVM(api.TokenView), s.listAppEndpoints},
		{"POST " + b, onVM(api.TokenAdmin), s.createAppEndpoint},
		{"PATCH " + b + "/{endpointId}", onVM(api.TokenAdmin), s.updateAppEndpoint},
		{"DELETE " + b + "/{endpointId}", onVM(api.TokenAdmin), s.deleteAppEndpoint},
	}
}

func (s *Server) appsService() (*appendpoint.Service, error) {
	if s.apps == nil {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "app endpoints need the tailnet gateway (tailnet.enabled)").
			WithDetail("feature", domain.FeatureAppEndpoints)
	}
	return s.apps, nil
}

func (s *Server) listAppEndpoints(w http.ResponseWriter, r *http.Request) error {
	a, err := s.appsService()
	if err != nil {
		return err
	}
	items, err := a.List(r.Context(), principal(r), r.PathValue("vmId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, api.AppEndpointList{Items: items})
}

func (s *Server) createAppEndpoint(w http.ResponseWriter, r *http.Request) error {
	a, err := s.appsService()
	if err != nil {
		return err
	}
	var body api.CreateAppEndpointRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	ep, err := a.Create(r.Context(), principal(r), r.PathValue("vmId"), body)
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/vms/"+string(ep.VMID)+"/app-endpoints/"+ep.ID)
	return writeJSON(w, http.StatusCreated, ep)
}

func (s *Server) updateAppEndpoint(w http.ResponseWriter, r *http.Request) error {
	a, err := s.appsService()
	if err != nil {
		return err
	}
	var body api.UpdateAppEndpointRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	ep, err := a.Update(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("endpointId"), body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, ep)
}

func (s *Server) deleteAppEndpoint(w http.ResponseWriter, r *http.Request) error {
	a, err := s.appsService()
	if err != nil {
		return err
	}
	if err := a.Delete(r.Context(), principal(r), r.PathValue("vmId"), r.PathValue("endpointId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
