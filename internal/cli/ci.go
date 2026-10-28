package cli

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
)

// `davit ci status` (docs/ci-runners.md): the host's queued, running and
// recent CI jobs.

const ciUsage = "status [--limit N]"

func runCI(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) != 1 || args[0] != "status" {
		return usageErr("usage: %s ci %s", buildinfo.Name, ciUsage)
	}
	var q url.Values
	if o.limit > 0 {
		q = url.Values{"limit": {strconv.Itoa(o.limit)}}
	}
	var st api.CIStatus
	if err := s.cl.get(ctx, "/ci/status", q, &st); err != nil {
		return err
	}
	s.out.result("", st)
	if !st.Enabled {
		s.out.human("CI runners are off on this host (ci.github in the daemon config; see docs/ci-runners.md)")
		return nil
	}
	now := time.Now()
	s.out.human("queued %d, running %d (at most %d at once)", len(st.Queued), len(st.Running), st.MaxConcurrentJobs)
	section := func(title string, jobs []api.CIJob) {
		if len(jobs) == 0 {
			return
		}
		s.out.human("\n%s", title)
		s.out.human("%-28s %-24s %-9s %-26s %9s  %s", "REPO", "JOB", "STATE", "BOX", "DURATION", "RESULT")
		for _, j := range jobs {
			s.out.human("%-28s %-24s %-9s %-26s %9s  %s", clip(j.Repo, 28), clip(j.Job, 24), j.State, boxOf(j), ciDuration(j, now), ciResult(j))
		}
	}
	section("QUEUED", st.Queued)
	section("RUNNING", st.Running)
	section("RECENT", st.Recent)
	return nil
}

// ciDuration is how long the job has queued, run or took.
func ciDuration(j api.CIJob, now time.Time) string {
	end := now
	if j.EndedAt != nil {
		end = *j.EndedAt
	}
	start := j.QueuedAt
	if j.StartedAt != nil {
		start = *j.StartedAt
	}
	return roundAge(end.Sub(start))
}

func boxOf(j api.CIJob) string {
	if j.VMID == "" {
		return "-"
	}
	return string(j.VMID)
}

// ciResult is the job's outcome for people: GitHub's conclusion when the
// runner ran a job, else the host's result, with the reason.
func ciResult(j api.CIJob) string {
	out := string(j.Result)
	if j.Conclusion != "" {
		out = j.Conclusion
		if j.Result != "" && j.Result != api.CIResultSucceeded {
			out += " (" + string(j.Result) + ")"
		}
	}
	if j.Error != nil {
		out = fmt.Sprintf("%s: %s", out, j.Error.Message)
		if j.Result == "" {
			out = j.Error.Message
		}
	}
	return out
}

// clip shortens s to n characters.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
