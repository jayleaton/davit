//go:build linux

package main_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/ci/github"
	"davit/internal/ci/githubtest"
	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/runtime/firecracker/fctest"
)

// guestFakeRunner stands in for run.sh in the real runner image: it keeps
// its just-in-time config where the test can read it and runs until the
// test creates the release file.
const guestFakeRunner = `umask 022; d=/tmp/fake-runner; mkdir -p "$d"
printf '%s' "$ACTIONS_RUNNER_INPUT_JITCONFIG" >"$d/jit.tmp" && mv "$d/jit.tmp" "$d/jit"
echo "fake runner listening as $(id -un) in $(pwd)"
while [ ! -e "$d/release" ]; do sleep 0.05; done
echo "fake runner done"`

// TestCIRunnerImage boots ubuntu-24.04-ci (DAVIT_TEST_CI_IMAGE_MANIFEST,
// exported by vm-tier when the host built it) on Firecracker: the real
// Actions runner answers --version as user runner, Docker runs a container,
// and GitHub Actions jobs run end to end (a fake GitHub and a fake runner
// script in the real image), each in a fresh box that is deleted at the
// end. It logs queued-to-box-ready and job-end-to-box-gone timings (the
// first job's box boots cold and captures the golden, later ones restore).
func TestCIRunnerImage(t *testing.T) {
	if os.Getenv("DAVIT_TEST_FIRECRACKER") != "1" {
		t.Skip("real Firecracker tests disabled; set DAVIT_TEST_FIRECRACKER=1")
	}
	src := os.Getenv("DAVIT_TEST_CI_IMAGE_MANIFEST")
	if src == "" {
		t.Skip("DAVIT_TEST_CI_IMAGE_MANIFEST unset (vm-tier exports it once the host has built ubuntu-24.04-ci)")
	}
	fx := fctest.Setup(t)
	manifest := fx.Derive(t, src)
	var m struct{ Name string }
	if b, err := os.ReadFile(manifest); err != nil || json.Unmarshal(b, &m) != nil || m.Name == "" {
		t.Fatalf("derived manifest %s: %v", manifest, err)
	}
	gh := githubtest.New(t)
	gh.AddRepo("acme/app", false)
	dir := t.TempDir()
	key, secretFile := filepath.Join(dir, "app.pem"), filepath.Join(dir, "hook-secret")
	secret := []byte("vm-tier-webhook-secret")
	for p, b := range map[string][]byte{key: gh.KeyPEM(), secretFile: secret} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	hookURL := "http://" + addr + "/"
	const shape = "davit-vmtier"
	h := newFCHost(t, func(c *config.Daemon) {
		c.Images = append(c.Images, config.ImageSource{Manifest: manifest, BlobDir: fx.BlobDir})
		c.CI.GitHub = config.GitHubCI{Enabled: true, APIURL: gh.URL, AppID: gh.AppID, PrivateKeyFile: key,
			Installations:    []config.GitHubInstallation{{ID: 1, Repos: []config.GitHubRepo{{Name: "acme/app"}}}},
			Webhook:          config.GitHubWebhook{Listen: addr, SecretFile: secretFile},
			PollInterval:     config.Duration(-1),
			Image:            m.Name,
			Shapes:           []config.CIShape{{Label: shape, CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}},
			DefaultShape:     shape,
			RunnerNamePrefix: "davit-vmtier",
			RunnerCommand:    []string{"/bin/sh", "-c", guestFakeRunner},
		}
	})

	// The image: the real runner, as runner; Docker.
	h.waited(h.cli("new", "ci-box", "--image", m.Name, "--cpus=2", "--memory=2GiB"), domain.OpCreate)
	// run.sh's listener says goodbye after the version.
	versions := h.shAs("ci-box", "runner", "cd /opt/actions-runner && ./config.sh --version && ./run.sh --version | head -1")
	lines := strings.Split(versions, "\n")
	if len(lines) != 2 || lines[0] != lines[1] || !strings.HasPrefix(lines[0], "2.") {
		t.Fatalf("runner versions: %q", versions)
	}
	t.Logf("actions runner %s", lines[0])
	if env := h.sh("ci-box", "cat /opt/actions-runner/.env"); !strings.Contains(env, "RUNNER_TOOL_CACHE=/opt/hostedtoolcache") {
		t.Fatalf("runner .env: %q", env)
	}
	hello := h.cli("exec", "ci-box", "--user", "runner", "--", "sh", "-c", "docker pull -q hello-world >/dev/null && docker run --rm hello-world | grep -c 'Hello from Docker'")
	if out := strings.TrimSpace(output(t, hello, "stdout")); hello.code == 0 && out == "1" {
		t.Log("docker run --rm hello-world: ok (pulled)")
	} else {
		// No registry: an offline equivalent, a container from a static
		// binary the image already has.
		t.Logf("hello-world pull failed (exit %d), running offline: %s", hello.code, output(t, hello, "stderr"))
		out := h.shAs("ci-box", "runner", `set -e; d=$(mktemp -d); cp /usr/local/bin/davit-guest-agent "$d/davit"
tar -C "$d" -c davit | docker import -q - davit-ci/hello >/dev/null; docker run --rm davit-ci/hello /davit --version; rm -rf "$d"`)
		if out == "" {
			t.Fatal("offline docker run printed nothing")
		}
		t.Logf("docker run (offline image): %q", out)
	}
	h.waited(h.cli("delete", "ci-box", "--yes"), domain.OpDelete)

	// Jobs end to end.
	deliver := func(action string, j github.Job) {
		t.Helper()
		if st, err := gh.Deliver(hookURL, secret, action, j); err != nil || st != http.StatusAccepted {
			t.Fatalf("deliver %s: %d %v", action, st, err)
		}
	}
	find := func(id int64) api.CIJob {
		r := h.cli("ci", "status")
		var st api.CIStatus
		json.Unmarshal(r.raw[len(r.raw)-1], &st)
		for _, l := range [][]api.CIJob{st.Queued, st.Running, st.Recent} {
			for _, j := range l {
				if j.JobID == id {
					return j
				}
			}
		}
		return api.CIJob{}
	}
	waitJob := func(id int64, what string, ok func(api.CIJob) bool) api.CIJob {
		t.Helper()
		var j api.CIJob
		for deadline := time.Now().Add(5 * time.Minute); ; time.Sleep(20 * time.Millisecond) {
			if j = find(id); ok(j) {
				return j
			}
			if time.Now().After(deadline) {
				t.Fatalf("job %d: timed out waiting for %s: %+v\ndaemon log:\n%s", id, what, j, tail(h.log.String(), 4000))
			}
		}
	}
	for i := range 3 {
		j := gh.Queue("acme/app", []string{"self-hosted", "davit"}, githubtest.QueueOptions{Name: fmt.Sprintf("vm-tier-%d", i)})
		deliver("queued", j)
		run := waitJob(j.ID, "the runner", func(c api.CIJob) bool { return c.State == api.CIJobRunning })
		vm := string(run.VMID)
		var jit string
		for deadline := time.Now().Add(time.Minute); jit == ""; time.Sleep(50 * time.Millisecond) {
			r := h.cli("exec", vm, "--", "cat", "/tmp/fake-runner/jit")
			if r.code == 0 {
				jit = output(t, r, "stdout")
			} else if time.Now().After(deadline) {
				t.Fatalf("no just-in-time config in the box: %+v", r.last())
			}
		}
		cfg, err := githubtest.DecodeJIT(jit)
		if err != nil || cfg.Runner != run.Runner {
			t.Fatalf("box got config %q (%v), want runner %s", jit, err, run.Runner)
		}
		a, err := gh.Assign(j.ID, run.Runner)
		if err != nil {
			t.Fatal(err)
		}
		deliver("in_progress", a)
		end := time.Now()
		h.shAs(vm, "runner", "touch /tmp/fake-runner/release")
		deliver("completed", gh.Complete(j.ID, "success"))
		done := waitJob(j.ID, "done", func(c api.CIJob) bool { return c.State == api.CIJobDone })
		for deadline := time.Now().Add(2 * time.Minute); h.info(vm).State != domain.StateDeleted; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("box %s not deleted: %s", vm, h.info(vm).State)
			}
		}
		gone := time.Since(end)
		if done.Result != api.CIResultSucceeded || done.Conclusion != "success" {
			t.Fatalf("job %+v", done)
		}
		hist := h.cli("history", vm, done.ProcessID)
		if out := output(t, hist, "stdout"); !strings.Contains(out, "listening as runner in /home/runner") || !strings.Contains(out, "done") {
			t.Fatalf("runner output in history: %q", out)
		}
		t.Logf("CI job %d: queued->box ready %s, queued->runner started %s, job end->box gone %s",
			i, done.BoxReadyAt.Sub(done.QueuedAt).Round(time.Millisecond), done.StartedAt.Sub(done.QueuedAt).Round(time.Millisecond), gone.Round(time.Millisecond))
	}
	if rs := gh.Runners(); len(rs) != 0 {
		t.Fatalf("runners left registered: %+v", rs)
	}
}
