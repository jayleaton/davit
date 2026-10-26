package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"davit/api"
	"davit/internal/domain"
)

// privacyServer fakes the API: it records each request's method, path and
// body, answers creates with an accepted operation and PATCHes by applying
// the policy to a stored VM.
type privacyServer struct {
	mu   sync.Mutex
	reqs []string // "METHOD PATH BODY"
	vm   api.VM
}

func newPrivacyServer(t *testing.T, vm api.VM) *privacyServer {
	ps := &privacyServer{vm: vm}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ps.mu.Lock()
		defer ps.mu.Unlock()
		ps.reqs = append(ps.reqs, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/vms":
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(api.OperationAccepted{Operation: api.Operation{ID: "op_1", Kind: domain.OpCreate, VMID: "vm_1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/vms/batch":
			json.NewEncoder(w).Encode(api.CreateVMsResult{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/vms/web":
			json.NewEncoder(w).Encode(ps.vm)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/vms/"+string(ps.vm.ID):
			var req api.UpdateVMRequest
			if err := json.Unmarshal(body, &req); err != nil || req.Privacy == nil {
				http.Error(w, `{"error":{"code":"invalid_argument","message":"bad body"}}`, http.StatusBadRequest)
				return
			}
			ps.vm.Privacy = *req.Privacy
			json.NewEncoder(w).Encode(ps.vm)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DAVIT_API_URL", srv.URL)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("DAVIT_CURRENT_ID", "")
	return ps
}

func (ps *privacyServer) take() []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r := ps.reqs
	ps.reqs = nil
	return r
}

// TestNewSendsPrivacyOnlyWhenSet: new (and new --count) sends a privacy
// policy only when a privacy flag turns something on.
func TestNewSendsPrivacyOnlyWhenSet(t *testing.T) {
	ps := newPrivacyServer(t, api.VM{})
	for _, c := range []struct {
		args []string
		want string // the request line
	}{
		{[]string{"new", "web", "--no-wait"}, `POST /api/v1/vms {"name":"web"}`},
		{[]string{"new", "web", "--no-wait", "--ephemeral=false"}, `POST /api/v1/vms {"name":"web"}`},
		{[]string{"new", "web", "--ephemeral", "--no-process-samples", "--no-wait"},
			`POST /api/v1/vms {"name":"web","privacy":{"ephemeral":true,"noProcessSamples":true}}`},
		{[]string{"new", "b", "--count", "2", "--no-snapshot", "--no-output-retention", "--no-wait"},
			`POST /api/v1/vms/batch {"count":2,"template":{"name":"b","privacy":{"noSnapshot":true,"noOutputRetention":true}}}`},
	} {
		if r := runCLI(t, false, c.args...); r.code != 0 {
			t.Fatalf("%v: exit %d %+v", c.args, r.code, r.last.Error)
		}
		if got := ps.take(); len(got) != 1 || got[0] != c.want {
			t.Fatalf("%v: requests %q, want %q", c.args, got, c.want)
		}
	}
}

// TestPrivacyMergesFlags: privacy reads the VM and PATCHes the whole policy
// with only the given flags changed; without flags it only reads.
func TestPrivacyMergesFlags(t *testing.T) {
	ps := newPrivacyServer(t, api.VM{ID: "vm_1", Name: "web", Privacy: api.PrivacyPolicy{Ephemeral: true, NoProcessSamples: true}})

	r := runCLI(t, false, "privacy", "web")
	if r.code != 0 {
		t.Fatalf("show: exit %d %+v", r.code, r.last.Error)
	}
	if got := ps.take(); len(got) != 1 || !strings.HasPrefix(got[0], "GET /api/v1/vms/web") {
		t.Fatalf("show sent %q, want one GET", got)
	}

	r = runCLI(t, false, "privacy", "web", "--ephemeral=false", "--no-snapshot")
	if r.code != 0 {
		t.Fatalf("set: exit %d %+v", r.code, r.last.Error)
	}
	got := ps.take()
	want := `PATCH /api/v1/vms/vm_1 {"privacy":{"noSnapshot":true,"noProcessSamples":true}}`
	if len(got) != 2 || got[1] != want {
		t.Fatalf("set sent %q, want GET then %q", got, want)
	}
	pv, _ := r.data["privacy"].(map[string]any)
	if pv["noSnapshot"] != true || pv["noProcessSamples"] != true || pv["ephemeral"] != nil {
		t.Fatalf("result privacy %v", r.data["privacy"])
	}
	if r := runCLI(t, false, "privacy", "web", "extra"); r.code != ExitUsage {
		t.Fatalf("two args: exit %d", r.code)
	}
}

func TestPrivacyWordsInInfo(t *testing.T) {
	if w := privacyWords(domain.PrivacyPolicy{}); w != "" {
		t.Fatalf("zero policy: %q", w)
	}
	if w := privacyWords(domain.PrivacyPolicy{Ephemeral: true, NoOutputRetention: true}); w != "ephemeral, no output retention" {
		t.Fatalf("got %q", w)
	}
}
