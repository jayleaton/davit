package harness

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	"davit/internal/domain"
	"davit/internal/state"
)

// Credential grants (G4). A VM's active grants reach every turn's harness
// as envVar=placeholder, and the provider's base URL variable points the
// harness at the VM's credential proxy, which swaps the placeholder for the
// real key on the way to the grant's upstream. The harness never sees a
// key. Grants take precedence over caller env and daemon secrets of the
// same name; the turn reports each such override (never a value).

// baseURLVar maps a grant to the base URL variable of its provider's
// clients and the path suffix they expect on it, by upstream host, else by
// the grant variable's prefix (an Anthropic- or OpenAI-compatible gateway).
func baseURLVar(g *domain.CredentialGrant) (name, suffix string, ok bool) {
	u, err := url.Parse(g.Upstream)
	if err != nil {
		return "", "", false
	}
	anthropic := u.Hostname() == "api.anthropic.com" || strings.HasPrefix(g.EnvVar, "ANTHROPIC_")
	openai := u.Hostname() == "api.openai.com" || strings.HasPrefix(g.EnvVar, "OPENAI_")
	switch {
	case anthropic && !openai:
		// Anthropic clients append /v1/... themselves.
		return "ANTHROPIC_BASE_URL", "", true
	case openai && !anthropic:
		// OpenAI clients (Codex) expect /v1 in the base URL; the proxy
		// appends the request path to the upstream, so a bare upstream
		// needs it here.
		if strings.Trim(u.Path, "/") == "" {
			suffix = "/v1"
		}
		return "OPENAI_BASE_URL", suffix, true
	}
	return "", "", false
}

// grantEnv returns the variables vm's active grants set (placeholders and
// proxy base URLs, in the form ad's CLI reads) and, when they replace a
// caller variable or one grant replaces another, a notice event for the
// turn. Without a credential proxy (proxyURL "") grants are not injected:
// a placeholder works only through it. A harness that cannot be pointed at
// the proxy gets no grants either, and the notice says so.
func grantEnv(tx *state.Tx, ad Adapter, vm domain.VMID, proxyURL string, caller map[string]string) (map[string]string, *Event, error) {
	if proxyURL == "" {
		return nil, nil, nil
	}
	gs, err := tx.Grants(vm)
	if err != nil {
		return nil, nil, err
	}
	env := map[string]string{}
	from := map[string]string{} // variable -> grant ID that set it
	var replaced []string       // "VAR (grant G replaces grant H)"
	for _, g := range gs {      // oldest first: the newest grant wins
		if g.State != domain.GrantActive || g.VMID != vm || !envName.MatchString(g.EnvVar) {
			continue
		}
		if prev, ok := from[g.EnvVar]; ok && env[g.EnvVar] != g.Placeholder {
			replaced = append(replaced, fmt.Sprintf("%s (grant %s replaces grant %s)", g.EnvVar, g.ID, prev))
		}
		env[g.EnvVar], from[g.EnvVar] = g.Placeholder, g.ID
		if name, suffix, ok := baseURLVar(g); ok {
			env[name], from[name] = strings.TrimRight(proxyURL, "/")+suffix, g.ID
		}
	}
	if r, ok := ad.(grantRouter); ok && len(env) > 0 {
		var routed bool
		if env, routed = r.routeGrants(env); !routed {
			raw, _ := json.Marshal(map[string]any{"type": "davit.notice", "code": "credential_grants_unsupported",
				"message": "credential grants are not applied to " + ad.Name() + ": it cannot be pointed at the credential proxy; pass the provider key with --secret or --env",
				"grants":  slices.Compact(slices.Sorted(maps.Values(from)))})
			return nil, &Event{Type: EventRaw, Raw: raw}, nil
		}
	}
	var overridden []string
	for name := range env {
		if _, ok := caller[name]; ok {
			overridden = append(overridden, name)
		}
	}
	if len(overridden) == 0 && len(replaced) == 0 {
		return env, nil, nil
	}
	slices.Sort(overridden)
	slices.Sort(replaced)
	msg := "credential grants take precedence"
	if len(overridden) > 0 {
		msg += "; replaced env/secret values of " + strings.Join(overridden, ", ")
	}
	if len(replaced) > 0 {
		msg += "; " + strings.Join(replaced, ", ")
	}
	raw, _ := json.Marshal(map[string]any{"type": "davit.notice", "code": "credential_env_conflict",
		"message": msg, "overridden": overridden, "grants": slices.Compact(slices.Sorted(maps.Values(from)))})
	return env, &Event{Type: EventRaw, Raw: raw}, nil
}
