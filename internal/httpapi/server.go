// Package httpapi is the HTTP transport for api/openapi.yaml. It owns no
// lifecycle state: it authenticates, decodes, calls lifecycle and maps
// domain errors to HTTP. Listeners are pluggable: M0 serves the local Unix
// socket; the gateway workstream adds a tsnet listener with a WhoIs
// Authenticator on the same Handler.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"davit/api"
	"davit/internal/appendpoint"
	"davit/internal/credproxy"
	"davit/internal/domain"
	"davit/internal/events"
	"davit/internal/guestexec"
	"davit/internal/harness"
	"davit/internal/jobs"
	"davit/internal/lifecycle"
	"davit/internal/reversefwd"
	"davit/internal/runs"
	"davit/internal/snapfiles"
	"davit/internal/tokens"
	"davit/internal/webhooks"
)

// Authenticator maps a request to a principal or fails closed. It must use
// transport facts only (peer credentials, tsnet WhoIs on the accepted peer
// address), never client-supplied identity headers.
type Authenticator interface {
	Authenticate(r *http.Request) (domain.PrincipalID, error)
}

// MaxBodyBytes bounds request bodies.
const MaxBodyBytes = 1 << 20

// Server serves API v1.
type Server struct {
	svc      *lifecycle.Service
	auth     Authenticator
	log      *slog.Logger
	procs    *guestexec.Manager
	prompts  *harness.Service
	jobs     *jobs.Service
	runs     *runs.Service
	ci       CIRunners
	sshCreds SSHCredentialIssuer
	tokens   *tokens.Service
	// credentials is the G4 control plane (credentials.go).
	credentials *credproxy.Control
	// snapFiles serves snapshot file reads (nil: not_implemented).
	snapFiles *snapfiles.Service
	reverse   *reversefwd.Service
	members   MembersAPI
	sessions  SessionTracker
	apps      *appendpoint.Service
	// events and webhooks serve G16 (nil: not_implemented).
	events   *events.Service
	webhooks *webhooks.Service
}

// New returns a server.
func New(svc *lifecycle.Service, auth Authenticator, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{svc: svc, auth: auth, log: log}
}

type ctxKey int

const (
	keyPrincipal ctxKey = iota
	keyRequestID
)

type handler func(w http.ResponseWriter, r *http.Request) error

// Handler returns the routed API handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, need access, h handler) { mux.Handle(pattern, s.wrapLimit(h, MaxBodyBytes, need)) }
	upload := func(pattern string, need access, h handler) {
		mux.Handle(pattern, s.wrapLimit(h, MaxUploadBytes, need))
	}
	b := api.BasePath
	view, exec, admin := api.TokenView, api.TokenExec, api.TokenAdmin
	route("GET "+b+"/capabilities", anyVM(view), s.getCapabilities)
	route("GET "+b+"/images", anyVM(view), s.listImages)
	route("GET "+b+"/images/{imageId}", anyVM(view), s.getImage)
	route("GET "+b+"/vms", listAll(view), s.listVMs)
	route("POST "+b+"/vms", allVMs(admin), s.createVM)
	route("GET "+b+"/vms/{vmId}", onVM(view), s.getVM)
	route("DELETE "+b+"/vms/{vmId}", onVM(admin), s.deleteVM)
	route("PATCH "+b+"/vms/{vmId}", onVM(admin), s.updateVM)
	route("POST "+b+"/vms/{vmId}/stop", onVM(admin), s.stopVM)
	route("POST "+b+"/vms/{vmId}/resume", onVM(admin), s.resumeVM)
	route("POST "+b+"/vms/{vmId}/fork", noMembers(onVM(admin)), s.forkVM) // a fork is a new VM of the owner's
	route("GET "+b+"/vms/{vmId}/snapshots", onVM(view), s.listVMSnapshots)
	route("GET "+b+"/snapshots", listAll(view), s.listSnapshots)
	route("GET "+b+"/snapshots/{snapshotId}", onSnapshot(view), s.getSnapshot)
	route("POST "+b+"/vms/{vmId}/snapshots", onVM(admin), s.snapshotVM)
	route("DELETE "+b+"/snapshots/{snapshotId}", onSnapshot(admin), s.deleteSnapshot)
	route("GET "+b+"/operations/{operationId}", onOperation(view), s.getOperation)
	route("GET "+b+"/operations/{operationId}/events", onOperation(view), s.listOperationEvents)
	route("GET "+b+"/vms/{vmId}/processes", onVM(view), s.listProcesses)
	route("POST "+b+"/vms/{vmId}/processes", onVM(exec), s.execProcess)
	route("GET "+b+"/vms/{vmId}/processes/{processId}", onVM(view), s.getProcess)
	route("DELETE "+b+"/vms/{vmId}/processes/{processId}", onVM(exec), s.killProcess)
	route("GET "+b+"/vms/{vmId}/processes/{processId}/stream", onVM(exec), s.attachProcess)
	route("POST "+b+"/vms/{vmId}/prompts", onVM(exec), s.createPrompt)
	route("GET "+b+"/vms/{vmId}/prompts/{promptId}", onVM(view), s.getPrompt)
	route("DELETE "+b+"/vms/{vmId}/prompts/{promptId}", onVM(exec), s.cancelPrompt)
	// Prompt events carry what the agent said, ran and read: exec (SEC-3).
	route("GET "+b+"/vms/{vmId}/prompts/{promptId}/events", onVM(exec), s.listPromptEvents)
	route("POST "+b+"/vms/{vmId}/prompts/{promptId}/steer", onVM(exec), s.steerPrompt)
	route("GET "+b+"/vms/{vmId}/prompt-events", onVM(exec), s.listVMPromptEvents)
	route("POST "+b+"/vms/{vmId}/interrupt", onVM(exec), s.interruptPrompts)
	route("GET "+b+"/vms/{vmId}/conversations", onVM(view), s.listConversations)
	route("GET "+b+"/vms/{vmId}/conversations/{conversationId}", onVM(view), s.getConversation)
	route("GET "+b+"/vms/{vmId}/files", onVM(exec), s.readFile)
	upload("PUT "+b+"/vms/{vmId}/files", onVM(exec), s.writeFile)
	route("POST "+b+"/vms/{vmId}/tunnels", onVM(exec), s.openTunnel)
	route("DELETE "+b+"/tunnels/{tunnelId}", onTunnel(exec), s.closeTunnel)
	route("GET "+b+"/tunnels/{tunnelId}/stream", onTunnel(exec), s.attachTunnel)
	route("POST "+b+"/vms/{vmId}/ssh-credentials", onVM(exec), s.issueSSHCredential)
	// Snapshot files expose a VM's disk contents: exec, as readFile.
	route("GET "+b+"/snapshots/{snapshotId}/tree", onSnapshot(exec), s.listSnapshotTree)
	route("GET "+b+"/snapshots/{snapshotId}/files", onSnapshot(exec), s.readSnapshotFile)
	route("GET "+b+"/snapshots/{snapshotId}/export", onSnapshot(exec), s.exportSnapshot)
	route("GET "+b+"/tokens", ownerOnly, s.listTokens)
	route("POST "+b+"/tokens", ownerOnly, s.createToken)
	s.g18Routes(route) // g18.go
	route("DELETE "+b+"/tokens/{tokenId}", ownerOnly, s.revokeToken)
	s.credentialRoutes(route)
	s.memberRoutes(route)
	for _, rr := range append(append(s.reverseRoutes(), s.appEndpointRoutes()...), s.desktopRoutes()...) {
		route(rr.pattern, rr.need, rr.h)
	}
	route("GET "+b+"/vms/{vmId}/history", onVM(view), s.listProcessHistory)
	route("GET "+b+"/vms/{vmId}/ps", onVM(view), s.listGuestProcesses)
	// Retained output is what attachProcess streams: exec (SEC-3).
	route("GET "+b+"/vms/{vmId}/processes/{processId}/output", onVM(exec), s.readProcessOutput)
	route("GET "+b+"/environments", ownerOrMemberList(), s.listEnvironments)
	route("POST "+b+"/environments", ownerOnly, s.createEnvironment)
	route("GET "+b+"/environments/{environmentId}", onEnvironment(view), s.getEnvironment)
	route("DELETE "+b+"/environments/{environmentId}", ownerOnly, s.deleteEnvironment)
	route("GET "+b+"/environments/{environmentId}/revisions", onEnvironment(view), s.listEnvironmentRevisions)
	route("POST "+b+"/environments/{environmentId}/revisions", ownerOnly, s.createEnvironmentRevision)
	route("GET "+b+"/environments/{environmentId}/revisions/{revision}", onEnvironment(view), s.getEnvironmentRevision)
	route("GET "+b+"/ci/status", ownerOnly, s.getCIStatus)
	route("GET "+b+"/runs", allVMs(view), s.listRuns)
	route("POST "+b+"/runs", allVMs(admin), s.createRun)
	route("GET "+b+"/runs/{runId}", allVMs(view), s.getRun)
	route("DELETE "+b+"/runs/{runId}", allVMs(admin), s.cancelRun)
	route("POST "+b+"/runs/{runId}/start", allVMs(admin), s.startRun)
	upload("PUT "+b+"/vms/{vmId}/tree", onVM(exec), s.pushTree)
	s.g9Routes(route)
	s.eventsRoutes(route)
	for _, pr := range parityRoutes() {
		if pr.upload {
			upload(pr.pattern, pr.need, notImplemented(pr.feature, pr.operation))
		} else {
			route(pr.pattern, pr.need, notImplemented(pr.feature, pr.operation))
		}
	}
	route(b+"/", ownerOnly, func(http.ResponseWriter, *http.Request) error {
		return domain.Errorf(domain.CodeNotFound, "no such endpoint")
	})
	return mux
}

func newRequestID() string {
	var b [8]byte
	rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

func validRequestID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// wrapLimit authenticates, enforces a token grant against need, bounds the
// body to limit bytes and maps errors.
func (s *Server) wrapLimit(h handler, limit int64, need access) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get(api.HeaderRequestID)
		if !validRequestID(rid) {
			rid = newRequestID()
		}
		w.Header().Set(api.HeaderRequestID, rid)
		ctx := context.WithValue(r.Context(), keyRequestID, rid)
		var p domain.PrincipalID
		var g *tokens.Grant
		var err error
		if ga, ok := s.auth.(GrantAuthenticator); ok {
			p, g, err = ga.AuthenticateGrant(r)
		} else {
			p, err = s.auth.Authenticate(r)
		}
		if err == nil {
			r = r.WithContext(context.WithValue(ctx, keyPrincipal, p))
			if g != nil {
				var vm domain.VMID
				if vm, err = s.checkGrant(r, p, g, need); err == nil {
					r = r.WithContext(context.WithValue(r.Context(), keyGrant, g))
					var done func()
					if g.Member != nil { // revocable: unsharing cuts it
						w, r, done = s.trackMember(w, r, g, vm)
					} else { // revoking, rotating or expiring the token cuts it (SEC-4)
						w, r, done = s.trackToken(w, r, g)
					}
					defer done()
				}
			}
		}
		if err == nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			err = h(w, r)
		}
		if err != nil {
			s.writeError(w, rid, err)
		}
	})
}

func principal(r *http.Request) domain.PrincipalID {
	p, _ := r.Context().Value(keyPrincipal).(domain.PrincipalID)
	return p
}

// StatusOf maps an error code to its HTTP status (api/openapi.yaml).
func StatusOf(c domain.Code) int {
	switch c {
	case domain.CodeInvalidArgument:
		return http.StatusBadRequest
	case domain.CodeUnauthenticated:
		return http.StatusUnauthorized
	case domain.CodePermissionDenied:
		return http.StatusForbidden
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeOperationInProgress, domain.CodeIdempotencyConflict, domain.CodeInvalidState, domain.CodeStopRequired:
		return http.StatusConflict
	case domain.CodeRevisionMismatch:
		return http.StatusPreconditionFailed
	case domain.CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case domain.CodeUnsupportedCapability:
		return http.StatusUnprocessableEntity
	case domain.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case domain.CodeNotImplemented:
		return http.StatusNotImplemented
	case domain.CodeUnavailable:
		return http.StatusServiceUnavailable
	case domain.CodeTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}

func (s *Server) writeError(w http.ResponseWriter, rid string, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		err = domain.Errorf(domain.CodePayloadTooLarge, "request body exceeds %d bytes", mbe.Limit)
	}
	de := domain.AsError(err)
	if de.Code == domain.CodeInternal {
		s.log.Error("internal error", "requestId", rid, "err", err)
	}
	status := StatusOf(de.Code)
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		w.Header().Set(api.HeaderRetryAfter, "1")
	}
	writeJSON(w, status, api.ErrorResponse{Error: api.Error{
		Code: de.Code, Message: de.Message, Retryable: de.Retryable, Details: de.Details,
		RequestID: rid, OperationID: de.OperationID,
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

// decode reads a JSON body into v. An empty body leaves v zero when
// optional; unknown fields are rejected so typos never pass silently.
func decode(r *http.Request, v any, optional bool) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if errors.Is(err, io.EOF) && optional {
		return nil
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return err
	}
	if err != nil {
		return domain.Errorf(domain.CodeInvalidArgument, "invalid JSON body: %v", err)
	}
	if dec.More() {
		return domain.Errorf(domain.CodeInvalidArgument, "invalid JSON body: trailing data")
	}
	return nil
}

func mutation(r *http.Request) (lifecycle.Request, error) {
	req := lifecycle.Request{
		Principal: principal(r), IdempotencyKey: r.Header.Get(api.HeaderIdempotencyKey),
		Method: r.Method, Path: r.URL.Path,
	}
	if m := r.Header.Get(api.HeaderIfMatch); m != "" {
		rev, err := strconv.ParseInt(strings.Trim(m, `"`), 10, 64)
		if err != nil {
			return req, domain.Errorf(domain.CodeInvalidArgument, "If-Match must be an integer revision")
		}
		req.IfMatch = &rev
	}
	return req, nil
}

func accepted(w http.ResponseWriter, a *lifecycle.Accepted) error {
	w.Header().Set("Location", api.BasePath+"/operations/"+string(a.Operation.ID))
	return writeJSON(w, http.StatusAccepted, api.OperationAccepted{Operation: a.Operation, VM: a.VM})
}

func page(r *http.Request) (lifecycle.Page, error) {
	q := r.URL.Query()
	p := lifecycle.Page{Limit: api.DefaultListLimit, Cursor: q.Get("cursor")}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > api.MaxListLimit {
			return p, domain.Errorf(domain.CodeInvalidArgument, "limit must be 1..%d", api.MaxListLimit).WithDetail("field", "limit")
		}
		p.Limit = n
	}
	return p, nil
}

func (s *Server) getCapabilities(w http.ResponseWriter, r *http.Request) error {
	return writeJSON(w, http.StatusOK, s.svc.Capabilities())
}

func (s *Server) listImages(w http.ResponseWriter, r *http.Request) error {
	p, err := page(r)
	if err != nil {
		return err
	}
	imgs, next, err := s.svc.ListImages(r.Context(), p)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, api.ImageList{Items: imgs, NextCursor: next})
}

func (s *Server) getImage(w http.ResponseWriter, r *http.Request) error {
	img, err := s.svc.GetImage(r.Context(), domain.ImageID(r.PathValue("imageId")))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, img)
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) error {
	p, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	var states []domain.VMState
	for _, v := range q["state"] {
		for _, st := range strings.Split(v, ",") {
			states = append(states, domain.VMState(st))
		}
	}
	all, _ := strconv.ParseBool(q.Get("all"))
	for _, l := range q["label"] { // key=value, repeatable: all must match
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return domain.Errorf(domain.CodeInvalidArgument, "label filter %q: want KEY=VALUE", l).WithDetail("field", "label")
		}
		if p.Labels == nil {
			p.Labels = map[string]string{}
		}
		p.Labels[k] = v
	}
	vms, next, err := s.svc.ListVMs(r.Context(), principal(r), states, all, p)
	if err != nil {
		return err
	}
	if g := memberOf(r); g != nil {
		vms = memberVMs(g, vms) // a page may come back short
	}
	return writeJSON(w, http.StatusOK, api.VMList{Items: vms, NextCursor: next})
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) error {
	req, err := mutation(r)
	if err != nil {
		return err
	}
	if req.CapacityWait, err = capacityWait(r); err != nil {
		return err
	}
	var body api.CreateVMRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	a, err := s.svc.Create(r.Context(), req, body)
	if err != nil {
		return err
	}
	return accepted(w, a)
}

func (s *Server) getVM(w http.ResponseWriter, r *http.Request) error {
	vm, err := s.svc.GetVM(r.Context(), principal(r), r.PathValue("vmId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, vm)
}

func (s *Server) updateVM(w http.ResponseWriter, r *http.Request) error {
	req, err := mutation(r)
	if err != nil {
		return err
	}
	var body api.UpdateVMRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	vm, err := s.svc.Update(r.Context(), req, r.PathValue("vmId"), body)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, vm)
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) error {
	req, err := mutation(r)
	if err != nil {
		return err
	}
	a, err := s.svc.Delete(r.Context(), req, r.PathValue("vmId"))
	if err != nil {
		return err
	}
	return accepted(w, a)
}

func (s *Server) stopVM(w http.ResponseWriter, r *http.Request) error {
	req, err := mutation(r)
	if err != nil {
		return err
	}
	var body api.StopVMRequest
	if err := decode(r, &body, true); err != nil {
		return err
	}
	a, err := s.svc.Stop(r.Context(), req, r.PathValue("vmId"), body.Force)
	if err != nil {
		return err
	}
	return accepted(w, a)
}

func (s *Server) resumeVM(w http.ResponseWriter, r *http.Request) error {
	req, err := mutation(r)
	if err != nil {
		return err
	}
	if req.CapacityWait, err = capacityWait(r); err != nil {
		return err
	}
	var body api.ResumeVMRequest
	if err := decode(r, &body, true); err != nil {
		return err
	}
	if err := s.mayUseEnvironment(r, body.Environment); err != nil {
		return err
	}
	a, err := s.svc.Resume(r.Context(), req, r.PathValue("vmId"), body)
	if err != nil {
		return err
	}
	return accepted(w, a)
}

func (s *Server) forkVM(w http.ResponseWriter, r *http.Request) error {
	req, err := mutation(r)
	if err != nil {
		return err
	}
	var body api.ForkVMRequest
	if err := decode(r, &body, true); err != nil {
		return err
	}
	if err := s.mayUseEnvironment(r, body.Environment); err != nil {
		return err
	}
	a, err := s.svc.Fork(r.Context(), req, r.PathValue("vmId"), body)
	if err != nil {
		return err
	}
	return accepted(w, a)
}

func (s *Server) listVMSnapshots(w http.ResponseWriter, r *http.Request) error {
	return s.snapshots(w, r, r.PathValue("vmId"))
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) error {
	return s.snapshots(w, r, "")
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request, vm string) error {
	p, err := page(r)
	if err != nil {
		return err
	}
	snaps, next, err := s.svc.ListSnapshots(r.Context(), principal(r), vm, p)
	if err != nil {
		return err
	}
	if g := memberOf(r); g != nil {
		snaps = memberSnapshots(g, snaps)
	}
	return writeJSON(w, http.StatusOK, api.SnapshotList{Items: snaps, NextCursor: next})
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request) error {
	snap, err := s.svc.GetSnapshot(r.Context(), principal(r), r.PathValue("snapshotId"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, snap)
}

func (s *Server) snapshotVM(w http.ResponseWriter, r *http.Request) error {
	req, err := mutation(r)
	if err != nil {
		return err
	}
	var body api.SnapshotVMRequest
	if err := decode(r, &body, false); err != nil {
		return err
	}
	a, err := s.svc.Snapshot(r.Context(), req, r.PathValue("vmId"), body)
	if err != nil {
		return err
	}
	return accepted(w, a)
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.svc.DeleteSnapshot(r.Context(), principal(r), r.PathValue("snapshotId")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) error {
	op, err := s.svc.GetOperation(r.Context(), principal(r), domain.OperationID(r.PathValue("operationId")))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, op)
}

// MaxEventWait bounds the events long-poll.
const MaxEventWait = 30 * time.Second

func (s *Server) listOperationEvents(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	var after int64
	var waitMs int
	var err error
	if v := q.Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			return domain.Errorf(domain.CodeInvalidArgument, "after must be a non-negative integer").WithDetail("field", "after")
		}
	}
	if v := q.Get("waitMs"); v != "" {
		if waitMs, err = strconv.Atoi(v); err != nil || waitMs < 0 || time.Duration(waitMs)*time.Millisecond > MaxEventWait {
			return domain.Errorf(domain.CodeInvalidArgument, "waitMs must be 0..%d", MaxEventWait.Milliseconds()).WithDetail("field", "waitMs")
		}
	}
	op, evs, err := s.svc.Events(r.Context(), principal(r), domain.OperationID(r.PathValue("operationId")), after, time.Duration(waitMs)*time.Millisecond)
	if err != nil {
		return err
	}
	out := api.OperationEventList{Operation: *op, Items: evs, NextCursor: strconv.FormatInt(after, 10)}
	if n := len(evs); n > 0 {
		out.NextCursor = strconv.FormatInt(evs[n-1].Seq, 10)
	}
	return writeJSON(w, http.StatusOK, out)
}
