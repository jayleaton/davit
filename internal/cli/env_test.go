package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
	"davit/internal/guestproto"
	"davit/internal/lifecycle"
	"davit/internal/runtime"
	"davit/internal/runtime/fake"
)

type readyPreparer struct{}

func (readyPreparer) PrepareWorkspace(_ context.Context, _ domain.PrincipalID, _ string, req guestproto.WorkspacePrepare, _ func(guestproto.WorkspaceProgress)) (guestproto.WorkspaceResult, error) {
	return guestproto.WorkspaceResult{Revision: req.Revision, State: guestproto.WorkspaceReady}, nil
}

// TestEnvCommands: env create/list/show/revise/revisions/delete, and new
// --environment pins a VM whose workspace info reports once prepared.
func TestEnvCommands(t *testing.T) {
	dir := t.TempDir()
	serveWith(t, func(c *lifecycle.Config) {
		c.Runtime, _ = fake.New(fake.Options{Dir: filepath.Join(dir, "rt"),
			GuestTransport: func(context.Context, runtime.Handle) (runtime.Dialer, error) { return nil, nil }})
		c.Workspace = readyPreparer{}
	})
	m1 := filepath.Join(dir, "m1.json")
	os.WriteFile(m1, []byte(`{"repos":[{"url":"https://example.com/w.git","path":"/work/w"}],"setup":[{"name":"deps","argv":["make"]}]}`), 0o600)
	m2 := filepath.Join(dir, "m2.json")
	os.WriteFile(m2, []byte(`{"variables":{"X":"1"}}`), 0o600)

	if r := runCLI(t, false, "env", "create", "web", "--file", m1); r.code != 0 || r.data["latestRevision"] != float64(1) {
		t.Fatalf("create: exit %d %+v", r.code, r.data)
	}
	if r := runCLI(t, false, "env", "revise", "web", "--file", m2, "--if-revision", "5"); r.code != ExitConflict {
		t.Fatalf("stale --if-revision: exit %d", r.code)
	}
	if r := runCLI(t, false, "env", "revise", "web", "--file", m2, "--if-revision", "1"); r.code != 0 || r.data["revision"] != float64(2) {
		t.Fatalf("revise: exit %d %+v", r.code, r.data)
	}
	if r := runCLI(t, false, "env", "show", "web@1"); r.code != 0 || r.data["revision"] != float64(1) {
		t.Fatalf("show @1: exit %d %+v", r.code, r.data)
	}
	if r := runCLI(t, false, "env", "revisions", "web"); r.code != 0 || r.data["count"] != float64(2) {
		t.Fatalf("revisions: exit %d %+v", r.code, r.data)
	}
	if r := runCLI(t, false, "env", "create", "bad", "--file", filepath.Join(dir, "missing.json")); r.code != ExitUsage {
		t.Fatalf("missing manifest: exit %d", r.code)
	}

	vm := waitedVM(t, "new", "box", "--environment", "web@1")
	if vm.Environment == nil || vm.Environment.Revision != 1 {
		t.Fatalf("pin: %+v", vm.Environment)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var stdout bytes.Buffer
		Main([]string{"info", "box"}, Stdio{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &bytes.Buffer{}})
		if strings.Contains(stdout.String(), "workspace  ready (web@1)") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("info never showed a ready workspace:\n%s", stdout.String())
		}
	}
	if r := runCLI(t, false, "new", "x", "--environment", "web@0"); r.code != ExitUsage {
		t.Fatalf("bad --environment: exit %d", r.code)
	}
	if r := runCLI(t, false, "env", "delete", "web", "--yes"); r.code != 0 {
		t.Fatalf("delete: exit %d", r.code)
	}
	if r := runCLI(t, false, "env", "show", "web"); r.code != ExitNotFound || r.last.Error.Code != domain.CodeNotFound {
		t.Fatalf("show after delete: exit %d", r.code)
	}
}
