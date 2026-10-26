package httpapi

import (
	"context"
	"net/http"

	"davit/api"
	"davit/internal/domain"
)

// SSHCredentialIssuer implements issueSSHCredential (sshca.Issuer).
type SSHCredentialIssuer interface {
	Issue(ctx context.Context, principal domain.PrincipalID, ref string, req api.IssueSSHCredentialRequest) (api.SSHCredential, error)
}

// SetSSHCredentials enables issueSSHCredential. Without it the endpoint
// answers 422 unsupported_capability (no guest transport, so no guest sshd
// to reach).
func (s *Server) SetSSHCredentials(i SSHCredentialIssuer) { s.sshCreds = i }

func (s *Server) issueSSHCredential(w http.ResponseWriter, r *http.Request) error {
	if s.sshCreds == nil {
		return domain.Errorf(domain.CodeUnsupportedCapability, "this runtime has no guest transport").
			WithDetail("capability", "guestTransport")
	}
	var body api.IssueSSHCredentialRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	c, err := s.sshCreds.Issue(r.Context(), principal(r), r.PathValue("vmId"), body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, c)
}
