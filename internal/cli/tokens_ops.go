package cli

import (
	"context"
	"flag"
	"net/http"
	"net/url"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// Token scope presets for `token create --preset`: what each grants on
// every VM, or on the --vm ones.
// Members, shares and tokens are owner-only whatever the preset.
var tokenPresets = map[string][]string{
	"readonly": {api.TokenView},  // list, info, history, desktop URL and status; no exec
	"ci":       {api.TokenExec},  // also run commands and move files; no lifecycle changes
	"admin":    {api.TokenAdmin}, // also lifecycle and opening/closing a desktop (never members, shares or tokens)
}

// tokenOpsFlags adds the G18 token flags to `davit token`.
func tokenOpsFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.token.preset, "preset", "", "scope preset for create: readonly (view), ci (exec) or admin")
	fs.DurationVar(&o.token.overlap, "overlap", 0, "rotate: how long the old secret keeps working (default 0: revoked at once)")
}

// applyTokenPreset turns --preset into --action values.
func applyTokenPreset(o *opts) error {
	if o.token.preset == "" {
		return nil
	}
	acts, ok := tokenPresets[o.token.preset]
	if !ok {
		return usageErr("--preset %q: want readonly, ci or admin", o.token.preset)
	}
	if len(o.token.actions) > 0 {
		return usageErr("--preset and --action are exclusive")
	}
	o.token.actions = acts
	return nil
}

// runTokenOps serves `token rotate` and `token usage`.
func runTokenOps(ctx context.Context, s *session, o *opts, args []string) error {
	switch args[0] {
	case "rotate":
		if len(args) != 2 {
			return usageErr("usage: %s token rotate TOKEN_ID [--overlap 10m] [--ttl 30d]", buildinfo.Name)
		}
		if o.token.overlap < 0 {
			return usageErr("--overlap must not be negative")
		}
		req := api.RotateTokenRequest{OverlapSeconds: int64(o.token.overlap / time.Second)}
		if o.token.ttl != "" {
			ttl, err := parseLifetime(o.token.ttl)
			if err != nil {
				return err
			}
			secs := int64(ttl / time.Second)
			req.TTLSeconds = &secs
		}
		key := o.key
		if key == "" {
			key = newIdempotencyKey()
		}
		var t api.CreatedToken
		if err := s.cl.do(ctx, http.MethodPost, "/tokens/"+url.PathEscape(args[1])+"/rotate", nil, req, map[string]string{api.HeaderIdempotencyKey: key}, &t); err != nil {
			return err
		}
		s.out.result("", t)
		s.out.human("%s", t.Secret)
		retired := "is revoked"
		if o.token.overlap > 0 {
			retired = "stops working in " + o.token.overlap.String()
		}
		s.out.diag("token %s (%s) replaces %s, which %s; expires %s. The secret above is shown only once: store it now.",
			t.ID, t.Name, args[1], retired, t.ExpiresAt.Format(time.RFC3339))
		return nil
	case "usage":
		if len(args) > 2 {
			return usageErr("usage: %s token usage [TOKEN_ID]", buildinfo.Name)
		}
		var l api.TokenList
		q := url.Values{}
		q.Set("limit", "200")
		if err := s.cl.get(ctx, "/tokens", q, &l); err != nil {
			return err
		}
		n := 0
		for _, t := range l.Items {
			if len(args) == 2 && t.ID != args[1] && t.Name != args[1] {
				continue
			}
			n++
			s.out.emit("item", "", 0, t, nil)
			last := "never"
			if t.LastUsedAt != nil {
				last = t.LastUsedAt.Format(time.RFC3339)
			}
			s.out.human("%-24s %-16s %8d uses  last %s", t.ID, t.Name, t.UseCount, last)
		}
		if len(args) == 2 && n == 0 {
			return clientErr(domain.CodeNotFound, "no live token %q", args[1])
		}
		s.out.result("", map[string]any{"count": n})
		return nil
	}
	return usageErr("unknown token subcommand %q", args[0])
}
