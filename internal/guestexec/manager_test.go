//go:build linux

package guestexec_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestagent"
	"davit/internal/guestclient"
	"davit/internal/guestexec"
	"davit/internal/guestproto"
	"davit/internal/lifecycle"
	"davit/internal/runtime"
	"davit/internal/state"
)

type nopSystem struct{}

func (nopSystem) SetClock(time.Time) error                     { return nil }
func (nopSystem) SetHostname(string) error                     { return nil }
func (nopSystem) RestartUnits(context.Context, []string) error { return nil }
func (nopSystem) SystemState(context.Context) guestproto.ServiceState {
	return guestproto.ServiceUnknown
}
func (nopSystem) UnitState(context.Context, string) guestproto.ServiceState {
	return guestproto.ServiceUnknown
}
func (nopSystem) SSHReady(context.Context) bool  { return false }
func (nopSystem) PowerOff(context.Context) error { return nil }

// guest is an in-process agent reachable over pipes; targets routes the
// test principal's VM to its current generation.
type guest struct {
	agent *guestagent.Agent
	mu    sync.Mutex
	key   runtime.Key
	// privacy is the policy the target reports (the store's copy may differ).
	privacy domain.PrivacyPolicy
}

func (g *guest) DialGuest(_ context.Context, _ uint32) (net.Conn, error) {
	a, b := net.Pipe()
	go g.agent.ServeConn(context.Background(), b)
	return a, nil
}

func (g *guest) GuestTarget(_ context.Context, p domain.PrincipalID, ref string) (lifecycle.GuestTarget, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p != "owner" || ref != string(g.key.VMID) {
		return lifecycle.GuestTarget{}, domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
	}
	return lifecycle.GuestTarget{Key: g.key, Dialer: g, Privacy: g.privacy}, nil
}

// boot runs the barrier for key so the manager can authenticate.
func (g *guest) boot(t *testing.T, secrets guestclient.SecretStore, key runtime.Key) {
	t.Helper()
	g.mu.Lock()
	g.key = key
	g.mu.Unlock()
	p := &guestclient.Prober{Secrets: secrets, Poll: time.Millisecond,
		Plan: func(context.Context, runtime.Key) (guestclient.Plan, error) {
			return guestclient.Plan{Hostname: "box"}, nil
		}}
	if _, err := p.Barrier(context.Background(), key, func(ctx context.Context) (net.Conn, error) { return g.DialGuest(ctx, 0) }, nil); err != nil {
		t.Fatal(err)
	}
}

func openDB(t *testing.T, path string) *state.DB {
	t.Helper()
	db, err := state.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Process rows reference their VM.
	_ = db.Tx(context.Background(), func(tx *state.Tx) error {
		now := time.Now()
		return tx.PutVM(&state.VMRecord{VM: domain.VM{ID: "vm_t", Owner: "owner", Name: "t", State: domain.StateReady,
			InstanceGeneration: 1, BootMode: domain.BootModeColdBoot, CreatedAt: now, UpdatedAt: now}})
	})
	return db
}

func setup(t *testing.T) (*guestexec.Manager, *guest, guestclient.SecretStore) {
	t.Helper()
	a, err := guestagent.New(guestagent.Config{Root: t.TempDir(), RunDir: t.TempDir(), System: nopSystem{}, SSHUnit: "-"})
	if err != nil {
		t.Fatal(err)
	}
	g := &guest{agent: a}
	secrets := &guestclient.MemorySecrets{}
	g.boot(t, secrets, runtime.Key{VMID: "vm_t", Generation: 1})
	m := guestexec.New(guestexec.Config{Targets: g, Secrets: secrets, AttachGrace: 300 * time.Millisecond,
		Store: guestexec.DBStore{DB: openDB(t, filepath.Join(t.TempDir(), "s.db"))}})
	t.Cleanup(m.Close)
	return m, g, secrets
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func collect(t *testing.T, m *guestexec.Manager, ctx context.Context, id string, after int64, run func(guestexec.Stream)) (string, *api.Process) {
	t.Helper()
	var out bytes.Buffer
	var final *api.Process
	err := m.Attach(ctx, "owner", "vm_t", id, after, func(msg api.ProcessStreamMessage) error {
		switch msg.Type {
		case api.StreamStdout, api.StreamStderr:
			b, _ := base64.StdEncoding.DecodeString(msg.Data)
			out.Write(b)
		case api.StreamExit:
			final = msg.Process
		case api.StreamError:
			t.Errorf("stream error %+v", msg.Error)
		}
		return nil
	}, run)
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), final
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestExecIdempotencyAndStdin(t *testing.T) {
	m, _, _ := setup(t)
	ctx := ctxT(t)
	req := api.ExecRequest{Argv: []string{"sh", "-c", "read x; echo got $x"}, Stdin: true}
	p1, err := m.Exec(ctx, "owner", "vm_t", "k1", req)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := m.Exec(ctx, "owner", "vm_t", "k1", req)
	if err != nil || p2.ID != p1.ID {
		t.Fatalf("replay: %+v %v (want %s)", p2, err, p1.ID)
	}
	other := req
	other.Argv = []string{"true"}
	if _, err := m.Exec(ctx, "owner", "vm_t", "k1", other); !domain.IsCode(err, domain.CodeIdempotencyConflict) {
		t.Fatalf("key reuse with another body: %v", err)
	}
	if _, err := m.Exec(ctx, "intruder", "vm_t", "k2", req); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("other principal: %v", err)
	}

	// The first stream owns stdin.
	out, final := collect(t, m, ctx, p1.ID, 0, func(st guestexec.Stream) {
		if st.Stdin == nil {
			t.Error("first stream did not get stdin")
			return
		}
		st.Stdin.Write([]byte("ping\n"))
		st.Stdin.Close()
	})
	if out != "got ping\n" || final == nil || final.Status != api.ProcessExited || *final.ExitCode != 0 {
		t.Fatalf("out %q final %+v", out, final)
	}
	list, _, err := m.List(ctx, "owner", "vm_t", 0, "")
	if err != nil || len(list) != 1 || list[0].ID != p1.ID {
		t.Fatalf("list %+v %v", list, err)
	}
}

func TestNonDetachedKilledWhenStreamLeaves(t *testing.T) {
	m, _, _ := setup(t)
	ctx := ctxT(t)
	pidOf := func(p api.Process) int {
		t.Helper()
		var buf strings.Builder
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()
		// Read the pid the process printed, then drop the stream.
		_ = m.Attach(sctx, "owner", "vm_t", p.ID, 0, func(msg api.ProcessStreamMessage) error {
			b, _ := base64.StdEncoding.DecodeString(msg.Data)
			buf.Write(b)
			if strings.Contains(buf.String(), "\n") {
				cancel()
			}
			return nil
		}, nil)
		var pid int
		for _, c := range strings.TrimSpace(buf.String()) {
			pid = pid*10 + int(c-'0')
		}
		return pid
	}
	waitDead := func(pid int) bool {
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if !alive(pid) {
				return true
			}
		}
		return false
	}

	fg, err := m.Exec(ctx, "owner", "vm_t", "fg", api.ExecRequest{Argv: []string{"sh", "-c", "echo $$; exec sleep 30"}})
	if err != nil {
		t.Fatal(err)
	}
	if pid := pidOf(fg); pid == 0 || !waitDead(pid) {
		t.Fatalf("non-detached process %d survived its stream", pid)
	}

	bg, err := m.Exec(ctx, "owner", "vm_t", "bg", api.ExecRequest{Argv: []string{"sh", "-c", "echo $$; exec sleep 30"}, Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	pid := pidOf(bg)
	time.Sleep(400 * time.Millisecond) // past AttachGrace too
	if !alive(pid) {
		t.Fatal("detached process died with its stream")
	}
	if p, err := m.Signal(ctx, "owner", "vm_t", bg.ID, "SIGKILL"); err != nil || p.ID != bg.ID {
		t.Fatalf("signal: %+v %v", p, err)
	}
	if !waitDead(pid) {
		t.Fatal("SIGKILL did not reach the process")
	}

	// Never attached: killed after AttachGrace.
	orphan, err := m.Exec(ctx, "owner", "vm_t", "orphan", api.ExecRequest{Argv: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	var st api.Process
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if st, err = m.Get(ctx, "owner", "vm_t", orphan.ID); err != nil || st.Status != api.ProcessRunning {
			break
		}
	}
	if st.Status != api.ProcessKilled || st.Signal != "KILL" {
		t.Fatalf("unattached process: %+v %v", st, err)
	}
}

func TestNewGenerationMarksProcessesLost(t *testing.T) {
	m, g, secrets := setup(t)
	ctx := ctxT(t)
	// Short-lived: once lost it can no longer be signalled through the API.
	p, err := m.Exec(ctx, "owner", "vm_t", "k", api.ExecRequest{Argv: []string{"sleep", "2"}, Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	g.boot(t, secrets, runtime.Key{VMID: "vm_t", Generation: 2})
	got, err := m.Get(ctx, "owner", "vm_t", p.ID)
	if err != nil || got.Status != api.ProcessLost || got.ExitedAt == nil {
		t.Fatalf("after restore: %+v %v", got, err)
	}
	if _, err := m.Signal(ctx, "owner", "vm_t", p.ID, "TERM"); !domain.IsCode(err, domain.CodeInvalidState) {
		t.Fatalf("signal lost process: %v", err)
	}
	if _, err := m.Signal(ctx, "owner", "vm_t", "proc_x", "SEGV"); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("bad signal: %v", err)
	}
}

// TestRecordsSurviveDaemonRestart restarts the manager (a new daemon run)
// over the same database while the guest agent keeps running, as a VM does.
func TestRecordsSurviveDaemonRestart(t *testing.T) {
	a, err := guestagent.New(guestagent.Config{Root: t.TempDir(), RunDir: t.TempDir(), System: nopSystem{}, SSHUnit: "-"})
	if err != nil {
		t.Fatal(err)
	}
	g := &guest{agent: a}
	secrets := &guestclient.MemorySecrets{}
	g.boot(t, secrets, runtime.Key{VMID: "vm_t", Generation: 1})
	dbPath := filepath.Join(t.TempDir(), "s.db")
	db := openDB(t, dbPath)
	newManager := func() *guestexec.Manager {
		return guestexec.New(guestexec.Config{Targets: g, Secrets: secrets, Store: guestexec.DBStore{DB: db}})
	}
	ctx := ctxT(t)

	m1 := newManager()
	bgReq := api.ExecRequest{Argv: []string{"sh", "-c", "echo a; sleep 0.3; echo b"}, Detached: true}
	bg, err := m1.Exec(ctx, "owner", "vm_t", "bg-key", bgReq)
	if err != nil {
		t.Fatal(err)
	}
	fg, err := m1.Exec(ctx, "owner", "vm_t", "fg-key", api.ExecRequest{Argv: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	m1.Close() // the daemon goes away; its agent session closes

	m2 := newManager()
	defer m2.Close()
	got, err := m2.Get(ctx, "owner", "vm_t", bg.ID)
	if err != nil || !slices.Equal(got.Argv, bgReq.Argv) || !got.CreatedAt.Equal(bg.CreatedAt) || !got.Detached {
		t.Fatalf("detached after restart: %+v %v", got, err)
	}
	out, final := collect(t, m2, ctx, bg.ID, 0, nil)
	if out != "a\nb\n" || final == nil || final.Status != api.ProcessExited || *final.ExitCode != 0 {
		t.Fatalf("attach after restart: %q %+v", out, final)
	}
	// Idempotency survives too.
	if again, err := m2.Exec(ctx, "owner", "vm_t", "bg-key", bgReq); err != nil || again.ID != bg.ID {
		t.Fatalf("replay after restart: %+v %v", again, err)
	}
	if _, err := m2.Exec(ctx, "owner", "vm_t", "bg-key", api.ExecRequest{Argv: []string{"true"}}); !domain.IsCode(err, domain.CodeIdempotencyConflict) {
		t.Fatalf("conflict after restart: %v", err)
	}
	// The non-detached process died with the old session; its record says so.
	var st api.Process
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if st, err = m2.Get(ctx, "owner", "vm_t", fg.ID); err != nil || st.Status != api.ProcessRunning {
			break
		}
	}
	if st.Status != api.ProcessKilled || !slices.Equal(st.Argv, []string{"sleep", "30"}) {
		t.Fatalf("non-detached after restart: %+v %v", st, err)
	}
	list, _, err := m2.List(ctx, "owner", "vm_t", 0, "")
	if err != nil || len(list) != 2 || list[0].ID != bg.ID || list[1].ID != fg.ID {
		t.Fatalf("list after restart: %+v %v", list, err)
	}
	if _, err := m2.Get(ctx, "intruder", "vm_t", bg.ID); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("other principal: %v", err)
	}
}

// TestExecCrashNeverRunsTwice kills the daemon (abandons its manager) mid
// execProcess, then retries the same Idempotency-Key on a new manager over
// the same database: the command must run at most once. The reservation's
// ID was the Exec's job handle, so the new daemon asks the agent: a crash
// before the agent saw the request settles as lost, and a crash after it
// started reattaches to the running process (status, output, idempotent
// replay) instead of marking it lost.
func TestExecCrashNeverRunsTwice(t *testing.T) {
	for _, tc := range []struct {
		stage string
		runs  int
	}{{"reserved", 0}, {"started", 1}} {
		t.Run(tc.stage, func(t *testing.T) {
			a, err := guestagent.New(guestagent.Config{Root: t.TempDir(), RunDir: t.TempDir(), System: nopSystem{}, SSHUnit: "-"})
			if err != nil {
				t.Fatal(err)
			}
			g := &guest{agent: a}
			secrets := &guestclient.MemorySecrets{}
			g.boot(t, secrets, runtime.Key{VMID: "vm_t", Generation: 1})
			db := openDB(t, filepath.Join(t.TempDir(), "s.db"))
			newManager := func() *guestexec.Manager {
				return guestexec.New(guestexec.Config{Targets: g, Secrets: secrets, Store: guestexec.DBStore{DB: db}})
			}
			ctx := ctxT(t)
			// The guest's own record of every start: one line per run.
			runs := filepath.Join(t.TempDir(), "runs")
			count := func() int {
				b, _ := os.ReadFile(runs)
				return strings.Count(string(b), "\n")
			}
			req := api.ExecRequest{Argv: []string{"sh", "-c", "echo x >> " + runs + "; echo hello; sleep 0.3"}, Detached: true}

			m1 := newManager()
			guestexec.SetCrash(m1, func(stage string) bool { return stage == tc.stage })
			if _, err := m1.Exec(ctx, "owner", "vm_t", "k", req); err == nil {
				t.Fatal("exec survived its crash")
			}
			m1.Close()

			m2 := newManager()
			defer m2.Close()
			// A restart settles the reservation on first sight (here, a list).
			list, _, err := m2.List(ctx, "owner", "vm_t", 0, "")
			if err != nil || len(list) != 1 {
				t.Fatalf("list after crash: %+v %v", list, err)
			}
			id := list[0].ID
			switch tc.stage {
			case "reserved":
				if list[0].Status != api.ProcessLost || list[0].ExitedAt == nil {
					t.Fatalf("never started: %+v", list[0])
				}
				if _, err := m2.Signal(ctx, "owner", "vm_t", id, "KILL"); !domain.IsCode(err, domain.CodeInvalidState) {
					t.Fatalf("signal lost reservation: %v", err)
				}
			case "started":
				if strings.HasPrefix(id, "pstart_") || list[0].Status == api.ProcessLost || list[0].Status == api.ProcessStarting {
					t.Fatalf("not reattached: %+v", list[0])
				}
				out, final := collect(t, m2, ctx, id, 0, nil)
				if out != "hello\n" || final == nil || final.Status != api.ProcessExited || *final.ExitCode != 0 {
					t.Fatalf("attach to reattached process: %q %+v", out, final)
				}
			}
			for i := range 2 {
				p, err := m2.Exec(ctx, "owner", "vm_t", "k", req)
				if err != nil || p.ID != id || !slices.Equal(p.Argv, req.Argv) {
					t.Fatalf("retry %d: %+v %v (want %s)", i, p, err, id)
				}
			}
			if _, err := m2.Exec(ctx, "owner", "vm_t", "k", api.ExecRequest{Argv: []string{"true"}}); !domain.IsCode(err, domain.CodeIdempotencyConflict) {
				t.Fatalf("other body: %v", err)
			}
			for end := time.Now().Add(time.Minute); count() < tc.runs && time.Now().Before(end); {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond) // a second run would land by now
			if n := count(); n != tc.runs {
				t.Fatalf("guest ran the command %d times, want %d", n, tc.runs)
			}
		})
	}
}

func TestListProcessesPages(t *testing.T) {
	m, _, _ := setup(t)
	ctx := ctxT(t)
	var want []string
	for _, k := range []string{"a", "b", "c"} {
		p, err := m.Exec(ctx, "owner", "vm_t", k, api.ExecRequest{Argv: []string{"true"}, Detached: true})
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, p.ID)
	}
	var got []string
	cursor := ""
	for range 4 {
		page, next, err := m.List(ctx, "owner", "vm_t", 1, cursor)
		if err != nil || len(page) != 1 {
			t.Fatalf("page %d: %+v %v", len(got), page, err)
		}
		got = append(got, page[0].ID)
		if cursor = next; cursor == "" {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("pages %v, want %v (oldest first)", got, want)
	}
	if _, _, err := m.List(ctx, "owner", "vm_t", 1, "bogus"); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("bad cursor: %v", err)
	}
	// A start the agent refuses leaves no reservation behind.
	for range 2 {
		if _, err := m.Exec(ctx, "owner", "vm_t", "missing", api.ExecRequest{Argv: []string{"/nonexistent"}}); !domain.IsCode(err, domain.CodeNotFound) {
			t.Fatalf("refused start: %v", err)
		}
	}
	if all, _, err := m.List(ctx, "owner", "vm_t", 0, ""); err != nil || len(all) != 3 {
		t.Fatalf("after refused start: %+v %v", all, err)
	}
}

// TestBusyUntilDetachedExit: a detached process keeps its VM busy (no
// reclaim or idle stop) while it runs, and its exit clears that without
// anyone asking for its status: the output collector records it.
func TestBusyUntilDetachedExit(t *testing.T) {
	m, _, _ := setup(t)
	ctx := ctxT(t)
	if m.Busy("vm_t", 1) {
		t.Fatal("busy before any process")
	}
	if _, err := m.Exec(ctx, "owner", "vm_t", "bg", api.ExecRequest{Argv: []string{"sleep", "1"}, Detached: true}); err != nil {
		t.Fatal(err)
	}
	if !m.Busy("vm_t", 1) {
		t.Fatal("not busy while a detached process runs")
	}
	if m.Busy("vm_t", 2) {
		t.Fatal("busy for another generation")
	}
	for deadline := time.Now().Add(10 * time.Second); m.Busy("vm_t", 1); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("still busy after the detached process exited")
		}
	}
}
