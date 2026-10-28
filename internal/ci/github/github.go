// Package github is the GitHub side of the CI runner controller
// (internal/ci): GitHub App authentication, the few REST calls the
// controller makes, and workflow_job webhook verification. It has no davit
// dependencies so that `davit doctor` can check an App key with it.
//
// The App authenticates with a short-lived RS256 JWT signed by its private
// key and exchanges it for installation access tokens (1 h), cached until
// shortly before they expire. Neither the key nor a token is ever logged or
// returned in an error.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAPIURL is github.com's REST API.
const DefaultAPIURL = "https://api.github.com"

// LoadPrivateKey reads a GitHub App private key (the PEM file GitHub
// generates: PKCS#1 "RSA PRIVATE KEY", or PKCS#8). It refuses a file that
// others can read: the key acts as the App on every installation.
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %04o); chmod 600 it", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// ParsePrivateKey parses a PEM-encoded RSA private key.
func ParsePrivateKey(b []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("not a PEM file (GitHub App private keys are .pem files)")
	}
	switch blk.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(blk.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("not an RSA key (GitHub App keys are RSA)")
		}
		return rk, nil
	}
	return nil, fmt.Errorf("PEM block %q is not a private key", blk.Type)
}

// Error is a GitHub API error response.
type Error struct {
	Status  int
	Message string
	// RetryAfter is set when GitHub asked the client to slow down
	// (secondary rate limit, or the primary limit is exhausted).
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("github: %d %s", e.Status, e.Message)
}

// IsStatus reports whether err is a GitHub API error with that status.
func IsStatus(err error, status int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == status
}

// App is a GitHub App's API client. It is safe for concurrent use.
type App struct {
	// BaseURL is the REST API root (default DefaultAPIURL; GitHub
	// Enterprise Server: https://HOST/api/v3).
	BaseURL string
	AppID   int64
	Key     *rsa.PrivateKey
	HTTP    *http.Client
	Now     func() time.Time

	mu     sync.Mutex
	tokens map[int64]token
}

type token struct {
	value   string
	expires time.Time
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) base() string {
	if a.BaseURL == "" {
		return DefaultAPIURL
	}
	return strings.TrimRight(a.BaseURL, "/")
}

func (a *App) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// JWT is the App's own bearer token: valid 9 minutes, issued a minute in
// the past against clock skew (GitHub's limits are 10 and 1).
func (a *App) JWT() (string, error) {
	if a.Key == nil {
		return "", errors.New("github: no App private key")
	}
	now := a.now()
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(a.AppID, 10)})
	if err != nil {
		return "", err
	}
	msg := head + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return msg + "." + enc.EncodeToString(sig), nil
}

// installationToken returns a cached access token for installation inst,
// fetching a new one when the cached one has less than 5 minutes left.
func (a *App) installationToken(ctx context.Context, inst int64) (string, error) {
	a.mu.Lock()
	t, ok := a.tokens[inst]
	a.mu.Unlock()
	if ok && a.now().Add(5*time.Minute).Before(t.expires) {
		return t.value, nil
	}
	jwt, err := a.JWT()
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.send(ctx, "Bearer "+jwt, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst), nil, &out); err != nil {
		return "", fmt.Errorf("installation %d access token: %w", inst, err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("installation %d access token: empty answer", inst)
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[int64]token{}
	}
	a.tokens[inst] = token{out.Token, out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}

// Installation is one App installation's API, as the installation.
type Installation struct {
	App *App
	ID  int64
}

// Installation returns the API of installation id.
func (a *App) Installation(id int64) Installation { return Installation{App: a, ID: id} }

func (in Installation) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := in.App.installationToken(ctx, in.ID)
	if err != nil {
		return err
	}
	return in.App.send(ctx, "token "+tok, method, path, body, out)
}

// send makes one API request. A 2xx answer decodes into out (when set);
// anything else is an *Error.
func (a *App) send(ctx context.Context, auth, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode, RetryAfter: retryAfter(resp, a.now())}
		var msg struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &msg) == nil && msg.Message != "" {
			e.Message = msg.Message
		} else {
			e.Message = http.StatusText(resp.StatusCode)
		}
		e.Message = method + " " + path + ": " + e.Message
		return e
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("github: %s %s: decode: %w", method, path, err)
	}
	return nil
}

// retryAfter reads GitHub's rate limit hints: Retry-After (seconds), or
// an exhausted primary limit's reset time.
func retryAfter(resp *http.Response, now time.Time) time.Duration {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if r, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if d := time.Unix(r, 0).Sub(now); d > 0 {
				return d
			}
		}
	}
	return 0
}

// Repository is the part of a repository the controller reads.
type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	// Visibility is public, private or internal (GitHub Enterprise).
	Visibility string `json:"visibility"`
	Owner      struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Public reports whether anyone can read the repository (and so open pull
// requests from forks against it).
func (r Repository) Public() bool {
	if r.Visibility != "" {
		return r.Visibility == "public"
	}
	return !r.Private
}

// Job is a workflow job (REST and the workflow_job webhook share it).
type Job struct {
	ID           int64      `json:"id"`
	RunID        int64      `json:"run_id"`
	Name         string     `json:"name"`
	WorkflowName string     `json:"workflow_name"`
	Status       string     `json:"status"` // queued, in_progress, completed, waiting, ...
	Conclusion   string     `json:"conclusion"`
	Labels       []string   `json:"labels"`
	RunnerID     int64      `json:"runner_id"`
	RunnerName   string     `json:"runner_name"`
	HeadBranch   string     `json:"head_branch"`
	HTMLURL      string     `json:"html_url"`
	CreatedAt    time.Time  `json:"created_at"`
	StartedAt    *time.Time `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at"`
}

// Run is the part of a workflow run the controller reads: what triggered
// it and where its code comes from.
type Run struct {
	ID             int64      `json:"id"`
	Event          string     `json:"event"`
	Status         string     `json:"status"`
	HeadBranch     string     `json:"head_branch"`
	Repository     Repository `json:"repository"`
	HeadRepository Repository `json:"head_repository"`
}

// FromFork reports whether the run builds code from another repository:
// a pull request (or pull_request_target, or a workflow_run it started)
// whose head lives in a fork.
func (r Run) FromFork() bool {
	return r.HeadRepository.FullName != "" && !strings.EqualFold(r.HeadRepository.FullName, r.Repository.FullName)
}

// Repo returns a repository ("owner/name").
func (in Installation) Repo(ctx context.Context, repo string) (Repository, error) {
	var r Repository
	err := in.do(ctx, http.MethodGet, "/repos/"+repo, nil, &r)
	return r, err
}

// WorkflowRun returns a run of repo.
func (in Installation) WorkflowRun(ctx context.Context, repo string, id int64) (Run, error) {
	var r Run
	err := in.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d", repo, id), nil, &r)
	return r, err
}

// maxPages bounds the pages one listing reads (100 items each).
const maxPages = 5

// Runs lists repo's workflow runs with status (queued, in_progress).
func (in Installation) Runs(ctx context.Context, repo, status string) ([]Run, error) {
	var out []Run
	for page := 1; page <= maxPages; page++ {
		var res struct {
			Total int   `json:"total_count"`
			Runs  []Run `json:"workflow_runs"`
		}
		q := url.Values{"status": {status}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := in.do(ctx, http.MethodGet, "/repos/"+repo+"/actions/runs?"+q.Encode(), nil, &res); err != nil {
			return out, err
		}
		out = append(out, res.Runs...)
		if len(res.Runs) < 100 || len(out) >= res.Total {
			break
		}
	}
	return out, nil
}

// RunJobs lists the jobs of a run's latest attempt.
func (in Installation) RunJobs(ctx context.Context, repo string, run int64) ([]Job, error) {
	var out []Job
	for page := 1; page <= maxPages; page++ {
		var res struct {
			Total int   `json:"total_count"`
			Jobs  []Job `json:"jobs"`
		}
		q := url.Values{"filter": {"latest"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := in.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?%s", repo, run, q.Encode()), nil, &res); err != nil {
			return out, err
		}
		out = append(out, res.Jobs...)
		if len(res.Jobs) < 100 || len(out) >= res.Total {
			break
		}
	}
	return out, nil
}

// Job returns one job of repo.
func (in Installation) Job(ctx context.Context, repo string, id int64) (Job, error) {
	var j Job
	err := in.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/jobs/%d", repo, id), nil, &j)
	return j, err
}

// Scope is where runners register: a repository ("owner/name") or, with
// Org set, an organization's runner group.
type Scope struct {
	Repo string
	Org  string
}

func (s Scope) path() string {
	if s.Org != "" {
		return "/orgs/" + s.Org
	}
	return "/repos/" + s.Repo
}

func (s Scope) String() string {
	if s.Org != "" {
		return "org " + s.Org
	}
	return s.Repo
}

// JITRequest asks for a single-use runner registration.
type JITRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int64    `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder,omitempty"`
}

// JITConfig is a just-in-time runner registration: Encoded is the secret
// `run.sh --jitconfig` takes. It registers the runner for one job; GitHub
// removes the runner once that job is done.
type JITConfig struct {
	RunnerID int64
	Encoded  string
}

// GenerateJITConfig registers a just-in-time runner in scope.
func (in Installation) GenerateJITConfig(ctx context.Context, scope Scope, req JITRequest) (JITConfig, error) {
	var out struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		Encoded string `json:"encoded_jit_config"`
	}
	if err := in.do(ctx, http.MethodPost, scope.path()+"/actions/runners/generate-jitconfig", req, &out); err != nil {
		return JITConfig{}, err
	}
	if out.Encoded == "" || out.Runner.ID == 0 {
		return JITConfig{}, errors.New("github: generate-jitconfig: incomplete answer")
	}
	return JITConfig{RunnerID: out.Runner.ID, Encoded: out.Encoded}, nil
}

// Runner is a registered self-hosted runner.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // online, offline
	Busy   bool   `json:"busy"`
}

// Runners lists the self-hosted runners registered in scope.
func (in Installation) Runners(ctx context.Context, scope Scope) ([]Runner, error) {
	var out []Runner
	for page := 1; page <= maxPages; page++ {
		var res struct {
			Total   int      `json:"total_count"`
			Runners []Runner `json:"runners"`
		}
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if err := in.do(ctx, http.MethodGet, scope.path()+"/actions/runners?"+q.Encode(), nil, &res); err != nil {
			return out, err
		}
		out = append(out, res.Runners...)
		if len(res.Runners) < 100 || len(out) >= res.Total {
			break
		}
	}
	return out, nil
}

// DeleteRunner removes a runner registration. A runner that is running a
// job cannot be removed (GitHub answers 422); one already gone is not an
// error.
func (in Installation) DeleteRunner(ctx context.Context, scope Scope, id int64) error {
	err := in.do(ctx, http.MethodDelete, fmt.Sprintf("%s/actions/runners/%d", scope.path(), id), nil, nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
