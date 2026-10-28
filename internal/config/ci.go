package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"davit/internal/domain"
)

// CI configures CI runners on this host (docs/ci-runners.md).
type CI struct {
	GitHub GitHubCI `json:"github,omitempty"`
}

// GitHubCI configures ephemeral GitHub Actions runners: one fresh box per
// queued workflow job, deleted when the job ends. Off unless enabled.
type GitHubCI struct {
	Enabled bool `json:"enabled,omitempty"`
	// APIURL is the GitHub REST API (default https://api.github.com;
	// GitHub Enterprise Server: https://HOST/api/v3).
	APIURL string `json:"apiUrl,omitempty"`
	// AppID and PrivateKeyFile are the GitHub App the host acts as. The key
	// file (the .pem GitHub generates) must be owner-only (0600).
	AppID          int64  `json:"appId,omitempty"`
	PrivateKeyFile string `json:"privateKeyFile,omitempty"`
	// Installations are the App's installations to serve, each with the
	// repositories that may run jobs here. Jobs from any other repository
	// are refused.
	Installations []GitHubInstallation `json:"installations,omitempty"`
	// Webhook receives workflow_job deliveries; optional, since polling
	// finds queued jobs without any inbound connection.
	Webhook GitHubWebhook `json:"webhook,omitempty"`
	// PollInterval is how often each repository is asked for queued jobs
	// (default 30s; negative: never, webhooks only).
	PollInterval Duration `json:"pollInterval,omitempty"`
	// Image is the image runner boxes boot (default ubuntu-24.04-ci).
	Image string `json:"image,omitempty"`
	// Label is the runs-on label that marks a job for this host (default
	// "davit"). A job must also ask for self-hosted.
	Label string `json:"label,omitempty"`
	// Shapes are the box sizes a job can ask for with one more runs-on
	// label; DefaultShape names the one a job without such a label gets
	// (default: the built-in "davit-default", 4 CPUs, 8 GiB, 64 GiB disk).
	Shapes       []CIShape `json:"shapes,omitempty"`
	DefaultShape string    `json:"defaultShape,omitempty"`
	// MaxConcurrentJobs caps the runner boxes alive at once (default 4);
	// later jobs wait in a FIFO queue. Host admission (memory, disk floor)
	// applies on top.
	MaxConcurrentJobs int `json:"maxConcurrentJobs,omitempty"`
	// JobTimeout is how long a runner may run before its box is deleted
	// (default 6h, GitHub's own job limit).
	JobTimeout Duration `json:"jobTimeout,omitempty"`
	// RunnerScope is "repo" (the default: each runner registers in the
	// job's repository) or "org" (in the organization's runner group
	// RunnerGroupID; the group must be limited to the allowed repositories).
	RunnerScope   string `json:"runnerScope,omitempty"`
	RunnerGroupID int64  `json:"runnerGroupId,omitempty"`
	// RunnerNamePrefix starts every runner name this host registers
	// (default "davit-" and the host name). Restart cleanup removes idle
	// runners with this prefix, so two hosts must not share it.
	RunnerNamePrefix string `json:"runnerNamePrefix,omitempty"`
	// RunnerCommand and RunnerUser run the runner in the box (default
	// /opt/actions-runner/run.sh as "runner", the image's layout).
	RunnerCommand []string `json:"runnerCommand,omitempty"`
	RunnerUser    string   `json:"runnerUser,omitempty"`
}

// GitHubInstallation is one installation of the App and its allowed
// repositories.
type GitHubInstallation struct {
	ID    int64        `json:"id"`
	Repos []GitHubRepo `json:"repos"`
}

// GitHubRepo is a repository allowed to run jobs on this host.
type GitHubRepo struct {
	// Name is "owner/name".
	Name string `json:"name"`
	// AllowPublic lets a public repository run jobs here: anyone can open a
	// pull request against it. Off, its jobs are refused.
	AllowPublic bool `json:"allowPublic,omitempty"`
	// AllowForks runs jobs of workflow runs whose code comes from a fork
	// (pull requests from forks). Off, they are refused.
	AllowForks bool `json:"allowForks,omitempty"`
}

// GitHubWebhook is the workflow_job webhook receiver.
type GitHubWebhook struct {
	// Listen is the address it serves on ("127.0.0.1:8475"); empty: no
	// receiver. Put it behind a tunnel or reverse proxy that GitHub can
	// reach; deliveries are only accepted with a valid signature.
	Listen string `json:"listen,omitempty"`
	// SecretFile holds the webhook secret (owner-only, 0600).
	SecretFile string `json:"secretFile,omitempty"`
}

// CIShape is a runner box size and the runs-on label that selects it.
type CIShape struct {
	Label     string `json:"label"`
	CPUs      int    `json:"cpus"`
	MemoryMiB int64  `json:"memoryMiB"`
	DiskGiB   int64  `json:"diskGiB"`
}

// Shape is s as a VM shape.
func (s CIShape) Shape() domain.Shape {
	return domain.Shape{CPUs: s.CPUs, MemoryBytes: s.MemoryMiB * domain.MiB, DiskBytes: s.DiskGiB * domain.GiB}
}

// CI defaults.
const (
	DefaultCIImage       = "ubuntu-24.04-ci"
	DefaultCILabel       = "davit"
	DefaultCIPoll        = 30 * time.Second
	DefaultCIConcurrency = 4
	DefaultCIJobTimeout  = 6 * time.Hour
	DefaultCIRunnerUser  = "runner"
)

// DefaultCIShape is the shape of a job that asks for none, unless
// defaultShape names another.
var DefaultCIShape = CIShape{Label: "davit-default", CPUs: 4, MemoryMiB: 8 << 10, DiskGiB: 64}

// DefaultCIRunnerCommand is the runner's start script in ubuntu-24.04-ci.
var DefaultCIRunnerCommand = []string{"/opt/actions-runner/run.sh"}

// MaxCIJobTimeout bounds jobTimeout (GitHub ends a job after 6 h on its
// hosted runners and 5 days on self-hosted ones).
const MaxCIJobTimeout = 5 * 24 * time.Hour

// ShapeFor returns the configured shape labelled label ("" or the default
// label: the default shape).
func (g GitHubCI) ShapeFor(label string) (CIShape, bool) {
	if label == "" {
		label = g.DefaultShape
	}
	for _, s := range g.Shapes {
		if strings.EqualFold(s.Label, label) {
			return s, true
		}
	}
	if label == "" || strings.EqualFold(label, DefaultCIShape.Label) {
		return DefaultCIShape, true
	}
	return CIShape{}, false
}

// Validate checks the CI section.
func (c CI) Validate() error {
	g := c.GitHub
	if !g.Enabled {
		return nil
	}
	pre := "config: ci.github."
	if g.AppID <= 0 {
		return fmt.Errorf("%sappId is required (the GitHub App's ID)", pre)
	}
	if g.PrivateKeyFile == "" || !filepath.IsAbs(g.PrivateKeyFile) {
		return fmt.Errorf("%sprivateKeyFile must be an absolute path (the App's .pem key)", pre)
	}
	if g.APIURL != "" {
		if u, err := url.Parse(g.APIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%sapiUrl must be an http(s) URL", pre)
		}
	}
	if len(g.Installations) == 0 {
		return fmt.Errorf("%sinstallations must list at least one installation and its repos", pre)
	}
	seenInst, seenRepo := map[int64]bool{}, map[string]bool{}
	for _, in := range g.Installations {
		if in.ID <= 0 || seenInst[in.ID] {
			return fmt.Errorf("%sinstallations: id %d must be a unique positive installation ID", pre, in.ID)
		}
		seenInst[in.ID] = true
		if len(in.Repos) == 0 {
			return fmt.Errorf("%sinstallations: installation %d lists no repos", pre, in.ID)
		}
		for _, r := range in.Repos {
			owner, name, ok := strings.Cut(r.Name, "/")
			if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
				return fmt.Errorf("%sinstallations: repo %q must be owner/name", pre, r.Name)
			}
			k := strings.ToLower(r.Name)
			if seenRepo[k] {
				return fmt.Errorf("%sinstallations: repo %q is listed twice", pre, r.Name)
			}
			seenRepo[k] = true
		}
	}
	if w := g.Webhook; w.Listen != "" || w.SecretFile != "" {
		if _, _, err := net.SplitHostPort(w.Listen); err != nil {
			return fmt.Errorf("%swebhook.listen must be host:port: %v", pre, err)
		}
		if w.SecretFile == "" || !filepath.IsAbs(w.SecretFile) {
			return fmt.Errorf("%swebhook.secretFile must be an absolute path (deliveries are only accepted signed)", pre)
		}
	}
	if g.PollInterval > 0 && g.PollInterval < Duration(5*time.Second) {
		return fmt.Errorf("%spollInterval must be at least 5s (0: the default, %s; negative: webhooks only)", pre, DefaultCIPoll)
	}
	if g.PollInterval < 0 && g.Webhook.Listen == "" {
		return fmt.Errorf("%spollInterval is negative (no polling) but no webhook.listen is set: no job would ever be seen", pre)
	}
	if g.Label != "" && !validLabel(g.Label) {
		return fmt.Errorf("%slabel %q must be a runner label (letters, digits, '-', '_', '.')", pre, g.Label)
	}
	labels := map[string]bool{}
	for _, s := range g.Shapes {
		k := strings.ToLower(s.Label)
		if !validLabel(s.Label) || labels[k] || reservedCILabel(k, g.Label) {
			return fmt.Errorf("%sshapes: label %q must be a unique runner label, not self-hosted, linux, x64, arm64 or the host label", pre, s.Label)
		}
		labels[k] = true
		if s.CPUs <= 0 || s.MemoryMiB <= 0 || s.DiskGiB <= 0 {
			return fmt.Errorf("%sshapes: %q needs cpus, memoryMiB and diskGiB", pre, s.Label)
		}
	}
	if _, ok := g.ShapeFor(""); !ok {
		return fmt.Errorf("%sdefaultShape %q is not one of shapes", pre, g.DefaultShape)
	}
	if g.MaxConcurrentJobs < 0 {
		return fmt.Errorf("%smaxConcurrentJobs must not be negative (0: the default, %d)", pre, DefaultCIConcurrency)
	}
	if g.JobTimeout < 0 || g.JobTimeout > Duration(MaxCIJobTimeout) || (g.JobTimeout > 0 && g.JobTimeout < Duration(time.Minute)) {
		return fmt.Errorf("%sjobTimeout must be 1m..%s (0: the default, %s)", pre, MaxCIJobTimeout, DefaultCIJobTimeout)
	}
	switch g.RunnerScope {
	case "", "repo":
		if g.RunnerGroupID != 0 {
			return fmt.Errorf("%srunnerGroupId needs runnerScope \"org\"", pre)
		}
	case "org":
		if g.RunnerGroupID < 0 {
			return fmt.Errorf("%srunnerGroupId must be positive (0: the default group, 1)", pre)
		}
	default:
		return fmt.Errorf("%srunnerScope must be \"repo\" or \"org\"", pre)
	}
	if p := g.RunnerNamePrefix; p != "" && !validLabel(p) {
		return fmt.Errorf("%srunnerNamePrefix %q may only hold letters, digits, '-', '_' and '.'", pre, p)
	}
	if len(g.RunnerCommand) > 0 && !filepath.IsAbs(g.RunnerCommand[0]) {
		return fmt.Errorf("%srunnerCommand must start with an absolute path in the box", pre)
	}
	return nil
}

func validLabel(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// CIBuiltinLabels are the labels GitHub gives every self-hosted runner
// here, beside the host label, a shape label and the architecture (x64,
// arm64). GitHub matches labels case-insensitively.
var CIBuiltinLabels = []string{"self-hosted", "linux"}

func reservedCILabel(k, hostLabel string) bool {
	if hostLabel == "" {
		hostLabel = DefaultCILabel
	}
	return slices.Contains(CIBuiltinLabels, k) || k == "x64" || k == "arm64" || k == strings.ToLower(hostLabel)
}
