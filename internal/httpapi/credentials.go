package httpapi

import (
	"net/http"
	"strconv"

	"davit/api"
	"davit/internal/credproxy"
	"davit/internal/domain"
)

// SetCredentials enables the credential control plane (G4: profiles,
// write-only secrets, per-VM grants).
func (s *Server) SetCredentials(c *credproxy.Control) { s.credentials = c }

// credentialRoutes registers the G4 operations (access rules as in the
// contract: profiles are the owner's, grants follow the VM's token scope).
func (s *Server) credentialRoutes(route func(string, access, handler)) {
	b := api.BasePath
	view, admin := api.TokenView, api.TokenAdmin
	route("GET "+b+"/credential-profiles", ownerOnly, s.listCredentialProfiles)
	route("POST "+b+"/credential-profiles", ownerOnly, s.createCredentialProfile)
	route("GET "+b+"/credential-profiles/{profileId}", ownerOnly, s.getCredentialProfile)
	route("DELETE "+b+"/credential-profiles/{profileId}", ownerOnly, s.deleteCredentialProfile)
	route("PUT "+b+"/credential-profiles/{profileId}/credentials/{credentialName}", ownerOnly, s.putCredential)
	route("DELETE "+b+"/credential-profiles/{profileId}/credentials/{credentialName}", ownerOnly, s.deleteCredential)
	// Members never see or manage credential grants, whatever their role:
	// a placeholder is the owner's key inside the VM.
	route("GET "+b+"/vms/{vmId}/credential-grants", noMembers(onVM(view)), s.listCredentialGrants)
	route("POST "+b+"/vms/{vmId}/credential-grants", noMembers(onVM(admin)), s.createCredentialGrant)
	route("DELETE "+b+"/vms/{vmId}/credential-grants/{grantId}", noMembers(onVM(admin)), s.revokeCredentialGrant)
}

func (s *Server) credentialControl() (*credproxy.Control, error) {
	if s.credentials == nil {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "credentials are not enabled on this daemon").
			WithDetail("capability", domain.FeatureCredentials)
	}
	return s.credentials, nil
}

func pageLimit(r *http.Request) (int, error) {
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

func (s *Server) listCredentialProfiles(w http.ResponseWriter, r *http.Request) error {
	c, err := s.credentialControl()
	if err != nil {
		return err
	}
	limit, err := pageLimit(r)
	if err != nil {
		return err
	}
	l, err := c.ListProfiles(r.Context(), principal(r), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}

func (s *Server) createCredentialProfile(w http.ResponseWriter, r *http.Request) error {
	c, err := s.credentialControl()
	if err != nil {
		return err
	}
	var body api.CreateCredentialProfileRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	p, err := c.CreateProfile(r.Context(), principal(r), body, r.Header.Get(api.HeaderIdempotencyKey))
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/credential-profiles/"+p.ID)
	return writeJSON(w, http.StatusCreated, p)
}

func (s *Server) getCredentialProfile(w http.ResponseWriter, r *http.Request) error {
	c, err := s.credentialControl()
	if err != nil {
		return err
	}
	p, err := c.GetProfile(r.Context(), principal(r), r.PathValue("profileId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

func (s *Server) deleteCredentialProfile(w http.ResponseWriter, r *http.Request) error {
	c, err := s.credentialControl()
	if err != nil {
		return err
	}
	if err := c.DeleteProfile(r.Context(), principal(r), r.PathValue("profileId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) putCredential(w http.ResponseWriter, r *http.Request) error {
	c, err := s.credentialControl()
	if err != nil {
		return err
	}
	var body api.PutCredentialRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	p, err := c.PutCredential(r.Context(), principal(r), r.PathValue("profileId"), r.PathValue("credentialName"), body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, p)
}

func (s *Server) deleteCredential(w http.ResponseWriter, r *http.Request) error {
	c, err := s.credentialControl()
	if err != nil {
		return err
	}
	if err := c.DeleteCredential(r.Context(), principal(r), r.PathValue("profileId"), r.PathValue("credentialName")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// grantVM resolves the path's VM for the caller (ID or name).
func (s *Server) grantVM(r *http.Request) (domain.VMID, error) {
	vm, err := s.svc.GetVM(r.Context(), principal(r), r.PathValue("vmId"))
	if err != nil {
		return "", err
	}
	if vm.State == domain.StateDeleted || vm.State == domain.StateDeleting {
		return "", domain.Errorf(domain.CodeNotFound, "vm %s not found", r.PathValue("vmId"))
	}
	return vm.ID, nil
}

func (s *Server) listCredentialGrants(w http.ResponseWriter, r *http.Request) error {
	c, err := s.credentialControl()
	if err != nil {
		return err
	}
	if _, err := pageLimit(r); err != nil {
		return err
	}
	vm, err := s.svc.GetVM(r.Context(), principal(r), r.PathValue("vmId"))
	if err != nil {
		return err
	}
	l, err := c.ListGrants(r.Context(), vm.ID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, l)
}

func (s *Server) createCredentialGrant(w http.ResponseWriter, r *http.Request) error {
	c, err := s.credentialControl()
	if err != nil {
		return err
	}
	var body api.CreateCredentialGrantRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	vm, err := s.grantVM(r)
	if err != nil {
		return err
	}
	g, err := c.CreateGrant(r.Context(), principal(r), vm, body, r.Header.Get(api.HeaderIdempotencyKey))
	if err != nil {
		return err
	}
	w.Header().Set("Location", api.BasePath+"/vms/"+string(vm)+"/credential-grants/"+g.ID)
	return writeJSON(w, http.StatusCreated, g)
}

func (s *Server) revokeCredentialGrant(w http.ResponseWriter, r *http.Request) error {
	c, err := s.credentialControl()
	if err != nil {
		return err
	}
	vm, err := s.svc.GetVM(r.Context(), principal(r), r.PathValue("vmId"))
	if err != nil {
		return err
	}
	g, err := c.RevokeGrant(r.Context(), principal(r), vm.ID, r.PathValue("grantId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, g)
}
