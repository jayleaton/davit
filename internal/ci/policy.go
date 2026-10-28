package ci

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"davit/internal/ci/github"
	"davit/internal/config"
	"davit/internal/domain"
)

// match decides from a job's runs-on labels whether it is this host's
// and which shape it gets. A job is ours when it asks for self-hosted and
// the host label, and every other label is one a davit runner has: linux,
// the host's architecture, or at most one configured shape (none: the
// default shape). Anything else is meant for another runner, so the job
// is left alone. GitHub matches labels case-insensitively; so does this.
func match(labels []string, set settings, g config.GitHubCI) (config.CIShape, bool) {
	var self, host bool
	shape := ""
	for _, l := range labels {
		k := strings.ToLower(l)
		switch {
		case k == "self-hosted":
			self = true
		case k == strings.ToLower(set.label):
			host = true
		case slices.Contains(config.CIBuiltinLabels, k) || k == set.archLabel:
		default:
			if _, ok := g.ShapeFor(k); !ok || shape != "" && shape != k {
				return config.CIShape{}, false
			}
			shape = k
		}
	}
	if !self || !host {
		return config.CIShape{}, false
	}
	sh, ok := g.ShapeFor(shape)
	return sh, ok
}

// runnerLabels are the custom labels of a runner for shape: the host label
// and the shape's (GitHub adds self-hosted, the OS and the architecture).
func runnerLabels(set settings, shape string) []string {
	return []string{set.label, shape}
}

// refusal is a policy decision against a job.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// asRefusal reports whether err is a policy refusal.
func asRefusal(err error) (*refusal, bool) {
	r, ok := err.(*refusal)
	return r, ok
}

// refusalError is the API error of a refusal.
func refusalError(r *refusal) *domain.Error {
	return domain.Errorf(domain.CodePermissionDenied, "%s", r.msg)
}

// admit applies the host's policy to a job of repo, run by installation
// inst: the repository must be allowed (and served through inst),
// a public repository needs allowPublic, and a run whose code comes from a
// fork needs allowForks. known, when set, is the repository as GitHub
// described it in a webhook (saves a lookup). A refusal is a *refusal;
// any other error means the policy could not be checked (retry later).
func (s *Service) admit(ctx context.Context, inst int64, repo string, known *github.Repository, runID int64) (repoPolicy, error) {
	p, ok := s.set.repos[strings.ToLower(repo)]
	if !ok {
		return p, refuse("repository %s is not allowed on this host (ci.github.installations)", repo)
	}
	if inst != 0 && inst != p.installation {
		return p, refuse("repository %s is configured for installation %d, not %d", repo, p.installation, inst)
	}
	info := known
	if info == nil || info.FullName == "" {
		r, err := s.cache.repo(ctx, s.cfg.App.Installation(p.installation), repo, s.cfg.Clock.Now())
		if err != nil {
			return p, fmt.Errorf("look up repository %s: %w", repo, err)
		}
		info = &r
	}
	if info.Public() && !p.AllowPublic {
		return p, refuse("repository %s is public: anyone could run code on this host through it (set allowPublic for it to allow)", repo)
	}
	if runID != 0 && !p.AllowForks {
		run, err := s.cache.run(ctx, s.cfg.App.Installation(p.installation), repo, runID, s.cfg.Clock.Now())
		if err != nil {
			return p, fmt.Errorf("look up workflow run %d of %s: %w", runID, repo, err)
		}
		if run.FromFork() {
			return p, refuse("workflow run %d of %s builds code from the fork %s (%s event; set allowForks for the repository to allow)",
				runID, repo, run.HeadRepository.FullName, run.Event)
		}
	}
	return p, nil
}

// cache keeps repository and run lookups briefly: a burst of jobs from one
// run or repository asks GitHub once.
type cache struct {
	mu    sync.Mutex
	repos map[string]cachedRepo
	runs  map[string]cachedRun
}

type cachedRepo struct {
	r  github.Repository
	at time.Time
}

type cachedRun struct {
	r  github.Run
	at time.Time
}

const (
	repoTTL = 10 * time.Minute
	runTTL  = time.Hour
	// cacheMax bounds each map; a full map is cleared.
	cacheMax = 4096
)

func (c *cache) repo(ctx context.Context, in github.Installation, name string, now time.Time) (github.Repository, error) {
	k := strings.ToLower(name)
	c.mu.Lock()
	if e, ok := c.repos[k]; ok && now.Sub(e.at) < repoTTL {
		c.mu.Unlock()
		return e.r, nil
	}
	c.mu.Unlock()
	r, err := in.Repo(ctx, name)
	if err != nil {
		return r, err
	}
	c.putRepo(r, now)
	return r, nil
}

func (c *cache) putRepo(r github.Repository, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.repos == nil || len(c.repos) >= cacheMax {
		c.repos = map[string]cachedRepo{}
	}
	c.repos[strings.ToLower(r.FullName)] = cachedRepo{r, now}
}

func (c *cache) run(ctx context.Context, in github.Installation, repo string, id int64, now time.Time) (github.Run, error) {
	k := fmt.Sprintf("%s#%d", strings.ToLower(repo), id)
	c.mu.Lock()
	if e, ok := c.runs[k]; ok && now.Sub(e.at) < runTTL {
		c.mu.Unlock()
		return e.r, nil
	}
	c.mu.Unlock()
	r, err := in.WorkflowRun(ctx, repo, id)
	if err != nil {
		return r, err
	}
	c.putRun(repo, r, now)
	return r, nil
}

// putRun caches run r of repo (the poller lists runs whole).
func (c *cache) putRun(repo string, r github.Run, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runs == nil || len(c.runs) >= cacheMax {
		c.runs = map[string]cachedRun{}
	}
	c.runs[fmt.Sprintf("%s#%d", strings.ToLower(repo), r.ID)] = cachedRun{r, now}
}
