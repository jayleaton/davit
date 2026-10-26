package domain

// Feature contract (parity v1, docs/research/CONTRACT-v1.md). Every API
// surface that is specified but not built yet is a Feature with status
// not_implemented: its operations exist in the OpenAPI document, answer a
// typed not_implemented error naming the feature, and are listed here so a
// client can tell "not built" from "not found" without calling them.

// FeatureStatus says whether a feature can be used on this host.
type FeatureStatus string

const (
	// FeatureAvailable: implemented and enabled.
	FeatureAvailable FeatureStatus = "available"
	// FeatureNotImplemented: specified, not built in this release; its
	// operations answer not_implemented.
	FeatureNotImplemented FeatureStatus = "not_implemented"
	// FeatureUnsupported: built, but this host or backend cannot provide it;
	// its operations answer unsupported_capability.
	FeatureUnsupported FeatureStatus = "unsupported"
)

// Feature is one capability entry in HostCapabilities.Features.
type Feature struct {
	Name   string        `json:"name"`
	Status FeatureStatus `json:"status"`
	// Gap is the PARITY.md gap ID it belongs to, e.g. "G3".
	Gap string `json:"gap"`
	// Operations are the OpenAPI operationIds (and request fields, as
	// operationId.field) the feature covers.
	Operations []string `json:"operations"`
	Detail     string   `json:"detail,omitempty"`
}

// Feature names.
const (
	FeatureEnvironments      = "environments"       // G3: env CRUD, revisions, pins
	FeatureWorkspacePrep     = "workspacePrepare"   // G3: clone/files/setup on new/fork/resume
	FeatureCredentials       = "credentialProfiles" // G4: profiles and credentials
	FeatureCredentialProxy   = "credentialProxy"    // G4: placeholder grants, real key never in the guest
	FeaturePromptFeed        = "promptFeed"         // G5: VM-wide events/replay with a cursor
	FeaturePromptInterrupt   = "promptInterrupt"    // G5: scoped interrupt
	FeaturePromptAttachments = "promptAttachments"  // G5
	FeaturePromptOptions     = "promptOptions"      // G5: reasoning effort, fast
	FeaturePromptTurnLease   = "promptTurnLease"    // G5: active-turn TTL / auto-stop lease
	FeaturePromptSteer       = "promptSteer"        // G5: per harness, see HarnessCapabilities.Steer
	FeatureRuns              = "runs"               // G6: one-command create/exec/cleanup
	FeaturePush              = "push"               // G6: git-aware tree upload
	FeatureProcessHistory    = "processHistory"     // G7: durable handles, retained output, history
	FeatureGuestPS           = "guestProcesses"     // G7: whole-guest process samples
	FeatureReverseForward    = "reverseForward"     // G10: guest listen / reverse connect
	FeatureAppEndpoints      = "appEndpoints"       // G11: tailnet-private app URLs
	FeatureMembers           = "members"            // G13: tailnet members and shares
	FeatureDesktop           = "desktop"            // G15: VNC desktop viewer over the gateway
	FeatureSnapshotFiles     = "snapshotFiles"      // G12: tree, file extract, export
	FeatureRecover           = "recover"            // G9: disk-preserving power cycle
	FeatureResize            = "resize"             // G9: shape change at resume, disk grow only
	FeatureHostCapacity      = "hostCapacity"       // G9: capacity report and admission
	FeatureBatchCreate       = "batchCreate"        // G9: createVMs, per-item results
	FeatureCapacityReclaim   = "capacityReclaim"    // G9: stop idle VMs for room, wait in a queue, reclaimVMs
	FeatureWhoami            = "whoami"             // G18: GET /me
	FeatureTokenRotate       = "tokenRotate"        // G18: rotateToken with overlap
	FeatureTokenUsage        = "tokenUsage"         // G18: Token.useCount
	FeatureEvents            = "events"             // G16: host event stream with a cursor
	FeatureWebhooks          = "webhooks"           // G16: signed deliveries from a durable outbox
	FeaturePrivacy           = "privacyPolicy"      // G17: ephemeral, no-snapshot, no-output, no-samples
)

// ParityFeatures is the parity v1 contract: every feature not implemented,
// except those already built, which start unsupported. The daemon flips a
// feature to available in its capability report when it is wired.
func ParityFeatures() []Feature {
	ni := func(name, gap, detail string, ops ...string) Feature {
		return Feature{Name: name, Status: FeatureNotImplemented, Gap: gap, Operations: ops, Detail: detail}
	}
	return []Feature{
		ni(FeatureEnvironments, "G3", "versioned environment manifests and pins",
			"listEnvironments", "createEnvironment", "getEnvironment", "deleteEnvironment",
			"listEnvironmentRevisions", "createEnvironmentRevision", "getEnvironmentRevision"),
		ni(FeatureWorkspacePrep, "G3", "VM workspace preparation from an environment revision",
			"createVM.environment", "forkVM.environment", "resumeVM.environment", "createRun.environment"),
		ni(FeatureCredentials, "G4", "credential profiles; secrets are write-only",
			"listCredentialProfiles", "createCredentialProfile", "getCredentialProfile", "deleteCredentialProfile",
			"putCredential", "deleteCredential"),
		ni(FeatureCredentialProxy, "G4", "per-VM placeholder grants through the host credential proxy",
			"listCredentialGrants", "createCredentialGrant", "revokeCredentialGrant"),
		ni(FeaturePromptFeed, "G5", "VM-wide prompt events with a replay cursor", "listVMPromptEvents"),
		ni(FeaturePromptInterrupt, "G5", "interrupt by conversation or all", "interruptPrompts"),
		ni(FeaturePromptAttachments, "G5", "", "createPrompt.attachments"),
		ni(FeaturePromptOptions, "G5", "", "createPrompt.options"),
		ni(FeaturePromptTurnLease, "G5", "an active turn holds the VM's auto-stop, bounded by its turn timeout",
			"createPrompt.turnTimeoutSeconds"),
		ni(FeaturePromptSteer, "G5", "per-harness support in harnesses[].steer", "steerPrompt"),
		ni(FeatureRuns, "G6", "", "createRun", "getRun", "startRun", "cancelRun"),
		ni(FeaturePush, "G6", "file selection (git-aware) is client-side; the server applies the tree with mode=merge. "+
			"mode=mirror (deleting files the tree lacks) needs guest-side deletion and answers not_implemented", "pushTree"),
		ni(FeatureProcessHistory, "G7", "managed-job history and output retained on the host",
			"listProcessHistory", "readProcessOutput"),
		ni(FeatureGuestPS, "G7", "", "listGuestProcesses"),
		// Built: the daemon reports it available when its guest agents can
		// listen (guest protocol feature "listen"); until then its
		// operations answer unsupported_capability.
		// Built: available when the tailnet gateway is enabled.
		{Name: FeatureMembers, Status: FeatureUnsupported, Gap: "G13",
			Detail:     "needs the tailnet gateway (tailnet.enabled)",
			Operations: []string{"listMembers", "addMember", "updateMember", "removeMember", "listShares", "createShare", "deleteShare"}},
		{Name: FeatureDesktop, Status: FeatureUnsupported, Gap: "G15",
			Detail:     "needs the tailnet gateway (tailnet.enabled) and a guest VNC server (desktop image profile)",
			Operations: []string{"getDesktop", "openDesktop", "closeDesktop"}},
		// Built: available when the tailnet gateway is enabled.
		{Name: FeatureAppEndpoints, Status: FeatureUnsupported, Gap: "G11",
			Detail:     "needs the tailnet gateway (tailnet.enabled)",
			Operations: []string{"listAppEndpoints", "createAppEndpoint", "updateAppEndpoint", "deleteAppEndpoint"}},
		{Name: FeatureReverseForward, Status: FeatureUnsupported, Gap: "G10",
			Detail:     "needs guest listeners (guest protocol feature \"listen\")",
			Operations: []string{"listReverseForwards", "createReverseForward", "revokeReverseForward", "attachReverseSession", "attachReverseConnection"}},
		ni(FeatureSnapshotFiles, "G12", "read files of a stopped snapshot without booting it",
			"listSnapshotTree", "readSnapshotFile", "exportSnapshot"),
		ni(FeatureRecover, "G9", "power-cycle a wedged VM on its current disk (no RAM or disk commit)", "recoverVM"),
		ni(FeatureResize, "G9", "change CPUs and memory at resume; the disk can only grow",
			"resumeVM.type", "resumeVM.shape"),
		ni(FeatureHostCapacity, "G9", "free memory, disk and VM slots, checked at admission", "getCapabilities.capacity"),
		ni(FeaturePrivacy, "G17", "per-VM ephemeral (delete on stop), no-snapshot, no output retention and no process samples",
			"createVM.privacy", "updateVM.privacy"),
		ni(FeatureWhoami, "G18", "who the caller is: principal, auth method, token scopes", "whoami"),
		ni(FeatureTokenRotate, "G18", "a new secret with the same scopes; the old one revoked after an overlap", "rotateToken"),
		ni(FeatureTokenUsage, "G18", "per-token use count next to lastUsedAt, written in batches", "listTokens.useCount"),
		ni(FeatureBatchCreate, "G9", "create up to 100 VMs from one template, one result per item", "createVMs"),
		ni(FeatureCapacityReclaim, "G9", "admission stops idle VMs to make room and queues requests for capacity; stop idle VMs on demand",
			"reclaimVMs", "createVM.waitSeconds", "resumeVM.waitSeconds"),
		ni(FeatureEvents, "G16", "VM, operation, run and prompt events with a cursor and long-poll", "listEvents"),
		ni(FeatureWebhooks, "G16", "HMAC-signed deliveries with retries, dead letters and secret rotation",
			"listWebhooks", "createWebhook", "getWebhook", "updateWebhook", "deleteWebhook",
			"rotateWebhookSecret", "listWebhookDeliveries", "retryWebhookDelivery"),
	}
}

// NotImplemented is the typed error of a specified but unbuilt feature.
// operation is the operationId (or operationId.field) the caller used.
func NotImplemented(feature, operation string) *Error {
	return Errorf(CodeNotImplemented, "%s is not implemented on this host (feature %s)", operation, feature).
		WithDetail("feature", feature).WithDetail("operation", operation)
}

// SteerSupport says how a harness takes a message during a running turn.
type SteerSupport string

const (
	// SteerNative: delivered into the running turn.
	SteerNative SteerSupport = "native"
	// SteerRestart: the turn is cancelled and a new turn starts with the
	// message; the result says so (never claimed as uninterrupted).
	SteerRestart SteerSupport = "restart"
	// SteerUnsupported: steerPrompt answers unsupported_capability.
	SteerUnsupported SteerSupport = "unsupported"
)

// HarnessCapabilities describes one prompt harness's controls.
type HarnessCapabilities struct {
	Name  string       `json:"name"`
	Steer SteerSupport `json:"steer"`
	// AttachmentTypes are accepted media types (e.g. "image/png"); empty
	// means attachments are not accepted.
	AttachmentTypes []string `json:"attachmentTypes"`
	// ReasoningEfforts are accepted PromptOptions.reasoningEffort values.
	ReasoningEfforts []string `json:"reasoningEfforts"`
	Fast             bool     `json:"fast"`
	// Experimental: the adapter follows the CLI's documentation but has
	// not yet been accepted against a real harness run; its flags and
	// event mapping may change.
	Experimental bool `json:"experimental,omitempty"`
}
