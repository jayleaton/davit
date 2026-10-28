package preflight

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"davit/internal/buildinfo"
	"davit/internal/ci/github"
	"davit/internal/config"
)

// ciOff is the detail of the CI checks while ci.github is disabled.
const ciOff = "CI runners off (optional; ci.github)"

// CIChecks grade the GitHub Actions runner setup (docs/ci-runners.md): the
// App's private key, the webhook receiver and the runner image. They read
// local files only; GitHub itself is not contacted.
func CIChecks(o Options) []Check {
	return []Check{ciAppCheck(o.CI), ciWebhookCheck(o.CI), ciImageCheck(o)}
}

func ciAppCheck(g config.GitHubCI) Check {
	c := Check{ID: "ci_github_app", Title: "GitHub App key"}
	if !g.Enabled {
		c.Status, c.Detail = Skip, ciOff
		return c
	}
	k, err := github.LoadPrivateKey(g.PrivateKeyFile)
	if err != nil {
		c.Status, c.Detail = Fail, "ci.github.privateKeyFile: "+err.Error()
		c.Hint = "download a private key on the App's settings page (Private keys → Generate) to that path and chmod 600 it"
		return c
	}
	repos := 0
	for _, in := range g.Installations {
		repos += len(in.Repos)
	}
	c.Status = Pass
	c.Detail = fmt.Sprintf("App %d, RSA-%d key %s; %d installation(s), %d allowed repo(s)", g.AppID, k.N.BitLen(), g.PrivateKeyFile, len(g.Installations), repos)
	return c
}

func ciWebhookCheck(g config.GitHubCI) Check {
	c := Check{ID: "ci_github_webhook", Title: "GitHub webhook receiver"}
	if !g.Enabled {
		c.Status, c.Detail = Skip, ciOff
		return c
	}
	poll := time.Duration(g.PollInterval)
	if poll == 0 {
		poll = config.DefaultCIPoll
	}
	w := g.Webhook
	if w.Listen == "" {
		c.Status = Pass
		c.Detail = fmt.Sprintf("no webhook receiver: polling every %s finds queued jobs (no inbound connection needed)", poll)
		return c
	}
	fi, err := os.Stat(w.SecretFile)
	switch {
	case err != nil:
		c.Status, c.Detail = Fail, "ci.github.webhook.secretFile: "+err.Error()
		c.Hint = "write the App's webhook secret to that file (chmod 600)"
		return c
	case fi.Mode().Perm()&0o077 != 0:
		c.Status = Fail
		c.Detail = fmt.Sprintf("ci.github.webhook.secretFile %s is readable by others (mode %04o)", w.SecretFile, fi.Mode().Perm())
		c.Hint = "chmod 600 " + w.SecretFile
		return c
	case fi.Size() == 0:
		c.Status, c.Detail = Fail, "ci.github.webhook.secretFile "+w.SecretFile+" is empty"
		c.Hint = "write the App's webhook secret to that file"
		return c
	}
	c.Status = Pass
	c.Detail = fmt.Sprintf("receives signed workflow_job deliveries on %s", w.Listen)
	if poll > 0 {
		c.Detail += fmt.Sprintf("; polling every %s as a fallback", poll)
	}
	if host, _, err := net.SplitHostPort(w.Listen); err == nil {
		if a, err := netip.ParseAddr(host); host == "" || err == nil && a.IsUnspecified() {
			c.Status = Warn
			c.Detail += " (all interfaces)"
			c.Hint = "deliveries are only accepted signed, but listening on 127.0.0.1 behind a tunnel or reverse proxy keeps the port off the LAN"
		}
	}
	return c
}

// ciImageCheck looks for the runner image among the installed images.
func ciImageCheck(o Options) Check {
	c := Check{ID: "ci_image", Title: "CI runner image"}
	g := o.CI
	if !g.Enabled {
		c.Status, c.Detail = Skip, ciOff
		return c
	}
	want := g.Image
	if want == "" {
		want = config.DefaultCIImage
	}
	if !o.needsVM() {
		c.Status, c.Detail = Skip, fmt.Sprintf("runtime is %q", o.Runtime)
		return c
	}
	srcs := slices.Clone(o.Images)
	if len(srcs) == 0 {
		found, _ := filepath.Glob(filepath.Join(o.DataDir, "images", "manifests", "*.json"))
		for _, m := range found {
			srcs = append(srcs, config.ImageSource{Manifest: m})
		}
	}
	var names []string
	for _, s := range srcs {
		f := inspectImage(s)
		if f.Name == want {
			c.Status, c.Detail = Pass, want+" installed ("+s.Manifest+")"
			return c
		}
		if f.Name != "" {
			names = append(names, f.Name)
		}
	}
	c.Status = Fail
	c.Detail = fmt.Sprintf("ci.github.image %s is not installed (have: %s); every job's box would fail to start", want, strings.Join(names, ", "))
	c.Hint = fmt.Sprintf("build it (IMAGES=%s images/build.sh) and install it (`%s init --image MANIFEST`), or set ci.github.image", want, buildinfo.Name)
	return c
}
