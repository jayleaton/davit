package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime"
	"davit/internal/state"
	"davit/internal/storage"
)

// Request carries the transport facts every mutation needs.
type Request struct {
	Principal      domain.PrincipalID
	IdempotencyKey string
	// Method and Path are the canonical HTTP method and path; together with
	// principal and key they scope the idempotency record.
	Method, Path string
	// VMRef is the VM ID or name the path addresses, if any. Idempotency
	// keys are scoped by the canonical VM ID, so a key replays the same
	// operation whether the VM was addressed by name or by ID.
	VMRef string
	// IfMatch, when set, must equal the VM's stateRevision.
	IfMatch *int64
	// CapacityWait is how long a create or resume may queue for host
	// capacity (waitSeconds) when idle VMs could not make room (reclaim.go).
	CapacityWait time.Duration
	// CapacitySnapshot is the snapshot a queued create boots from, which the
	// low-disk cleanup must not delete while it waits.
	CapacitySnapshot string
}

// Accepted is the result of a lifecycle mutation.
type Accepted struct {
	Operation domain.Operation
	VM        *domain.VM
	// Replayed is true when an idempotency key matched an earlier request.
	Replayed bool
}

const maxIdempotencyKey = 255

func hashOf(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// plan runs inside the mutation transaction. It validates the request and
// writes the VM record; it returns the new operation (not yet stored).
type plan func(tx *state.Tx, now time.Time) (*domain.Operation, error)

func (s *Service) mutate(ctx context.Context, req Request, body any, keyRequired bool, p plan) (*Accepted, error) {
	if req.Principal == "" {
		return nil, domain.Errorf(domain.CodeUnauthenticated, "no principal")
	}
	if len(req.IdempotencyKey) > maxIdempotencyKey {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "Idempotency-Key longer than %d", maxIdempotencyKey)
	}
	if keyRequired && req.IdempotencyKey == "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "Idempotency-Key is required for this request")
	}
	if s.ctx.Err() != nil {
		return nil, domain.Errorf(domain.CodeUnavailable, "daemon is shutting down")
	}
	// json.Marshal of a typed struct is canonical: fixed field order, sorted
	// map keys. Every semantic field is included.
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	bodyHash := hashOf(string(raw))

	var out Accepted
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		now := s.now()
		scopes := idempotencyScopes(tx, req)
		if req.IdempotencyKey != "" {
			var prev *state.Idempotency
			for _, scope := range scopes {
				p, err := tx.GetIdempotency(scope)
				if err != nil {
					return err
				}
				if p != nil {
					prev = p
					break
				}
			}
			if prev != nil {
				if prev.BodyHash != bodyHash {
					return domain.Errorf(domain.CodeIdempotencyConflict, "Idempotency-Key was used with a different request body").
						WithDetail("operationId", prev.OperationID)
				}
				op, err := tx.GetOperation(prev.OperationID)
				if err != nil {
					return err
				}
				// Reauthorize every replay.
				if op.Owner != req.Principal {
					return domain.Errorf(domain.CodeNotFound, "operation not found")
				}
				out.Operation, out.Replayed = *op, true
				if vm, err := tx.GetVM(op.VMID); err == nil {
					out.VM = &vm.VM
					s.decorate(out.VM)
				}
				return nil
			}
		}
		op, err := p(tx, now)
		if err != nil {
			return err
		}
		op.Owner, op.CreatedAt, op.UpdatedAt = req.Principal, now, now
		if op.State == "" {
			op.State = domain.OpQueued
		}
		if op.State.Terminal() {
			op.CompletedAt = &now
		}
		if err := tx.PutOperation(op); err != nil {
			return err
		}
		if err := tx.AppendEvent(&domain.OperationEvent{OperationID: op.ID, Time: now, State: op.State, Phase: op.Phase, Progress: op.Progress, Message: "accepted"}); err != nil {
			return err
		}
		if req.IdempotencyKey != "" {
			for _, scope := range scopes {
				if err := tx.PutIdempotency(&state.Idempotency{Scope: scope, BodyHash: bodyHash, OperationID: op.ID, CreatedAt: now}); err != nil {
					return err
				}
			}
			if op.State.Terminal() {
				if err := tx.ExpireIdempotency(op.ID, now.Add(s.cfg.IdempotencyRetention)); err != nil {
					return err
				}
			}
		}
		vm, err := tx.GetVM(op.VMID)
		if err != nil {
			return err
		}
		out.Operation, out.VM = *op, &vm.VM
		s.decorate(out.VM)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.notify()
	if !out.Replayed && !out.Operation.State.Terminal() {
		s.dispatch(out.Operation.ID)
	}
	return &out, nil
}

// idempotencyScopes returns the key scopes of req: by canonical VM ID first
// (so name and ID address the same operation), then by the raw path (so a
// replay by name still finds the operation after the VM is deleted and the
// name no longer resolves).
func idempotencyScopes(tx *state.Tx, req Request) []string {
	raw := hashOf(string(req.Principal), req.Method, req.Path, req.IdempotencyKey)
	if req.VMRef == "" || req.IdempotencyKey == "" {
		return []string{raw}
	}
	rec, err := lookupVM(tx, req.Principal, req.VMRef)
	if err != nil {
		return []string{raw}
	}
	segs := strings.Split(req.Path, "/")
	for i := 0; i+1 < len(segs); i++ {
		if segs[i] == "vms" && segs[i+1] == req.VMRef {
			segs[i+1] = string(rec.VM.ID)
			break
		}
	}
	canonical := hashOf(string(req.Principal), req.Method, strings.Join(segs, "/"), req.IdempotencyKey)
	if canonical == raw {
		return []string{raw}
	}
	return []string{canonical, raw}
}

// lookupVM resolves an ID or name visible to principal. Other owners' VMs
// are indistinguishable from absent ones.
func lookupVM(tx *state.Tx, principal domain.PrincipalID, ref string) (*state.VMRecord, error) {
	if ref == "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "VM ID or name is required")
	}
	var rec *state.VMRecord
	var err error
	if strings.HasPrefix(ref, "vm_") {
		rec, err = tx.GetVM(domain.VMID(ref))
	} else {
		rec, err = tx.FindVMByName(principal, ref)
	}
	if err != nil {
		return nil, err
	}
	if rec.VM.Owner != principal {
		return nil, domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
	}
	return rec, nil
}

func checkMutable(rec *state.VMRecord, req Request) error {
	if rec.VM.State == domain.StateDeleted {
		return domain.Errorf(domain.CodeNotFound, "vm %s not found", rec.VM.ID)
	}
	if req.IfMatch != nil && *req.IfMatch != rec.VM.StateRevision {
		return domain.Errorf(domain.CodeRevisionMismatch, "stateRevision is %d, not %d", rec.VM.StateRevision, *req.IfMatch).
			WithDetail("stateRevision", rec.VM.StateRevision)
	}
	if rec.VM.ActiveOperationID != "" {
		e := domain.Errorf(domain.CodeOperationInProgress, "vm %s has operation %s in progress", rec.VM.ID, rec.VM.ActiveOperationID)
		e.OperationID = rec.VM.ActiveOperationID
		e.Retryable = true
		return e.WithDetail("operationId", rec.VM.ActiveOperationID)
	}
	return nil
}

// profileShape returns the shape of the named administrator profile, or
// base when name is empty.
func (s *Service) profileShape(name string, base domain.Shape) (domain.Shape, error) {
	if name == "" {
		return base, nil
	}
	for _, p := range s.cfg.Profiles {
		if p.Name == name {
			return p.Shape.Merge(base), nil
		}
	}
	names := make([]string, 0, len(s.cfg.Profiles))
	for _, p := range s.cfg.Profiles {
		names = append(names, p.Name)
	}
	return base, domain.Errorf(domain.CodeInvalidArgument, "unknown profile %q (configured: %v)", name, names).WithDetail("field", "type")
}

func (s *Service) resolveShape(o *api.ShapeOverride, base domain.Shape, minDisk int64) (domain.Shape, error) {
	sh := base
	if o != nil {
		sh = domain.Shape{CPUs: o.CPUs, MemoryBytes: o.MemoryBytes, DiskBytes: o.DiskBytes}.Merge(base)
		if o.CPUs < 0 || o.MemoryBytes < 0 || o.DiskBytes < 0 {
			return sh, domain.Errorf(domain.CodeInvalidArgument, "shape values must be positive")
		}
	}
	if sh.DiskBytes < minDisk {
		return sh, domain.Errorf(domain.CodeInvalidArgument, "disk cannot shrink below %d bytes", minDisk).WithDetail("field", "shape.diskBytes")
	}
	return sh, sh.Validate(s.caps.Shape)
}

func (s *Service) ttl(seconds *int64, autoStop *bool, cur int64, curAuto bool) (int64, bool, error) {
	if seconds != nil {
		if *seconds < 0 {
			return 0, false, domain.Errorf(domain.CodeInvalidArgument, "ttlSeconds must be >= 0")
		}
		cur = *seconds
	}
	if autoStop != nil {
		curAuto = *autoStop
	}
	return cur, curAuto && cur > 0, nil
}

func envList(env map[string]string) ([]string, error) {
	out := make([]string, 0, len(env))
	for k, v := range env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return nil, domain.Errorf(domain.CodeInvalidArgument, "invalid environment variable %q", k).WithDetail("field", "env")
		}
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out, nil
}

// admit reserves host capacity for want: CPU/RAM/disk limits plus the
// growth headroom of a VM of shape boot above the data dir's free-space
// floor (disk.go). Stop and delete never call it, so space can always be
// reclaimed.
func (s *Service) admit(tx *state.Tx, want, boot domain.Shape) error {
	return s.admitRestoring(tx, want, boot, 0)
}

// admitRestoring is admit for a VM whose boot first copies restore bytes
// back from cold storage (coldtier.go): they are disk it needs too.
func (s *Service) admitRestoring(tx *state.Tx, want, boot domain.Shape, restore int64) error {
	u, err := liveUsage(tx, s.cfg.Limits)
	if err != nil {
		return err
	}
	used, err := tx.Allocated()
	if err != nil {
		return err
	}
	err = s.admitHost(u, want)
	if err == nil {
		err = s.cfg.Limits.Admit(used, want)
	}
	if domain.IsRoomError(err) {
		// What the VM needs, for the capacity queue (reclaim.go).
		return domain.AsError(err).WithDetail("vmMemoryBytes", want.MemoryBytes)
	}
	if err != nil {
		return err
	}
	// Last, so that a request the limits refuse anyway never queues (and
	// runs the low-disk cleanup) for disk space.
	if d, ok, err := s.diskUsage(); err != nil {
		return err
	} else if ok {
		err := s.cfg.Limits.AdmitDisk(d, u.pendingDisk, s.cfg.Limits.DiskHeadroom(boot)+restore)
		if restore > 0 && domain.IsDiskSpaceError(err) {
			e := domain.AsError(err)
			e.Message = fmt.Sprintf("restoring from cold storage needs %s: %s", domain.HumanBytes(restore), e.Message)
			return e.WithDetail("restoreBytes", restore)
		}
		return err
	}
	return nil
}

// rearmReserve restores the journal's space reserve after a disk-full
// event, once free space is back above the floor.
func (s *Service) rearmReserve() {
	if s.cfg.DB.ReserveArmed() {
		return
	}
	if d, ok, err := s.diskUsage(); err != nil || ok && d.FreeBytes < s.cfg.Limits.DiskFloor(d.TotalBytes) {
		return
	}
	if err := s.cfg.DB.Rearm(); err != nil {
		s.cfg.Log.Warn("journal space reserve still unavailable", "err", err)
		return
	}
	s.cfg.Log.Info("journal space reserve re-armed")
}

func stopCommitID(op domain.OperationID) storage.CommitID { return storage.CommitID("c-" + string(op)) }

func diskID(vm domain.VMID, gen int64) string { return fmt.Sprintf("%s-g%d", vm, gen) }

// newVM builds a provisioning VM record for create and fork.
func (s *Service) newVM(tx *state.Tx, now time.Time, owner domain.PrincipalID, name string, shape domain.Shape,
	image domain.ImageID, src domain.SnapshotID, env []string, ttl int64, auto bool, egress domain.Egress, op *domain.Operation) (*state.VMRecord, error) {
	id := domain.NewVMID()
	if name == "" {
		name = strings.ReplaceAll(string(id), "_", "-")
	}
	if !domain.ValidName(name) {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "invalid name %q: use 1-63 lowercase letters, digits and '-'", name).WithDetail("field", "name")
	}
	if err := s.admit(tx, shape, shape); err != nil {
		return nil, err
	}
	op.VMID = id
	op.Params.Generation = 1
	rec := &state.VMRecord{
		VM: domain.VM{
			ID: id, Owner: owner, Name: name, State: domain.StateProvisioning, StateRevision: 1,
			InstanceGeneration: 1, Shape: shape, ImageID: image, TTLSeconds: ttl, AutoStop: auto,
			ActiveOperationID: op.ID, SourceSnapshotID: src,
			Readiness: domain.Readiness{Services: map[string]bool{}},
			Endpoint:  domain.Endpoint{Mode: "gateway", State: "none"},
			Storage:   domain.StorageInfo{Mode: string(s.probe.Mode), FastClone: s.probe.FastClone},
			Egress:    egress,
			CreatedAt: now, UpdatedAt: now,
		},
		Env:    env,
		DiskID: diskID(id, 1),
	}
	return rec, tx.PutVM(rec)
}

// Create accepts a new VM.
func (s *Service) Create(ctx context.Context, req Request, body api.CreateVMRequest) (*Accepted, error) {
	if err := s.workspaceServed(body.Environment, "createVM.environment"); err != nil {
		return nil, err
	}
	if err := validWait(req.CapacityWait); err != nil {
		return nil, err
	}
	req.CapacitySnapshot = string(body.SnapshotID)
	return s.withRoom(ctx, req, req.CapacityWait, func() (*Accepted, error) { return s.create(ctx, req, body) })
}

// validWait checks a createVM/resumeVM waitSeconds.
func validWait(d time.Duration) error {
	if d < 0 || d > domain.MaxCapacityWaitSeconds*time.Second {
		return domain.Errorf(domain.CodeInvalidArgument, "waitSeconds must be 0..%d", domain.MaxCapacityWaitSeconds).WithDetail("field", "waitSeconds")
	}
	return nil
}

func (s *Service) create(ctx context.Context, req Request, body api.CreateVMRequest) (*Accepted, error) {
	return s.mutate(ctx, req, body, true, func(tx *state.Tx, now time.Time) (*domain.Operation, error) {
		p, err := s.planCreate(ctx, tx, req.Principal, body)
		if err != nil {
			return nil, err
		}
		rec, err := s.newVM(tx, now, req.Principal, body.Name, p.shape, p.image, p.op.Params.SourceSnapshotID, p.env, p.ttl, p.auto, p.egress, p.op)
		if err != nil {
			return p.op, err
		}
		rec.VM.Labels, rec.VM.IdleTimeoutSeconds, rec.VM.Keep = body.Labels, body.IdleTimeoutSeconds, body.Keep
		if body.Environment == nil && body.Privacy == nil {
			return p.op, tx.PutVM(rec)
		}
		if body.Privacy != nil {
			rec.VM.Privacy = *body.Privacy
		}
		if body.Environment != nil {
			if err := s.pinEnvironment(tx, rec, body.Environment, "createVM.environment"); err != nil {
				return nil, err
			}
		}
		return p.op, tx.PutVM(rec)
	})
}

// createPlan is a validated createVM body, not yet admitted.
type createPlan struct {
	op     *domain.Operation
	shape  domain.Shape
	image  domain.ImageID
	env    []string
	ttl    int64
	auto   bool
	egress domain.Egress
}

// planCreate validates a createVM body (everything but the name and
// admission) and resolves its image or snapshot and shape.
func (s *Service) planCreate(ctx context.Context, tx *state.Tx, principal domain.PrincipalID, body api.CreateVMRequest) (*createPlan, error) {
	if body.Image != "" && body.SnapshotID != "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "image and snapshotId are mutually exclusive")
	}
	if body.RequireGolden && !s.caps.GoldenRestore {
		return nil, domain.Errorf(domain.CodeUnsupportedCapability, "%s runtime has no golden restore; refusing to fall back to cold boot", s.caps.Engine).
			WithDetail("capability", "goldenRestore")
	}
	p := &createPlan{op: &domain.Operation{ID: domain.NewOperationID(), Kind: domain.OpCreate, Phase: "queued"}}
	p.op.Params.RequireGolden = body.RequireGolden
	minDisk := int64(0)
	base, err := s.profileShape(body.Type, domain.DefaultShape)
	if err != nil {
		return nil, err
	}
	if body.SnapshotID != "" {
		snap, err := lookupSnapshot(tx, principal, string(body.SnapshotID))
		if err != nil {
			return nil, err
		}
		p.image, minDisk = snap.ImageID, snap.SizeBytes
		p.op.Params.SourceSnapshotID = snap.ID
	} else {
		img, err := s.cfg.Images.Resolve(ctx, body.Image)
		if err != nil {
			return nil, err
		}
		p.image, minDisk = img.ID, img.RootfsSize
	}
	if base.DiskBytes < minDisk {
		base.DiskBytes = minDisk
	}
	if p.shape, err = s.resolveShape(body.Shape, base, minDisk); err != nil {
		return nil, err
	}
	if p.env, err = envList(body.Env); err != nil {
		return nil, err
	}
	if p.ttl, p.auto, err = s.ttl(body.TTLSeconds, body.AutoStop, int64(s.cfg.DefaultTTL/time.Second), true); err != nil {
		return nil, err
	}
	if p.egress, err = egressOr(body.Egress, s.cfg.DefaultEgress); err != nil {
		return nil, err
	}
	if err := domain.ValidateLabels(body.Labels); err != nil {
		return nil, err
	}
	if err := validIdle(body.IdleTimeoutSeconds); err != nil {
		return nil, err
	}
	return p, nil
}

func validIdle(secs int64) error {
	if secs < 0 || time.Duration(secs)*time.Second > domain.MaxIdleTimeout {
		return domain.Errorf(domain.CodeInvalidArgument, "idleTimeoutSeconds must be 0..%d", int64(domain.MaxIdleTimeout/time.Second)).
			WithDetail("field", "idleTimeoutSeconds")
	}
	return nil
}

// Fork accepts an independent VM from a committed disk snapshot of ref.
func (s *Service) Fork(ctx context.Context, req Request, ref string, body api.ForkVMRequest) (*Accepted, error) {
	if err := s.workspaceServed(body.Environment, "forkVM.environment"); err != nil {
		return nil, err
	}
	req.VMRef = ref
	return s.withRoom(ctx, req, 0, func() (*Accepted, error) { return s.fork(ctx, req, ref, body) })
}

func (s *Service) fork(ctx context.Context, req Request, ref string, body api.ForkVMRequest) (*Accepted, error) {
	return s.mutate(ctx, req, body, true, func(tx *state.Tx, now time.Time) (*domain.Operation, error) {
		src, err := lookupVM(tx, req.Principal, ref)
		if err != nil {
			return nil, err
		}
		if src.VM.State == domain.StateDeleted {
			return nil, domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
		}
		if req.IfMatch != nil && *req.IfMatch != src.VM.StateRevision {
			return nil, domain.Errorf(domain.CodeRevisionMismatch, "stateRevision is %d, not %d", src.VM.StateRevision, *req.IfMatch).
				WithDetail("stateRevision", src.VM.StateRevision)
		}
		if err := keepsDisk(src.VM, "fork"); err != nil {
			return nil, err
		}
		if body.Live {
			return s.planLiveFork(tx, now, req, src, body)
		}
		snapRef := string(body.SnapshotID)
		if snapRef == "" {
			snapRef = string(src.VM.LastSnapshotID)
		}
		if snapRef == "" {
			return nil, domain.Errorf(domain.CodeStopRequired, "vm %s has no committed snapshot; stop it first", src.VM.ID)
		}
		snap, err := lookupSnapshot(tx, req.Principal, snapRef)
		if err != nil || snap.SourceVMID != src.VM.ID {
			return nil, domain.Errorf(domain.CodeNotFound, "snapshot %s of vm %s not found", snapRef, src.VM.ID)
		}
		base, err := s.profileShape(body.Type, src.VM.Shape)
		if err != nil {
			return nil, err
		}
		base.DiskBytes = max(base.DiskBytes, src.VM.Shape.DiskBytes)
		shape, err := s.resolveShape(body.Shape, base, max(src.VM.Shape.DiskBytes, snap.SizeBytes))
		if err != nil {
			return nil, err
		}
		ttl, auto, err := s.ttl(body.TTLSeconds, body.AutoStop, int64(s.cfg.DefaultTTL/time.Second), true)
		if err != nil {
			return nil, err
		}
		op := &domain.Operation{ID: domain.NewOperationID(), Kind: domain.OpFork, Phase: "queued",
			Params: domain.OpParams{SourceVMID: src.VM.ID, SourceSnapshotID: snap.ID}}
		// A fork never widens its source's egress unless asked to.
		egress, err := egressOr(body.Egress, src.VM.Egress)
		if err != nil {
			return nil, err
		}
		// Host-managed secrets (env) are not inherited by forks.
		rec, err := s.newVM(tx, now, req.Principal, body.Name, shape, src.VM.ImageID, snap.ID, nil, ttl, auto, egress, op)
		if err != nil {
			return op, err
		}
		rec.VM.Privacy = src.VM.Privacy // forks inherit the privacy policy
		if err := s.forkWorkspace(tx, rec, src, body.Environment); err != nil {
			return nil, err
		}
		return op, tx.PutVM(rec)
	})
}

func noop(kind domain.OperationKind, vm domain.VMID, params domain.OpParams) *domain.Operation {
	params.NoOp = true
	return &domain.Operation{ID: domain.NewOperationID(), Kind: kind, VMID: vm, State: domain.OpSucceeded,
		Phase: "done", Progress: 1, Params: params, Result: &domain.OpResult{VMID: vm, NoOp: true}}
}

// Stop accepts a disk-snapshot stop. Stop of a stopped VM is a no-op.
func (s *Service) Stop(ctx context.Context, req Request, ref string, force bool) (*Accepted, error) {
	return s.stop(ctx, req, ref, force, "")
}

// stop is Stop with the Operation.Reason of a stop the host decided on.
func (s *Service) stop(ctx context.Context, req Request, ref string, force bool, reason string) (*Accepted, error) {
	req.VMRef = ref
	return s.mutate(ctx, req, api.StopVMRequest{Force: force}, false, func(tx *state.Tx, now time.Time) (*domain.Operation, error) {
		rec, err := lookupVM(tx, req.Principal, ref)
		if err != nil {
			return nil, err
		}
		if err := checkMutable(rec, req); err != nil {
			return nil, err
		}
		if rec.VM.State == domain.StateStopped {
			return noop(domain.OpStop, rec.VM.ID, domain.OpParams{}), nil
		}
		if rec.VM.State != domain.StateReady && rec.VM.State != domain.StateRunning {
			return nil, domain.Errorf(domain.CodeInvalidState, "cannot stop vm in state %s", rec.VM.State).WithDetail("state", rec.VM.State)
		}
		op := &domain.Operation{ID: domain.NewOperationID(), Kind: domain.OpStop, VMID: rec.VM.ID, Phase: "queued", Reason: reason,
			Params: domain.OpParams{Generation: rec.VM.InstanceGeneration, Force: force, SnapshotID: domain.NewSnapshotID(),
				// Journaled now, so a replay after a crash discards or deletes
				// exactly as decided, whatever the policy says later.
				Discard: !rec.VM.Privacy.KeepsDisk(), Delete: rec.VM.Privacy.Ephemeral}}
		if err := rec.VM.Transition(domain.StateStopping, now); err != nil {
			return nil, err
		}
		rec.VM.ActiveOperationID = op.ID
		return op, tx.PutVM(rec)
	})
}

// Resume accepts a cold boot of the last committed disk. Resume of a ready
// or running VM is a no-op. Type and Shape resize the VM (resize.go); the
// new shape is persisted when the resume is accepted.
func (s *Service) Resume(ctx context.Context, req Request, ref string, body api.ResumeVMRequest) (*Accepted, error) {
	if err := s.workspaceServed(body.Environment, "resumeVM.environment"); err != nil {
		return nil, err
	}
	if err := validWait(req.CapacityWait); err != nil {
		return nil, err
	}
	req.VMRef = ref
	s.cancelDemoteRef(ctx, req.Principal, ref) // coldtier.go
	// A ready or running VM needs no room (its resume is a no-op or a
	// refused resize): answer at once, never behind the capacity queue.
	if acc, err := s.resume(ctx, req, ref, body, true); !errors.Is(err, errNotUp) {
		return acc, err
	}
	return s.withRoom(ctx, req, req.CapacityWait, func() (*Accepted, error) { return s.resume(ctx, req, ref, body, false) })
}

// errNotUp is resume's answer, with upOnly, for a VM that is not ready or
// running.
var errNotUp = errors.New("vm is not up")

func (s *Service) resume(ctx context.Context, req Request, ref string, body api.ResumeVMRequest, upOnly bool) (*Accepted, error) {
	return s.mutate(ctx, req, body, false, func(tx *state.Tx, now time.Time) (*domain.Operation, error) {
		rec, err := lookupVM(tx, req.Principal, ref)
		if err != nil {
			return nil, err
		}
		if err := checkMutable(rec, req); err != nil {
			return nil, err
		}
		vm := &rec.VM
		shape, err := s.resizeShape(vm.Shape, body)
		if err != nil {
			return nil, err
		}
		if vm.State == domain.StateReady || vm.State == domain.StateRunning {
			if err := resizeRefusal(vm, shape); err != nil {
				return nil, err
			}
			return noop(domain.OpResume, vm.ID, domain.OpParams{}), nil
		}
		if upOnly {
			return nil, errNotUp
		}
		if vm.State != domain.StateStopped && vm.State != domain.StateError {
			return nil, domain.Errorf(domain.CodeInvalidState, "cannot resume vm in state %s", vm.State).WithDetail("state", vm.State)
		}
		if err := keepsDisk(*vm, "resume"); err != nil {
			return nil, err
		}
		if vm.TTLSeconds, vm.AutoStop, err = s.ttl(body.TTLSeconds, body.AutoStop, vm.TTLSeconds, vm.AutoStop); err != nil {
			return nil, err
		}
		// Disk is already reserved by the stopped VM; admit CPU and memory
		// of the shape it boots, plus any disk growth and, for a cold VM,
		// the copy back to hot storage.
		if err := s.admitRestoring(tx, resizeAdmission(vm.Shape, shape), shape, coldRestore(*vm)); err != nil {
			return nil, err
		}
		src := vm.LastSnapshotID
		if src == "" {
			src = vm.SourceSnapshotID
		}
		op := &domain.Operation{ID: domain.NewOperationID(), Kind: domain.OpResume, VMID: vm.ID, Phase: "queued",
			Params: domain.OpParams{Generation: vm.InstanceGeneration + 1, SourceSnapshotID: src}}
		if shape != vm.Shape {
			op.Params.Shape, vm.Shape = shape, shape // the boot below clones the disk at this size
		}
		if vm.State == domain.StateError && rec.RetainedDiskID == "" {
			// The working disk of a failed VM may hold uncommitted data:
			// keep it for recovery instead of collecting it.
			rec.RetainedDiskID = rec.DiskID
		}
		if err := vm.Transition(domain.StateProvisioning, now); err != nil {
			return nil, err
		}
		if body.Environment != nil {
			if err := s.pinEnvironment(tx, rec, body.Environment, "resumeVM.environment"); err != nil {
				return nil, err
			}
		} else if vm.Workspace != nil && vm.Workspace.State == domain.WorkspacePreparing {
			vm.Workspace = &domain.Workspace{State: domain.WorkspacePending} // interrupted by the stop
		}
		vm.InstanceGeneration++
		vm.ActiveOperationID, vm.LastError, vm.BootMode = op.ID, nil, domain.BootModeNone
		rec.DiskID, rec.Handle = diskID(vm.ID, vm.InstanceGeneration), nil
		return op, tx.PutVM(rec)
	})
}

// Delete accepts deletion. Access closes immediately; data is collected
// before the operation succeeds. Named snapshots survive.
func (s *Service) Delete(ctx context.Context, req Request, ref string) (*Accepted, error) {
	s.cancelDemoteRef(ctx, req.Principal, ref)
	return s.deleteVM(ctx, req, ref, "", "", nil)
}

// deleteVM is Delete; a delete the host decides on its own carries reason
// (Operation.reason) and why (VM.deleteReason), and check, if set, vetoes it
// inside the transaction (the VM changed since it was chosen).
func (s *Service) deleteVM(ctx context.Context, req Request, ref, reason, why string, check func(tx *state.Tx, rec *state.VMRecord) error) (*Accepted, error) {
	req.VMRef = ref
	return s.mutate(ctx, req, struct{}{}, false, func(tx *state.Tx, now time.Time) (*domain.Operation, error) {
		rec, err := lookupVM(tx, req.Principal, ref)
		if err != nil {
			return nil, err
		}
		if err := checkMutable(rec, req); err != nil {
			return nil, err
		}
		if check != nil {
			if err := check(tx, rec); err != nil {
				return nil, err
			}
		}
		rec.VM.DeleteReason = why
		op := &domain.Operation{ID: domain.NewOperationID(), Kind: domain.OpDelete, VMID: rec.VM.ID, Phase: "queued", Reason: reason,
			Params: domain.OpParams{Generation: rec.VM.InstanceGeneration}}
		if err := rec.VM.Transition(domain.StateDeleting, now); err != nil {
			return nil, err
		}
		rec.VM.ActiveOperationID = op.ID
		rec.VM.Readiness = domain.Readiness{Services: map[string]bool{}}
		return op, tx.PutVM(rec)
	})
}

func decodeHandle(raw json.RawMessage) (*runtime.Handle, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var h runtime.Handle
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// keepSet lists every disk and commit still referenced, for storage GC.
func keepSet(tx *state.Tx) (storage.KeepSet, error) {
	var k storage.KeepSet
	vms, err := tx.AllVMs()
	if err != nil {
		return k, err
	}
	for _, r := range vms {
		if r.VM.State == domain.StateDeleted || r.VM.State == domain.StateDeleting {
			continue
		}
		for _, c := range r.ColdCommits {
			k.Cold = append(k.Cold, storage.CommitID(c))
		}
		for _, d := range []string{r.DiskID, r.RetainedDiskID} {
			if d != "" {
				k.Disks = append(k.Disks, storage.DiskID(d))
			}
		}
	}
	commits, err := tx.AllCommitIDs()
	if err != nil {
		return k, err
	}
	for _, c := range commits {
		k.Commits = append(k.Commits, storage.CommitID(c))
	}
	// A stop publishes its commit before the snapshot row that references
	// it; keep in-flight commits so a concurrent collection cannot race it.
	ops, err := tx.UnfinishedOperations()
	for _, o := range ops {
		switch {
		case o.Kind == domain.OpStop:
			k.Commits = append(k.Commits, stopCommitID(o.ID))
		case o.Kind == domain.OpFork && o.Params.LiveCommitID != "":
			// A live fork's disk commit, from before it exists until the
			// operation ends.
			k.Commits = append(k.Commits, storage.CommitID(o.Params.LiveCommitID))
		}
	}
	return k, err
}

// Update renames a VM, changes its TTL (SPEC §3 `extend`) or replaces its
// egress policy. It is synchronous: TTLSeconds sets the VM's TTL and, while
// it is ready or running, restarts the auto-stop countdown from now; Egress
// takes effect on a running VM's network at once (new connections and DNS
// lookups) and on later boots of a stopped one. Naturally idempotent;
// If-Match guards concurrent edits. Name and TTL changes are allowed during
// operations (they touch no lifecycle state); an egress change is not.
func (s *Service) Update(ctx context.Context, req Request, ref string, body api.UpdateVMRequest) (*domain.VM, error) {
	if req.Principal == "" {
		return nil, domain.Errorf(domain.CodeUnauthenticated, "no principal")
	}
	if body.KeepHot != nil && *body.KeepHot {
		s.cancelDemoteRef(ctx, req.Principal, ref)
	}
	var eg *egressChange
	if body.Egress != nil {
		var err error
		if eg, err = s.applyEgress(ctx, req, ref, *body.Egress); err != nil {
			return nil, err
		}
	}
	var out domain.VM
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec, err := lookupVM(tx, req.Principal, ref)
		if err != nil {
			return err
		}
		vm := &rec.VM
		if vm.State == domain.StateDeleted || vm.State == domain.StateDeleting {
			return domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
		}
		if req.IfMatch != nil && *req.IfMatch != vm.StateRevision {
			return domain.Errorf(domain.CodeRevisionMismatch, "stateRevision is %d, not %d", vm.StateRevision, *req.IfMatch).
				WithDetail("stateRevision", vm.StateRevision)
		}
		now := s.now()
		changed := false
		if body.Name != nil && *body.Name != vm.Name {
			if !domain.ValidName(*body.Name) {
				return domain.Errorf(domain.CodeInvalidArgument, "invalid name %q: use 1-63 lowercase letters, digits and '-'", *body.Name).WithDetail("field", "name")
			}
			vm.Name, changed = *body.Name, true
		}
		if body.TTLSeconds != nil || body.AutoStop != nil {
			if vm.TTLSeconds, vm.AutoStop, err = s.ttl(body.TTLSeconds, body.AutoStop, vm.TTLSeconds, vm.AutoStop); err != nil {
				return err
			}
			vm.ExpiresAt = nil
			if vm.AutoStop && (vm.State == domain.StateReady || vm.State == domain.StateRunning) {
				exp := now.Add(time.Duration(vm.TTLSeconds) * time.Second)
				vm.ExpiresAt = &exp
			}
			changed = true
		}
		if eg != nil {
			c, err := eg.record(rec)
			if err != nil {
				return err
			}
			changed = changed || c
		}
		if body.IdleTimeoutSeconds != nil && *body.IdleTimeoutSeconds != vm.IdleTimeoutSeconds {
			if err := validIdle(*body.IdleTimeoutSeconds); err != nil {
				return err
			}
			vm.IdleTimeoutSeconds, changed = *body.IdleTimeoutSeconds, true
			s.touch(vm.ID, now) // the countdown starts now
		}
		if body.Keep != nil && *body.Keep != vm.Keep {
			vm.Keep, changed = *body.Keep, true
		}
		if body.KeepHot != nil && *body.KeepHot != vm.KeepHot {
			vm.KeepHot, changed = *body.KeepHot, true
		}
		if body.Privacy != nil && *body.Privacy != vm.Privacy {
			if err := setPrivacy(tx, rec, *body.Privacy); err != nil { // privacy.go
				return err
			}
			changed = true
		}
		if changed {
			vm.StateRevision++
			vm.UpdatedAt = now
			if err := tx.PutVM(rec); err != nil {
				return err
			}
		}
		out = *vm
		return nil
	})
	if err != nil {
		s.undoEgress(ctx, eg)
		return nil, err
	}
	if eg != nil {
		s.cfg.Log.Info("egress set", "vm", out.ID, "mode", out.Egress.Mode, "allow", len(out.Egress.Allow))
	}
	s.notify()
	s.decorate(&out)
	return &out, nil
}
