package main_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pb33f/libopenapi"
	"golang.org/x/crypto/ssh"

	"davit/api"
	"davit/internal/domain"
)

// mutatingOp is one implemented non-GET operation of api/openapi.yaml.
type mutatingOp struct {
	id, method, path string
	takesKey         bool
}

func mutatingOps(t *testing.T) []mutatingOp {
	t.Helper()
	doc, err := libopenapi.NewDocument(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	m, err := doc.BuildV3Model()
	if err != nil {
		t.Fatal(err)
	}
	var ops []mutatingOp
	for path, item := range m.Model.Paths.PathItems.FromOldest() {
		for method, op := range item.GetOperations().FromOldest() {
			impl, ok := op.Extensions.Get("x-davit-implemented")
			if strings.EqualFold(method, "get") || !ok || impl.Value != "true" {
				continue
			}
			o := mutatingOp{id: op.OperationId, method: strings.ToUpper(method), path: path}
			for _, p := range slices.Concat(item.Parameters, op.Parameters) {
				o.takesKey = o.takesKey || p.Name == api.HeaderIdempotencyKey
			}
			ops = append(ops, o)
		}
	}
	return ops
}

type apiConn struct {
	t *testing.T
	c *http.Client
}

func (a apiConn) call(method, path string, body any, key string) (int, []byte) {
	a.t.Helper()
	var rd io.Reader
	ctype := "application/json"
	switch b := body.(type) {
	case nil:
	case []byte:
		rd, ctype = bytes.NewReader(b), "application/octet-stream"
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, "http://local"+api.BasePath+path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", ctype)
	}
	if key != "" {
		req.Header.Set(api.HeaderIdempotencyKey, key)
	}
	resp, err := a.c.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// must calls and decodes a 2xx JSON response into out.
func (a apiConn) must(method, path string, body any, key string, out any) {
	a.t.Helper()
	st, b := a.call(method, path, body, key)
	if st/100 != 2 {
		a.t.Fatalf("%s %s: HTTP %d %s", method, path, st, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			a.t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}

func (a apiConn) waitOp(id string) api.Operation {
	a.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		var page api.OperationEventList
		a.must("GET", "/operations/"+id+"/events?waitMs=2000&after=0", nil, "", &page)
		if page.Operation.State.Terminal() {
			return page.Operation
		}
	}
	a.t.Fatalf("operation %s did not finish", id)
	return api.Operation{}
}

// twice sends the same request twice with the same key and returns both
// decoded bodies, requiring the same status each time.
func twice[T any](a apiConn, method, path string, body any, key string) (T, T) {
	a.t.Helper()
	var r1, r2 T
	s1, b1 := a.call(method, path, body, key)
	s2, b2 := a.call(method, path, body, key)
	if s1/100 != 2 || s1 != s2 {
		a.t.Fatalf("%s %s replay: HTTP %d then %d: %s / %s", method, path, s1, s2, b1, b2)
	}
	json.Unmarshal(b1, &r1)
	json.Unmarshal(b2, &r2)
	return r1, r2
}

// TestEveryMutationIsIdempotent runs every implemented mutating endpoint of
// api/openapi.yaml twice: with the same Idempotency-Key where the operation
// declares one (one effect, the same result), and plainly where it is
// naturally idempotent (no second effect). A new mutating operation fails
// this test until it gets a case here.
func TestEveryMutationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	repoRoot, _ := filepath.Abs("../..")
	cfg := fmt.Sprintf(`{"dataDir":%q,"socket":%q,"runtime":"fake","logLevel":"warn","fake":{"guestAgent":true},
		"harness":{"commands":{"claude":%q}}}`, filepath.Join(dir, "data"), sock,
		filepath.Join(repoRoot, "internal", "harness", "testdata", "fake-claude"))
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d := exec.Command(bin, "daemon", "--config", cfgPath)
	d.Env = append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+dir, "XDG_DATA_HOME="+dir, "DAVIT_TS_AUTHKEY=")
	var log bytes.Buffer
	d.Stdout, d.Stderr = &log, &log
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Process.Kill()
		d.Wait()
		if t.Failed() {
			t.Logf("daemon log:\n%s", log.String())
		}
	})
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not start:\n%s", log.String())
		}
	}
	a := apiConn{t: t, c: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dl net.Dialer
		return dl.DialContext(ctx, "unix", sock)
	}}}}

	countVMs := func(name string) int {
		var l api.VMList
		a.must("GET", "/vms?all=true&limit=200", nil, "", &l)
		n := 0
		for _, vm := range l.Items {
			if name == "" || vm.Name == name {
				n++
			}
		}
		return n
	}
	vmPath := func(name string) string { return "/vms/" + url.PathEscape(name) }
	sameOp := func(id string, r1, r2 api.OperationAccepted) api.Operation {
		t.Helper()
		if r1.Operation.ID == "" || r1.Operation.ID != r2.Operation.ID {
			t.Fatalf("%s: replay created operation %s, first was %s", id, r2.Operation.ID, r1.Operation.ID)
		}
		op := a.waitOp(string(r1.Operation.ID))
		if op.State != "succeeded" {
			t.Fatalf("%s: %s %+v", id, op.State, op.Error)
		}
		return op
	}

	keyed := map[string]func(){}
	natural := map[string]func(){}

	keyed["createVM"] = func() {
		r1, r2 := twice[api.OperationAccepted](a, "POST", "/vms", api.CreateVMRequest{Name: "idem"}, "k-create")
		sameOp("createVM", r1, r2)
		if n := countVMs("idem"); n != 1 {
			t.Fatalf("createVM replay made %d VMs", n)
		}
	}
	keyed["createVMs"] = func() {
		body := api.CreateVMsRequest{Count: 2, Template: api.CreateVMRequest{Name: "idem-batch"}}
		r1, r2 := twice[api.CreateVMsResult](a, "POST", "/vms/batch", body, "k-batch")
		if len(r1.Items) != 2 || len(r2.Items) != 2 {
			t.Fatalf("createVMs: %+v / %+v", r1, r2)
		}
		for i := range r1.Items {
			if r1.Items[i].Accepted == nil || r2.Items[i].Accepted == nil {
				t.Fatalf("createVMs item %d refused: %+v / %+v", i+1, r1.Items[i].Error, r2.Items[i].Error)
			}
			sameOp("createVMs", *r1.Items[i].Accepted, *r2.Items[i].Accepted)
			if n := countVMs(r1.Items[i].Name); n != 1 {
				t.Fatalf("createVMs replay made %d VMs named %s", n, r1.Items[i].Name)
			}
		}
		// Later cases count VMs: remove the batch again.
		for _, it := range r1.Items {
			var acc api.OperationAccepted
			a.must("DELETE", vmPath(it.Name), nil, "k-batch-delete-"+it.Name, &acc)
			a.waitOp(string(acc.Operation.ID))
		}
	}
	natural["reclaimVMs"] = func() {
		// No key: each stop is keyed by the VM's generation and revision, so
		// a repeat stops nothing twice.
		var acc api.OperationAccepted
		a.must("POST", "/vms", api.CreateVMRequest{Name: "idem-gc", Labels: map[string]string{"idem": "gc"}}, "k-gc-create", &acc)
		a.waitOp(string(acc.Operation.ID))
		body := api.ReclaimVMsRequest{IdleSeconds: 0, Labels: map[string]string{"idem": "gc"}}
		var r1, r2 api.ReclaimVMsResult
		a.must("POST", "/vms/reclaim", body, "", &r1)
		a.must("POST", "/vms/reclaim", body, "", &r2)
		if len(r1.Items) != 1 || r1.Items[0].OperationID == "" || len(r2.Items) != 0 {
			t.Fatalf("reclaimVMs twice: %+v / %+v", r1, r2)
		}
		if op := a.waitOp(string(r1.Items[0].OperationID)); op.State != domain.OpSucceeded || op.Reason != "gc" {
			t.Fatalf("reclaimVMs stop: %+v", op)
		}
		a.must("DELETE", vmPath("idem-gc"), nil, "k-gc-delete", &acc)
		a.waitOp(string(acc.Operation.ID))
	}
	natural["pruneImages"] = func() {
		// No key: a repeat finds nothing left to remove.
		var r1, r2 api.PruneImagesResult
		a.must("POST", "/images/prune", api.PruneImagesRequest{}, "", &r1)
		a.must("POST", "/images/prune", api.PruneImagesRequest{}, "", &r2)
		if len(r2.Removed) != 0 {
			t.Fatalf("pruneImages twice: %+v / %+v", r1, r2)
		}
	}
	keyed["stopVM"] = func() {
		r1, r2 := twice[api.OperationAccepted](a, "POST", vmPath("idem")+"/stop", api.StopVMRequest{}, "k-stop")
		sameOp("stopVM", r1, r2)
		var l api.SnapshotList
		a.must("GET", vmPath("idem")+"/snapshots", nil, "", &l)
		if len(l.Items) != 1 {
			t.Fatalf("stopVM replay committed %d snapshots", len(l.Items))
		}
	}
	keyed["snapshotVM"] = func() {
		r1, r2 := twice[api.OperationAccepted](a, "POST", vmPath("idem")+"/snapshots", api.SnapshotVMRequest{Name: "idem-snap"}, "k-snap")
		sameOp("snapshotVM", r1, r2)
		var l api.SnapshotList
		a.must("GET", vmPath("idem")+"/snapshots", nil, "", &l)
		if len(l.Items) != 2 {
			t.Fatalf("snapshotVM replay: %d snapshots, want automatic + named", len(l.Items))
		}
	}
	keyed["forkVM"] = func() {
		before := countVMs("")
		r1, r2 := twice[api.OperationAccepted](a, "POST", vmPath("idem")+"/fork", api.ForkVMRequest{Name: "idem-fork"}, "k-fork")
		sameOp("forkVM", r1, r2)
		if n := countVMs(""); n != before+1 {
			t.Fatalf("forkVM replay: %d new VMs", n-before)
		}
	}
	keyed["resumeVM"] = func() {
		// With a resize (feature resize): the replay applies it once.
		body := api.ResumeVMRequest{Shape: &api.ShapeOverride{CPUs: 3}}
		r1, r2 := twice[api.OperationAccepted](a, "POST", vmPath("idem")+"/resume", body, "k-resume")
		sameOp("resumeVM", r1, r2)
		var vm api.VM
		a.must("GET", vmPath("idem"), nil, "", &vm)
		if vm.InstanceGeneration != 2 || vm.Shape.CPUs != 3 {
			t.Fatalf("resumeVM replay booted generation %d with %d cpus", vm.InstanceGeneration, vm.Shape.CPUs)
		}
	}
	keyed["recoverVM"] = func() {
		r1, r2 := twice[api.OperationAccepted](a, "POST", vmPath("idem")+"/recover", api.RecoverVMRequest{}, "k-recover")
		op := sameOp("recoverVM", r1, r2)
		var vm api.VM
		a.must("GET", vmPath("idem"), nil, "", &vm)
		if vm.InstanceGeneration != 3 || op.Kind != "recover" || op.Result.BootMode != "cold_boot" {
			t.Fatalf("recoverVM replay: generation %d, %s %+v", vm.InstanceGeneration, op.Kind, op.Result)
		}
	}
	var procID string
	keyed["execProcess"] = func() {
		body := api.ExecRequest{Argv: []string{"sh", "-c", "echo once"}}
		p1, p2 := twice[api.Process](a, "POST", vmPath("idem")+"/processes", body, "k-exec")
		if p1.ID == "" || p1.ID != p2.ID {
			t.Fatalf("execProcess replay started %s, first was %s", p2.ID, p1.ID)
		}
		var l api.ProcessList
		a.must("GET", vmPath("idem")+"/processes", nil, "", &l)
		if len(l.Items) != 1 {
			t.Fatalf("execProcess replay: %d processes", len(l.Items))
		}
	}
	keyed["createPrompt"] = func() {
		body := api.PromptRequest{Text: "hello"}
		p1, p2 := twice[api.Prompt](a, "POST", vmPath("idem")+"/prompts", body, "k-prompt")
		if p1.ID == "" || p1.ID != p2.ID {
			t.Fatalf("createPrompt replay started %s, first was %s", p2.ID, p1.ID)
		}
		var done api.Prompt
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			a.must("GET", vmPath("idem")+"/prompts/"+p1.ID, nil, "", &done)
			if done.State != "starting" && done.State != "running" {
				break
			}
		}
		if p3, _ := twice[api.Prompt](a, "POST", vmPath("idem")+"/prompts", body, "k-prompt"); p3.ID != p1.ID {
			t.Fatalf("createPrompt replay after the turn finished started %s", p3.ID)
		}
		var conv api.Conversation
		a.must("GET", vmPath("idem")+"/conversations/"+p1.ConversationID, nil, "", &conv)
		if conv.Turns != 1 {
			t.Fatalf("createPrompt replay ran %d turns", conv.Turns)
		}
	}
	keyed["deleteVM"] = func() {
		r1, r2 := twice[api.OperationAccepted](a, "DELETE", vmPath("idem-fork"), nil, "k-delete")
		sameOp("deleteVM", r1, r2)
		// Replaying after the VM is gone still returns the same operation.
		_, r3 := twice[api.OperationAccepted](a, "DELETE", vmPath("idem-fork"), nil, "k-delete")
		if r3.Operation.ID != r1.Operation.ID {
			t.Fatalf("deleteVM replay after deletion: %s, want %s", r3.Operation.ID, r1.Operation.ID)
		}
	}

	natural["killProcess"] = func() {
		var p api.Process
		a.must("POST", vmPath("idem")+"/processes", api.ExecRequest{Argv: []string{"sleep", "30"}, Detached: true}, "k-sleep", &p)
		procID = p.ID
		path := vmPath("idem") + "/processes/" + procID + "?signal=KILL"
		a.must("DELETE", path, nil, "", nil)
		st, body := a.call("DELETE", path, nil, "")
		if st != http.StatusOK && st != http.StatusConflict {
			t.Fatalf("second kill: HTTP %d %s", st, body)
		}
		var got api.Process
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			a.must("GET", vmPath("idem")+"/processes/"+procID, nil, "", &got)
			if got.Status != api.ProcessRunning {
				break
			}
		}
		if got.Status == api.ProcessRunning {
			t.Fatal("process still running after kill")
		}
	}
	natural["writeFile"] = func() {
		var vm api.VM
		a.must("GET", vmPath("idem"), nil, "", &vm)
		target := filepath.Join(dir, "idem-file")
		path := fmt.Sprintf("%s/files?path=%s&generation=%d&mode=0640", vmPath("idem"), url.QueryEscape(target), vm.InstanceGeneration)
		var f1, f2 api.FileInfo
		a.must("PUT", path, []byte("same bytes"), "", &f1)
		a.must("PUT", path, []byte("same bytes"), "", &f2)
		if f1.SHA256 == "" || f1.SHA256 != f2.SHA256 || f1.Mode != f2.Mode || f1.SizeBytes != f2.SizeBytes {
			t.Fatalf("writeFile repeat differs: %+v vs %+v", f1, f2)
		}
		if b, _ := os.ReadFile(target); string(b) != "same bytes" {
			t.Fatalf("file content %q", b)
		}
	}
	natural["cancelPrompt"] = func() {
		var p api.Prompt
		a.must("POST", vmPath("idem")+"/prompts", api.PromptRequest{Text: "slow please", New: true}, "k-slow", &p)
		path := vmPath("idem") + "/prompts/" + p.ID
		a.must("DELETE", path, nil, "", nil)
		st, body := a.call("DELETE", path, nil, "")
		if st != http.StatusOK && st != http.StatusConflict {
			t.Fatalf("second cancel: HTTP %d %s", st, body)
		}
		var got api.Prompt
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			a.must("GET", path, nil, "", &got)
			if got.State != "starting" && got.State != "running" {
				break
			}
		}
		if got.State != "cancelled" {
			t.Fatalf("prompt after double cancel: %s", got.State)
		}
	}
	natural["interruptPrompts"] = func() {
		var p api.Prompt
		a.must("POST", vmPath("idem")+"/prompts", api.PromptRequest{Text: "slow please", New: true}, "k-interrupt", &p)
		var r1, r2 api.InterruptResult
		a.must("POST", vmPath("idem")+"/interrupt", api.InterruptRequest{All: true}, "", &r1)
		a.must("POST", vmPath("idem")+"/interrupt", api.InterruptRequest{All: true}, "", &r2)
		if len(r1.Cancelled) != 1 || r1.Cancelled[0].ID != p.ID || r1.Cancelled[0].State != api.PromptCancelled || len(r2.Cancelled) != 0 {
			t.Fatalf("interrupt twice: %+v / %+v", r1, r2)
		}
	}
	// steerPrompt: the same key delivers the message once, also when
	// repeated after the turn ended; other text with it is 409.
	keyed["steerPrompt"] = func() {
		var p api.Prompt
		a.must("POST", vmPath("idem")+"/prompts", api.PromptRequest{Text: "slow please", New: true}, "k-steer", &p)
		path := vmPath("idem") + "/prompts/" + p.ID
		var res api.SteerResult
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			st, body := a.call("POST", path+"/steer", api.SteerRequest{Text: "nudge"}, "k-nudge")
			if st == http.StatusOK {
				json.Unmarshal(body, &res)
				break
			}
			if st != http.StatusConflict || time.Now().After(deadline) { // still starting
				t.Fatalf("steer: HTTP %d %s", st, body)
			}
		}
		if res.Delivery != "native" || res.Prompt.ID != p.ID {
			t.Fatalf("steer: %+v", res)
		}
		var again api.SteerResult
		a.must("POST", path+"/steer", api.SteerRequest{Text: "nudge"}, "k-nudge", &again)
		if again.Delivery != res.Delivery || again.Prompt.ID != p.ID {
			t.Fatalf("steer replay: %+v", again)
		}
		if st, _ := a.call("POST", path+"/steer", api.SteerRequest{Text: "other"}, "k-nudge"); st != http.StatusConflict {
			t.Fatalf("steer with a reused key and other text: HTTP %d, want 409", st)
		}
		var got api.Prompt
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			a.must("GET", path, nil, "", &got)
			if got.State != "starting" && got.State != "running" {
				break
			}
		}
		var events api.PromptEventList
		a.must("GET", path+"/events", nil, "", &events)
		deltas := 0
		for _, e := range events.Items {
			if e.Type == api.PromptEventMessageDelta {
				deltas++
			}
		}
		if got.State != "succeeded" || deltas != 2 {
			t.Fatalf("steered turn %s with %d messages, want 2 (prompt + one steer)", got.State, deltas)
		}
		if st, body := a.call("POST", path+"/steer", api.SteerRequest{Text: "nudge"}, "k-nudge"); st != http.StatusOK {
			t.Fatalf("keyed steer replay after the turn: HTTP %d %s", st, body)
		}
		if st, _ := a.call("POST", path+"/steer", api.SteerRequest{Text: "nudge"}, ""); st != http.StatusConflict {
			t.Fatalf("unkeyed steer after the turn: HTTP %d, want 409", st)
		}
	}
	// A guest TCP service for the tunnel cases: the fake guest's loopback is
	// the host's.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	echoPort := echo.Addr().(*net.TCPAddr).Port
	keyed["openTunnel"] = func() {
		r1, r2 := twice[api.Tunnel](a, "POST", vmPath("idem")+"/tunnels", api.OpenTunnelRequest{Port: echoPort}, "k-tunnel")
		if r1.ID == "" || r1.ID != r2.ID {
			t.Fatalf("openTunnel replay opened tunnel %q, first was %q", r2.ID, r1.ID)
		}
		if st, _ := a.call("POST", vmPath("idem")+"/tunnels", api.OpenTunnelRequest{Port: echoPort + 1}, "k-tunnel"); st != http.StatusConflict {
			t.Fatalf("openTunnel with a reused key and another port: HTTP %d, want 409", st)
		}
		a.must("DELETE", "/tunnels/"+r1.ID, nil, "", nil)
	}
	natural["closeTunnel"] = func() {
		var tn api.Tunnel
		a.must("POST", vmPath("idem")+"/tunnels", api.OpenTunnelRequest{Port: echoPort}, "", &tn)
		a.must("DELETE", "/tunnels/"+tn.ID, nil, "", nil)
		if st, _ := a.call("DELETE", "/tunnels/"+tn.ID, nil, ""); st != http.StatusNotFound {
			t.Fatalf("second tunnel close: HTTP %d, want 404", st)
		}
	}
	natural["issueSSHCredential"] = func() {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		pub, _ := ssh.NewPublicKey(priv.Public())
		req := api.IssueSSHCredentialRequest{PublicKey: string(ssh.MarshalAuthorizedKey(pub))}
		var c1, c2 api.SSHCredential
		a.must("POST", vmPath("idem")+"/ssh-credentials", req, "", &c1)
		a.must("POST", vmPath("idem")+"/ssh-credentials", req, "", &c2)
		// No server state: each call signs afresh, for the same VM
		// generation and the same pinned host key.
		if c1.Certificate == "" || c1.Certificate == c2.Certificate || c1.Generation != c2.Generation ||
			len(c1.HostKeys) == 0 || !slices.Equal(c1.HostKeys, c2.HostKeys) {
			t.Fatalf("issueSSHCredential twice: %+v / %+v", c1, c2)
		}
	}
	natural["updateVM"] = func() {
		body := api.UpdateVMRequest{Name: ptrTo("idem-renamed"), TTLSeconds: ptrTo(int64(7200))}
		var v1, v2 api.VM
		a.must("PATCH", vmPath("idem"), body, "", &v1)
		a.must("PATCH", vmPath("idem-renamed"), body, "", &v2)
		if v1.Name != "idem-renamed" || v2.Name != v1.Name || v2.TTLSeconds != 7200 || v2.StateRevision != v1.StateRevision+1 {
			t.Fatalf("updateVM twice: %+v / %+v", v1, v2)
		}
		// Rename back so later cases find the VM by its original name.
		a.must("PATCH", vmPath("idem-renamed"), api.UpdateVMRequest{Name: ptrTo("idem")}, "", nil)

		// The same egress policy twice: recorded once, normalized.
		eg := api.UpdateVMRequest{Egress: &api.Egress{Mode: "allowlist", Allow: []string{"GitHub.com", "10.9.0.0/16", "github.com"}}}
		a.must("PATCH", vmPath("idem"), eg, "", &v1)
		a.must("PATCH", vmPath("idem"), eg, "", &v2)
		want := []string{"10.9.0.0/16", "github.com"}
		if v1.Egress.Mode != "allowlist" || !slices.Equal(v1.Egress.Allow, want) || !slices.Equal(v2.Egress.Allow, want) ||
			v2.StateRevision != v1.StateRevision {
			t.Fatalf("updateVM egress twice: %+v (rev %d) / %+v (rev %d)", v1.Egress, v1.StateRevision, v2.Egress, v2.StateRevision)
		}
		a.must("PATCH", vmPath("idem"), api.UpdateVMRequest{Egress: &api.Egress{Mode: "filtered"}}, "", nil)

		// The same privacy policy twice: recorded once (G17).
		pv := api.UpdateVMRequest{Privacy: &api.PrivacyPolicy{NoOutputRetention: true, NoProcessSamples: true}}
		a.must("PATCH", vmPath("idem"), pv, "", &v1)
		a.must("PATCH", vmPath("idem"), pv, "", &v2)
		if !v1.Privacy.NoOutputRetention || v2.Privacy != v1.Privacy || v2.StateRevision != v1.StateRevision {
			t.Fatalf("updateVM privacy twice: %+v (rev %d) / %+v (rev %d)", v1.Privacy, v1.StateRevision, v2.Privacy, v2.StateRevision)
		}
		a.must("PATCH", vmPath("idem"), api.UpdateVMRequest{Privacy: &api.PrivacyPolicy{}}, "", nil)
	}
	keyed["createToken"] = func() {
		req := api.CreateTokenRequest{Name: "idem", TTLSeconds: 600, Scopes: []api.TokenScope{{Actions: []string{"view"}}}}
		r1, r2 := twice[api.CreatedToken](a, "POST", "/tokens", req, "k-token")
		if r1.ID == "" || r1.ID != r2.ID || r1.Secret == "" || r1.Secret != r2.Secret {
			t.Fatalf("createToken replay: %+v / %+v", r1, r2)
		}
		var l api.TokenList
		a.must("GET", "/tokens", nil, "", &l)
		if len(l.Items) != 1 {
			t.Fatalf("createToken replay made %d tokens", len(l.Items))
		}
	}
	var hookID string
	keyed["createWebhook"] = func() {
		// An endpoint that never resolves (.invalid): later events queue
		// failing deliveries for the retry case.
		req := api.CreateWebhookRequest{URL: "https://hooks.invalid/hook", Events: []string{"vm.", "operation.state"}}
		r1, r2 := twice[api.WebhookSecret](a, "POST", "/webhooks", req, "k-webhook")
		if r1.Webhook.ID == "" || r1.Webhook.ID != r2.Webhook.ID || r1.Secret == "" || r1.Secret != r2.Secret {
			t.Fatalf("createWebhook replay: %+v / %+v", r1, r2)
		}
		var l api.WebhookList
		a.must("GET", "/webhooks", nil, "", &l)
		if len(l.Items) != 1 {
			t.Fatalf("createWebhook replay made %d webhooks", len(l.Items))
		}
		hookID = r1.Webhook.ID
	}
	keyed["rotateWebhookSecret"] = func() {
		r1, r2 := twice[api.WebhookSecret](a, "POST", "/webhooks/"+hookID+"/rotate-secret", api.RotateWebhookSecretRequest{}, "k-rotate")
		if r1.Secret == "" || r1.Secret != r2.Secret {
			t.Fatalf("rotateWebhookSecret replay rotated twice")
		}
	}
	natural["updateWebhook"] = func() {
		var w1, w2 api.Webhook
		a.must("PATCH", "/webhooks/"+hookID, api.UpdateWebhookRequest{Description: ptrTo("idem")}, "", &w1)
		a.must("PATCH", "/webhooks/"+hookID, api.UpdateWebhookRequest{Description: ptrTo("idem")}, "", &w2)
		if w1.Description != "idem" || !w1.UpdatedAt.Equal(w2.UpdatedAt) {
			t.Fatalf("updateWebhook twice: %+v / %+v", w1, w2)
		}
	}
	natural["retryWebhookDelivery"] = func() {
		var l api.WebhookDeliveryList
		for deadline := time.Now().Add(20 * time.Second); len(l.Items) == 0; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("no webhook delivery was queued")
			}
			a.must("GET", "/webhooks/"+hookID+"/deliveries", nil, "", &l)
		}
		var d1, d2 api.WebhookDelivery
		path := "/webhooks/" + hookID + "/deliveries/" + l.Items[0].ID + "/retry"
		a.must("POST", path, nil, "", &d1)
		a.must("POST", path, nil, "", &d2)
		if d1.ID != d2.ID || d1.State != api.DeliveryPending || d2.State != api.DeliveryPending {
			t.Fatalf("retryWebhookDelivery twice: %+v / %+v", d1, d2)
		}
	}
	natural["deleteWebhook"] = func() {
		a.must("DELETE", "/webhooks/"+hookID, nil, "", nil)
		if st, _ := a.call("DELETE", "/webhooks/"+hookID, nil, ""); st != http.StatusNotFound {
			t.Fatalf("second webhook delete: HTTP %d, want 404", st)
		}
	}
	keyed["rotateToken"] = func() {
		var l api.TokenList
		a.must("GET", "/tokens", nil, "", &l)
		r1, r2 := twice[api.CreatedToken](a, "POST", "/tokens/"+l.Items[0].ID+"/rotate", api.RotateTokenRequest{}, "k-rotate")
		if r1.ID == "" || r1.ID == l.Items[0].ID || r1.ID != r2.ID || r1.Secret == "" || r1.Secret != r2.Secret {
			t.Fatalf("rotateToken replay: %+v / %+v", r1, r2)
		}
		var after api.TokenList
		a.must("GET", "/tokens", nil, "", &after)
		if len(after.Items) != 1 || after.Items[0].ID != r1.ID {
			t.Fatalf("rotateToken replay left %+v", after.Items)
		}
	}
	natural["revokeToken"] = func() {
		var l api.TokenList
		a.must("GET", "/tokens", nil, "", &l)
		a.must("DELETE", "/tokens/"+l.Items[0].ID, nil, "", nil)
		if st, _ := a.call("DELETE", "/tokens/"+l.Items[0].ID, nil, ""); st != http.StatusNotFound {
			t.Fatalf("second token revoke: HTTP %d, want 404", st)
		}
	}
	keyed["createCredentialProfile"] = func() {
		req := api.CreateCredentialProfileRequest{Name: "idem-creds"}
		r1, r2 := twice[api.CredentialProfile](a, "POST", "/credential-profiles", req, "k-profile")
		if r1.ID == "" || r1.ID != r2.ID {
			t.Fatalf("createCredentialProfile replay: %+v / %+v", r1, r2)
		}
		var l api.CredentialProfileList
		a.must("GET", "/credential-profiles", nil, "", &l)
		if len(l.Items) != 1 {
			t.Fatalf("createCredentialProfile replay made %d profiles", len(l.Items))
		}
	}
	natural["putCredential"] = func() {
		req := api.PutCredentialRequest{Kind: domain.CredentialAPIKey, Provider: "anthropic", EnvVar: "ANTHROPIC_API_KEY", Secret: "sk-idem-secret"}
		var p1, p2 api.CredentialProfile
		a.must("PUT", "/credential-profiles/idem-creds/credentials/anthropic", req, "", &p1)
		a.must("PUT", "/credential-profiles/idem-creds/credentials/anthropic", req, "", &p2)
		if len(p2.Credentials) != 1 || !p2.Credentials[0].HasSecret || len(p1.Credentials) != 1 {
			t.Fatalf("putCredential twice: %+v / %+v", p1, p2)
		}
		if b, _ := json.Marshal(p2); strings.Contains(string(b), req.Secret) {
			t.Fatalf("putCredential returned the secret: %s", b)
		}
	}
	keyed["createCredentialGrant"] = func() {
		req := api.CreateCredentialGrantRequest{Profile: "idem-creds", Credential: "anthropic"}
		r1, r2 := twice[api.CredentialGrant](a, "POST", vmPath("idem")+"/credential-grants", req, "k-grant")
		if r1.ID == "" || r1.ID != r2.ID || r1.Placeholder != r2.Placeholder {
			t.Fatalf("createCredentialGrant replay: %+v / %+v", r1, r2)
		}
		var l api.CredentialGrantList
		a.must("GET", vmPath("idem")+"/credential-grants", nil, "", &l)
		if len(l.Items) != 1 {
			t.Fatalf("createCredentialGrant replay made %d grants", len(l.Items))
		}
	}
	natural["revokeCredentialGrant"] = func() {
		var l api.CredentialGrantList
		a.must("GET", vmPath("idem")+"/credential-grants", nil, "", &l)
		var g1, g2 api.CredentialGrant
		a.must("DELETE", vmPath("idem")+"/credential-grants/"+l.Items[0].ID, nil, "", &g1)
		a.must("DELETE", vmPath("idem")+"/credential-grants/"+l.Items[0].ID, nil, "", &g2)
		if g1.State != domain.GrantRevoked || g2.State != g1.State || g2.RevokeReason != domain.RevokeExplicit {
			t.Fatalf("revokeCredentialGrant twice: %+v / %+v", g1, g2)
		}
	}
	natural["deleteCredential"] = func() {
		a.must("DELETE", "/credential-profiles/idem-creds/credentials/anthropic", nil, "", nil)
		if st, _ := a.call("DELETE", "/credential-profiles/idem-creds/credentials/anthropic", nil, ""); st != http.StatusNotFound {
			t.Fatalf("second credential delete: HTTP %d, want 404", st)
		}
	}
	natural["deleteCredentialProfile"] = func() {
		a.must("DELETE", "/credential-profiles/idem-creds", nil, "", nil)
		if st, _ := a.call("DELETE", "/credential-profiles/idem-creds", nil, ""); st != http.StatusNotFound {
			t.Fatalf("second profile delete: HTTP %d, want 404", st)
		}
	}
	// One reverse forward per guest port: a retried create is refused
	// (409) instead of opening a second one; list finds the first.
	// The fake runtime's "guest" listens on this host's loopback: use ports
	// free right now, never fixed ones another process may hold.
	freePort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	natural["createReverseForward"] = func() {
		req := api.CreateReverseForwardRequest{GuestPort: freePort(), Target: api.ReverseTarget{Kind: api.ReverseTargetSession}}
		var fw api.ReverseForward
		a.must("POST", vmPath("idem")+"/reverse-forwards", req, "", &fw)
		if st, _ := a.call("POST", vmPath("idem")+"/reverse-forwards", req, ""); st != http.StatusConflict {
			t.Fatalf("second reverse forward on the port: HTTP %d, want 409", st)
		}
		var l api.ReverseForwardList
		a.must("GET", vmPath("idem")+"/reverse-forwards", nil, "", &l)
		if len(l.Items) != 1 || l.Items[0].ID != fw.ID {
			t.Fatalf("reverse forwards after a retried create: %+v", l.Items)
		}
	}
	// App endpoints and the desktop (openDesktop returns the open one, 200;
	// a second closeDesktop is 404) need the tailnet gateway, which this
	// daemon has not:
	// both calls answer the same 422. Their natural idempotency (one
	// endpoint per guest port and name, 409; PATCH sets absolute values;
	// a second delete is 404) is exercised by TestAppEndpointsOverTailnet
	// and the appendpoint tests.
	// Members and shares (owner only) also need the tailnet gateway; their
	// natural idempotency (addMember 409 on the same login, createShare an
	// upsert, PATCH absolute, removes 404) is exercised by the members tests.
	for id, call := range map[string][2]string{
		"addMember":         {"POST", "/members"},
		"updateMember":      {"PATCH", "/members/bob"},
		"removeMember":      {"DELETE", "/members/bob"},
		"createShare":       {"POST", "/shares"},
		"deleteShare":       {"DELETE", "/shares/shr_x"},
		"openDesktop":       {"POST", vmPath("idem") + "/desktop"},
		"closeDesktop":      {"DELETE", vmPath("idem") + "/desktop"},
		"createAppEndpoint": {"POST", vmPath("idem") + "/app-endpoints"},
		"updateAppEndpoint": {"PATCH", vmPath("idem") + "/app-endpoints/web"},
		"deleteAppEndpoint": {"DELETE", vmPath("idem") + "/app-endpoints/web"},
	} {
		natural[id] = func() {
			for range 2 {
				if st, _ := a.call(call[0], call[1], map[string]any{"port": 3000}, ""); st != http.StatusUnprocessableEntity {
					t.Fatalf("%s without a gateway: HTTP %d, want 422", id, st)
				}
			}
		}
	}
	natural["revokeReverseForward"] = func() {
		var fw api.ReverseForward
		req := api.CreateReverseForwardRequest{GuestPort: freePort(), Target: api.ReverseTarget{Kind: api.ReverseTargetSession}}
		a.must("POST", vmPath("idem")+"/reverse-forwards", req, "", &fw)
		a.must("DELETE", vmPath("idem")+"/reverse-forwards/"+fw.ID, nil, "", nil)
		if st, _ := a.call("DELETE", vmPath("idem")+"/reverse-forwards/"+fw.ID, nil, ""); st != http.StatusNotFound {
			t.Fatalf("second reverse forward revoke: HTTP %d, want 404", st)
		}
	}
	manifest := api.EnvManifest{Repos: []api.RepoSpec{{URL: "https://example.com/r.git", Path: "/work/r"}}}
	keyed["createEnvironment"] = func() {
		r1, r2 := twice[api.Environment](a, "POST", "/environments", api.CreateEnvironmentRequest{Name: "idem-env", Manifest: manifest}, "k-env")
		if r1.ID == "" || r1.ID != r2.ID || r2.LatestRevision != 1 {
			t.Fatalf("createEnvironment replay: %+v / %+v", r1, r2)
		}
	}
	keyed["createEnvironmentRevision"] = func() {
		next := manifest
		next.Variables = map[string]string{"X": "1"}
		r1, r2 := twice[api.EnvironmentRevision](a, "POST", "/environments/idem-env/revisions", api.CreateEnvironmentRevisionRequest{Manifest: next}, "k-rev")
		if r1.Revision != 2 || r2.Revision != 2 {
			t.Fatalf("createEnvironmentRevision replay: %+v / %+v", r1, r2)
		}
		var env api.Environment
		a.must("GET", "/environments/idem-env", nil, "", &env)
		if env.LatestRevision != 2 {
			t.Fatalf("replay minted revision %d", env.LatestRevision)
		}
	}
	natural["deleteEnvironment"] = func() {
		a.must("DELETE", "/environments/idem-env", nil, "", nil)
		if st, _ := a.call("DELETE", "/environments/idem-env", nil, ""); st != http.StatusNotFound {
			t.Fatalf("second environment delete: HTTP %d, want 404", st)
		}
	}
	natural["pushTree"] = func() {
		var tarBuf bytes.Buffer
		tw := tar.NewWriter(&tarBuf)
		tw.WriteHeader(&tar.Header{Name: "src/a.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
		tw.Write([]byte("same"))
		tw.Close()
		target := filepath.Join(dir, "idem-tree")
		path := vmPath("idem") + "/tree?mode=merge&path=" + url.QueryEscape(target)
		var p1, p2 api.PushResult
		a.must("PUT", path, tarBuf.Bytes(), "", &p1)
		a.must("PUT", path, tarBuf.Bytes(), "", &p2)
		if p1.Files != 1 || p1.SHA256 == "" || p1 != p2 {
			t.Fatalf("pushTree repeat differs: %+v vs %+v", p1, p2)
		}
		if b, _ := os.ReadFile(filepath.Join(target, "src", "a.txt")); string(b) != "same" {
			t.Fatalf("pushed content %q", b)
		}
	}
	// Runs: their VMs are named after the run, so a replay that made a
	// second VM would show up in the list.
	waitRun := func(id string, ok func(api.Run) bool) api.Run {
		t.Helper()
		var r api.Run
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			a.must("GET", "/runs/"+id, nil, "", &r)
			if ok(r) {
				return r
			}
		}
		t.Fatalf("run %s: %+v", id, r)
		return r
	}
	ended := func(r api.Run) bool { return r.EndedAt != nil }
	runVMs := func(r api.Run) int {
		var vm api.VM
		a.must("GET", vmPath(string(r.VMID)), nil, "", &vm)
		return countVMs(vm.Name)
	}
	keyed["createRun"] = func() {
		marker := filepath.Join(dir, "run-once")
		body := api.RunRequest{Argv: []string{"sh", "-c", "echo once >> " + marker}}
		r1, r2 := twice[api.Run](a, "POST", "/runs", body, "k-run")
		if r1.ID == "" || r1.ID != r2.ID {
			t.Fatalf("createRun replay created %s, first was %s", r2.ID, r1.ID)
		}
		r := waitRun(r1.ID, ended)
		if r.State != api.RunSucceeded {
			t.Fatalf("run %+v", r)
		}
		if r3, _ := twice[api.Run](a, "POST", "/runs", body, "k-run"); r3.ID != r1.ID {
			t.Fatalf("createRun replay after the end created %s", r3.ID)
		}
		if b, _ := os.ReadFile(marker); string(b) != "once\n" || runVMs(r) != 1 {
			t.Fatalf("createRun replay ran %q in %d VMs", b, runVMs(r))
		}
		if st, _ := a.call("POST", "/runs", api.RunRequest{Argv: []string{"true"}}, "k-run"); st != http.StatusConflict {
			t.Fatalf("createRun with a reused key and another body: HTTP %d, want 409", st)
		}
	}
	natural["startRun"] = func() {
		marker := filepath.Join(dir, "start-once")
		var r api.Run
		a.must("POST", "/runs", api.RunRequest{Argv: []string{"sh", "-c", "echo once >> " + marker}, Hold: true}, "k-held", &r)
		waitRun(r.ID, func(r api.Run) bool { return r.State == api.RunWaiting })
		a.must("POST", "/runs/"+r.ID+"/start", nil, "", nil)
		if st, body := a.call("POST", "/runs/"+r.ID+"/start", nil, ""); st != http.StatusOK && st != http.StatusConflict {
			t.Fatalf("second start: HTTP %d %s", st, body)
		}
		if r = waitRun(r.ID, ended); r.State != api.RunSucceeded {
			t.Fatalf("held run %+v", r)
		}
		if b, _ := os.ReadFile(marker); string(b) != "once\n" {
			t.Fatalf("startRun twice ran %q", b)
		}
	}
	natural["cancelRun"] = func() {
		var r api.Run
		a.must("POST", "/runs", api.RunRequest{Argv: []string{"sleep", "30"}}, "k-cancel", &r)
		r = waitRun(r.ID, func(r api.Run) bool { return r.ProcessID != "" })
		a.must("DELETE", "/runs/"+r.ID, nil, "", nil)
		a.must("DELETE", "/runs/"+r.ID, nil, "", nil)
		if r = waitRun(r.ID, ended); r.State != api.RunCancelled || runVMs(r) != 1 {
			t.Fatalf("cancelled run %+v", r)
		}
	}
	natural["deleteSnapshot"] = func() {
		a.must("DELETE", "/snapshots/idem-snap", nil, "", nil)
		if st, _ := a.call("DELETE", "/snapshots/idem-snap", nil, ""); st != http.StatusNotFound {
			t.Fatalf("second snapshot delete: HTTP %d, want 404", st)
		}
	}

	// Every implemented mutation must be covered, in the right category.
	ops := mutatingOps(t)
	for _, o := range ops {
		_, k := keyed[o.id]
		_, n := natural[o.id]
		switch {
		case o.takesKey && !k:
			t.Errorf("%s %s takes an Idempotency-Key but has no replay case here", o.method, o.path)
		case !o.takesKey && !n:
			t.Errorf("%s %s takes no Idempotency-Key: add a natural-idempotency case (or give it a key)", o.method, o.path)
		}
	}
	if t.Failed() {
		return
	}
	// Order matters: later cases use the VMs and snapshots earlier ones made.
	for _, id := range []string{"createVM", "createWebhook", "createVMs", "reclaimVMs", "stopVM", "snapshotVM", "forkVM", "resumeVM", "recoverVM", "execProcess",
		"killProcess", "writeFile", "pushTree", "updateVM", "openTunnel", "closeTunnel", "issueSSHCredential", "createToken", "rotateToken", "revokeToken",
		"createCredentialProfile", "putCredential", "createCredentialGrant", "revokeCredentialGrant", "deleteCredential", "deleteCredentialProfile",
		"createPrompt", "cancelPrompt", "interruptPrompts", "steerPrompt", "createReverseForward", "revokeReverseForward",
		"createAppEndpoint", "updateAppEndpoint", "deleteAppEndpoint", "openDesktop", "closeDesktop",
		"rotateWebhookSecret", "updateWebhook", "retryWebhookDelivery", "deleteWebhook",
		"addMember", "updateMember", "removeMember", "createShare", "deleteShare", "deleteVM", "deleteSnapshot",
		"createEnvironment", "createEnvironmentRevision", "deleteEnvironment",
		"createRun", "startRun", "cancelRun"} {
		run := keyed[id]
		if run == nil {
			run = natural[id]
		}
		if run == nil {
			t.Fatalf("case %s is not in the operation list", id)
		}
		t.Run(id, func(t *testing.T) { run() })
	}
	if len(keyed)+len(natural) != len(ops) {
		t.Errorf("%d cases for %d mutating operations: remove cases for operations that no longer exist", len(keyed)+len(natural), len(ops))
	}
}

func ptrTo[T any](v T) *T { return &v }
