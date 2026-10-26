package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
)

// G16: `davit events` and `davit webhook …`.

type eventsOpts struct {
	follow      bool
	types       string
	vm          string
	start       string
	events      multiFlag
	description string
	urlFlag     string
	enable      bool
	disable     bool
	overlap     time.Duration
	state       string
}

func eventsFlags(fs *flag.FlagSet, o *opts) {
	fs.BoolVar(&o.events.follow, "follow", false, "keep waiting for new events (from now unless --cursor)")
	fs.BoolVar(&o.events.follow, "f", false, "same as --follow")
	fs.StringVar(&o.events.types, "types", "", "comma-separated types or prefixes (vm., operation.state)")
	fs.StringVar(&o.events.vm, "vm", "", "only this VM's events")
	fs.StringVar(&o.events.start, "start", "", "without --cursor: oldest (default) or latest")
	fs.IntVar(&o.limit, "limit", 0, "page size (max 200)")
	fs.StringVar(&o.cursor, "cursor", "", "continue from a previous nextCursor")
}

func webhookFlags(fs *flag.FlagSet, o *opts) {
	fs.Var(&o.events.events, "event", "event type or prefix to deliver (repeatable; default: all)")
	fs.StringVar(&o.events.description, "description", "", "a note for the webhook")
	fs.StringVar(&o.events.urlFlag, "url", "", "new endpoint URL (update)")
	fs.BoolVar(&o.events.enable, "enable", false, "enable deliveries (update)")
	fs.BoolVar(&o.events.disable, "disable", false, "pause deliveries (create, update); events meanwhile are skipped")
	fs.DurationVar(&o.events.overlap, "overlap", 24*time.Hour, "rotate: how long the old secret keeps signing (0 drops it now)")
	fs.StringVar(&o.events.state, "state", "", "deliveries: pending, delivered or dead")
	fs.IntVar(&o.limit, "limit", 0, "page size (list, deliveries)")
	fs.StringVar(&o.cursor, "cursor", "", "page cursor (list, deliveries)")
	fs.BoolVar(&o.yes, "yes", false, "do not ask for confirmation (delete)")
	fs.StringVar(&o.key, "idempotency-key", "", "deduplicate separate invocations (create, rotate)")
}

func eventLine(e api.Event) string {
	keys := make([]string, 0, len(e.Data))
	for k := range e.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var kv []string
	for _, k := range keys {
		kv = append(kv, fmt.Sprintf("%s=%v", k, e.Data[k]))
	}
	return fmt.Sprintf("%s %6d %-18s %s %s", e.Time.Local().Format("15:04:05.000"), e.Seq, e.Type, e.Subject, strings.Join(kv, " "))
}

// runEvents prints the host event stream: every retained event (paged),
// or with --follow the new ones as they happen, until interrupted.
func runEvents(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) != 0 {
		return usageErr("usage: %s events [--follow] [--types T,..] [--vm VM] [--start oldest|latest] [--cursor C]", buildinfo.Name)
	}
	e := o.events
	q := url.Values{}
	if e.types != "" {
		q.Set("types", e.types)
	}
	if e.vm != "" {
		q.Set("vmId", e.vm)
	}
	if o.limit != 0 {
		q.Set("limit", strconv.Itoa(o.limit))
	}
	cursor := o.cursor
	switch {
	case cursor != "":
	case e.start != "":
		q.Set("start", e.start)
	case e.follow:
		q.Set("start", "latest")
	}
	if e.follow {
		q.Set("waitMs", "25000")
	}
	count := 0
	for {
		if cursor != "" {
			q.Set("cursor", cursor)
			q.Del("start")
		}
		var l api.EventList
		if err := s.cl.get(ctx, "/events", q, &l); err != nil {
			if e.follow && ctx.Err() != nil {
				break // interrupted: a clean end of following
			}
			return err
		}
		for _, ev := range l.Items {
			s.out.emit("item", "", 0, ev, nil)
			s.out.human("%s", eventLine(ev))
		}
		count += len(l.Items)
		cursor = l.NextCursor
		if len(l.Items) == 0 && !e.follow {
			break
		}
	}
	s.out.result("", map[string]any{"count": count, "nextCursor": cursor})
	return nil
}

func webhookPath(id string) string { return "/webhooks/" + url.PathEscape(id) }

func (s *session) printWebhook(w api.Webhook) {
	state := "enabled"
	if !w.Enabled {
		state = "disabled"
	}
	evs := strings.Join(w.Events, ",")
	if evs == "" {
		evs = "all events"
	}
	s.out.human("%-26s %-8s %-30s %s (pending %d, dead %d)", w.ID, state, evs, w.URL, w.Pending, w.Dead)
}

func runWebhook(ctx context.Context, s *session, o *opts, args []string) error {
	usage := usageErr("usage: %s webhook create URL [--event T]... | list | show ID | update ID [--url U] [--event T]... [--enable|--disable] | delete ID [--yes] | rotate ID [--overlap 24h] | deliveries ID [--state dead] | retry ID DELIVERY", buildinfo.Name)
	if len(args) == 0 {
		return usage
	}
	e := o.events
	key := o.key
	if key == "" {
		key = newIdempotencyKey()
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "create":
		if len(rest) != 1 {
			return usage
		}
		req := api.CreateWebhookRequest{URL: rest[0], Events: e.events, Description: e.description}
		if e.disable {
			off := false
			req.Enabled = &off
		}
		var out api.WebhookSecret
		if err := s.cl.do(ctx, http.MethodPost, "/webhooks", nil, req, map[string]string{api.HeaderIdempotencyKey: key}, &out); err != nil {
			return err
		}
		s.out.result("", out)
		s.printWebhook(out.Webhook)
		s.out.human("signing secret (shown once; verify X-Davit-Signature with it): %s", out.Secret)
		return nil
	case "list":
		q := url.Values{}
		if o.limit != 0 {
			q.Set("limit", strconv.Itoa(o.limit))
		}
		if o.cursor != "" {
			q.Set("cursor", o.cursor)
		}
		var l api.WebhookList
		if err := s.cl.get(ctx, "/webhooks", q, &l); err != nil {
			return err
		}
		for _, w := range l.Items {
			s.out.emit("item", "", 0, w, nil)
			s.printWebhook(w)
		}
		s.out.result("", map[string]any{"count": len(l.Items), "nextCursor": l.NextCursor})
		return nil
	case "show":
		if len(rest) != 1 {
			return usage
		}
		var w api.Webhook
		if err := s.cl.get(ctx, webhookPath(rest[0]), nil, &w); err != nil {
			return err
		}
		s.out.result("", w)
		s.printWebhook(w)
		return nil
	case "update":
		if len(rest) != 1 {
			return usage
		}
		if e.enable && e.disable {
			return usageErr("--enable and --disable are exclusive")
		}
		var req api.UpdateWebhookRequest
		if e.urlFlag != "" {
			req.URL = &e.urlFlag
		}
		if len(e.events) > 0 {
			evs := []string(e.events)
			req.Events = &evs
		}
		if e.description != "" {
			req.Description = &e.description
		}
		if e.enable || e.disable {
			on := e.enable
			req.Enabled = &on
		}
		var w api.Webhook
		if err := s.cl.do(ctx, http.MethodPatch, webhookPath(rest[0]), nil, req, nil, &w); err != nil {
			return err
		}
		s.out.result("", w)
		s.printWebhook(w)
		return nil
	case "delete":
		if len(rest) != 1 {
			return usage
		}
		if err := s.confirm(o, "Delete webhook "+rest[0]+" and its undelivered events?"); err != nil {
			return err
		}
		if err := s.cl.do(ctx, http.MethodDelete, webhookPath(rest[0]), nil, nil, nil, nil); err != nil {
			return err
		}
		s.out.result("", map[string]any{"deleted": rest[0]})
		s.out.human("deleted webhook %s", rest[0])
		return nil
	case "rotate":
		if len(rest) != 1 {
			return usage
		}
		secs := int64(e.overlap / time.Second)
		var out api.WebhookSecret
		if err := s.cl.do(ctx, http.MethodPost, webhookPath(rest[0])+"/rotate-secret", nil, api.RotateWebhookSecretRequest{OverlapSeconds: &secs},
			map[string]string{api.HeaderIdempotencyKey: key}, &out); err != nil {
			return err
		}
		s.out.result("", out)
		if out.Webhook.PreviousSecretExpiresAt != nil {
			s.out.human("the old secret keeps signing until %s", out.Webhook.PreviousSecretExpiresAt.Local().Format(time.RFC3339))
		}
		s.out.human("new signing secret (shown once): %s", out.Secret)
		return nil
	case "deliveries":
		if len(rest) != 1 {
			return usage
		}
		q := url.Values{}
		if e.state != "" {
			q.Set("state", e.state)
		}
		if o.limit != 0 {
			q.Set("limit", strconv.Itoa(o.limit))
		}
		if o.cursor != "" {
			q.Set("cursor", o.cursor)
		}
		var l api.WebhookDeliveryList
		if err := s.cl.get(ctx, webhookPath(rest[0])+"/deliveries", q, &l); err != nil {
			return err
		}
		for _, d := range l.Items {
			s.out.emit("item", "", 0, d, nil)
			s.out.human("%-28s %-9s %6d %-18s attempts %d %s", d.ID, d.State, d.EventSeq, d.EventType, d.Attempts, d.LastError)
		}
		s.out.result("", map[string]any{"count": len(l.Items), "nextCursor": l.NextCursor})
		return nil
	case "retry":
		if len(rest) != 2 {
			return usage
		}
		var d api.WebhookDelivery
		if err := s.cl.do(ctx, http.MethodPost, webhookPath(rest[0])+"/deliveries/"+url.PathEscape(rest[1])+"/retry", nil, nil, nil, &d); err != nil {
			return err
		}
		s.out.result("", d)
		s.out.human("delivery %s is %s again", d.ID, d.State)
		return nil
	}
	return usage
}
