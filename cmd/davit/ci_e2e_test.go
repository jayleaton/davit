//go:build linux

package main_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/ci/github"
	"davit/internal/ci/githubtest"
)

// fakeRunner stands in for /opt/actions-runner/run.sh in the fake runtime,
// whose guest is this host: it writes its just-in-time config to a file in
// $1 and runs until the test removes that file.
const fakeRunner = `#!/bin/sh
f="$1/jit.$$"
printf '%s' "$ACTIONS_RUNNER_INPUT_JITCONFIG" >"$f.tmp" && mv "$f.tmp" "$f"
echo "fake runner $$ listening"
while [ -e "$f" ]; do sleep 0.05; done
echo "fake runner $$ done"
`

// ciHost is a daemon with ci.github on, wired to a fake GitHub.
type ciHost struct {
	*host
	gh      *githubtest.Server
	secret  []byte
	hookURL string
	jits    string
}

func newCIHost(t *testing.T, maxJobs int) *ciHost {
	t.Helper()
	h := newHost(t)
	h.guestAgent = true
	gh := githubtest.New(t)
	gh.AddRepo("acme/app", false)
	gh.AddRepo("acme/oss", true)
	c := &ciHost{host: h, gh: gh, secret: []byte("e2e-webhook-secret"), jits: filepath.Join(h.dir, "jits")}
	key, secretFile, runner := filepath.Join(h.dir, "app.pem"), filepath.Join(h.dir, "hook-secret"), filepath.Join(h.dir, "run.sh")
	for p, b := range map[string][]byte{key: gh.KeyPEM(), secretFile: c.secret, runner: []byte(fakeRunner)} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(runner, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.jits, 0o700); err != nil {
		t.Fatal(err)
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	// A free port for the receiver; the daemon config needs it up front.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	c.hookURL = "http://" + addr + "/"
	cfg := map[string]any{"github": map[string]any{
		"enabled": true, "apiUrl": gh.URL, "appId": gh.AppID, "privateKeyFile": key,
		"installations": []any{map[string]any{"id": 1, "repos": []any{map[string]any{"name": "acme/app"}, map[string]any{"name": "acme/oss"}}}},
		"webhook":       map[string]any{"listen": addr, "secretFile": secretFile},
		"pollInterval":  "-1s", "image": "dev", "maxConcurrentJobs": maxJobs, "runnerNamePrefix": "davit-e2e",
		"shapes":        []any{map[string]any{"label": "davit-small", "cpus": 1, "memoryMiB": 256, "diskGiB": 1}},
		"defaultShape":  "davit-small",
		"runnerCommand": []string{runner, c.jits}, "runnerUser": me.Username,
	}}
	b, _ := json.Marshal(cfg)
	h.extraConfig = `"ci":` + string(b)
	h.start(20*time.Millisecond, 0)
	return c
}

// deliver posts a signed workflow_job webhook and expects it accepted.
func (c *ciHost) deliver(action string, j github.Job) {
	c.t.Helper()
	if st, err := c.gh.Deliver(c.hookURL, c.secret, action, j); err != nil || st != http.StatusAccepted {
		c.t.Fatalf("deliver %s job %d: %d %v", action, j.ID, st, err)
	}
}

func (c *ciHost) status() api.CIStatus {
	c.t.Helper()
	r := c.cli("ci", "status")
	var st api.CIStatus
	if r.code != 0 || json.Unmarshal(r.raw[len(r.raw)-1], &st) != nil {
		c.t.Fatalf("ci status: exit %d %+v %s", r.code, r.last(), r.stderr)
	}
	return st
}

// ciJob finds GitHub job id in status.
func (c *ciHost) ciJob(id int64) (api.CIJob, bool) {
	st := c.status()
	for _, l := range [][]api.CIJob{st.Queued, st.Running, st.Recent} {
		for _, j := range l {
			if j.JobID == id {
				return j, true
			}
		}
	}
	return api.CIJob{}, false
}

// runners maps each started fake runner's name to its config file.
func (c *ciHost) runners() map[string]string {
	c.t.Helper()
	out := map[string]string{}
	files, _ := filepath.Glob(filepath.Join(c.jits, "jit.*"))
	for _, f := range files {
		if strings.HasSuffix(f, ".tmp") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue // released meanwhile
		}
		jit, err := githubtest.DecodeJIT(string(b))
		if err != nil {
			c.t.Fatalf("runner got a config GitHub did not issue (%q): %v", b, err)
		}
		out[jit.Runner] = f
	}
	return out
}

// take has GitHub give job j to a started runner that has none yet, and
// returns that runner's config file (removing it ends the runner).
func (c *ciHost) take(j github.Job, busy map[string]bool) string {
	c.t.Helper()
	var file string
	eventually(c.t, fmt.Sprintf("a runner for job %d", j.ID), func() bool {
		for name, f := range c.runners() {
			if busy[name] {
				continue
			}
			a, err := c.gh.Assign(j.ID, name)
			if err != nil {
				c.t.Fatal(err)
			}
			busy[name], file = true, f
			c.deliver("in_progress", a)
			return true
		}
		return false
	})
	return file
}

// TestCIRunnersEndToEnd drives GitHub Actions jobs through the real binary:
// a fake GitHub queues jobs and fires signed webhooks, the daemon forks a
// box per job and starts the (fake) runner in it with a just-in-time config,
// and deletes the box when the runner exits. A burst beyond
// maxConcurrentJobs queues; refused jobs get no box.
func TestCIRunnersEndToEnd(t *testing.T) {
	c := newCIHost(t, 2)
	labels := []string{"self-hosted", "davit"}

	// One job, start to finish.
	j := c.gh.Queue("acme/app", labels, githubtest.QueueOptions{Name: "test"})
	c.deliver("queued", j)
	busy := map[string]bool{}
	release := c.take(j, busy)
	eventually(t, "job running", func() bool { r, _ := c.ciJob(j.ID); return r.State == api.CIJobRunning && r.VMID != "" })
	running, _ := c.ciJob(j.ID)
	if running.Repo != "acme/app" || running.Job != "test" || running.Shape != "davit-small" || running.BoxReadyAt == nil || running.ProcessID == "" {
		t.Fatalf("running job %+v", running)
	}
	if n := c.liveVMs(); n != 1 {
		t.Fatalf("%d boxes for one job", n)
	}
	if err := os.Remove(release); err != nil {
		t.Fatal(err)
	}
	c.deliver("completed", c.gh.Complete(j.ID, "success"))
	eventually(t, "job done", func() bool { r, _ := c.ciJob(j.ID); return r.State == api.CIJobDone })
	done, _ := c.ciJob(j.ID)
	if done.Result != api.CIResultSucceeded || done.Conclusion != "success" || done.EndedAt == nil || done.ExitCode == nil || *done.ExitCode != 0 {
		t.Fatalf("done job %+v", done)
	}
	eventually(t, "box deleted", func() bool { return c.liveVMs() == 0 })
	// The runner's output stays in the box's history.
	hist := c.cli("history", string(done.VMID), done.ProcessID)
	if out := output(t, hist, "stdout"); hist.code != 0 || !strings.Contains(out, "listening") || !strings.Contains(out, "done") {
		t.Fatalf("runner history: exit %d %q %s", hist.code, out, hist.stderr)
	}

	// Refused: a public repository without allowPublic, and a fork's run.
	oss := c.gh.Queue("acme/oss", labels, githubtest.QueueOptions{})
	fork := c.gh.Queue("acme/app", labels, githubtest.QueueOptions{Fork: "mallory/app"})
	c.deliver("queued", oss)
	c.deliver("queued", fork)
	for _, id := range []int64{oss.ID, fork.ID} {
		eventually(t, "refused", func() bool { r, _ := c.ciJob(id); return r.State == api.CIJobRefused })
		if r, _ := c.ciJob(id); r.VMID != "" || r.Error == nil {
			t.Fatalf("refused job %+v", r)
		}
	}

	// A burst of 4 with room for 2: two run, two wait in order.
	var burst []github.Job
	for i := range 4 {
		b := c.gh.Queue("acme/app", labels, githubtest.QueueOptions{Name: fmt.Sprintf("matrix-%d", i)})
		c.deliver("queued", b)
		burst = append(burst, b)
	}
	eventually(t, "two runners", func() bool { return len(c.runners()) == 2 })
	time.Sleep(300 * time.Millisecond) // a third would have started by now
	st := c.status()
	if len(c.runners()) != 2 || len(st.Running) != 2 || len(st.Queued) != 2 || st.MaxConcurrentJobs != 2 {
		t.Fatalf("burst: %d runners, status %+v", len(c.runners()), st)
	}
	if n := c.liveVMs(); n != 2 {
		t.Fatalf("burst: %d boxes, want 2", n)
	}
	for _, b := range burst {
		f := c.take(b, busy)
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
		c.deliver("completed", c.gh.Complete(b.ID, "success"))
	}
	for _, b := range burst {
		eventually(t, "burst job done", func() bool { r, _ := c.ciJob(b.ID); return r.State == api.CIJobDone })
		if r, _ := c.ciJob(b.ID); r.Result != api.CIResultSucceeded {
			t.Fatalf("burst job %+v", r)
		}
	}
	eventually(t, "burst boxes deleted", func() bool { return c.liveVMs() == 0 })
	if rs := c.gh.Runners(); len(rs) != 0 {
		t.Fatalf("runners left registered: %+v", rs)
	}

	// The events tell the story.
	r := c.cli("events", "--types", "ci.job.")
	types := map[string]int{}
	for _, raw := range r.raw[:len(r.raw)-1] {
		var ev api.Event
		json.Unmarshal(raw, &ev)
		types[ev.Type]++
	}
	if r.code != 0 || types[api.EventCIJobCreated] != 5 || types[api.EventCIJobRefused] != 2 || types[api.EventCIJobState] < 5*3 {
		t.Fatalf("ci events %v (%d lines)", types, len(r.lines))
	}
}
