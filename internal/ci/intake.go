package ci

import (
	"context"
	"slices"
	"strings"

	"davit/api"
	"davit/internal/ci/github"
	"davit/internal/domain"
	"davit/internal/state"
)

// Intake: what a job's status (from a webhook or a poll) means here.
//
// GitHub gives a queued job to any idle runner whose labels it matches, not
// necessarily the one registered for it. So a record is one runner and the
// job it serves (JobID): at first the job it was made for, and once GitHub
// gives it another job (in_progress or completed with its runner_name),
// that one. The job it was made for then still needs a runner: the handoff
// gives it to the idle runner made for the job that was taken, when that
// runner can serve it, or else to a new record. A runner that has no job
// yet when its job disappears (cancelled, or taken with no handoff) is
// deregistered and its box deleted; a busy one runs to the end.

// observe handles one job status seen for repo through installation inst.
// known is the repository as a webhook described it (nil from a poll).
func (s *Service) observe(ctx context.Context, inst int64, repo string, known *github.Repository, j github.Job) error {
	switch j.Status {
	case "queued":
		return s.queued(ctx, inst, repo, known, j)
	case "in_progress":
		return s.inProgress(ctx, inst, repo, known, j)
	case "completed":
		return s.completed(ctx, j)
	}
	return nil
}

// queued records a new job that is this host's: queued for a runner, or
// refused by the policy.
func (s *Service) queued(ctx context.Context, inst int64, repo string, known *github.Repository, j github.Job) error {
	shape, ok := match(j.Labels, s.set, s.cfg.GitHub)
	if !ok {
		return nil
	}
	var seen string
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		seen, err = tx.CIJobByProvider(providerJob(j.ID))
		return err
	}); err != nil || seen != "" {
		return err
	}
	pol, err := s.admit(ctx, inst, repo, known, j.RunID)
	rf, refused := asRefusal(err)
	if err != nil && !refused {
		return err
	}
	now := s.now()
	rec := &record{CIJob: api.CIJob{ID: domain.NewID("ci"), Repo: repo, JobID: j.ID, RunID: j.RunID, Job: j.Name,
		Workflow: j.WorkflowName, Labels: j.Labels, Shape: shape.Label, QueuedAt: now},
		Phase: phaseQueued, Installation: pol.installation, Spec: shape.Shape(), Key: providerJob(j.ID)}
	if rec.Labels == nil {
		rec.Labels = []string{}
	}
	if refused {
		rec.Phase, rec.State, rec.Error = phaseDone, api.CIJobRefused, apiError(refusalError(rf))
		rec.CompletedAt, rec.EndedAt = &now, &now
	}
	fresh := false
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		id, err := tx.CIJobByProvider(rec.providerJob())
		if err != nil || id != "" {
			return err
		}
		fresh = true
		return s.put(tx, rec, "", true)
	})
	if err != nil || !fresh {
		return err
	}
	if refused {
		s.cfg.Log.Warn("ci: job refused", "repo", repo, "githubJob", j.ID, "job", j.Name, "reason", rf.msg)
		return nil
	}
	s.cfg.Log.Info("ci: job queued", "job", rec.ID, "repo", repo, "githubJob", j.ID, "name", j.Name, "shape", shape.Label)
	s.spawn(rec)
	return nil
}

func providerJob(id int64) string { return "github:" + itoa(id) }

// inProgress notes which runner took a job: ours gets busy (and is killed
// if the job is one the policy refuses, e.g. a fork's that another job's
// runner was given).
func (s *Service) inProgress(ctx context.Context, inst int64, repo string, known *github.Repository, j github.Job) error {
	if j.RunnerName == "" {
		return nil
	}
	var recs []record
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		recs, err = loadRecords(tx, state.CIJobUnfinished, false, 0)
		return err
	}); err != nil {
		return err
	}
	i := slices.IndexFunc(recs, func(r record) bool { return r.runs(j.RunnerName) })
	if i < 0 || recs[i].Busy && recs[i].JobID == j.ID {
		return s.assign(ctx, j, nil) // not our runner, or nothing new
	}
	var kill *api.Error
	if _, err := s.admit(ctx, inst, repo, known, j.RunID); err != nil {
		rf, refused := asRefusal(err)
		if !refused {
			return err // unchanged: the next poll sees the job in progress again
		}
		s.cfg.Log.Warn("ci: runner was given a refused job; deleting its box", "job", recs[i].ID, "githubJob", j.ID, "repo", repo, "reason", rf.msg)
		kill = apiError(refusalError(rf))
	}
	return s.assign(ctx, j, kill)
}

// completed notes a job's end: our runner's job is done (its box goes
// soon, if the runner does not exit by itself); a record whose job ended
// before its runner got one loses its job.
func (s *Service) completed(ctx context.Context, j github.Job) error {
	return s.assign(ctx, j, nil)
}

// assign applies job j's status (in_progress or completed) to every
// unfinished record in one transaction: the runner that has j serves it
// (killed with kill, when set), and the job it was made for is handed off.
func (s *Service) assign(ctx context.Context, j github.Job, kill *api.Error) error {
	done := j.Status == "completed"
	var poke []string
	var spawn []*record
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		poke, spawn = nil, nil
		recs, err := loadRecords(tx, state.CIJobUnfinished, false, 0)
		if err != nil {
			return err
		}
		save := func(r *record, prev api.CIJobState) error {
			poke = append(poke, r.ID)
			return s.put(tx, r, prev, false)
		}
		// The runner that has j.
		var lost *record // what that runner was made for, now without one
		taker := slices.IndexFunc(recs, func(r record) bool { return j.RunnerName != "" && r.runs(j.RunnerName) })
		if taker >= 0 {
			r := &recs[taker]
			prev, changed := stateOf(r), false
			if r.JobID != j.ID {
				if !r.DemandGone {
					cp := *r
					lost = &cp
				}
				r.describe(j)
				r.AssignedJobID, changed = j.ID, true
			}
			if !r.Busy {
				r.Busy, changed = true, true
			}
			if kill != nil && r.Kill == "" {
				r.Kill, r.Error, changed = api.CIResultRefused, kill, true
			}
			if done && r.JobDoneAt == nil {
				now := s.now()
				r.JobDoneAt, r.Conclusion, changed = &now, j.Conclusion, true
			}
			if changed {
				if err := save(r, prev); err != nil {
					return err
				}
			}
		}
		// The idle runner made for j: it takes over what the taker was
		// made for, when it can serve that job, or is no longer needed.
		for k := range recs {
			r := &recs[k]
			if k == taker || r.JobID != j.ID || r.Busy || r.DemandGone || r.Phase == phaseDone {
				continue
			}
			prev := stateOf(r)
			if lost != nil && r.Kill == "" && r.Shape == lost.Shape && strings.EqualFold(r.Repo, lost.Repo) {
				s.cfg.Log.Info("ci: runner takes over a job whose runner got another one", "job", r.ID, "githubJob", lost.JobID, "from", lost.ID)
				r.describeFrom(lost)
				lost = nil
			} else {
				r.DemandGone = true
				if done && j.Conclusion != "" && r.Conclusion == "" {
					r.Conclusion = j.Conclusion
				}
			}
			if err := save(r, prev); err != nil {
				return err
			}
		}
		if lost == nil {
			return nil
		}
		// No runner of ours can take it over: a new record does.
		rec := &record{CIJob: api.CIJob{ID: domain.NewID("ci"), Repo: lost.Repo, Labels: lost.Labels, Shape: lost.Shape, QueuedAt: s.now()},
			Phase: phaseQueued, Installation: lost.Installation, Spec: lost.Spec}
		rec.describeFrom(lost)
		rec.Key = providerJob(lost.JobID) + "/" + rec.ID
		s.cfg.Log.Info("ci: job queued again: its runner got another job", "job", rec.ID, "githubJob", lost.JobID, "from", lost.ID)
		spawn = append(spawn, rec)
		return s.put(tx, rec, "", true)
	})
	if err != nil {
		return err
	}
	for _, id := range poke {
		s.poke(id)
	}
	for _, r := range spawn {
		s.spawn(r)
	}
	return nil
}

// runs reports whether r's runner is name.
func (r *record) runs(name string) bool { return r.Runner != "" && strings.EqualFold(r.Runner, name) }

// describe makes r describe job j.
func (r *record) describe(j github.Job) {
	r.JobID, r.RunID, r.Job, r.Workflow = j.ID, j.RunID, j.Name, j.WorkflowName
	if j.Labels != nil {
		r.Labels = j.Labels
	}
}

// describeFrom makes r describe the job o describes.
func (r *record) describeFrom(o *record) {
	r.JobID, r.RunID, r.Job, r.Workflow, r.Labels = o.JobID, o.RunID, o.Job, o.Workflow, o.Labels
}
