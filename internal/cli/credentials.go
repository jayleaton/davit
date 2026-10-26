package cli

import (
	"bytes"
	"context"
	"flag"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// credOpts are `davit credentials` flags.
type credOpts struct {
	kind, provider, envVar, baseURL string
	makeDefault                     bool
}

const credUsage = `profiles | profile create NAME [--default] | profile show PROFILE | profile delete PROFILE [--yes]
       | set PROFILE NAME --env VAR [--provider P | --base-url URL] (secret on stdin) | rm PROFILE NAME [--yes]
       | grant [ID] PROFILE NAME | grants [ID] | revoke [ID] GRANT_ID [--yes]`

func credFlags(fs *flag.FlagSet, o *opts) {
	c := &o.cred
	fs.StringVar(&c.kind, "kind", "", "credential kind: api_key (default) or custom_api (default when --base-url is set)")
	fs.StringVar(&c.provider, "provider", "", "provider of an api_key: anthropic or openai (sets the upstream)")
	fs.StringVar(&c.envVar, "env", "", "guest variable that carries the grant's placeholder, e.g. ANTHROPIC_API_KEY")
	fs.StringVar(&c.baseURL, "base-url", "", "upstream base URL (custom_api, or to override a provider's)")
	fs.BoolVar(&c.makeDefault, "default", false, "make the new profile the default")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation (delete, rm, revoke)")
	fs.StringVar(&o.key, "idempotency-key", "", "deduplicate separate invocations (create, grant)")
	fs.IntVar(&o.limit, "limit", 0, "page size (profiles)")
	fs.StringVar(&o.cursor, "cursor", "", "page cursor (profiles)")
}

func runCredentials(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) == 0 {
		return usageErr("usage: %s credentials %s", buildinfo.Name, credUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "profiles":
		return s.credProfiles(ctx, o)
	case "profile":
		return s.credProfile(ctx, o, rest)
	case "set":
		return s.credSet(ctx, o, rest)
	case "rm":
		if len(rest) != 2 {
			return usageErr("usage: %s credentials rm PROFILE NAME [--yes]", buildinfo.Name)
		}
		if err := s.confirm(o, "remove credential "+rest[1]+" of "+rest[0]+" (its grants are revoked)?"); err != nil {
			return err
		}
		if err := s.cl.do(ctx, http.MethodDelete, credPath(rest[0], rest[1]), nil, nil, nil, nil); err != nil {
			return err
		}
		s.out.result("", map[string]string{"profile": rest[0], "credential": rest[1], "removed": "true"})
		s.out.human("removed %s/%s", rest[0], rest[1])
		return nil
	case "grant", "grants", "revoke":
		return s.credGrants(ctx, o, sub, rest)
	}
	return usageErr("unknown credentials subcommand %q (profiles, profile, set, rm, grant, grants, revoke)", sub)
}

func profilePath(p string) string { return "/credential-profiles/" + url.PathEscape(p) }

func credPath(p, name string) string { return profilePath(p) + "/credentials/" + url.PathEscape(name) }

func (s *session) credProfiles(ctx context.Context, o *opts) error {
	q := url.Values{}
	if o.limit != 0 {
		q.Set("limit", strconv.Itoa(o.limit))
	}
	if o.cursor != "" {
		q.Set("cursor", o.cursor)
	}
	var l api.CredentialProfileList
	if err := s.cl.get(ctx, "/credential-profiles", q, &l); err != nil {
		return err
	}
	for _, p := range l.Items {
		s.out.emit("item", "", 0, p, nil)
		def := ""
		if p.Default {
			def = " (default)"
		}
		s.out.human("%-24s %s%s, %d credentials", p.ID, p.Name, def, len(p.Credentials))
	}
	s.out.result("", map[string]any{"count": len(l.Items), "nextCursor": l.NextCursor})
	return nil
}

func (s *session) credProfile(ctx context.Context, o *opts, args []string) error {
	if len(args) != 2 {
		return usageErr("usage: %s credentials profile create NAME [--default] | show PROFILE | delete PROFILE [--yes]", buildinfo.Name)
	}
	switch args[0] {
	case "create":
		key := o.key
		if key == "" {
			key = newIdempotencyKey()
		}
		var p api.CredentialProfile
		req := api.CreateCredentialProfileRequest{Name: args[1], Default: o.cred.makeDefault}
		if err := s.cl.do(ctx, http.MethodPost, "/credential-profiles", nil, req, map[string]string{api.HeaderIdempotencyKey: key}, &p); err != nil {
			return err
		}
		s.out.result("", p)
		s.out.human("created profile %s (%s)", p.Name, p.ID)
		return nil
	case "show":
		var p api.CredentialProfile
		if err := s.cl.get(ctx, profilePath(args[1]), nil, &p); err != nil {
			return err
		}
		s.out.result("", p)
		printProfile(s.out, &p)
		return nil
	case "delete":
		if err := s.confirm(o, "delete credential profile "+args[1]+" (its grants are revoked, its secrets deleted)?"); err != nil {
			return err
		}
		if err := s.cl.do(ctx, http.MethodDelete, profilePath(args[1]), nil, nil, nil, nil); err != nil {
			return err
		}
		s.out.result("", map[string]string{"profile": args[1], "deleted": "true"})
		s.out.human("deleted profile %s", args[1])
		return nil
	}
	return usageErr("unknown credentials profile subcommand %q (create, show, delete)", args[0])
}

func printProfile(p *printer, cp *api.CredentialProfile) {
	p.human("%s (%s)", cp.Name, cp.ID)
	for _, c := range cp.Credentials {
		up := c.BaseURL
		if up == "" {
			up = c.Provider
		}
		secret := "no secret"
		if c.HasSecret {
			secret = "secret set"
		}
		p.human("  %-20s %-10s %-24s %s, %s", c.Name, c.Kind, c.EnvVar, up, secret)
	}
}

// credSet reads the secret from stdin only: never from a flag, where it
// would land in shell history and the process list.
func (s *session) credSet(ctx context.Context, o *opts, args []string) error {
	if len(args) != 2 {
		return usageErr("usage: %s credentials set PROFILE NAME --env VAR [--provider P | --base-url URL] < secret", buildinfo.Name)
	}
	c := o.cred
	if c.envVar == "" {
		return usageErr("credentials set needs --env (the guest variable for the placeholder)")
	}
	kind := domain.CredentialKind(c.kind)
	if kind == "" {
		kind = domain.CredentialAPIKey
		if c.baseURL != "" && c.provider == "" {
			kind = domain.CredentialCustomAPI
		}
	}
	secret, err := io.ReadAll(io.LimitReader(s.io.Stdin, 64<<10))
	if err != nil {
		return clientErr(domain.CodeInvalidArgument, "reading the secret from stdin: %v", err)
	}
	secret = bytes.TrimRight(secret, "\r\n")
	req := api.PutCredentialRequest{Kind: kind, Provider: c.provider, BaseURL: c.baseURL, EnvVar: c.envVar, Secret: string(secret)}
	var p api.CredentialProfile
	if err := s.cl.do(ctx, http.MethodPut, credPath(args[0], args[1]), nil, req, nil, &p); err != nil {
		return err
	}
	s.out.result("", p)
	if len(secret) == 0 {
		s.out.human("updated %s/%s (secret unchanged)", args[0], args[1])
	} else {
		s.out.human("set %s/%s", args[0], args[1])
	}
	return nil
}

func (s *session) credGrants(ctx context.Context, o *opts, sub string, args []string) error {
	want := map[string]int{"grant": 2, "grants": 0, "revoke": 1}[sub]
	usage := map[string]string{"grant": "grant [ID] PROFILE NAME", "grants": "grants [ID]", "revoke": "revoke [ID] GRANT_ID [--yes]"}[sub]
	var ref string
	switch len(args) {
	case want:
	case want + 1:
		ref, args = args[0], args[1:]
	default:
		return usageErr("usage: %s credentials %s", buildinfo.Name, usage)
	}
	if ref == "" {
		id, err := s.currentID(nil)
		if err != nil {
			return err
		}
		ref = id
	}
	base := vmPath(ref) + "/credential-grants"
	switch sub {
	case "grant":
		key := o.key
		if key == "" {
			key = newIdempotencyKey()
		}
		var g api.CredentialGrant
		req := api.CreateCredentialGrantRequest{Profile: args[0], Credential: args[1]}
		if err := s.cl.do(ctx, http.MethodPost, base, nil, req, map[string]string{api.HeaderIdempotencyKey: key}, &g); err != nil {
			return err
		}
		s.out.result("", g)
		s.out.human("%s=%s", g.EnvVar, g.Placeholder)
		s.out.diag("grant %s: in the VM, set %s to the placeholder above and point the client at the credential proxy, http://172.16.0.1:15080 (e.g. ANTHROPIC_BASE_URL); requests go to %s with the real key", g.ID, g.EnvVar, g.Upstream)
		return nil
	case "grants":
		var l api.CredentialGrantList
		if err := s.cl.get(ctx, base, nil, &l); err != nil {
			return err
		}
		for _, g := range l.Items {
			s.out.emit("item", "", 0, g, nil)
			state := string(g.State)
			if g.RevokeReason != "" {
				state += " (" + string(g.RevokeReason) + ")"
			}
			s.out.human("%-26s %-20s %-24s %-32s %s", g.ID, g.ProfileID+"/"+g.Credential, g.EnvVar, g.Upstream, state)
		}
		s.out.result("", map[string]any{"count": len(l.Items)})
		return nil
	}
	if err := s.confirm(o, "revoke credential grant "+args[0]+"?"); err != nil {
		return err
	}
	var g api.CredentialGrant
	if err := s.cl.do(ctx, http.MethodDelete, base+"/"+url.PathEscape(args[0]), nil, nil, nil, &g); err != nil {
		return err
	}
	s.out.result("", g)
	s.out.human("revoked %s", g.ID)
	return nil
}
