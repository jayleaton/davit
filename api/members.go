package api

import "time"

// G13 members and shares (feature members): tailnet users other than the
// owner, with rights only on what the owner shares. The owner remains the
// superuser; members act within the owner's VMs, never as the owner.

// MemberRole is a member's right on a shared resource; each includes the
// ones before it (as token actions: view < exec < admin).
type MemberRole string

const (
	// RoleViewer reads: state, lists, operations, process records, events.
	RoleViewer MemberRole = "viewer"
	// RoleExec also runs code: exec, files, tunnels and ssh, prompts,
	// app endpoint requests, snapshot file reads.
	RoleExec MemberRole = "exec"
	// RoleAdmin also manages the VM: stop, resume, update, delete,
	// snapshots, reverse forwards, app endpoints.
	RoleAdmin MemberRole = "admin"
)

// Member is a tailnet user with rights on shared resources.
type Member struct {
	ID string `json:"id"`
	// Login is the tailnet login the member was added by.
	Login string `json:"login"`
	// UserID is the stable tailnet user ID the login was pinned to when
	// the member first connected (0 until then). From then on the member is
	// that user, whatever its login becomes; a recycled login is refused.
	UserID int64 `json:"userId,omitempty"`
	// Role is the member's role on VMs shared with all members without a
	// role of their own (the default visibility).
	Role    MemberRole `json:"role"`
	AddedAt time.Time  `json:"addedAt"`
}

// AddMemberRequest is the body of addMember.
type AddMemberRequest struct {
	Login string     `json:"login"`
	Role  MemberRole `json:"role"`
}

// UpdateMemberRequest is the body of updateMember.
type UpdateMemberRequest struct {
	Role MemberRole `json:"role"`
}

// MemberList is the host's members.
type MemberList struct {
	Items []Member `json:"items"`
}

// ShareKind is what a share grants access to.
type ShareKind string

const (
	ShareVM          ShareKind = "vm"
	ShareSnapshot    ShareKind = "snapshot"
	ShareEnvironment ShareKind = "environment"
)

// ShareAllMembers as a share's member means every member.
const ShareAllMembers = "*"

// Share grants one member (or every member) a role on one VM, snapshot or
// environment template.
type Share struct {
	ID         string    `json:"id"`
	Kind       ShareKind `json:"kind"`
	ResourceID string    `json:"resourceId"`
	// Member is a member ID, or "*" for every member.
	Member string `json:"member"`
	// Role is empty on a share with every member at each member's own role
	// (the default visibility of new VMs).
	Role MemberRole `json:"role,omitempty"`
	// Credentials lets the member(s) use the VM's credential grants. Off,
	// an exec or admin share pauses the VM's credential grants for
	// everyone while it exists: processes in one guest cannot be told
	// apart, so the owner's keys would otherwise be usable by the member.
	Credentials bool      `json:"credentials"`
	CreatedAt   time.Time `json:"createdAt"`
}

// CreateShareRequest is the body of createShare. Sharing the same resource
// with the same member again replaces the role and credentials flag.
type CreateShareRequest struct {
	Kind ShareKind `json:"kind"`
	// Resource is the VM, snapshot or environment (ID or name).
	Resource string `json:"resource"`
	// Member is a member ID, its login, or "*" for every member.
	Member string `json:"member"`
	// Role defaults to the member's role ("*": each member's own role).
	Role        MemberRole `json:"role,omitempty"`
	Credentials bool       `json:"credentials,omitempty"`
}

// ShareList is a page of shares.
type ShareList struct {
	Items []Share `json:"items"`
}
