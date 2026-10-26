// Package scripts tests the VM-host scripts without a VM host:
// host-preflight.sh against recorded probe facts and, through fake ssh and
// rsync on PATH, host-tier.sh's step order and failure handling. Nothing
// here boots a VM or reaches a real host. Git runs only in throwaway
// repositories under the test's temp dir, with every GIT_* variable unset.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func needBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the VM-host scripts target Linux (GNU tools)")
	}
	for _, tool := range []string{"bash", "git", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
}

// cleanEnv is the environment without GIT_* (so git never reaches the
// real repository) plus extra.
func cleanEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// run runs argv in dir and returns combined output and exit status.
func run(t *testing.T, dir string, env []string, argv ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("%v: %v", argv, err)
	}
	return string(out), 0
}

// status returns a check's status in the preflight table.
func status(out, check string) string {
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(check) + ` +(PASS|FAIL|WARN) `).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestPreflightFixtures(t *testing.T) {
	needBash(t)
	for _, tc := range []struct {
		facts string
		code  int
		want  map[string]string // check -> status
		fixes []string
	}{
		{"good.facts", 0, map[string]string{"arch": "PASS", "kvm": "PASS", "userns": "PASS", "reflink": "PASS", "disk": "PASS",
			"firecracker": "PASS", "nft": "PASS", "tools": "PASS", "subids": "PASS", "cgroup": "PASS"}, nil},
		// A stock Ubuntu 24.04 box before setup: the usual fixes.
		{"fresh-ubuntu.facts", 1, map[string]string{"kvm": "FAIL", "userns": "FAIL", "subids": "FAIL", "reflink": "FAIL",
			"tools": "FAIL", "disk": "WARN", "memory": "WARN", "nft": "WARN", "vsock": "PASS", "firecracker": "PASS"}, []string{
			"FAIL kvm: sudo usermod -aG kvm dev, then log in again",
			"kernel.apparmor_restrict_unprivileged_userns=0",
			"sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 dev && sudo apt install uidmap",
			"FAIL tools: sudo apt install e2fsprogs rsync util-linux",
			"VM_TIER_FLAGS='--fs ",
		}},
		// No virtualization at all, an old kernel, junk numbers.
		{"no-virt.facts", 1, map[string]string{"cpu-virt": "FAIL", "kernel": "FAIL", "kvm": "FAIL", "tun": "FAIL",
			"cgroup": "WARN", "userns": "FAIL", "reflink": "WARN", "disk": "FAIL", "memory": "FAIL"}, []string{
			"kernel.unprivileged_userns_clone=1",
			"sudo modprobe tun",
			"systemd.unified_cgroup_hierarchy=1",
		}},
	} {
		t.Run(tc.facts, func(t *testing.T) {
			out, code := run(t, ".", cleanEnv(), "./host-preflight.sh", "--facts", filepath.Join("testdata", "preflight", tc.facts))
			if code != tc.code {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			for check, want := range tc.want {
				if got := status(out, check); got != want {
					t.Errorf("%s: %q, want %s", check, got, want)
				}
			}
			for _, f := range tc.fixes {
				if !strings.Contains(out, f) {
					t.Errorf("no fix %q", f)
				}
			}
			// Every FAIL and WARN row has a fix line.
			for _, m := range regexp.MustCompile(`(?m)^([a-z-]+) +(FAIL|WARN) `).FindAllStringSubmatch(out, -1) {
				if !strings.Contains(out, "  "+m[2]+" "+m[1]+": ") {
					t.Errorf("%s %s has no fix", m[1], m[2])
				}
			}
		})
	}
	if out, code := run(t, ".", cleanEnv(), "./host-preflight.sh", "--facts", "/dev/null"); code != 2 {
		t.Fatalf("empty facts: exit %d\n%s", code, out)
	}
}

// shims writes fake ssh and rsync into a new dir and returns it. ssh logs
// each call (one line: the remote command) to $FAKE_LOG and answers by the
// remote command; see the script.
func shims(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("ssh", `#!/usr/bin/env bash
# Fake ssh: options, HOST, then the remote command (joined, as ssh does).
args=()
while [ $# -gt 0 ]; do
	case "$1" in
	-O) echo "control $2" >>"$FAKE_LOG"; exit 0 ;;
	-o) shift 2 ;;
	-*) shift ;;
	*) break ;;
	esac
done
host=$1; shift
cmd="$*"
[ -n "${FAKE_SSH_DOWN:-}" ] && exit 255
state=$FAKE_STATE
case "$cmd" in
"bash -s")
	cat >/dev/null
	echo "probe $host" >>"$FAKE_LOG"
	cat "$FAKE_FACTS"
	exit 0 ;;
true) exit 0 ;;
esac
cat >/dev/null
case "$cmd" in
*"vm-tier-remote.sh images "*) echo "remote images" >>"$FAKE_LOG"; echo images >"$state"; exit "${FAKE_IMAGES_RC:-0}" ;;
*"vm-tier-remote.sh prepare "*) echo "remote prepare" >>"$FAKE_LOG"; echo prepare >"$state"; exit "${FAKE_PREPARE_RC:-0}" ;;
*"vm-tier-remote.sh run "*)
	echo "remote run" >>"$FAKE_LOG"; echo run >"$state"
	echo "--- PASS: TestBoot (1.20s)"
	exit "${FAKE_RUN_RC:-0}" ;;
*"vm-tier-remote.sh cleanup "*) echo "remote cleanup $(cat "$state")" >>"$FAKE_LOG"; exit 0 ;;
*"-cf - ."*)
	# The run's logs, as a tar stream.
	d=$(mktemp -d)
	if [ "$(cat "$state")" = run ]; then
		printf -- '--- PASS: TestBoot (1.20s)\n' >"$d/summary.txt"
		[ "${FAKE_RUN_RC:-0}" = 1 ] && printf -- '--- FAIL: TestFork (3.00s)\nFAIL\tdavit/cmd/davit\t9.1s\n' >>"$d/summary.txt"
		echo log >"$d/test.log"
	else
		echo "$(cat "$state") log" >"$d/$(cat "$state").log"
	fi
	tar -C "$d" -cf - .
	rm -rf "$d"
	exit 0 ;;
*"test -f "*manifests*) exit "${FAKE_BUILT_RC:-0}" ;;
*"command -v rsync"*) exit 0 ;;
*"echo "*/runs/*) echo "/home/fake/.svm/runs/${cmd##*/runs/}" ;;
esac
exit 0
`)
	write("rsync", `#!/usr/bin/env bash
echo "rsync ${*: -1}" >>"$FAKE_LOG"
`)
	return dir
}

// tierRepo is a throwaway git repository with the scripts host-tier.sh
// needs, committed.
func tierRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, f := range []string{"host-tier.sh", "host-preflight.sh", "vm-tier.sh", "vm-tier-remote.sh"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "scripts", f), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := os.ReadFile("../images/inputs.lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "images", "inputs.lock"), lock, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("/.build/\n/results/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := cleanEnv("HOME=" + repo)
	for _, argv := range [][]string{
		{"git", "init", "-q"},
		{"git", "add", "-A"},
		{"git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "fixture"},
	} {
		if out, code := run(t, repo, env, argv...); code != 0 {
			t.Fatalf("%v: %s", argv, out)
		}
	}
	return repo
}

// TestHostTierOrchestration drives make vm-tier's script with fake ssh and
// rsync: the steps run in order, each vm-tier step cleans up on the host,
// the first failure stops the run with its exit status, and the summary
// says what happened.
func TestHostTierOrchestration(t *testing.T) {
	needBash(t)
	good, err := filepath.Abs("testdata/preflight/good.facts")
	if err != nil {
		t.Fatal(err)
	}
	bad, _ := filepath.Abs("testdata/preflight/fresh-ubuntu.facts")
	bin := shims(t)
	for _, tc := range []struct {
		name  string
		env   []string
		code  int
		calls []string // FAKE_LOG lines, in order (control/rsync lines dropped)
		sum   []string // in summary.md
	}{
		{"pass", nil, 0,
			[]string{"probe vmhost", "remote images", "remote cleanup images", "remote prepare", "remote cleanup prepare", "remote run", "remote cleanup run"},
			[]string{"result: PASS", "| preflight | pass |", "| images | pass | 0 |", "| prepare | pass | 0 |", "| tier | pass | 0 |", "pass 1, fail 0"}},
		{"preflight fails", []string{"FAKE_FACTS=" + bad}, 2,
			[]string{"probe vmhost"},
			[]string{"result: FAIL (exit 2)", "| preflight | FAIL | 1 |", "kvm          FAIL", "preflight: FAIL"}},
		{"ssh down", []string{"FAKE_SSH_DOWN=1"}, 255, nil,
			[]string{"result: FAIL (exit 255)", "| preflight | FAIL | 255 |"}},
		{"image build fails", []string{"FAKE_IMAGES_RC=3"}, 3,
			[]string{"probe vmhost", "remote images", "remote cleanup images"},
			[]string{"| images | FAIL | 3 |"}},
		{"prepare fails", []string{"FAKE_PREPARE_RC=2"}, 2,
			[]string{"probe vmhost", "remote images", "remote cleanup images", "remote prepare", "remote cleanup prepare"},
			[]string{"| prepare | FAIL | 2 |"}},
		{"tests fail", []string{"FAKE_RUN_RC=1"}, 1,
			[]string{"probe vmhost", "remote images", "remote cleanup images", "remote prepare", "remote cleanup prepare", "remote run", "remote cleanup run"},
			[]string{"result: FAIL (exit 1)", "| tier | FAIL | 1 |", "pass 1, fail 1", "--- FAIL: TestFork"}},
		{"skip images", []string{"ARGS=--skip-images"}, 0,
			[]string{"probe vmhost", "remote prepare", "remote cleanup prepare", "remote run", "remote cleanup run"},
			[]string{"| images | skipped |"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := tierRepo(t)
			log := filepath.Join(t.TempDir(), "calls")
			env := cleanEnv("HOME="+repo, "PATH="+bin+":"+os.Getenv("PATH"), "FAKE_LOG="+log,
				"FAKE_STATE="+filepath.Join(t.TempDir(), "state"), "FAKE_FACTS="+good)
			argv := []string{"scripts/host-tier.sh"}
			for _, kv := range tc.env {
				if a, ok := strings.CutPrefix(kv, "ARGS="); ok {
					argv = append(argv, a)
					continue
				}
				env = append(env, kv)
			}
			argv = append(argv, "vmhost", "TestBoot$")
			out, code := run(t, repo, env, argv...)
			if code != tc.code {
				t.Fatalf("exit %d, want %d\n%s", code, tc.code, out)
			}
			b, _ := os.ReadFile(log)
			var calls []string
			for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if l != "" && !strings.HasPrefix(l, "control ") && !strings.HasPrefix(l, "rsync ") {
					calls = append(calls, l)
				}
			}
			if strings.Join(calls, ";") != strings.Join(tc.calls, ";") {
				t.Fatalf("calls %q\nwant  %q\n%s", calls, tc.calls, out)
			}
			// Every vm-tier step shipped its payload with rsync.
			if n := strings.Count(string(b), "rsync "); n != strings.Count(strings.Join(tc.calls, ";"), "remote cleanup") {
				t.Errorf("%d rsync uploads for calls %q", n, tc.calls)
			}
			sums, _ := filepath.Glob(filepath.Join(repo, "results", "vm-tier", "*", "summary.md"))
			if len(sums) != 1 {
				t.Fatalf("summaries %v\n%s", sums, out)
			}
			s, _ := os.ReadFile(sums[0])
			for _, w := range tc.sum {
				if !strings.Contains(string(s), w) {
					t.Errorf("summary lacks %q:\n%s", w, s)
				}
			}
			if !strings.Contains(string(s), "`TestBoot$`") {
				t.Errorf("summary does not name the tests:\n%s", s)
			}
		})
	}
}

// TestPreflightProbeOverSSH runs the real probe through the fake ssh (which
// just runs it here): read-only, and its facts parse.
func TestPreflightProbeOverSSH(t *testing.T) {
	needBash(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/usr/bin/env bash\nexec bash -s\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	env := cleanEnv("PATH="+bin+":"+os.Getenv("PATH"), "HOME="+home)
	out, code := run(t, ".", env, "./host-preflight.sh", "--probe-only", "--fs", "~/fs", "somehost")
	if code != 0 || !strings.Contains(out, "probe_version=1\n") || !strings.Contains(out, "fs_path="+home+"/fs\n") ||
		!strings.Contains(out, "fs_checked="+home+"\n") || !regexp.MustCompile(`(?m)^arch=\S+$`).MatchString(out) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// The probe wrote nothing.
	if ents, _ := os.ReadDir(home); len(ents) != 0 {
		t.Fatalf("probe created %v in HOME", ents)
	}
	if out, code := run(t, ".", env, "./host-preflight.sh", "somehost"); code > 1 || !strings.Contains(out, "CHECK") {
		t.Fatalf("verdict: exit %d\n%s", code, out)
	}
}
