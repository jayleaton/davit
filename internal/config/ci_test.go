package config

import (
	"strings"
	"testing"
	"time"
)

func TestCIValidate(t *testing.T) {
	ok := func() GitHubCI {
		return GitHubCI{Enabled: true, AppID: 1, PrivateKeyFile: "/etc/davit/app.pem",
			Installations: []GitHubInstallation{{ID: 9, Repos: []GitHubRepo{{Name: "acme/app"}}}}}
	}
	for name, c := range map[string]struct {
		mod  func(*GitHubCI)
		want string // "" valid
	}{
		"minimal":          {func(*GitHubCI) {}, ""},
		"off ignores rest": {func(g *GitHubCI) { *g = GitHubCI{AppID: -1} }, ""},
		"no app":           {func(g *GitHubCI) { g.AppID = 0 }, "appId"},
		"relative key":     {func(g *GitHubCI) { g.PrivateKeyFile = "app.pem" }, "privateKeyFile"},
		"no installations": {func(g *GitHubCI) { g.Installations = nil }, "installations"},
		"no repos":         {func(g *GitHubCI) { g.Installations[0].Repos = nil }, "lists no repos"},
		"bad repo":         {func(g *GitHubCI) { g.Installations[0].Repos[0].Name = "acme" }, "owner/name"},
		"dup repo": {func(g *GitHubCI) {
			g.Installations[0].Repos = append(g.Installations[0].Repos, GitHubRepo{Name: "ACME/app"})
		}, "twice"},
		"dup installation":  {func(g *GitHubCI) { g.Installations = append(g.Installations, g.Installations[0]) }, "unique"},
		"bad api url":       {func(g *GitHubCI) { g.APIURL = "ftp://x" }, "apiUrl"},
		"webhook no secret": {func(g *GitHubCI) { g.Webhook.Listen = "127.0.0.1:8475" }, "secretFile"},
		"webhook bad addr":  {func(g *GitHubCI) { g.Webhook = GitHubWebhook{Listen: "8475", SecretFile: "/s"} }, "host:port"},
		"webhook":           {func(g *GitHubCI) { g.Webhook = GitHubWebhook{Listen: "127.0.0.1:8475", SecretFile: "/s"} }, ""},
		"fast poll":         {func(g *GitHubCI) { g.PollInterval = Duration(time.Second) }, "at least 5s"},
		"never seen":        {func(g *GitHubCI) { g.PollInterval = Duration(-1) }, "no job would ever be seen"},
		"webhook only": {func(g *GitHubCI) {
			g.PollInterval, g.Webhook = Duration(-1), GitHubWebhook{Listen: ":8475", SecretFile: "/s"}
		}, ""},
		"shape": {func(g *GitHubCI) {
			g.Shapes = []CIShape{{Label: "davit-8c-16g", CPUs: 8, MemoryMiB: 16384, DiskGiB: 64}}
		}, ""},
		"shape no size":    {func(g *GitHubCI) { g.Shapes = []CIShape{{Label: "big"}} }, "needs cpus"},
		"shape builtin":    {func(g *GitHubCI) { g.Shapes = []CIShape{{Label: "Linux", CPUs: 1, MemoryMiB: 1, DiskGiB: 1}} }, "unique runner label"},
		"shape host label": {func(g *GitHubCI) { g.Shapes = []CIShape{{Label: "davit", CPUs: 1, MemoryMiB: 1, DiskGiB: 1}} }, "unique runner label"},
		"shape bad chars":  {func(g *GitHubCI) { g.Shapes = []CIShape{{Label: "a b", CPUs: 1, MemoryMiB: 1, DiskGiB: 1}} }, "unique runner label"},
		"unknown default":  {func(g *GitHubCI) { g.DefaultShape = "huge" }, "defaultShape"},
		"default builtin":  {func(g *GitHubCI) { g.DefaultShape = "davit-default" }, ""},
		"negative cap":     {func(g *GitHubCI) { g.MaxConcurrentJobs = -1 }, "maxConcurrentJobs"},
		"short timeout":    {func(g *GitHubCI) { g.JobTimeout = Duration(time.Second) }, "jobTimeout"},
		"long timeout":     {func(g *GitHubCI) { g.JobTimeout = Duration(6 * 24 * time.Hour) }, "jobTimeout"},
		"org scope":        {func(g *GitHubCI) { g.RunnerScope, g.RunnerGroupID = "org", 3 }, ""},
		"group needs org":  {func(g *GitHubCI) { g.RunnerGroupID = 3 }, "runnerScope"},
		"bad scope":        {func(g *GitHubCI) { g.RunnerScope = "enterprise" }, "runnerScope"},
		"bad prefix":       {func(g *GitHubCI) { g.RunnerNamePrefix = "a/b" }, "runnerNamePrefix"},
		"relative command": {func(g *GitHubCI) { g.RunnerCommand = []string{"run.sh"} }, "runnerCommand"},
		"bad host label":   {func(g *GitHubCI) { g.Label = "x y" }, "label"},
	} {
		g := ok()
		c.mod(&g)
		d := Defaults()
		d.DataDir, d.Socket, d.CI.GitHub = "/data", "/run/s.sock", g
		err := d.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want an error about %q", name, err, c.want)
		}
	}
}

func TestCIShapeFor(t *testing.T) {
	g := GitHubCI{Shapes: []CIShape{{Label: "Big", CPUs: 16, MemoryMiB: 65536, DiskGiB: 200}}}
	if s, ok := g.ShapeFor("big"); !ok || s.CPUs != 16 || s.Shape().MemoryBytes != 64<<30 || s.Shape().DiskBytes != 200<<30 {
		t.Fatalf("big: %+v %v", s, ok)
	}
	if s, ok := g.ShapeFor(""); !ok || s != DefaultCIShape {
		t.Fatalf("default: %+v %v", s, ok)
	}
	if _, ok := g.ShapeFor("tiny"); ok {
		t.Fatal("unknown shape found")
	}
}
