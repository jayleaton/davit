package cli

import (
	"context"
	"flag"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// tokenOpts are `davit token` flags.
type tokenOpts struct {
	vms     multiFlag
	actions multiFlag
	ttl     string
	preset  string        // tokens_ops.go
	overlap time.Duration // tokens_ops.go
}

func tokenFlags(fs *flag.FlagSet, o *opts) {
	fs.Var(&o.token.vms, "vm", "limit the token to this VM (repeatable; default: all VMs)")
	fs.Var(&o.token.actions, "action", "view, exec or admin (repeatable; exec implies view, admin implies both)")
	fs.StringVar(&o.token.ttl, "ttl", "", "lifetime, e.g. 12h or 30d (at most 365d; create default 30d, rotate default the old token's)")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation (revoke)")
	fs.StringVar(&o.key, "idempotency-key", "", "deduplicate separate invocations (create)")
	tokenOpsFlags(fs, o)
}

// parseLifetime accepts Go durations plus a whole-day suffix (30d).
func parseLifetime(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 1 {
			return 0, usageErr("--ttl %q: want e.g. 12h or 30d", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < time.Second {
		return 0, usageErr("--ttl %q: want e.g. 12h or 30d", s)
	}
	return v, nil
}

func runToken(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) == 0 {
		return usageErr("usage: %s token create NAME --action ACTION [--vm VM]... [--ttl 30d] | list | revoke TOKEN_ID", buildinfo.Name)
	}
	switch args[0] {
	case "create":
		if len(args) != 2 {
			return usageErr("usage: %s token create NAME --action ACTION [--vm VM]... [--ttl 30d]", buildinfo.Name)
		}
		if err := applyTokenPreset(o); err != nil {
			return err
		}
		if len(o.token.actions) == 0 {
			return usageErr("token create needs at least one --action (view, exec or admin) or a --preset")
		}
		if o.token.ttl == "" {
			o.token.ttl = "30d"
		}
		ttl, err := parseLifetime(o.token.ttl)
		if err != nil {
			return err
		}
		req := api.CreateTokenRequest{Name: args[1], TTLSeconds: int64(ttl / time.Second)}
		if len(o.token.vms) == 0 {
			req.Scopes = []api.TokenScope{{Actions: o.token.actions}}
		}
		for _, vm := range o.token.vms {
			req.Scopes = append(req.Scopes, api.TokenScope{VMID: domain.VMID(vm), Actions: o.token.actions})
		}
		key := o.key
		if key == "" {
			key = newIdempotencyKey()
		}
		var t api.CreatedToken
		if err := s.cl.do(ctx, http.MethodPost, "/tokens", nil, req, map[string]string{api.HeaderIdempotencyKey: key}, &t); err != nil {
			return err
		}
		s.out.result("", t)
		s.out.human("%s", t.Secret)
		s.out.diag("token %s (%s), expires %s. The secret above is shown only once: store it now.", t.ID, t.Name, t.ExpiresAt.Format(time.RFC3339))
		s.out.diag("use it from another tailnet host: %s=<secret> %s --host <this host> ...", buildinfo.Env("TOKEN"), buildinfo.Name)
		return nil
	case "list":
		if len(args) != 1 {
			return usageErr("usage: %s token list", buildinfo.Name)
		}
		q := url.Values{}
		if o.limit != 0 {
			q.Set("limit", strconv.Itoa(o.limit))
		}
		if o.cursor != "" {
			q.Set("cursor", o.cursor)
		}
		var l api.TokenList
		if err := s.cl.get(ctx, "/tokens", q, &l); err != nil {
			return err
		}
		for _, t := range l.Items {
			s.out.emit("item", "", 0, t, nil)
			var scopes []string
			for _, sc := range t.Scopes {
				vm := "*"
				if sc.VMID != "" {
					vm = string(sc.VMID)
				}
				scopes = append(scopes, vm+":"+strings.Join(sc.Actions, ","))
			}
			s.out.human("%-24s %-16s %-40s expires %s", t.ID, t.Name, strings.Join(scopes, " "), t.ExpiresAt.Format(time.RFC3339))
		}
		s.out.result("", map[string]any{"count": len(l.Items), "nextCursor": l.NextCursor})
		return nil
	case "revoke":
		if len(args) != 2 {
			return usageErr("usage: %s token revoke TOKEN_ID [--yes]", buildinfo.Name)
		}
		if err := s.confirm(o, "revoke token "+args[1]+"?"); err != nil {
			return err
		}
		if err := s.cl.do(ctx, http.MethodDelete, "/tokens/"+url.PathEscape(args[1]), nil, nil, nil, nil); err != nil {
			return err
		}
		s.out.result("", map[string]string{"id": args[1], "revoked": "true"})
		s.out.human("revoked %s", args[1])
		return nil
	}
	if args[0] == "rotate" || args[0] == "usage" {
		return runTokenOps(ctx, s, o, args)
	}
	return usageErr("unknown token subcommand %q (create, list, revoke, rotate, usage)", args[0])
}
