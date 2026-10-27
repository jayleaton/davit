package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/config"
)

// runWhoami shows who the caller is to the daemon (GET /me) and how this
// client reaches it. It never prints a secret: a token is shown by ID,
// name and scopes, and the client side only says where its token came from.
func runWhoami(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 0, "whoami"); err != nil {
		return err
	}
	var me api.Me
	if err := s.cl.get(ctx, "/me", nil, &me); err != nil {
		return err
	}
	endpoint := redactURL(s.g.apiURL)
	tokenSource := ""
	if s.cl.token != "" {
		tokenSource = buildinfo.Env("TOKEN")
	}
	s.out.result("", map[string]any{"me": me, "endpoint": endpoint, "tokenSource": tokenSource})
	s.out.human("principal  %s", me.Principal)
	auth := me.Auth
	if me.Identity != "" {
		auth += " (" + me.Identity + ")"
	}
	s.out.human("auth       %s", auth)
	if t := me.Token; t != nil {
		name := t.ID
		if t.Name != "" {
			name += " (" + t.Name + ")"
		}
		s.out.human("token      %s, from %s", name, tokenSource)
		for _, sc := range t.Scopes {
			vm := "all VMs"
			if sc.VMID != "" {
				vm = string(sc.VMID)
			}
			s.out.human("  scope    %s on %s", strings.Join(sc.Actions, ","), vm)
		}
		if !t.ExpiresAt.IsZero() {
			s.out.human("  expires  %s", t.ExpiresAt.Format(time.RFC3339))
		}
	}
	if m := me.Member; m != nil {
		s.out.human("member     %s (%s)", m.Login, m.ID)
		if len(m.Scopes)+len(m.Snapshots)+len(m.Environments) == 0 {
			s.out.human("  nothing is shared with you yet")
		}
		for _, sc := range m.Scopes {
			vm := "every VM shared with all members"
			if sc.VMID != "" {
				vm = string(sc.VMID)
			}
			s.out.human("  vm       %s: %s", vm, memberRole(sc.Actions))
		}
		for _, id := range slices.Sorted(maps.Keys(m.Snapshots)) {
			s.out.human("  snapshot %s: %s", id, memberRole([]string{m.Snapshots[id]}))
		}
		for _, id := range slices.Sorted(maps.Keys(m.Environments)) {
			s.out.human("  env      %s: %s", id, memberRole([]string{m.Environments[id]}))
		}
	}
	s.out.human("endpoint   %s", endpoint)
	s.out.human("daemon     %s %s", me.Host, me.Version)
	return nil
}

// versionCheckTimeout bounds the release index fetch (offline-safe: a
// slow or absent network gives an "unknown" answer, never a hang).
const versionCheckTimeout = 5 * time.Second

// versionRe is what install.sh accepts as a release tag.
var versionRe = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)

// latestReleaseURL is GitHub's "latest release" page, which redirects to the
// latest release's tag; a variable so tests stay offline.
var latestReleaseURL = "https://github.com/" + buildinfo.Repo + "/releases/latest"

// releasesAPIURL lists the published releases newest first, prereleases
// included: the answer while a repository has only prereleases, which the
// latest-release page skips.
var releasesAPIURL = "https://api.github.com/repos/" + buildinfo.Repo + "/releases?per_page=1"

// errNoLatestRelease: the latest-release page names no tag (nothing but
// prereleases, or nothing published).
var errNoLatestRelease = errors.New("no release published yet")

// VersionCheck is the result of `davit version --check`.
type VersionCheck struct {
	Current string `json:"current"`
	Latest  string `json:"latest,omitempty"`
	// Status is up-to-date, update-available, ahead, different or unknown.
	Status string `json:"status"`
	Source string `json:"source,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// versionCheck compares this build with the latest release: the published
// GitHub releases, or the release directory in DAVIT_RELEASE_URL (else the
// config's releaseBaseUrl). It only reports; updating is a rerun of
// install.sh.
func versionCheck(ctx context.Context, out *printer, configPath string) int {
	vc := VersionCheck{Current: buildinfo.Version}
	src := strings.TrimSpace(os.Getenv(buildinfo.Env("RELEASE_URL")))
	if src == "" {
		if cfg, _, err := config.LoadDefault(configPath); err == nil {
			src = cfg.ReleaseBaseURL
		}
	}
	var latest string
	var err error
	if src == "" {
		vc.Source = latestReleaseURL
		latest, err = latestGitHubTag(ctx, vc.Source)
		if errors.Is(err, errNoLatestRelease) {
			vc.Source = releasesAPIURL
			latest, err = newestGitHubRelease(ctx, vc.Source)
		}
	} else {
		vc.Source = strings.TrimRight(src, "/") + "/release.json"
		latest, err = releaseDirVersion(ctx, vc.Source)
	}
	if err != nil {
		vc.Status, vc.Detail = "unknown", err.Error()
	} else {
		vc.Latest = latest
		vc.Status = compareVersions(vc.Current, latest)
	}
	out.result("", vc)
	switch vc.Status {
	case "unknown":
		out.human("%s %s: %s", buildinfo.Name, vc.Current, vc.Detail)
	case "up-to-date":
		out.human("%s %s is the latest release", buildinfo.Name, vc.Current)
	case "update-available":
		out.human("%s %s: release %s is available (%s); rerun install.sh to update, nothing is installed automatically", buildinfo.Name, vc.Current, vc.Latest, vc.Source)
	default:
		out.human("%s %s: the latest release is %s (%s)", buildinfo.Name, vc.Current, vc.Latest, vc.Status)
	}
	return ExitOK
}

// latestGitHubTag reads the tag GitHub's latest-release page redirects to
// (.../releases/tag/<tag>), without following it.
func latestGitHubTag(ctx context.Context, src string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, src, nil)
	if err != nil {
		return "", err
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("releases unreachable (offline?): %v", err)
	}
	resp.Body.Close()
	_, tag, ok := strings.Cut(resp.Header.Get("Location"), "/releases/tag/")
	if !ok || !versionRe.MatchString(tag) {
		return "", fmt.Errorf("%s: HTTP %d, %w", src, resp.StatusCode, errNoLatestRelease)
	}
	return tag, nil
}

// newestGitHubRelease reads the newest published release, a prerelease
// included, from GitHub's releases API.
func newestGitHubRelease(ctx context.Context, src string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("releases unreachable (offline?): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", src, resp.StatusCode)
	}
	var rels []struct {
		TagName string `json:"tag_name"`
		Draft   bool   `json:"draft"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rels); err != nil {
		return "", fmt.Errorf("%s: %v", src, err)
	}
	for _, r := range rels {
		if !r.Draft && versionRe.MatchString(r.TagName) {
			return r.TagName, nil
		}
	}
	return "", fmt.Errorf("%s: %w", src, errNoLatestRelease)
}

// releaseDirVersion reads the version of a release directory (`make release`
// output) from its release.json, over https or from file://.
func releaseDirVersion(ctx context.Context, src string) (string, error) {
	u, err := url.Parse(src)
	if err != nil {
		return "", fmt.Errorf("release %q: %v", src, err)
	}
	var r io.ReadCloser
	switch u.Scheme {
	case "file":
		f, err := os.Open(u.Path)
		if err != nil {
			return "", fmt.Errorf("release: %v", err)
		}
		r = f
	case "https":
		ctx, cancel := context.WithTimeout(ctx, versionCheckTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("release unreachable (offline?): %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", fmt.Errorf("release %s: HTTP %d", src, resp.StatusCode)
		}
		r = resp.Body
	default:
		return "", fmt.Errorf("release %q: want https:// or file://", src)
	}
	defer r.Close()
	var rel struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&rel); err != nil {
		return "", fmt.Errorf("release %s: %v", src, err)
	}
	if !versionRe.MatchString(rel.Version) {
		return "", fmt.Errorf("release %s does not name a version", src)
	}
	return rel.Version, nil
}

// compareVersions compares release names: equal (ignoring a leading v) is
// up-to-date; two semantic versions are ordered (a prerelease sorts before
// its release); anything else is just "different".
func compareVersions(current, latest string) string {
	c, l := strings.TrimPrefix(current, "v"), strings.TrimPrefix(latest, "v")
	if c == l {
		return "up-to-date"
	}
	cv, okc := parseSemver(c)
	lv, okl := parseSemver(l)
	if !okc || !okl {
		return "different"
	}
	for i := range 3 {
		if cv.n[i] != lv.n[i] {
			if cv.n[i] < lv.n[i] {
				return "update-available"
			}
			return "ahead"
		}
	}
	switch {
	case cv.pre == lv.pre:
		return "up-to-date"
	case cv.pre != "" && (lv.pre == "" || cv.pre < lv.pre):
		return "update-available"
	}
	return "ahead"
}

type semver struct {
	n   [3]int
	pre string
}

func parseSemver(s string) (semver, bool) {
	s, _, _ = strings.Cut(s, "+")
	core, pre, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var v semver
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, false
		}
		v.n[i] = n
	}
	v.pre = pre
	return v, true
}

// memberRole names the strongest of a share's actions as a member role.
func memberRole(actions []string) string {
	role := ""
	for _, a := range actions {
		switch a {
		case api.TokenAdmin:
			return string(api.RoleAdmin)
		case api.TokenExec:
			role = string(api.RoleExec)
		case api.TokenView:
			if role == "" {
				role = string(api.RoleViewer)
			}
		}
	}
	return role
}
