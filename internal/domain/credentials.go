package domain

import "time"

// Credentials (G4, contract only). A credential profile holds named
// credentials whose secret values are write-only: the API never returns
// them, and they never enter a guest. A VM gets a CredentialGrant instead:
// a random placeholder token, visible in the guest (e.g. as
// ANTHROPIC_API_KEY), which the host credential proxy swaps for the real
// key on requests to the credential's upstream. Revoking the grant makes the
// placeholder worthless at once; the guest never held anything else.

// CredentialKind is what a credential authenticates with.
type CredentialKind string

const (
	CredentialAPIKey    CredentialKind = "api_key"    // provider API key
	CredentialCustomAPI CredentialKind = "custom_api" // key + base URL of any HTTP API
)

// CredentialProfile is a named set of credentials.
type CredentialProfile struct {
	ID          string       `json:"id"`
	Owner       PrincipalID  `json:"-"`
	Name        string       `json:"name"`
	Default     bool         `json:"default"`
	Credentials []Credential `json:"credentials"`
	CreatedAt   time.Time    `json:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt"`
}

// Credential is one credential's metadata; the secret is write-only.
type Credential struct {
	Name     string         `json:"name"`
	Kind     CredentialKind `json:"kind"`
	Provider string         `json:"provider,omitempty"` // e.g. anthropic, openai
	BaseURL  string         `json:"baseUrl,omitempty"`  // custom_api: the upstream
	// EnvVar is the guest variable that carries the grant's placeholder.
	EnvVar    string    `json:"envVar"`
	HasSecret bool      `json:"hasSecret"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// GrantState is a credential grant's state.
type GrantState string

const (
	GrantActive  GrantState = "active"
	GrantRevoked GrantState = "revoked"
)

// RevokeReason says why a grant stopped working.
type RevokeReason string

const (
	RevokeExplicit          RevokeReason = "revoked"            // revokeCredentialGrant
	RevokeVMDeleted         RevokeReason = "vm_deleted"         // the VM was deleted
	RevokeProfileDeleted    RevokeReason = "profile_deleted"    // its profile was deleted
	RevokeCredentialRemoved RevokeReason = "credential_removed" // its credential was removed
)

// CredentialGrant gives one VM proxied use of one credential.
type CredentialGrant struct {
	ID         string     `json:"id"`
	VMID       VMID       `json:"vmId"`
	ProfileID  string     `json:"profileId"`
	Credential string     `json:"credential"`
	State      GrantState `json:"state"`
	// Placeholder is the token the guest sees in EnvVar. It is not the
	// credential and authenticates nothing outside the proxy.
	Placeholder  string       `json:"placeholder"`
	EnvVar       string       `json:"envVar"`
	Upstream     string       `json:"upstream"`
	RevokeReason RevokeReason `json:"revokeReason,omitempty"`
	CreatedAt    time.Time    `json:"createdAt"`
	RevokedAt    *time.Time   `json:"revokedAt,omitempty"`
}

// GrantEvent is a VM lifecycle event as seen by its grants.
type GrantEvent string

const (
	GrantOnStop   GrantEvent = "stop"
	GrantOnResume GrantEvent = "resume"
	GrantOnFork   GrantEvent = "fork"
	GrantOnDelete GrantEvent = "delete"
)

// GrantAfter is the revoke contract across lifecycle events, for one active
// grant of the VM the event happens to:
//
//   - stop: the grant stays active but serves nothing (no guest runs);
//     a stopped disk keeps only the placeholder.
//   - resume: the same VM, the same grant; the placeholder on disk works again.
//   - fork: the source keeps its grants; the child inherits NONE. The child's
//     disk carries the source's placeholder, which the proxy refuses from the
//     child (grants bind to a VM ID, not to a token alone). A child that
//     needs credentials gets its own grants.
//   - delete: every grant of the VM is revoked (vm_deleted).
//
// keep reports whether the grant stays active; inherit whether a fork child
// gets a copy (always false).
func GrantAfter(e GrantEvent) (keep, inherit bool, reason RevokeReason) {
	switch e {
	case GrantOnStop, GrantOnResume, GrantOnFork:
		return true, false, ""
	case GrantOnDelete:
		return false, false, RevokeVMDeleted
	}
	return true, false, ""
}
