package lifecycle

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestproto"
	"davit/internal/state"
)

// Workspace preparation (G3). A VM pinned to an environment revision is
// prepared (clone, secret files, setup) after it becomes ready:
//
//	pending -> preparing -> ready | failed
//
// VM readiness and workspace readiness stay distinct: the VM is usable while
// its workspace prepares, and a failed workspace leaves it usable. A VM that
// stops while preparing goes back to pending and prepares again on its next
// boot; a daemon restart re-drives preparing workspaces. A fork inherits the
// source's pin and state (its disk holds the prepared workspace) unless the
// fork names another revision; resume with an environment re-pins.

// WorkspacePreparer runs guestproto WorkspacePrepare in a VM's guest
// (feature "workspace"), reporting progress, and returns the result; an
// error means the outcome is unknown. guestexec.Manager implements it.
type WorkspacePreparer interface {
	PrepareWorkspace(ctx context.Context, principal domain.PrincipalID, ref string, req guestproto.WorkspacePrepare,
		progress func(guestproto.WorkspaceProgress)) (guestproto.WorkspaceResult, error)
}

// PreparesWorkspaces reports whether environment pins are served (a
// WorkspacePreparer is configured).
func (s *Service) PreparesWorkspaces() bool { return s.cfg.Workspace != nil }

// workspaceServed refuses an environment pin when no preparer is
// configured, before anything else is looked up.
func (s *Service) workspaceServed(ref *api.EnvironmentRef, op string) error {
	if ref != nil && s.cfg.Workspace == nil {
		return domain.NotImplemented(domain.FeatureWorkspacePrep, op)
	}
	return nil
}

// pinEnvironment resolves ref and pins rec to that revision: the VM gets
// the pin, a pending workspace and a private copy of the manifest.
func (s *Service) pinEnvironment(tx *state.Tx, rec *state.VMRecord, ref *api.EnvironmentRef, op string) error {
	if s.cfg.Workspace == nil {
		return domain.NotImplemented(domain.FeatureWorkspacePrep, op)
	}
	if ref.Revision < 0 {
		return domain.Errorf(domain.CodeInvalidArgument, "environment revision must be >= 0").WithDetail("field", "environment.revision")
	}
	env, err := tx.GetEnvironment(rec.VM.Owner, ref.Environment)
	if err != nil {
		return err
	}
	n := ref.Revision
	if n == 0 {
		n = env.LatestRevision
	}
	rev, err := tx.GetEnvironmentRevision(env.ID, n)
	if err != nil {
		return err
	}
	m := rev.Manifest
	rec.Manifest = &m
	rec.VM.Environment = &domain.EnvironmentPin{EnvironmentID: env.ID, Name: env.Name, Revision: n}
	rec.VM.Workspace = &domain.Workspace{State: domain.WorkspacePending}
	return nil
}

// forkWorkspace pins a fork child: to ref when given, else to its
// source's pin and workspace state.
func (s *Service) forkWorkspace(tx *state.Tx, child, src *state.VMRecord, ref *api.EnvironmentRef) error {
	if ref != nil {
		return s.pinEnvironment(tx, child, ref, "forkVM.environment")
	}
	inheritWorkspace(child, src)
	return nil
}

// inheritWorkspace gives a fork child its source's pin and workspace state.
func inheritWorkspace(child, src *state.VMRecord) {
	if src.VM.Environment == nil {
		return
	}
	pin, m := *src.VM.Environment, src.Manifest
	child.VM.Environment, child.Manifest = &pin, m
	st := domain.WorkspacePending
	if src.VM.Workspace != nil && src.VM.Workspace.State == domain.WorkspaceReady {
		st = domain.WorkspaceReady // the copied disk holds it
	}
	ws := domain.Workspace{State: st}
	if st == domain.WorkspaceReady {
		ws.StartedAt, ws.ReadyAt = src.VM.Workspace.StartedAt, src.VM.Workspace.ReadyAt
	}
	child.VM.Workspace = &ws
}

// kickWorkspaces starts preparation for every ready VM whose pinned
// workspace is not prepared (after an operation succeeds, and at start).
func (s *Service) kickWorkspaces(ctx context.Context, ids ...domain.VMID) {
	if s.cfg.Workspace == nil {
		return
	}
	if ids == nil {
		_ = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
			recs, err := tx.AllVMs()
			for _, r := range recs {
				ids = append(ids, r.VM.ID)
			}
			return err
		})
	}
	for _, id := range ids {
		s.mu.Lock()
		busy := s.preparing[id]
		if !busy {
			s.preparing[id] = true
		}
		s.mu.Unlock()
		if busy {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { s.mu.Lock(); delete(s.preparing, id); s.mu.Unlock() }()
			s.prepareWorkspace(s.ctx, id)
		}()
	}
}

// prepareWorkspace runs one preparation of vm if it is ready and pinned to
// an unprepared workspace.
func (s *Service) prepareWorkspace(ctx context.Context, id domain.VMID) {
	var owner domain.PrincipalID
	var req guestproto.WorkspacePrepare
	var claimed bool
	var setupErr *domain.SetupError
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec, err := tx.GetVM(id)
		if err != nil {
			return err
		}
		vm := &rec.VM
		if vm.Workspace == nil || rec.Manifest == nil || vm.Workspace.State == domain.WorkspaceReady ||
			vm.Workspace.State == domain.WorkspaceFailed || vm.State != domain.StateReady && vm.State != domain.StateRunning {
			return nil
		}
		owner = vm.Owner
		req, setupErr = s.workspaceRequest(ctx, *vm.Environment, *rec.Manifest)
		now := s.now()
		vm.Workspace = &domain.Workspace{State: domain.WorkspacePreparing, StartedAt: &now}
		vm.StateRevision++
		vm.UpdatedAt = now
		claimed = true
		return tx.PutVM(rec)
	})
	if err != nil || !claimed {
		if err != nil {
			s.cfg.Log.Warn("workspace: claim", "vm", id, "err", err)
		}
		return
	}
	s.notify()
	if setupErr != nil {
		s.settleWorkspace(ctx, id, &guestproto.WorkspaceResult{Revision: req.Revision, State: guestproto.WorkspaceFailed}, setupErr)
		return
	}
	res, err := s.cfg.Workspace.PrepareWorkspace(ctx, owner, string(id), req, func(p guestproto.WorkspaceProgress) {
		s.cfg.Log.Debug("workspace: progress", "vm", id, "step", p.Step, "phase", p.Phase, "state", p.State)
	})
	if err != nil {
		if ctx.Err() != nil {
			return // daemon shutting down: stays preparing; Start re-drives it
		}
		s.settleWorkspace(ctx, id, nil, &domain.SetupError{Message: domain.AsError(err).Message})
		return
	}
	var fail *domain.SetupError
	if res.State != guestproto.WorkspaceReady {
		fail = &domain.SetupError{Step: res.FailedStep, Phase: s.failedPhase(req, res.FailedStep), ExitCode: res.ExitCode, Message: res.Message}
	}
	s.settleWorkspace(ctx, id, &res, fail)
}

// settleWorkspace records the outcome. Without a result (the guest went
// away), a VM that is no longer up goes back to pending, to prepare on its
// next boot; one still up has failed.
func (s *Service) settleWorkspace(ctx context.Context, id domain.VMID, res *guestproto.WorkspaceResult, fail *domain.SetupError) {
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		rec, err := tx.GetVM(id)
		if err != nil {
			return err
		}
		vm := &rec.VM
		if vm.Workspace == nil || vm.Workspace.State != domain.WorkspacePreparing {
			return nil // re-pinned or reset meanwhile
		}
		now := s.now()
		ws := &domain.Workspace{StartedAt: vm.Workspace.StartedAt}
		switch {
		case fail == nil:
			ws.State, ws.ReadyAt = domain.WorkspaceReady, &now
		case res == nil && vm.State != domain.StateReady && vm.State != domain.StateRunning:
			ws = &domain.Workspace{State: domain.WorkspacePending}
		default:
			ws.State, ws.Error = domain.WorkspaceFailed, fail
		}
		vm.Workspace = ws
		vm.StateRevision++
		vm.UpdatedAt = now
		return tx.PutVM(rec)
	})
	if err != nil {
		s.cfg.Log.Warn("workspace: settle", "vm", id, "err", err)
		return
	}
	s.notify()
}

// workspaceRequest builds the guest request from a pinned manifest,
// resolving secret files on the host. A missing secret fails preparation
// before anything reaches the guest.
func (s *Service) workspaceRequest(ctx context.Context, pin domain.EnvironmentPin, m domain.EnvManifest) (guestproto.WorkspacePrepare, *domain.SetupError) {
	req := guestproto.WorkspacePrepare{Revision: fmt.Sprintf("%s@%d", pin.EnvironmentID, pin.Revision)}
	for _, r := range m.Repos {
		req.Repos = append(req.Repos, guestproto.RepoCheckout{URL: r.URL, Ref: r.Ref, Path: r.Path})
	}
	for _, f := range m.SecretFiles {
		var data []byte
		var err error
		if s.cfg.WorkspaceSecret != nil {
			data, err = s.cfg.WorkspaceSecret(ctx, f.Secret)
		} else {
			err = domain.Errorf(domain.CodeNotFound, "no host secret store configured")
		}
		if err != nil {
			return req, &domain.SetupError{Step: f.Path, Phase: string(guestproto.WorkspacePhaseFile),
				Message: fmt.Sprintf("secret %q: %s", f.Secret, domain.AsError(err).Message)}
		}
		mode := uint32(0o600)
		if f.Mode != "" {
			v, _ := strconv.ParseUint(f.Mode, 8, 32) // validated octal
			mode = uint32(v)
		}
		req.Files = append(req.Files, guestproto.WorkspaceFile{Path: f.Path, Mode: mode, Data: data})
	}
	for _, k := range slices.Sorted(maps.Keys(m.Variables)) {
		req.Env = append(req.Env, k+"="+m.Variables[k])
	}
	for _, a := range m.Setup {
		req.Setup = append(req.Setup, guestproto.SetupStep{Name: a.Name, Argv: a.Argv, Cwd: a.Cwd, User: a.User, TimeoutSeconds: int(a.TimeoutSeconds)})
	}
	return req, nil
}

// failedPhase names the phase of a failed step: a setup action name, else a
// repo or file path.
func (s *Service) failedPhase(req guestproto.WorkspacePrepare, step string) string {
	for _, a := range req.Setup {
		if a.Name == step {
			return string(guestproto.WorkspacePhaseSetup)
		}
	}
	for _, r := range req.Repos {
		if r.Path == step {
			return string(guestproto.WorkspacePhaseClone)
		}
	}
	for _, f := range req.Files {
		if f.Path == step {
			return string(guestproto.WorkspacePhaseFile)
		}
	}
	return ""
}
