package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
	"davit/internal/config"
	"davit/internal/daemon"
)

// The API fuzzers run the real daemon in-process (fake runtime with its
// in-process guest agent, every service wired as in production) and send
// it fuzzed request bodies and headers over its Unix socket. The invariants:
// the daemon never panics or stops answering, never answers 500 (an
// internal error is a bug: bad input must be a typed 4xx), and every error
// is a well-formed ErrorResponse. `go test` runs only the seeds; run a
// fuzzer with `go test -run '^$' -fuzz FuzzAPIBodies ./internal/httpapi`.

// mutatingRoutes are the create/update operations whose bodies are decoded
// (method and path; {vm} is the fixture VM, other IDs need not exist).
var mutatingRoutes = []struct{ method, path string }{
	{"POST", "/vms"},
	{"POST", "/vms/batch"},
	{"PATCH", "/vms/{vm}"},
	{"POST", "/vms/{vm}/stop"},
	{"POST", "/vms/{vm}/resume"},
	{"POST", "/vms/{vm}/fork"},
	{"POST", "/vms/{vm}/snapshots"},
	{"POST", "/vms/{vm}/recover"},
	{"POST", "/vms/{vm}/processes"},
	{"POST", "/vms/{vm}/prompts"},
	{"POST", "/vms/{vm}/prompts/prm_x/steer"},
	{"POST", "/vms/{vm}/interrupt"},
	{"POST", "/vms/{vm}/tunnels"},
	{"POST", "/vms/{vm}/ssh-credentials"},
	{"POST", "/vms/{vm}/reverse-forwards"},
	{"POST", "/vms/{vm}/app-endpoints"},
	{"PATCH", "/vms/{vm}/app-endpoints/app_x"},
	{"POST", "/vms/{vm}/desktop"},
	{"POST", "/vms/{vm}/credential-grants"},
	{"POST", "/environments"},
	{"POST", "/environments/env_x/revisions"},
	{"POST", "/runs"},
	{"POST", "/runs/run_x/start"},
	{"POST", "/credential-profiles"},
	{"PUT", "/credential-profiles/cp_x/credentials/c"},
	{"POST", "/webhooks"},
	{"PATCH", "/webhooks/wh_x"},
	{"POST", "/webhooks/wh_x/rotate-secret"},
	{"POST", "/tokens"},
	{"POST", "/tokens/tok_x/rotate"},
	{"POST", "/members"},
	{"PATCH", "/members/mem_x"},
	{"POST", "/shares"},
}

type fuzzDaemon struct {
	c    *http.Client
	vm   string
	logs *bytes.Buffer
}

// startFuzzDaemon runs a daemon until the fuzz target ends and creates the
// fixture VM.
func startFuzzDaemon(f testing.TB) *fuzzDaemon {
	f.Helper()
	dir, err := os.MkdirTemp("", "slwfz")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	cfg := config.Defaults()
	cfg.DataDir, cfg.Socket, cfg.Runtime = filepath.Join(dir, "data"), sock, "fake"
	raw := fmt.Sprintf(`{"dataDir":%q,"socket":%q,"fake":{"guestAgent":true}}`, cfg.DataDir, sock)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		f.Fatal(err)
	}
	if cfg, err = config.Load(path); err != nil {
		f.Fatal(err)
	}
	logs := new(bytes.Buffer)
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: logs}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg, log) }()
	f.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	})
	d := &fuzzDaemon{logs: logs, c: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		select {
		case err := <-done:
			f.Fatalf("daemon exited: %v\n%s", err, logs)
		default:
		}
		if time.Now().After(deadline) {
			f.Fatal("daemon did not start")
		}
	}
	status, body := d.do(f, "POST", "/vms", "fixture-vm", "", []byte(`{"name":"fz"}`))
	if status != http.StatusAccepted {
		f.Fatalf("fixture VM: %d %s", status, body)
	}
	var acc api.OperationAccepted
	json.Unmarshal(body, &acc)
	d.vm = string(acc.VM.ID)
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
		_, b := d.do(f, "GET", "/vms/"+d.vm, "", "", nil)
		var vm api.VM
		json.Unmarshal(b, &vm)
		if vm.State == "ready" {
			break
		}
		if time.Now().After(deadline) {
			f.Fatalf("fixture VM never ran: %s", b)
		}
	}
	return d
}

type lockedWriter struct {
	mu chan struct{}
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	if l.mu == nil {
		l.mu = make(chan struct{}, 1)
	}
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	return l.w.Write(p)
}

func (d *fuzzDaemon) do(t testing.TB, method, path, key, ifMatch string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "http://davit"+api.BasePath+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil // the fuzzer made an invalid URL; not the daemon's input
	}
	if key != "" {
		req.Header.Set(api.HeaderIdempotencyKey, key)
	}
	if ifMatch != "" {
		req.Header.Set(api.HeaderIfMatch, ifMatch)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: daemon stopped answering: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b
}

// settle waits for operation id to finish.
func (d *fuzzDaemon) settle(t *testing.T, id string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		_, b := d.do(t, "GET", "/operations/"+id, "", "", nil)
		var op api.Operation
		json.Unmarshal(b, &op)
		if op.State == "succeeded" || op.State == "failed" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s never finished: %s\ndaemon log:\n%s", id, b, d.logs)
		}
	}
}

// check enforces the response invariants.
func (d *fuzzDaemon) check(t *testing.T, method, path string, status int, body []byte) {
	t.Helper()
	if status == 0 {
		return
	}
	if status == http.StatusInternalServerError {
		t.Fatalf("%s %s: 500 %s\ndaemon log:\n%s", method, path, body, d.logs)
	}
	if status >= 400 {
		var er api.ErrorResponse
		if err := json.Unmarshal(body, &er); err != nil || er.Error.Code == "" || er.Error.RequestID == "" {
			t.Fatalf("%s %s: %d with a malformed error body %q", method, path, status, body)
		}
	}
}

func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r > 0x7e {
			return -1
		}
		return r
	}, s)
}

// FuzzAPIBodies: any body, Idempotency-Key and If-Match on any create or
// update operation gets a typed answer.
func FuzzAPIBodies(f *testing.F) {
	d := startFuzzDaemon(f)
	seeds := []string{``, `{}`, `null`, `[]`, `{"name":"x"}`, `{"name":"`, `{"a":1}{"b":2}`, `"x"`, `1e999`,
		`{"name":"y","cpus":-1,"memoryMiB":1e30}`, `{"argv":[]}`, `{"argv":["true"],"env":{"A":"1"},"timeoutSeconds":-5}`,
		`{"privacy":{"noOutputRetention":true}}`, `{"items":[{"name":"b1"},{"name":"b1"}]}`, `{"port":0}`, `{"port":70000}`,
		`{"url":"http://[::1","events":["vm."]}`, `{"egress":{"mode":"allowlist","allow":["*"]}}`, `{"ttlSeconds":9223372036854775807}`,
		`{"name":"` + strings.Repeat("é", 300) + `"}`, `{"prompt":"","harness":"x"}`, `{"manifest":{"setup":[{"argv":[]}]}}`}
	for i := range mutatingRoutes {
		for _, s := range seeds {
			f.Add(uint8(i), "", "", []byte(s))
		}
	}
	f.Add(uint8(0), strings.Repeat("k", 256), "", []byte(`{"name":"long-key"}`))
	f.Add(uint8(2), "k-patch", `"abc"`, []byte(`{"ttlSeconds":60}`))
	f.Add(uint8(2), "k-patch", `-1`, []byte(`{}`))
	f.Fuzz(func(t *testing.T, route uint8, key, ifMatch string, body []byte) {
		r := mutatingRoutes[int(route)%len(mutatingRoutes)]
		path := strings.ReplaceAll(r.path, "{vm}", d.vm)
		key, ifMatch = headerSafe(key), headerSafe(ifMatch)
		if key == "" {
			key = fmt.Sprintf("fz-%x", body) // most mutations require a key
			if len(key) > 200 {
				key = key[:200]
			}
		}
		status, resp := d.do(t, r.method, path, key, ifMatch, body)
		d.check(t, r.method, path, status, resp)
	})
}

// FuzzIdempotencyKey: for any key, a replay of the same createVM returns
// the same operation, and the key with another body is a typed conflict.
func FuzzIdempotencyKey(f *testing.F) {
	d := startFuzzDaemon(f)
	for _, k := range []string{"k", "a b", "../x", strings.Repeat("z", 255), strings.Repeat("z", 256), "ÿ", "%00", "k\"", "*"} {
		f.Add(k, "n1")
	}
	f.Fuzz(func(t *testing.T, key, name string) {
		key = headerSafe(key)
		body := func(n string) []byte { b, _ := json.Marshal(map[string]string{"name": n}); return b }
		s1, b1 := d.do(t, "POST", "/vms", key, "", body(name))
		d.check(t, "POST", "/vms", s1, b1)
		s2, b2 := d.do(t, "POST", "/vms", key, "", body(name))
		d.check(t, "POST", "/vms", s2, b2)
		if key == "" || s1 != http.StatusAccepted {
			return
		}
		var a1, a2 api.OperationAccepted
		json.Unmarshal(b1, &a1)
		json.Unmarshal(b2, &a2)
		if s2 != http.StatusAccepted || a1.Operation.ID != a2.Operation.ID {
			t.Fatalf("replay of key %q: %d %s, first %s", key, s2, b2, b1)
		}
		s3, b3 := d.do(t, "POST", "/vms", key, "", body(name+"-other"))
		var er api.ErrorResponse
		json.Unmarshal(b3, &er)
		if s3 != http.StatusConflict || er.Error.Code != "idempotency_conflict" {
			t.Fatalf("key %q reused with another body: %d %s", key, s3, b3)
		}
		// Clean up so the fuzzer does not fill the host with VMs (and then
		// meet only memory admission): delete once the create is done.
		d.settle(t, string(a1.Operation.ID))
		s4, b4 := d.do(t, "DELETE", "/vms/"+string(a1.VM.ID), "del-"+string(a1.VM.ID), "", nil)
		if s4 != http.StatusAccepted {
			t.Fatalf("delete after create: %d %s", s4, b4)
		}
		var a4 api.OperationAccepted
		json.Unmarshal(b4, &a4)
		d.settle(t, string(a4.Operation.ID))
	})
}
