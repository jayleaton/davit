package api

import (
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pb33f/libopenapi"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"

	"davit/internal/domain"
)

const schemaRefPrefix = "#/components/schemas/"

// loadModel parses api.OpenAPI as OpenAPI 3.1 and builds the model with every
// $ref resolved, failing on any error.
func loadModel(t *testing.T) *v3.Document {
	t.Helper()
	doc, err := libopenapi.NewDocument(OpenAPI)
	if err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	m, err := doc.BuildV3Model()
	if err != nil {
		t.Fatalf("build v3 model: %v", err)
	}
	if m == nil {
		t.Fatal("build v3 model: nil model")
	}
	if errs := m.Index.GetReferenceIndexErrors(); len(errs) > 0 {
		t.Fatalf("reference index errors: %v", errs)
	}
	if r := m.Index.GetResolver(); r != nil {
		if errs := r.GetResolvingErrors(); len(errs) > 0 {
			t.Fatalf("ref resolving errors: %v", errs)
		}
	}
	return &m.Model
}

func schema(t *testing.T, d *v3.Document, name string) *base.Schema {
	t.Helper()
	p, ok := d.Components.Schemas.Get(name)
	if !ok || p == nil {
		t.Fatalf("components.schemas.%s missing", name)
	}
	s := p.Schema()
	if s == nil {
		t.Fatalf("components.schemas.%s: %v", name, p.GetBuildError())
	}
	return s
}

func TestOpenAPIDocumentValid(t *testing.T) {
	d := loadModel(t)
	if d.Version != "3.1.0" {
		t.Errorf("openapi = %q, want 3.1.0", d.Version)
	}
	if d.Info == nil || d.Info.Title != "Davit API" || d.Info.Version != "1.0.0" {
		t.Errorf("info = %+v, want {Davit API 1.0.0}", d.Info)
	}
	if len(d.Servers) == 0 || d.Servers[0].URL != "https://{host}/api/v1" {
		t.Errorf("servers[0] must be https://{host}%s", BasePath)
	}
	if _, ok := d.Components.SecuritySchemes.Get("bearerToken"); !ok {
		t.Error("components.securitySchemes.bearerToken missing")
	}
	if len(d.Security) != 2 {
		t.Errorf("security: want [bearerToken, {}], got %d alternatives", len(d.Security))
	}
	// Every schema, including ones only reached through $ref, must build.
	for name, p := range d.Components.Schemas.FromOldest() {
		if p.Schema() == nil {
			t.Errorf("schema %s does not build: %v", name, p.GetBuildError())
		}
	}
}

// expectedOperations is the full v1 operation set (SPEC §4 minus the M2 image
// pull/build operations) and whether M0 implements each.
var expectedOperations = map[string]bool{
	"getCapabilities":     true,
	"listImages":          true,
	"getImage":            true,
	"listVMs":             true,
	"createVM":            true,
	"getVM":               true,
	"deleteVM":            true,
	"updateVM":            true,
	"stopVM":              true,
	"resumeVM":            true,
	"forkVM":              true,
	"listVMSnapshots":     true,
	"snapshotVM":          true,
	"listSnapshots":       true,
	"getSnapshot":         true,
	"deleteSnapshot":      true,
	"getOperation":        true,
	"listOperationEvents": true,
	"listProcesses":       true,
	"execProcess":         true,
	"getProcess":          true,
	"killProcess":         true,
	"attachProcess":       true,
	"readFile":            true,
	"writeFile":           true,
	"openTunnel":          true,
	"closeTunnel":         true,
	"attachTunnel":        true,
	"issueSSHCredential":  true,
	"listTokens":          true,
	"createToken":         true,
	"revokeToken":         true,
	"rotateToken":         true,
	"whoami":              true,
	"createPrompt":        true,
	"getPrompt":           true,
	"cancelPrompt":        true,
	"listPromptEvents":    true,
	"listConversations":   true,
	"getConversation":     true,
	// G13 members and shares (feature members).
	"listMembers":  true,
	"addMember":    true,
	"updateMember": true,
	"removeMember": true,
	"listShares":   true,
	"createShare":  true,
	"deleteShare":  true,
	// G15 desktop (feature desktop).
	"getDesktop":   true,
	"openDesktop":  true,
	"closeDesktop": true,
	// G11 app endpoints (feature appEndpoints).
	"listAppEndpoints":  true,
	"createAppEndpoint": true,
	"updateAppEndpoint": true,
	"deleteAppEndpoint": true,
	// G10 reverse forwards (feature reverseForward).
	"listReverseForwards":     true,
	"createReverseForward":    true,
	"revokeReverseForward":    true,
	"attachReverseSession":    true,
	"attachReverseConnection": true,
	// Parity v1 contract (docs/research/CONTRACT-v1.md): specified, answering
	// not_implemented until built.
	"listEnvironments":          true,
	"createEnvironment":         true,
	"getEnvironment":            true,
	"deleteEnvironment":         true,
	"listEnvironmentRevisions":  true,
	"createEnvironmentRevision": true,
	"getEnvironmentRevision":    true,
	"listCredentialProfiles":    true,
	"createCredentialProfile":   true,
	"getCredentialProfile":      true,
	"deleteCredentialProfile":   true,
	"putCredential":             true,
	"deleteCredential":          true,
	"listCredentialGrants":      true,
	"createCredentialGrant":     true,
	"revokeCredentialGrant":     true,
	"listVMPromptEvents":        true,
	"interruptPrompts":          true,
	"steerPrompt":               true,
	"listRuns":                  true,
	"createRun":                 true,
	"getRun":                    true,
	"cancelRun":                 true,
	"startRun":                  true,
	"pushTree":                  true,
	"listProcessHistory":        true,
	"readProcessOutput":         true,
	"listGuestProcesses":        true,
	"recoverVM":                 true,
	"createVMs":                 true,
	"reclaimVMs":                true,
	"pruneImages":               true,
	"listSnapshotTree":          true,
	"readSnapshotFile":          true,
	"exportSnapshot":            true,
	"listEvents":                true,
	"listWebhooks":              true,
	"createWebhook":             true,
	"getWebhook":                true,
	"updateWebhook":             true,
	"deleteWebhook":             true,
	"rotateWebhookSecret":       true,
	"listWebhookDeliveries":     true,
	"retryWebhookDelivery":      true,
}

type opInfo struct {
	path, method string
	item         *v3.PathItem
	op           *v3.Operation
}

func operations(d *v3.Document) []opInfo {
	var ops []opInfo
	for path, item := range d.Paths.PathItems.FromOldest() {
		for method, op := range item.GetOperations().FromOldest() {
			ops = append(ops, opInfo{path, method, item, op})
		}
	}
	return ops
}

func TestOpenAPIOperations(t *testing.T) {
	d := loadModel(t)
	seen := map[string]string{}
	for _, o := range operations(d) {
		where := strings.ToUpper(o.method) + " " + o.path
		id := o.op.OperationId
		if id == "" {
			t.Errorf("%s: missing operationId", where)
			continue
		}
		if prev, dup := seen[id]; dup {
			t.Errorf("operationId %s used by %s and %s", id, prev, where)
		}
		seen[id] = where

		want, known := expectedOperations[id]
		if !known {
			t.Errorf("%s: unexpected operationId %s", where, id)
		}
		node, ok := o.op.Extensions.Get("x-davit-implemented")
		if !ok || node == nil || node.Tag != "!!bool" {
			t.Errorf("%s: x-davit-implemented must be a boolean", id)
		} else if known && (node.Value == "true") != want {
			t.Errorf("%s: x-davit-implemented = %s, want %v", id, node.Value, want)
		}
		if known && !want {
			if _, ok := o.op.Responses.Codes.Get("501"); !ok {
				t.Errorf("%s: not implemented but has no 501 response", id)
			}
		}
	}
	for id := range expectedOperations {
		if _, ok := seen[id]; !ok {
			t.Errorf("operationId %s missing from document", id)
		}
	}
}

var pathParamRE = regexp.MustCompile(`\{([^}]+)\}`)

func TestOpenAPIPathParameters(t *testing.T) {
	d := loadModel(t)
	for _, o := range operations(d) {
		declared := map[string]bool{}
		for _, p := range slices.Concat(o.item.Parameters, o.op.Parameters) {
			if p.In == "path" {
				if p.Required == nil || !*p.Required {
					t.Errorf("%s: path parameter %s must be required", o.op.OperationId, p.Name)
				}
				declared[p.Name] = true
			}
		}
		var inTemplate []string
		for _, m := range pathParamRE.FindAllStringSubmatch(o.path, -1) {
			inTemplate = append(inTemplate, m[1])
			if !declared[m[1]] {
				t.Errorf("%s %s: template parameter {%s} not declared in: path", o.op.OperationId, o.path, m[1])
			}
		}
		for name := range declared {
			if !slices.Contains(inTemplate, name) {
				t.Errorf("%s %s: path parameter %s not in template", o.op.OperationId, o.path, name)
			}
		}
	}
}

// wireTypes maps component schema names to the Go types they describe.
var wireTypes = map[string]reflect.Type{
	"VM":                               reflect.TypeFor[VM](),
	"Shape":                            reflect.TypeFor[Shape](),
	"ShapeOverride":                    reflect.TypeFor[ShapeOverride](),
	"Readiness":                        reflect.TypeFor[Readiness](),
	"Endpoint":                         reflect.TypeFor[Endpoint](),
	"StorageInfo":                      reflect.TypeFor[StorageInfo](),
	"Operation":                        reflect.TypeFor[Operation](),
	"OpResult":                         reflect.TypeFor[OpResult](),
	"OperationEvent":                   reflect.TypeFor[OperationEvent](),
	"Snapshot":                         reflect.TypeFor[Snapshot](),
	"Image":                            reflect.TypeFor[Image](),
	"HostCapabilities":                 reflect.TypeFor[HostCapabilities](),
	"RuntimeCapabilities":              reflect.TypeFor[domain.RuntimeCapabilities](),
	"ShapeConstraints":                 reflect.TypeFor[domain.ShapeConstraints](),
	"StorageCapabilities":              reflect.TypeFor[domain.StorageCapabilities](),
	"NetworkCapabilities":              reflect.TypeFor[domain.NetworkCapabilities](),
	"HostCheck":                        reflect.TypeFor[domain.HostCheck](),
	"CreateVMRequest":                  reflect.TypeFor[CreateVMRequest](),
	"ForkVMRequest":                    reflect.TypeFor[ForkVMRequest](),
	"StopVMRequest":                    reflect.TypeFor[StopVMRequest](),
	"UpdateVMRequest":                  reflect.TypeFor[UpdateVMRequest](),
	"Profile":                          reflect.TypeFor[Profile](),
	"ResumeVMRequest":                  reflect.TypeFor[ResumeVMRequest](),
	"SnapshotVMRequest":                reflect.TypeFor[SnapshotVMRequest](),
	"OperationAccepted":                reflect.TypeFor[OperationAccepted](),
	"VMList":                           reflect.TypeFor[VMList](),
	"SnapshotList":                     reflect.TypeFor[SnapshotList](),
	"ImageList":                        reflect.TypeFor[ImageList](),
	"OperationEventList":               reflect.TypeFor[OperationEventList](),
	"Error":                            reflect.TypeFor[Error](),
	"ErrorResponse":                    reflect.TypeFor[ErrorResponse](),
	"ExecRequest":                      reflect.TypeFor[ExecRequest](),
	"Process":                          reflect.TypeFor[Process](),
	"ProcessList":                      reflect.TypeFor[ProcessList](),
	"ProcessStreamMessage":             reflect.TypeFor[ProcessStreamMessage](),
	"FileInfo":                         reflect.TypeFor[FileInfo](),
	"Egress":                           reflect.TypeFor[Egress](),
	"PrivacyPolicy":                    reflect.TypeFor[PrivacyPolicy](),
	"PromptRequest":                    reflect.TypeFor[PromptRequest](),
	"Prompt":                           reflect.TypeFor[Prompt](),
	"PromptUsage":                      reflect.TypeFor[PromptUsage](),
	"PromptError":                      reflect.TypeFor[PromptError](),
	"Conversation":                     reflect.TypeFor[Conversation](),
	"ConversationList":                 reflect.TypeFor[ConversationList](),
	"PromptEvent":                      reflect.TypeFor[PromptEvent](),
	"PromptEventList":                  reflect.TypeFor[PromptEventList](),
	"OpenTunnelRequest":                reflect.TypeFor[OpenTunnelRequest](),
	"Tunnel":                           reflect.TypeFor[Tunnel](),
	"ReverseTarget":                    reflect.TypeFor[ReverseTarget](),
	"AppEndpoint":                      reflect.TypeFor[AppEndpoint](),
	"Member":                           reflect.TypeFor[Member](),
	"AddMemberRequest":                 reflect.TypeFor[AddMemberRequest](),
	"UpdateMemberRequest":              reflect.TypeFor[UpdateMemberRequest](),
	"MemberList":                       reflect.TypeFor[MemberList](),
	"Share":                            reflect.TypeFor[Share](),
	"CreateShareRequest":               reflect.TypeFor[CreateShareRequest](),
	"ShareList":                        reflect.TypeFor[ShareList](),
	"OpenDesktopRequest":               reflect.TypeFor[OpenDesktopRequest](),
	"CreateAppEndpointRequest":         reflect.TypeFor[CreateAppEndpointRequest](),
	"UpdateAppEndpointRequest":         reflect.TypeFor[UpdateAppEndpointRequest](),
	"AppEndpointList":                  reflect.TypeFor[AppEndpointList](),
	"CreateReverseForwardRequest":      reflect.TypeFor[CreateReverseForwardRequest](),
	"ReverseForward":                   reflect.TypeFor[ReverseForward](),
	"ReverseForwardList":               reflect.TypeFor[ReverseForwardList](),
	"ReverseSessionEvent":              reflect.TypeFor[ReverseSessionEvent](),
	"IssueSSHCredentialRequest":        reflect.TypeFor[IssueSSHCredentialRequest](),
	"TokenScope":                       reflect.TypeFor[TokenScope](),
	"CreateTokenRequest":               reflect.TypeFor[CreateTokenRequest](),
	"Token":                            reflect.TypeFor[Token](),
	"CreatedToken":                     reflect.TypeFor[CreatedToken](),
	"RotateTokenRequest":               reflect.TypeFor[RotateTokenRequest](),
	"Me":                               reflect.TypeFor[Me](),
	"MeMember":                         reflect.TypeFor[MeMember](),
	"TokenList":                        reflect.TypeFor[TokenList](),
	"SSHCredential":                    reflect.TypeFor[SSHCredential](),
	"Feature":                          reflect.TypeFor[Feature](),
	"HarnessCapabilities":              reflect.TypeFor[HarnessCapabilities](),
	"Environment":                      reflect.TypeFor[Environment](),
	"EnvironmentRevision":              reflect.TypeFor[EnvironmentRevision](),
	"EnvManifest":                      reflect.TypeFor[EnvManifest](),
	"RepoSpec":                         reflect.TypeFor[RepoSpec](),
	"SecretFileRef":                    reflect.TypeFor[SecretFileRef](),
	"SetupAction":                      reflect.TypeFor[SetupAction](),
	"EnvironmentRef":                   reflect.TypeFor[EnvironmentRef](),
	"EnvironmentPin":                   reflect.TypeFor[EnvironmentPin](),
	"Workspace":                        reflect.TypeFor[Workspace](),
	"SetupError":                       reflect.TypeFor[SetupError](),
	"CreateEnvironmentRequest":         reflect.TypeFor[CreateEnvironmentRequest](),
	"CreateEnvironmentRevisionRequest": reflect.TypeFor[CreateEnvironmentRevisionRequest](),
	"EnvironmentList":                  reflect.TypeFor[EnvironmentList](),
	"EnvironmentRevisionList":          reflect.TypeFor[EnvironmentRevisionList](),
	"Credential":                       reflect.TypeFor[Credential](),
	"CredentialProfile":                reflect.TypeFor[CredentialProfile](),
	"CreateCredentialProfileRequest":   reflect.TypeFor[CreateCredentialProfileRequest](),
	"PutCredentialRequest":             reflect.TypeFor[PutCredentialRequest](),
	"CredentialProfileList":            reflect.TypeFor[CredentialProfileList](),
	"CredentialGrant":                  reflect.TypeFor[CredentialGrant](),
	"CreateCredentialGrantRequest":     reflect.TypeFor[CreateCredentialGrantRequest](),
	"CredentialGrantList":              reflect.TypeFor[CredentialGrantList](),
	"HostCapacity":                     reflect.TypeFor[HostCapacity](),
	"CapacityWaiter":                   reflect.TypeFor[CapacityWaiter](),
	"ColdCapacity":                     reflect.TypeFor[ColdCapacity](),
	"Limits":                           reflect.TypeFor[Limits](),
	"RecoverVMRequest":                 reflect.TypeFor[RecoverVMRequest](),
	"CreateVMsRequest":                 reflect.TypeFor[CreateVMsRequest](),
	"CreateVMsResult":                  reflect.TypeFor[CreateVMsResult](),
	"ReclaimVMsRequest":                reflect.TypeFor[ReclaimVMsRequest](),
	"ReclaimVMsResult":                 reflect.TypeFor[ReclaimVMsResult](),
	"ReclaimedVM":                      reflect.TypeFor[ReclaimedVM](),
	"PruneImagesRequest":               reflect.TypeFor[PruneImagesRequest](),
	"PruneImagesResult":                reflect.TypeFor[PruneImagesResult](),
	"PrunedBlob":                       reflect.TypeFor[PrunedBlob](),
	"AutoDeletion":                     reflect.TypeFor[AutoDeletion](),
	"BatchItem":                        reflect.TypeFor[BatchItem](),
	"PromptAttachment":                 reflect.TypeFor[PromptAttachment](),
	"PromptOptions":                    reflect.TypeFor[PromptOptions](),
	"PromptFeedItem":                   reflect.TypeFor[PromptFeedItem](),
	"PromptFeed":                       reflect.TypeFor[PromptFeed](),
	"InterruptRequest":                 reflect.TypeFor[InterruptRequest](),
	"InterruptResult":                  reflect.TypeFor[InterruptResult](),
	"SteerRequest":                     reflect.TypeFor[SteerRequest](),
	"SteerResult":                      reflect.TypeFor[SteerResult](),
	"RunRequest":                       reflect.TypeFor[RunRequest](),
	"RunList":                          reflect.TypeFor[RunList](),
	"Run":                              reflect.TypeFor[Run](),
	"PushResult":                       reflect.TypeFor[PushResult](),
	"HistoryEntry":                     reflect.TypeFor[HistoryEntry](),
	"Event":                            reflect.TypeFor[Event](),
	"EventList":                        reflect.TypeFor[EventList](),
	"Webhook":                          reflect.TypeFor[Webhook](),
	"WebhookList":                      reflect.TypeFor[WebhookList](),
	"CreateWebhookRequest":             reflect.TypeFor[CreateWebhookRequest](),
	"UpdateWebhookRequest":             reflect.TypeFor[UpdateWebhookRequest](),
	"RotateWebhookSecretRequest":       reflect.TypeFor[RotateWebhookSecretRequest](),
	"WebhookSecret":                    reflect.TypeFor[WebhookSecret](),
	"WebhookDelivery":                  reflect.TypeFor[WebhookDelivery](),
	"WebhookDeliveryList":              reflect.TypeFor[WebhookDeliveryList](),
	"WebhookPayload":                   reflect.TypeFor[WebhookPayload](),
	"HistoryList":                      reflect.TypeFor[HistoryList](),
	"ProcessOutput":                    reflect.TypeFor[ProcessOutput](),
	"GuestProcess":                     reflect.TypeFor[GuestProcess](),
	"GuestProcessList":                 reflect.TypeFor[GuestProcessList](),
	"SnapshotEntry":                    reflect.TypeFor[SnapshotEntry](),
	"SnapshotTree":                     reflect.TypeFor[SnapshotTree](),
	// ErrorInfo is the error embedded in Operation.error and VM.lastError.
	"ErrorInfo": reflect.TypeFor[domain.Error](),
}

// enumTypes maps Go string enum types to their component schema names. Fields
// of these types must $ref the named schema.
var enumTypes = map[reflect.Type]string{
	reflect.TypeFor[domain.VMState]():          "VMState",
	reflect.TypeFor[domain.OperationState]():   "OperationState",
	reflect.TypeFor[domain.OperationKind]():    "OperationKind",
	reflect.TypeFor[domain.BootMode]():         "BootMode",
	reflect.TypeFor[domain.SnapshotKind]():     "SnapshotKind",
	reflect.TypeFor[domain.Consistency]():      "Consistency",
	reflect.TypeFor[domain.Arch]():             "Arch",
	reflect.TypeFor[domain.StorageTier]():      "StorageTier",
	reflect.TypeFor[domain.Code]():             "ErrorCode",
	reflect.TypeFor[ProcessStatus]():           "ProcessStatus",
	reflect.TypeFor[PromptState]():             "PromptState",
	reflect.TypeFor[domain.EgressMode]():       "EgressMode",
	reflect.TypeFor[domain.FeatureStatus]():    "FeatureStatus",
	reflect.TypeFor[domain.SteerSupport]():     "SteerSupport",
	reflect.TypeFor[domain.WorkspaceState]():   "WorkspaceState",
	reflect.TypeFor[domain.CredentialKind]():   "CredentialKind",
	reflect.TypeFor[domain.GrantState]():       "GrantState",
	reflect.TypeFor[domain.RevokeReason]():     "RevokeReason",
	reflect.TypeFor[RunState]():                "RunState",
	reflect.TypeFor[OutputNotRetainedReason](): "OutputNotRetainedReason",
}

type goField struct {
	name     string
	typ      reflect.Type
	required bool
}

func jsonFields(t reflect.Type) []goField {
	var out []goField
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		omit := slices.Contains(strings.Split(opts, ","), "omitempty") ||
			slices.Contains(strings.Split(opts, ","), "omitzero")
		out = append(out, goField{name, f.Type, !omit && f.Type.Kind() != reflect.Pointer})
	}
	return out
}

var timeType = reflect.TypeFor[time.Time]()

// jsonKind returns the JSON Schema type and expected format for a Go type; an
// empty kind means any JSON value.
func jsonKind(t reflect.Type) (kind, format string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return "string", "date-time"
	}
	switch t.Kind() {
	case reflect.String:
		return "string", ""
	case reflect.Bool:
		return "boolean", ""
	case reflect.Int64, reflect.Uint64:
		return "integer", "int64"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return "integer", ""
	case reflect.Float32, reflect.Float64:
		return "number", ""
	case reflect.Slice, reflect.Array:
		return "array", ""
	case reflect.Map, reflect.Struct:
		return "object", ""
	}
	return "", ""
}

func refName(p *base.SchemaProxy) string {
	if p == nil || !p.IsReference() {
		return ""
	}
	return strings.TrimPrefix(p.GetReference(), schemaRefPrefix)
}

func schemaKind(s *base.Schema) string {
	for _, typ := range s.Type {
		if typ != "null" {
			return typ
		}
	}
	return ""
}

// checkValue checks that the schema behind p can carry Go values of type t.
func checkValue(t *testing.T, where string, p *base.SchemaProxy, typ reflect.Type) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	s := p.Schema()
	if s == nil {
		t.Errorf("%s: schema does not build: %v", where, p.GetBuildError())
		return
	}
	wantKind, wantFormat := jsonKind(typ)
	if wantKind == "" {
		if len(s.Type) != 0 {
			t.Errorf("%s: Go type %v is any JSON value, schema type %v", where, typ, s.Type)
		}
		return
	}
	if got := schemaKind(s); got != wantKind {
		t.Errorf("%s: schema type %q, Go %v wants %q", where, got, typ, wantKind)
		return
	}
	if wantFormat != "" && s.Format != wantFormat {
		t.Errorf("%s: schema format %q, Go %v wants %q", where, s.Format, typ, wantFormat)
	}
	if enum, ok := enumTypes[typ]; ok && refName(p) != enum {
		t.Errorf("%s: Go %v must $ref %s, got %q", where, typ, enum, refName(p))
	}
	switch {
	case typ.Kind() == reflect.Struct && typ != timeType:
		name := refName(p)
		if wireTypes[name] != typ {
			t.Errorf("%s: Go %v must $ref its own component schema, got %q", where, typ, name)
		}
	case typ.Kind() == reflect.Slice:
		if s.Items == nil || !s.Items.IsA() {
			t.Errorf("%s: array without items schema", where)
			return
		}
		checkValue(t, where+"[]", s.Items.A, typ.Elem())
	case typ.Kind() == reflect.Map:
		ap := s.AdditionalProperties
		if ap == nil {
			t.Errorf("%s: map needs additionalProperties", where)
			return
		}
		if ap.IsA() {
			checkValue(t, where+"{}", ap.A, typ.Elem())
		} else if !ap.B {
			t.Errorf("%s: map has additionalProperties: false", where)
		} else if typ.Elem().Kind() != reflect.Interface {
			t.Errorf("%s: map of %v needs a typed additionalProperties schema", where, typ.Elem())
		}
	}
}

func TestOpenAPISchemasMatchGoTypes(t *testing.T) {
	d := loadModel(t)
	names := make([]string, 0, len(wireTypes))
	for n := range wireTypes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		typ := wireTypes[name]
		t.Run(name, func(t *testing.T) {
			s := schema(t, d, name)
			if schemaKind(s) != "object" {
				t.Fatalf("type %v, want object", s.Type)
			}
			props := map[string]*base.SchemaProxy{}
			if s.Properties != nil {
				for k, v := range s.Properties.FromOldest() {
					props[k] = v
				}
			}
			var goNames, goRequired []string
			for _, f := range jsonFields(typ) {
				goNames = append(goNames, f.name)
				if f.required {
					goRequired = append(goRequired, f.name)
				}
				p, ok := props[f.name]
				if !ok {
					continue
				}
				checkValue(t, name+"."+f.name, p, f.typ)
			}
			schemaNames := make([]string, 0, len(props))
			for k := range props {
				schemaNames = append(schemaNames, k)
			}
			assertSameSet(t, "properties", schemaNames, goNames)
			assertSameSet(t, "required", s.Required, goRequired)
		})
	}
}

func assertSameSet(t *testing.T, what string, schemaSide, goSide []string) {
	t.Helper()
	for _, v := range schemaSide {
		if !slices.Contains(goSide, v) {
			t.Errorf("%s: %q in schema but not in Go", what, v)
		}
	}
	for _, v := range goSide {
		if !slices.Contains(schemaSide, v) {
			t.Errorf("%s: %q in Go but not in schema", what, v)
		}
	}
}

func enumOf(t *testing.T, d *v3.Document, name string) []string {
	t.Helper()
	s := schema(t, d, name)
	out := make([]string, 0, len(s.Enum))
	for _, n := range s.Enum {
		out = append(out, n.Value)
	}
	return out
}

func strs[T ~string](vs ...T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

func TestOpenAPIEnumsMatchDomain(t *testing.T) {
	d := loadModel(t)
	cases := map[string][]string{
		"ErrorCode": strs(
			domain.CodeInvalidArgument, domain.CodeUnauthenticated, domain.CodePermissionDenied,
			domain.CodeNotFound, domain.CodeOperationInProgress, domain.CodeIdempotencyConflict,
			domain.CodeInvalidState, domain.CodeStopRequired, domain.CodeRevisionMismatch,
			domain.CodePayloadTooLarge, domain.CodeUnsupportedCapability, domain.CodeResourceExhausted,
			domain.CodeUnavailable, domain.CodeNotImplemented, domain.CodeTimeout, domain.CodeInternal,
		),
		"VMState":        strs(domain.AllStates...),
		"OperationState": strs(domain.OpQueued, domain.OpRunning, domain.OpSucceeded, domain.OpFailed),
		"OperationKind": strs(domain.OpCreate, domain.OpFork, domain.OpStop, domain.OpResume,
			domain.OpDelete, domain.OpSnapshot, domain.OpRecover),
		"BootMode": strs(domain.BootModeNone, domain.BootModeColdBoot, domain.BootModeGoldenRestore,
			domain.BootModeBootstrapRestore, domain.BootModeRAMRestore),
		"SnapshotKind": strs(domain.SnapshotKindDisk, domain.SnapshotKindRAM),
		"Consistency":  strs(domain.ConsistencyClean, domain.ConsistencyCrash),
		"Arch":         strs(domain.ArchAMD64, domain.ArchARM64),
		"StorageTier":  strs(domain.TierHot, domain.TierCold),
		"ProcessStatus": strs(ProcessStarting, ProcessRunning, ProcessExited, ProcessKilled,
			ProcessUnknown, ProcessLost),
		"PromptState":   strs(PromptStarting, PromptRunning, PromptSucceeded, PromptFailed, PromptCancelled),
		"EgressMode":    strs(domain.EgressOff, domain.EgressFiltered, domain.EgressAllowlist),
		"FeatureStatus": strs(domain.FeatureAvailable, domain.FeatureNotImplemented, domain.FeatureUnsupported),
		"SteerSupport":  strs(domain.SteerNative, domain.SteerRestart, domain.SteerUnsupported),
		"WorkspaceState": strs(domain.WorkspacePending, domain.WorkspacePreparing, domain.WorkspaceReady,
			domain.WorkspaceFailed),
		"CredentialKind": strs(domain.CredentialAPIKey, domain.CredentialCustomAPI),
		"GrantState":     strs(domain.GrantActive, domain.GrantRevoked),
		"RevokeReason": strs(domain.RevokeExplicit, domain.RevokeVMDeleted, domain.RevokeProfileDeleted,
			domain.RevokeCredentialRemoved),
		"RunState":                strs(RunCreating, RunWaiting, RunRunning, RunSucceeded, RunFailed, RunCancelled),
		"OutputNotRetainedReason": strs(OutputNotRetainedPolicy),
		"ReverseTargetKind":       strs(ReverseTargetSession, ReverseTargetHost),
		"MemberRole":              strs(RoleViewer, RoleExec, RoleAdmin),
		"ShareKind":               strs(ShareVM, ShareSnapshot, ShareEnvironment),
		"AppEndpointKind":         strs(AppEndpointHTTP, AppEndpointDesktop),
		"AppEndpointState":        strs(AppEndpointActive, AppEndpointHidden, AppEndpointVMNotRunning, AppEndpointGatewayDown),
		"ReverseForwardState":     strs(ReverseAwaitingSession, ReverseActive),
		"ReverseSessionEventType": strs(ReverseEventConnection, ReverseEventPing, ReverseEventRevoked),
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got := enumOf(t, d, name)
			if len(got) != len(want) {
				t.Errorf("enum has %d values %v, want %d %v", len(got), got, len(want), want)
			}
			assertSameSet(t, "enum", got, want)
		})
	}
}

func TestOpenAPIListParameters(t *testing.T) {
	d := loadModel(t)
	for _, name := range []string{"Limit", "Cursor"} {
		if _, ok := d.Components.Parameters.Get(name); !ok {
			t.Fatalf("components.parameters.%s missing", name)
		}
	}
	limit, _ := d.Components.Parameters.Get("Limit")
	ls := limit.Schema.Schema()
	if ls.Maximum == nil || int(*ls.Maximum) != MaxListLimit {
		t.Errorf("limit maximum = %v, want %d", ls.Maximum, MaxListLimit)
	}
	if ls.Default == nil || ls.Default.Value != "50" || DefaultListLimit != 50 {
		t.Errorf("limit default must be %d", DefaultListLimit)
	}
	for _, name := range []string{"IdempotencyKeyRequired", "IdempotencyKeyOptional"} {
		p, _ := d.Components.Parameters.Get(name)
		if p == nil || p.Name != HeaderIdempotencyKey {
			t.Errorf("%s must be the %s header", name, HeaderIdempotencyKey)
		}
	}
	if p, _ := d.Components.Parameters.Get("IfMatch"); p == nil || p.Name != HeaderIfMatch {
		t.Errorf("IfMatch must be the %s header", HeaderIfMatch)
	}
	if p, _ := d.Components.Parameters.Get("RequestIdHeader"); p == nil || p.Name != HeaderRequestID {
		t.Errorf("RequestIdHeader must be the %s header", HeaderRequestID)
	}
	for _, h := range []string{HeaderRequestID, HeaderRetryAfter, "Location"} {
		if _, ok := d.Components.Headers.Get(h); !ok {
			t.Errorf("components.headers.%s missing", h)
		}
	}
}
