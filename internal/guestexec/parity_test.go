//go:build linux

package guestexec_test

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/guestagent"
	"davit/internal/guestclient"
	"davit/internal/guestexec"
	"davit/internal/guestproto"
	"davit/internal/runtime"
	"davit/internal/state"
)

func readOutput(t *testing.T, db *state.DB, id string) []state.OutputChunk {
	t.Helper()
	var out []state.OutputChunk
	if err := db.Tx(context.Background(), func(tx *state.Tx) (err error) {
		out, err = tx.ReadOutput(id, 0, 100)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestOutputRetainedAcrossDaemonRestart: a managed process's output lands
// in the store with its guest seqs; after a daemon restart the collector
// resumes after the last stored chunk, with nothing duplicated or skipped.
func TestOutputRetainedAcrossDaemonRestart(t *testing.T) {
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
	m1 := newManager()
	p, err := m1.Exec(ctx, "owner", "vm_t", "k", api.ExecRequest{Argv: []string{"sh", "-c", "echo a; sleep 0.6; echo b >&2"}, Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for end := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s: %+v", what, readOutput(t, db, p.ID))
			}
		}
	}
	waitFor("first chunk", func() bool { return len(readOutput(t, db, p.ID)) == 1 })
	m1.Close() // the daemon goes away before the second chunk

	m2 := newManager()
	defer m2.Close()
	if _, err := m2.Get(ctx, "owner", "vm_t", p.ID); err != nil { // first sight resumes collection
		t.Fatal(err)
	}
	waitFor("second chunk", func() bool { return len(readOutput(t, db, p.ID)) >= 2 })
	got := readOutput(t, db, p.ID)
	want := []state.OutputChunk{{Seq: 1, Stream: api.StreamStdout, Data: []byte("a\n")}, {Seq: 2, Stream: api.StreamStderr, Data: []byte("b\n")}}
	if !slices.EqualFunc(got, want, func(x, y state.OutputChunk) bool {
		return x.Seq == y.Seq && x.Stream == y.Stream && string(x.Data) == string(y.Data)
	}) {
		t.Fatalf("retained output %+v", got)
	}
}

// TestGuestProcessesMapsManagedJobs: a whole-guest sample names managed
// jobs by their API process ID and is kept for a stopped VM.
func TestGuestProcessesMapsManagedJobs(t *testing.T) {
	m, _, _ := setup(t)
	ctx := ctxT(t)
	p, err := m.Exec(ctx, "owner", "vm_t", "k", api.ExecRequest{Argv: []string{"sleep", "30"}, Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = m.Signal(context.Background(), "owner", "vm_t", p.ID, "KILL") }()
	l, err := m.GuestProcesses(ctx, "owner", "vm_t")
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(l.Items, func(g api.GuestProcess) bool { return g.ProcessID == p.ID })
	if i < 0 || !slices.Equal(l.Items[i].Args, []string{"sleep", "30"}) || l.SampledAt.IsZero() || l.Items[i].StartedAt.IsZero() {
		t.Fatalf("managed job not in sample: %d %+v", i, l)
	}
	if _, err := m.GuestProcesses(ctx, "intruder", "vm_t"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("other principal: %v", err)
	}
}

// TestListenGuest: the manager's reverse-connection listener reaches the
// host through the agent and refuses a stale generation.
func TestListenGuest(t *testing.T) {
	m, g, _ := setup(t)
	ctx := ctxT(t)
	tg, err := g.GuestTarget(ctx, "owner", "vm_t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ListenGuest(ctx, tg, 2, 0); !domain.IsCode(err, domain.CodeInvalidState) {
		t.Fatalf("stale generation: %v", err)
	}
	l, err := m.ListenGuest(ctx, tg, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var _ net.Listener = l
	gc, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(l.Port())), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()
	if _, err := gc.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	hc, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	_ = hc.SetDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 2)
	if _, err := io.ReadFull(hc, b); err != nil || string(b) != "hi" {
		t.Fatalf("host read %q %v", b, err)
	}
}

// TestPrepareWorkspaceCancelFreesAgent: abandoning a preparation kills its
// running step (its session closes), so the next one is not refused as
// already running.
func TestPrepareWorkspaceCancelFreesAgent(t *testing.T) {
	m, _, _ := setup(t)
	ctx := ctxT(t)
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	slow := guestproto.WorkspacePrepare{Revision: "env_a@1", Setup: []guestproto.SetupStep{{Name: "slow", Argv: []string{"sleep", "30"}}}}
	if _, err := m.PrepareWorkspace(short, "owner", "vm_t", slow, nil); err == nil {
		t.Fatal("abandoned preparation reported a result")
	}
	var res guestproto.WorkspaceResult
	var err error
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		res, err = m.PrepareWorkspace(ctx, "owner", "vm_t", guestproto.WorkspacePrepare{Revision: "env_a@2",
			Setup: []guestproto.SetupStep{{Name: "ok", Argv: []string{"true"}}}}, nil)
		if err == nil {
			break
		}
	}
	if err != nil || res.State != guestproto.WorkspaceReady || res.Revision != "env_a@2" {
		t.Fatalf("next preparation: %+v %v", res, err)
	}
}

// TestOutputRetentionKeepsNewest: past the bound the oldest output is
// dropped (and the drop recorded), so the tail agents care about survives.
func TestOutputRetentionKeepsNewest(t *testing.T) {
	a, err := guestagent.New(guestagent.Config{Root: t.TempDir(), RunDir: t.TempDir(), System: nopSystem{}, SSHUnit: "-"})
	if err != nil {
		t.Fatal(err)
	}
	g := &guest{agent: a}
	secrets := &guestclient.MemorySecrets{}
	g.boot(t, secrets, runtime.Key{VMID: "vm_t", Generation: 1})
	db := openDB(t, filepath.Join(t.TempDir(), "s.db"))
	m := guestexec.New(guestexec.Config{Targets: g, Secrets: secrets, Store: guestexec.DBStore{DB: db}, RetainOutput: 8})
	defer m.Close()
	ctx := ctxT(t)
	p, err := m.Exec(ctx, "owner", "vm_t", "k", api.ExecRequest{Argv: []string{"sh", "-c", "echo 0123456789; sleep 0.2; echo tail"}, Detached: true})
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		got := readOutput(t, db, p.ID)
		if len(got) == 1 && string(got[0].Data) == "tail\n" {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("retained %+v, want only the newest chunk", got)
		}
	}
	var dropped bool
	if err := db.Tx(ctx, func(tx *state.Tx) (err error) { st, err := tx.OutputStats(p.ID); dropped = st.Dropped; return err }); err != nil || !dropped {
		t.Fatalf("drop not recorded: %v %v", dropped, err)
	}
}
