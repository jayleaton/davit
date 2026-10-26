package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"davit/api"
	"davit/internal/buildinfo"
)

// `davit env`: environments (G3), versioned workspace manifests that new,
// fork and resume pin with --environment NAME[@REVISION].

const envUsage = "list | create NAME --file MANIFEST | show NAME[@REV] | revise NAME --file MANIFEST [--if-revision N] | revisions NAME | delete NAME [--yes]"

// parseEnvRef reads NAME[@REVISION] (no revision: the latest).
func parseEnvRef(s string) (*api.EnvironmentRef, error) {
	if s == "" {
		return nil, nil
	}
	name, rev, found := strings.Cut(s, "@")
	ref := &api.EnvironmentRef{Environment: name}
	if found {
		n, err := strconv.ParseInt(rev, 10, 64)
		if err != nil || n < 1 {
			return nil, usageErr("--environment %q: want NAME or NAME@REVISION", s)
		}
		ref.Revision = n
	}
	if name == "" {
		return nil, usageErr("--environment %q: want NAME or NAME@REVISION", s)
	}
	return ref, nil
}

// readManifest reads a JSON manifest from a file or stdin ("-").
func (s *session) readManifest(path string) (api.EnvManifest, error) {
	var m api.EnvManifest
	if path == "" {
		return m, usageErr("--file MANIFEST (JSON, or - for stdin) is required")
	}
	var r io.Reader
	if path == "-" {
		r = s.io.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return m, usageErr("%v", err)
		}
		defer f.Close()
		r = f
	}
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, usageErr("manifest %s: %v", path, err)
	}
	return m, nil
}

func envPath(name string) string { return "/environments/" + url.PathEscape(name) }

func runEnv(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) == 0 {
		return usageErr("usage: %s env %s", buildinfo.Name, envUsage)
	}
	want := func(n int, usage string) error {
		if len(args) != n {
			return usageErr("usage: %s env %s", buildinfo.Name, usage)
		}
		return nil
	}
	switch args[0] {
	case "list":
		if err := want(1, "list"); err != nil {
			return err
		}
		q := url.Values{}
		if o.limit != 0 {
			q.Set("limit", strconv.Itoa(o.limit))
		}
		if o.cursor != "" {
			q.Set("cursor", o.cursor)
		}
		var l api.EnvironmentList
		if err := s.cl.get(ctx, "/environments", q, &l); err != nil {
			return err
		}
		for _, e := range l.Items {
			s.out.emit("item", "", 0, e, nil)
			s.out.human("%-24s %-24s rev %d", e.Name, e.ID, e.LatestRevision)
		}
		s.out.result("", map[string]any{"count": len(l.Items), "nextCursor": l.NextCursor})
		return nil
	case "create":
		if err := want(2, "create NAME --file MANIFEST"); err != nil {
			return err
		}
		m, err := s.readManifest(o.file)
		if err != nil {
			return err
		}
		var e api.Environment
		if err := s.cl.do(ctx, http.MethodPost, "/environments", nil, api.CreateEnvironmentRequest{Name: args[1], Manifest: m}, nil, &e); err != nil {
			return err
		}
		s.out.result("", e)
		s.out.human("environment %s (%s), revision %d", e.Name, e.ID, e.LatestRevision)
		return nil
	case "show":
		if err := want(2, "show NAME[@REV]"); err != nil {
			return err
		}
		ref, err := parseEnvRef(args[1])
		if err != nil {
			return err
		}
		var e api.Environment
		if err := s.cl.get(ctx, envPath(ref.Environment), nil, &e); err != nil {
			return err
		}
		n := ref.Revision
		if n == 0 {
			n = e.LatestRevision
		}
		var r api.EnvironmentRevision
		if err := s.cl.get(ctx, envPath(ref.Environment)+"/revisions/"+strconv.FormatInt(n, 10), nil, &r); err != nil {
			return err
		}
		s.out.result("", r)
		b, _ := json.MarshalIndent(r.Manifest, "", "  ")
		s.out.human("%s (%s) revision %d of %d, %s\n%s", e.Name, e.ID, r.Revision, e.LatestRevision, r.CreatedAt.Local().Format("2006-01-02 15:04:05"), b)
		return nil
	case "revise":
		if err := want(2, "revise NAME --file MANIFEST [--if-revision N]"); err != nil {
			return err
		}
		m, err := s.readManifest(o.file)
		if err != nil {
			return err
		}
		hdr := map[string]string{}
		if o.key != "" {
			hdr[api.HeaderIdempotencyKey] = o.key
		}
		if o.ifRevision > 0 {
			hdr[api.HeaderIfMatch] = strconv.FormatInt(o.ifRevision, 10)
		}
		var r api.EnvironmentRevision
		if err := s.cl.do(ctx, http.MethodPost, envPath(args[1])+"/revisions", nil, api.CreateEnvironmentRevisionRequest{Manifest: m}, hdr, &r); err != nil {
			return err
		}
		s.out.result("", r)
		s.out.human("%s revision %d", args[1], r.Revision)
		return nil
	case "revisions":
		if err := want(2, "revisions NAME"); err != nil {
			return err
		}
		q := url.Values{}
		if o.limit != 0 {
			q.Set("limit", strconv.Itoa(o.limit))
		}
		if o.cursor != "" {
			q.Set("cursor", o.cursor)
		}
		var l api.EnvironmentRevisionList
		if err := s.cl.get(ctx, envPath(args[1])+"/revisions", q, &l); err != nil {
			return err
		}
		for _, r := range l.Items {
			s.out.emit("item", "", 0, r, nil)
			s.out.human("rev %-4d %s  %d repos, %d setup steps", r.Revision, r.CreatedAt.Local().Format("2006-01-02 15:04:05"), len(r.Manifest.Repos), len(r.Manifest.Setup))
		}
		s.out.result("", map[string]any{"count": len(l.Items), "nextCursor": l.NextCursor})
		return nil
	case "delete":
		if err := want(2, "delete NAME [--yes]"); err != nil {
			return err
		}
		if err := s.confirm(o, "delete environment "+args[1]+"? (pinned VMs keep their copy)"); err != nil {
			return err
		}
		if err := s.cl.do(ctx, http.MethodDelete, envPath(args[1]), nil, nil, nil, nil); err != nil {
			return err
		}
		s.out.result("", map[string]string{"name": args[1], "deleted": "true"})
		s.out.human("deleted environment %s", args[1])
		return nil
	}
	return usageErr("unknown env subcommand %q (list, create, show, revise, revisions, delete)", args[0])
}
