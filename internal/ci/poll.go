package ci

import (
	"context"
	"errors"
	"strings"
	"time"

	"davit/internal/ci/github"
	"davit/internal/state"
)

// Polling finds queued jobs without any inbound connection: every
// PollInterval each allowed repository's queued and in-progress runs are
// listed with their jobs, and each job's status goes through the same
// intake as a webhook. A job a record waits on that no longer shows up is
// looked up by ID, so a missed completed webhook still ends its runner.

func (s *Service) pollLoop() {
	if s.set.poll < 0 {
		return
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		wait := s.pollOnce(s.ctx)
		t.Reset(max(wait, s.set.poll))
	}
}

// pollOnce polls every repository once and returns how long GitHub asked
// to wait before the next poll (0 when it did not).
func (s *Service) pollOnce(ctx context.Context) time.Duration {
	seen := map[int64]bool{} // jobs seen queued or in progress
	var wait time.Duration
	note := func(repo string, err error) {
		var ge *github.Error
		if errors.As(err, &ge) && ge.RetryAfter > wait {
			wait = ge.RetryAfter
		}
		s.cfg.Log.Warn("ci: poll", "repo", repo, "err", err)
	}
	for name, p := range s.set.repos {
		if ctx.Err() != nil || wait > 0 {
			break
		}
		repo := name
		in := s.cfg.App.Installation(p.installation)
		for _, status := range []string{"queued", "in_progress"} {
			runs, err := in.Runs(ctx, repo, status)
			if err != nil {
				note(repo, err)
				break
			}
			for _, run := range runs {
				if run.Repository.FullName != "" {
					repo = run.Repository.FullName // GitHub's spelling
					s.cache.putRepo(run.Repository, s.cfg.Clock.Now())
				}
				s.cache.putRun(repo, run, s.cfg.Clock.Now())
				jobs, err := in.RunJobs(ctx, repo, run.ID)
				if err != nil {
					note(repo, err)
					continue
				}
				for _, j := range jobs {
					if j.Status != "queued" && j.Status != "in_progress" {
						continue
					}
					seen[j.ID] = true
					if err := s.observe(ctx, p.installation, repo, nil, j); err != nil {
						note(repo, err)
					}
				}
			}
		}
	}
	if wait == 0 && ctx.Err() == nil {
		s.pollMissing(ctx, seen)
	}
	return wait
}

// pollMissing looks up the jobs unfinished records wait on that the poll
// did not see queued or in progress: they completed (or moved) unseen.
func (s *Service) pollMissing(ctx context.Context, seen map[int64]bool) {
	var recs []record
	if err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		recs, err = loadRecords(tx, state.CIJobUnfinished, false, 0)
		return err
	}); err != nil {
		s.cfg.Log.Warn("ci: poll", "err", err)
		return
	}
	for _, r := range recs {
		if r.Phase == phaseCleanup || r.Kill != "" {
			continue
		}
		watch := r.JobID
		switch {
		case r.Busy && r.AssignedJobID != 0:
			watch = r.AssignedJobID
		case r.Busy && r.JobDoneAt != nil, !r.Busy && r.DemandGone:
			continue
		}
		if seen[watch] {
			continue
		}
		j, err := s.cfg.App.Installation(r.Installation).Job(ctx, r.Repo, watch)
		if err != nil {
			s.cfg.Log.Warn("ci: poll job", "job", r.ID, "githubJob", watch, "err", err)
			continue
		}
		if !strings.EqualFold(j.Status, "completed") && !(j.Status == "in_progress" && j.RunnerName != "") {
			continue
		}
		if err := s.observe(ctx, r.Installation, r.Repo, nil, j); err != nil {
			s.cfg.Log.Warn("ci: poll job", "job", r.ID, "err", err)
		}
	}
}
