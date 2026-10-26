package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/lifecycle"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
)

// lines runs `davit --json args...` and returns the exit code and every
// stdout envelope with its raw data.
func lines(t *testing.T, args ...string) (int, []Envelope, []json.RawMessage) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(append([]string{"--json"}, args...), Stdio{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
	envs, raws := parseLines(t, stdout.String())
	return code, envs, raws
}

func parseLines(t *testing.T, out string) ([]Envelope, []json.RawMessage) {
	t.Helper()
	var envs []Envelope
	var raws []json.RawMessage
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var e Envelope
		var raw struct {
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(l), &e) != nil || json.Unmarshal([]byte(l), &raw) != nil {
			t.Fatalf("stdout line is not JSON: %q", l)
		}
		envs, raws = append(envs, e), append(raws, raw.Data)
	}
	return envs, raws
}

// waitedVM runs a waited lifecycle command and returns the result's VM.
func waitedVM(t *testing.T, args ...string) api.VM {
	t.Helper()
	code, envs, raws := lines(t, args...)
	if code != 0 {
		t.Fatalf("%v: exit %d %+v", args, code, envs[len(envs)-1].Error)
	}
	var res struct{ VM api.VM }
	if err := json.Unmarshal(raws[len(raws)-1], &res); err != nil {
		t.Fatal(err)
	}
	return res.VM
}

// gatedRuntime holds cold boots and clean stops until the test opens the
// matching gate, so a test sees an operation in flight however slow the host.
type gatedRuntime struct {
	runtime.Runtime
	mu         sync.Mutex
	boot, stop chan struct{}
}

// hold closes gate until the returned function opens it.
func (g *gatedRuntime) hold(gate *chan struct{}) (open func()) {
	ch := make(chan struct{})
	g.mu.Lock()
	*gate = ch
	g.mu.Unlock()
	return func() { close(ch) }
}

func (g *gatedRuntime) gate(gate *chan struct{}) <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return *gate
}

func (g *gatedRuntime) ColdBoot(ctx context.Context, inst runtime.Instance, disks runtime.DiskSet, net runtime.NetworkHandle) (runtime.Handle, error) {
	select {
	case <-g.gate(&g.boot):
	case <-ctx.Done():
		return runtime.Handle{}, ctx.Err()
	}
	return g.Runtime.ColdBoot(ctx, inst, disks, net)
}

func (g *gatedRuntime) Stop(ctx context.Context, h runtime.Handle, grace time.Duration) error {
	select {
	case <-g.gate(&g.stop):
	case <-ctx.Done():
		return ctx.Err()
	}
	return g.Runtime.Stop(ctx, h, grace)
}

// gated serves the API with a gatedRuntime; both gates start open.
func gated(t *testing.T) *gatedRuntime {
	rt, err := fake.New(fake.Options{Dir: filepath.Join(t.TempDir(), "rt")})
	if err != nil {
		t.Fatal(err)
	}
	g := &gatedRuntime{Runtime: rt, boot: make(chan struct{}), stop: make(chan struct{})}
	close(g.boot)
	close(g.stop)
	serveWith(t, func(c *lifecycle.Config) { c.Runtime = g })
	return g
}

// TestWaitTimeoutLeavesOperationRunning: --wait-timeout and the global
// --timeout end the client wait with exit 7 and a client-origin `timeout`
// error naming the operation; the operation itself still succeeds.
func TestWaitTimeoutLeavesOperationRunning(t *testing.T) {
	g := gated(t)
	for _, args := range [][]string{{"new", "a", "--wait-timeout", "150ms"}, {"--timeout", "150ms", "new", "b"}} {
		openBoot := g.hold(&g.boot) // the boot waits until the client has given up
		r := runCLI(t, false, args...)
		e := r.last.Error
		if r.code != ExitTransport || r.last.Event != "error" || e == nil || e.Code != domain.CodeTimeout || e.Origin != "client" ||
			e.OperationID == "" || r.last.OperationID != e.OperationID || !strings.Contains(e.Message, string(e.OperationID)+" continues") {
			t.Fatalf("%v: exit %d %+v", args, r.code, e)
		}
		if op := runCLI(t, false, "operations", string(e.OperationID)); op.data["state"] != string(domain.OpRunning) {
			t.Fatalf("%v: the client gave up after the operation ended: %+v", args, op.data)
		}
		openBoot()
		w := runCLI(t, false, "operations", string(e.OperationID), "--watch")
		if op, _ := w.data["operation"].(map[string]any); w.code != 0 || op["state"] != string(domain.OpSucceeded) {
			t.Fatalf("%v: operation after the client gave up: exit %d %+v", args, w.code, w.data)
		}
	}
}

type fixedAuth struct{ err error }

func (a fixedAuth) Authenticate(*http.Request) (domain.PrincipalID, error) { return "", a.err }

// TestExitCodesForRejectedAndBrokenServers: the server's 401/403 map to
// exits 3/4, and a non-JSON 500 to exit 1 (internal, origin server).
func TestExitCodesForRejectedAndBrokenServers(t *testing.T) {
	for _, c := range []struct {
		code domain.Code
		exit int
	}{{domain.CodeUnauthenticated, ExitAuthn}, {domain.CodePermissionDenied, ExitAuthz}} {
		t.Run(string(c.code), func(t *testing.T) {
			serveAuth(t, nil, fixedAuth{domain.Errorf(c.code, "no")})
			r := runCLI(t, false, "info", "web")
			if r.code != c.exit || r.last.Error == nil || r.last.Error.Code != c.code || r.last.Error.Origin != "server" {
				t.Fatalf("exit %d %+v", r.code, r.last.Error)
			}
		})
	}
	t.Run("non-JSON 500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "<html>proxy exploded</html>", http.StatusInternalServerError)
		}))
		defer srv.Close()
		t.Setenv("DAVIT_API_URL", srv.URL)
		r := runCLI(t, false, "info", "web")
		if r.code != ExitGeneral || r.last.Error == nil || r.last.Error.Code != domain.CodeInternal ||
			r.last.Error.Origin != "server" || !strings.Contains(r.last.Error.Message, "HTTP 500") {
			t.Fatalf("exit %d %+v", r.code, r.last.Error)
		}
	})
}

// TestTTLFlagsReachVM: new --ttl / --no-auto-stop and resume --ttl set the
// VM's TTL, auto-stop and expiry.
func TestTTLFlagsReachVM(t *testing.T) {
	serve(t)
	// The countdown starts while the command runs, however long that takes.
	near := func(ttl time.Duration, args ...string) {
		t.Helper()
		before := time.Now()
		vm := waitedVM(t, args...)
		after := time.Now()
		if vm.TTLSeconds != int64(ttl/time.Second) || !vm.AutoStop || vm.ExpiresAt == nil {
			t.Fatalf("want ttl %s with auto-stop: %+v", ttl, vm)
		}
		if e := *vm.ExpiresAt; e.Before(before.Add(ttl).Truncate(time.Second)) || e.After(after.Add(ttl)) {
			t.Fatalf("expiresAt %s outside [%s, %s] + %s", e, before, after, ttl)
		}
	}
	near(90*time.Second, "new", "web", "--ttl", "90s")
	if vm := waitedVM(t, "new", "box", "--no-auto-stop"); vm.AutoStop || vm.ExpiresAt != nil {
		t.Fatalf("--no-auto-stop: %+v", vm)
	}
	waitedVM(t, "stop", "web")
	near(2*time.Minute, "resume", "web", "--ttl", "2m")
}

// TestStopForceRecordsCrashConsistency: a forced stop commits a
// crash-consistent snapshot, a clean stop a clean one.
func TestStopForceRecordsCrashConsistency(t *testing.T) {
	serve(t)
	waitedVM(t, "new", "clean")
	waitedVM(t, "new", "forced")
	want := map[domain.SnapshotID]domain.Consistency{
		waitedVM(t, "stop", "clean").LastSnapshotID:             domain.ConsistencyClean,
		waitedVM(t, "stop", "forced", "--force").LastSnapshotID: domain.ConsistencyCrash,
	}
	for _, vm := range []string{"clean", "forced"} {
		code, envs, raws := lines(t, "snapshots", vm)
		if code != 0 {
			t.Fatalf("snapshots %s: exit %d", vm, code)
		}
		for i, e := range envs {
			var sn api.Snapshot
			if json.Unmarshal(raws[i], &sn); e.Event == "item" {
				if c, ok := want[sn.ID]; ok && sn.Consistency != c {
					t.Fatalf("%s: snapshot %s consistency %s, want %s", vm, sn.ID, sn.Consistency, c)
				}
				delete(want, sn.ID)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("stop snapshots not listed: %v", want)
	}
}

// TestListFilterAndPaging: --filter selects states (and the default hides
// stopped VMs); --limit 1 with --cursor walks every VM exactly once.
func TestListFilterAndPaging(t *testing.T) {
	serve(t)
	for _, n := range []string{"a", "b", "c"} {
		waitedVM(t, "new", n)
	}
	waitedVM(t, "stop", "b")
	names := func(args ...string) ([]string, string) {
		t.Helper()
		code, envs, raws := lines(t, append([]string{"list"}, args...)...)
		if code != 0 || envs[len(envs)-1].Event != "result" {
			t.Fatalf("list %v: exit %d", args, code)
		}
		var out []string
		for i, e := range envs {
			var vm api.VM
			if json.Unmarshal(raws[i], &vm); e.Event == "item" {
				out = append(out, vm.Name)
			}
		}
		var res struct{ NextCursor string }
		json.Unmarshal(raws[len(raws)-1], &res)
		return out, res.NextCursor
	}
	if got, _ := names("--filter", "stopped"); strings.Join(got, ",") != "b" {
		t.Fatalf("--filter stopped: %v", got)
	}
	if got, _ := names(); strings.Join(got, ",") != "a,c" && strings.Join(got, ",") != "c,a" {
		t.Fatalf("default list: %v", got)
	}
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		args := []string{"--all", "--limit", "1"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		got, next := names(args...)
		pages++
		if len(got) > 1 || pages > 4 {
			t.Fatalf("page %d: %v", pages, got)
		}
		for _, n := range got {
			if seen[n] {
				t.Fatalf("%s listed twice", n)
			}
			seen[n] = true
		}
		if cursor = next; cursor == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Fatalf("paging saw %v", seen)
	}
}

// syncBuffer is a goroutine-safe stdout.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// watch starts `info --watch` in the background and waits until it has
// printed its first VM.
func watch(t *testing.T, args ...string) (*syncBuffer, <-chan int) {
	t.Helper()
	out := &syncBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- Main(append([]string{"info"}, args...), Stdio{Stdin: strings.NewReader(""), Stdout: out, Stderr: &syncBuffer{}})
	}()
	for deadline := time.Now().Add(time.Minute); out.String() == ""; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("watch printed nothing")
		}
	}
	return out, done
}

func exitOf(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case c := <-done:
		return c
	case <-time.After(time.Minute):
		t.Fatal("watch did not end")
	}
	return -1
}

// TestInfoWatch follows a VM through stop: one `item` per stateRevision
// (ready, stopping, stopped), then a `result` at --until; human mode prints
// a timestamped block per change; the budget ends a watch with exit 7.
func TestInfoWatch(t *testing.T) {
	// The stop is held until both watches have printed "stopping".
	g := gated(t)
	waitedVM(t, "new", "web")

	out, done := watch(t, "--json", "web", "--watch", "--until", "stopped")
	human, hdone := watch(t, "web", "--until", "stopped") // --until implies --watch
	openStop := g.hold(&g.stop)
	runCLI(t, false, "stop", "web", "--no-wait")
	for deadline := time.Now().Add(time.Minute); !strings.Contains(out.String(), `"stopping"`) || !strings.Contains(human.String(), "stopping"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the watches never showed stopping:\n%s\n%s", out.String(), human.String())
		}
	}
	openStop()
	if c := exitOf(t, done); c != 0 {
		t.Fatalf("watch exit %d: %s", c, out.String())
	}
	envs, raws := parseLines(t, out.String())
	var states []string
	rev := int64(-1)
	for i, e := range envs[:len(envs)-1] {
		var vm api.VM
		json.Unmarshal(raws[i], &vm)
		if e.Event != "item" || vm.StateRevision <= rev {
			t.Fatalf("line %d: %s rev %d after %d", i, e.Event, vm.StateRevision, rev)
		}
		rev = vm.StateRevision
		states = append(states, string(vm.State))
	}
	var final api.VM
	json.Unmarshal(raws[len(raws)-1], &final)
	if got := strings.Join(states, ","); got != "ready,stopping,stopped" || envs[len(envs)-1].Event != "result" || final.State != domain.StateStopped {
		t.Fatalf("watch saw %s, then %s %s", got, envs[len(envs)-1].Event, final.State)
	}
	if c := exitOf(t, hdone); c != 0 || strings.Count(human.String(), "--- ") != 3 || !strings.Contains(human.String(), "state      stopped") {
		t.Fatalf("human watch exit %d:\n%s", c, human.String())
	}

	// Nothing changes on a stopped VM, so only the budget ends this watch.
	r := runCLI(t, false, "info", "web", "--watch", "--wait-timeout", "300ms")
	if r.code != ExitTransport || r.last.Error == nil || r.last.Error.Code != domain.CodeTimeout {
		t.Fatalf("budget: exit %d %+v", r.code, r.last.Error)
	}
	if r := runCLI(t, false, "info", "web", "--watch", "--until", "asleep"); r.code != ExitUsage {
		t.Fatalf("bad --until: exit %d", r.code)
	}
}
