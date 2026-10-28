// Package githubtest is a fake GitHub API for the CI runner controller's
// tests: GitHub App authentication (it verifies the App JWT), repositories,
// workflow runs and jobs, just-in-time runner registration, and
// workflow_job webhook deliveries. Jobs move like GitHub's: queued, then
// in_progress once a runner with matching labels takes one (Assign), then
// completed; a just-in-time runner is removed after its job.
package githubtest

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/internal/ci/github"
)

// Server is the fake API. Safe for concurrent use.
type Server struct {
	*httptest.Server
	AppID int64
	Key   *rsa.PrivateKey

	mu      sync.Mutex
	next    int64
	repos   map[string]*github.Repository // by lower-case full name
	runs    map[int64]*github.Run
	jobs    map[int64]*job
	runners map[int64]*runner
	tokens  map[string]int64 // installation tokens issued
	calls   map[string]int
	// fail, when set, answers a request with this status instead.
	fail func(r *http.Request) int
}

type job struct {
	github.Job
	repo string
}

type runner struct {
	github.Runner
	scope  string
	labels []string
}

// New starts a fake GitHub with a fresh App key; it stops with the test.
func New(t testing.TB) *Server {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{AppID: 4242, Key: key, next: 1000, repos: map[string]*github.Repository{}, runs: map[int64]*github.Run{},
		jobs: map[int64]*job{}, runners: map[int64]*runner{}, tokens: map[string]int64{}, calls: map[string]int{}}
	s.Server = httptest.NewServer(s.routes())
	t.Cleanup(s.Close)
	return s
}

// KeyPEM is the App private key as GitHub hands it out.
func (s *Server) KeyPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(s.Key)})
}

// App returns a client of this server.
func (s *Server) App() *github.App { return &github.App{BaseURL: s.URL, AppID: s.AppID, Key: s.Key} }

// FailWith makes the server answer requests f picks (a non-zero status)
// with that status; nil stops it.
func (s *Server) FailWith(f func(r *http.Request) int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = f
}

// Calls counts requests by "METHOD pattern".
func (s *Server) Calls(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[key]
}

func (s *Server) id() int64 { s.next++; return s.next }

// AddRepo adds a repository (owner/name).
func (s *Server) AddRepo(name string, public bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, _, _ := strings.Cut(name, "/")
	r := &github.Repository{ID: s.id(), FullName: name, Private: !public, Visibility: map[bool]string{true: "public", false: "private"}[public]}
	r.Owner.Login = owner
	s.repos[strings.ToLower(name)] = r
}

// QueueOptions shape a queued job.
type QueueOptions struct {
	Name string
	// Fork is the head repository of a pull request from a fork.
	Fork string
	// RunID puts the job in an existing run.
	RunID int64
}

// Queue queues a job with labels in repo and returns it.
func (s *Server) Queue(repo string, labels []string, o QueueOptions) github.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.repos[strings.ToLower(repo)]
	if r == nil {
		panic("githubtest: unknown repo " + repo)
	}
	run := s.runs[o.RunID]
	if run == nil {
		run = &github.Run{ID: s.id(), Event: "push", Repository: *r, HeadRepository: *r, HeadBranch: "main"}
		if o.Fork != "" {
			run.Event = "pull_request"
			run.HeadRepository = github.Repository{ID: s.id(), FullName: o.Fork}
		}
		s.runs[run.ID] = run
	}
	name := o.Name
	if name == "" {
		name = "build"
	}
	j := &job{Job: github.Job{ID: s.id(), RunID: run.ID, Name: name, WorkflowName: "CI", Status: "queued", Labels: slices.Clone(labels),
		HeadBranch: run.HeadBranch, CreatedAt: time.Now().UTC()}, repo: r.FullName}
	s.jobs[j.ID] = j
	return j.Job
}

// RepoOf returns the repository (owner/name) of a job.
func (s *Server) RepoOf(j github.Job) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[j.ID].repo
}

// Job returns job id as GitHub would.
func (s *Server) Job(id int64) github.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id].Job
}

// Assign gives a queued job to the named runner, which must exist, be idle
// and carry every label of the job (as GitHub matches): the job goes
// in_progress and the runner busy.
func (s *Server) Assign(jobID int64, runnerName string) (github.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	if j == nil || j.Status != "queued" {
		return github.Job{}, fmt.Errorf("job %d is not queued", jobID)
	}
	for _, rn := range s.runners {
		if rn.Name != runnerName {
			continue
		}
		if rn.Busy {
			return github.Job{}, fmt.Errorf("runner %s is busy", runnerName)
		}
		have := append([]string{"self-hosted", "linux", "x64"}, rn.labels...)
		for _, l := range j.Labels {
			if !slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, l) }) {
				return github.Job{}, fmt.Errorf("runner %s lacks label %q", runnerName, l)
			}
		}
		rn.Busy, rn.Status = true, "online"
		now := time.Now().UTC()
		j.Status, j.RunnerID, j.RunnerName, j.StartedAt = "in_progress", rn.ID, rn.Name, &now
		return j.Job, nil
	}
	return github.Job{}, fmt.Errorf("no runner %s", runnerName)
}

// Complete ends a job with conclusion; its just-in-time runner is removed.
func (s *Server) Complete(jobID int64, conclusion string) github.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[jobID]
	now := time.Now().UTC()
	j.Status, j.Conclusion, j.CompletedAt = "completed", conclusion, &now
	if j.RunnerID != 0 {
		delete(s.runners, j.RunnerID)
	}
	return j.Job
}

// Runners lists the registered runners.
func (s *Server) Runners() []github.Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []github.Runner
	for _, r := range s.runners {
		out = append(out, r.Runner)
	}
	slices.SortFunc(out, func(a, b github.Runner) int { return int(a.ID - b.ID) })
	return out
}

// RegisterRunner adds a runner as if registered earlier (restart tests).
func (s *Server) RegisterRunner(repo, name string, busy bool) github.Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &runner{Runner: github.Runner{ID: s.id(), Name: name, Status: "offline", Busy: busy}, scope: "repos/" + strings.ToLower(repo)}
	s.runners[r.ID] = r
	return r.Runner
}

// JIT is what an encoded just-in-time config holds here.
type JIT struct {
	Runner   string `json:"runner"`
	RunnerID int64  `json:"runnerId"`
}

// DecodeJIT reads an encoded config this server issued.
func DecodeJIT(enc string) (JIT, error) {
	var j JIT
	b, err := base64.StdEncoding.DecodeString(enc)
	if err == nil {
		err = json.Unmarshal(b, &j)
	}
	return j, err
}

// Deliver posts a workflow_job delivery for job (action: queued,
// in_progress, completed) to url, signed with secret, and returns the
// response status.
func (s *Server) Deliver(url string, secret []byte, action string, j github.Job) (int, error) {
	s.mu.Lock()
	sj := s.jobs[j.ID]
	repo := *s.repos[strings.ToLower(sj.repo)]
	s.mu.Unlock()
	ev := map[string]any{"action": action, "workflow_job": j, "repository": repo, "installation": map[string]any{"id": 1}}
	body, _ := json.Marshal(ev)
	return Post(url, secret, "workflow_job", body)
}

// Post sends a webhook delivery of event with body, signed with secret
// (nil: unsigned).
func Post(url string, secret []byte, event string, body []byte) (int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set(github.HeaderEvent, event)
	req.Header.Set(github.HeaderDelivery, strconv.FormatInt(time.Now().UnixNano(), 10))
	req.Header.Set("Content-Type", "application/json")
	if secret != nil {
		req.Header.Set(github.HeaderSignature, github.Sign(secret, body))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, f func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			s.calls[pattern]++
			fail := s.fail
			s.mu.Unlock()
			if fail != nil {
				if st := fail(r); st != 0 {
					writeJSON(w, st, map[string]string{"message": "injected failure"})
					return
				}
			}
			if pattern != "POST /app/installations/{id}/access_tokens" && !s.authorized(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
				return
			}
			f(w, r)
		})
	}
	h("POST /app/installations/{id}/access_tokens", s.accessToken)
	h("GET /repos/{owner}/{repo}", s.getRepo)
	h("GET /repos/{owner}/{repo}/actions/runs", s.listRuns)
	h("GET /repos/{owner}/{repo}/actions/runs/{id}", s.getRun)
	h("GET /repos/{owner}/{repo}/actions/runs/{id}/jobs", s.listJobs)
	h("GET /repos/{owner}/{repo}/actions/jobs/{id}", s.getJob)
	for _, sc := range []string{"/repos/{owner}/{repo}", "/orgs/{owner}"} {
		h("POST "+sc+"/actions/runners/generate-jitconfig", s.generateJIT)
		h("GET "+sc+"/actions/runners", s.listRunners)
		h("DELETE "+sc+"/actions/runners/{id}", s.deleteRunner)
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) authorized(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	s.mu.Lock()
	defer s.mu.Unlock()
	_, known := s.tokens[tok]
	return ok && known
}

// accessToken checks the App JWT (RS256 under the App key, iss = App ID,
// unexpired) and issues an installation token.
func (s *Server) accessToken(w http.ResponseWriter, r *http.Request) {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(jwt, ".")
	if !ok || len(parts) != 3 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "A JSON web token could not be decoded"})
		return
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || rsa.VerifyPKCS1v15(&s.Key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "'Issuer' claim ('iss') must be an Integer"})
		return
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Iss != strconv.FormatInt(s.AppID, 10) || time.Unix(claims.Exp, 0).Before(time.Now()) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "bad JWT claims"})
		return
	}
	inst, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	tok := "ghs_" + base64.RawURLEncoding.EncodeToString(b)
	s.mu.Lock()
	s.tokens[tok] = inst
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": time.Now().Add(time.Hour).UTC()})
}

func (s *Server) repo(r *http.Request) (*github.Repository, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rp, ok := s.repos[strings.ToLower(r.PathValue("owner")+"/"+r.PathValue("repo"))]
	return rp, ok
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, rp)
}

// runStatus derives a run's status from its jobs.
func (s *Server) runStatus(id int64) string {
	var q, p, c int
	for _, j := range s.jobs {
		if j.RunID != id {
			continue
		}
		switch j.Status {
		case "queued":
			q++
		case "in_progress":
			p++
		default:
			c++
		}
	}
	switch {
	case p > 0 || q > 0 && c > 0:
		return "in_progress"
	case q > 0:
		return "queued"
	}
	return "completed"
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.repo(r)
	if !ok {
		notFound(w)
		return
	}
	want := r.URL.Query().Get("status")
	s.mu.Lock()
	out := []github.Run{}
	for _, run := range s.runs {
		if run.Repository.ID != rp.ID {
			continue
		}
		cp := *run
		cp.Status = s.runStatus(run.ID)
		if want == "" || cp.Status == want {
			out = append(out, cp)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b github.Run) int { return int(a.ID - b.ID) })
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "workflow_runs": out})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	run, ok := s.runs[id]
	var cp github.Run
	if ok {
		cp = *run
		cp.Status = s.runStatus(id)
	}
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	out := []github.Job{}
	for _, j := range s.jobs {
		if j.RunID == id {
			out = append(out, j.Job)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b github.Job) int { return int(a.ID - b.ID) })
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "jobs": out})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	j, ok := s.jobs[id]
	var cp github.Job
	if ok {
		cp = j.Job
	}
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

func scopeOf(r *http.Request) string {
	if r.PathValue("repo") != "" {
		return "repos/" + strings.ToLower(r.PathValue("owner")+"/"+r.PathValue("repo"))
	}
	return "orgs/" + strings.ToLower(r.PathValue("owner"))
}

func (s *Server) generateJIT(w http.ResponseWriter, r *http.Request) {
	var req github.JITRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || len(req.Labels) == 0 || req.RunnerGroupID == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name, runner_group_id and labels are required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rn := range s.runners {
		if rn.Name == req.Name {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "Already exists - A runner with the name " + req.Name + " already exists."})
			return
		}
	}
	rn := &runner{Runner: github.Runner{ID: s.id(), Name: req.Name, Status: "offline"}, scope: scopeOf(r), labels: req.Labels}
	s.runners[rn.ID] = rn
	b, _ := json.Marshal(JIT{Runner: rn.Name, RunnerID: rn.ID})
	writeJSON(w, http.StatusCreated, map[string]any{"runner": map[string]any{"id": rn.ID, "name": rn.Name}, "encoded_jit_config": base64.StdEncoding.EncodeToString(b)})
}

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	sc := scopeOf(r)
	s.mu.Lock()
	out := []github.Runner{}
	for _, rn := range s.runners {
		if rn.scope == sc {
			out = append(out, rn.Runner)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b github.Runner) int { return int(a.ID - b.ID) })
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "runners": out})
}

func (s *Server) deleteRunner(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	rn, ok := s.runners[id]
	switch {
	case !ok || rn.scope != scopeOf(r):
		notFound(w)
	case rn.Busy:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Bad request - Runner \"" + rn.Name + "\" is still running a job\""})
	default:
		delete(s.runners, id)
		w.WriteHeader(http.StatusNoContent)
	}
}
