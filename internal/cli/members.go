package cli

import (
	"context"
	"flag"
	"net/http"
	"net/url"
	"strconv"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// shareOpts are the flags of members, share, unshare and shares.
type shareOpts struct {
	role        string
	credentials bool
	snapshot    string
	environment string
}

func memberFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.share.role, "role", "", "viewer, exec or admin")
}

func shareFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.share.role, "role", "", "viewer, exec or admin (default: the member's role)")
	fs.BoolVar(&o.share.credentials, "credentials", false,
		"let the member use the VM's credential grants (otherwise an exec/admin share pauses them)")
	fs.StringVar(&o.share.snapshot, "snapshot", "", "share a snapshot instead of a VM")
	fs.StringVar(&o.share.environment, "environment", "", "share an environment template instead of a VM")
}

// memberRef maps "all" to every member.
func memberRef(s string) string {
	if s == "all" {
		return api.ShareAllMembers
	}
	return s
}

func runMembers(ctx context.Context, s *session, o *opts, args []string) error {
	usage := "members [list] | members add LOGIN --role viewer|exec|admin | members role MEMBER ROLE | members remove MEMBER"
	verb := "list"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	show := func(m api.Member) {
		pinned := "not connected yet"
		if m.UserID != 0 {
			pinned = "user " + itoa64(m.UserID)
		}
		s.out.human("%-18s %-30s %-7s %s", m.ID, m.Login, m.Role, pinned)
	}
	switch {
	case verb == "list" && len(args) == 0:
		var l api.MemberList
		if err := s.cl.get(ctx, "/members", nil, &l); err != nil {
			return err
		}
		for _, m := range l.Items {
			s.out.emit("item", "", 0, m, nil)
			show(m)
		}
		if len(l.Items) == 0 {
			s.out.human("no members (add one with `%s members add LOGIN --role exec`)", buildinfo.Name)
		}
		s.out.result("", map[string]any{"count": len(l.Items)})
		return nil
	case verb == "add" && len(args) == 1:
		var m api.Member
		if err := s.cl.do(ctx, http.MethodPost, "/members", nil, api.AddMemberRequest{Login: args[0], Role: api.MemberRole(o.share.role)}, nil, &m); err != nil {
			return err
		}
		show(m)
		s.out.result("", m)
		return nil
	case verb == "role" && len(args) == 2:
		var m api.Member
		if err := s.cl.do(ctx, http.MethodPatch, "/members/"+url.PathEscape(args[0]), nil, api.UpdateMemberRequest{Role: api.MemberRole(args[1])}, nil, &m); err != nil {
			return err
		}
		show(m)
		s.out.result("", m)
		return nil
	case verb == "remove" && len(args) == 1:
		if err := s.cl.do(ctx, http.MethodDelete, "/members/"+url.PathEscape(args[0]), nil, nil, nil, nil); err != nil {
			return err
		}
		s.out.human("removed %s (its shares are gone and its live sessions were cut)", args[0])
		s.out.result("", map[string]any{"member": args[0], "removed": true})
		return nil
	}
	return usageErr("usage: %s %s", buildinfo.Name, usage)
}

// shareTarget resolves the resource of share/unshare/shares: --snapshot,
// --environment, else the VM (args, or the current VM).
func shareTarget(s *session, o *opts, args []string) (api.ShareKind, string, error) {
	switch {
	case o.share.snapshot != "" && o.share.environment != "":
		return "", "", usageErr("--snapshot and --environment are exclusive")
	case o.share.snapshot != "":
		if len(args) > 0 {
			return "", "", usageErr("with --snapshot give no VM")
		}
		return api.ShareSnapshot, o.share.snapshot, nil
	case o.share.environment != "":
		if len(args) > 0 {
			return "", "", usageErr("with --environment give no VM")
		}
		return api.ShareEnvironment, o.share.environment, nil
	}
	ref, err := s.currentID(args)
	return api.ShareVM, ref, err
}

func showShare(s *session, sh api.Share) {
	role := string(sh.Role)
	if role == "" {
		role = "(member's role)"
	}
	who := sh.Member
	if who == api.ShareAllMembers {
		who = "all members"
	}
	creds := ""
	if sh.Credentials {
		creds = "  +credentials"
	}
	s.out.human("%-18s %-11s %-24s %-20s %s%s", sh.ID, sh.Kind, sh.ResourceID, who, role, creds)
}

// runShare is `share [VM] MEMBER [--role R] [--credentials]` (or a
// --snapshot / --environment); MEMBER "all" is every member.
func runShare(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) == 0 || len(args) > 2 {
		return usageErr("usage: %s share [ID] MEMBER|all [--role viewer|exec|admin] [--credentials] [--snapshot S | --environment E]", buildinfo.Name)
	}
	kind, ref, err := shareTarget(s, o, args[:len(args)-1])
	if err != nil {
		return err
	}
	req := api.CreateShareRequest{Kind: kind, Resource: ref, Member: memberRef(args[len(args)-1]),
		Role: api.MemberRole(o.share.role), Credentials: o.share.credentials}
	var sh api.Share
	if err := s.cl.do(ctx, http.MethodPost, "/shares", nil, req, nil, &sh); err != nil {
		return err
	}
	showShare(s, sh)
	if kind == api.ShareVM && !sh.Credentials && sh.Role != api.RoleViewer {
		s.out.diag("the VM's credential grants are paused while it is shared with exec or admin (share with --credentials to allow their use)")
	}
	s.out.result("", sh)
	return nil
}

// findShare finds the share of a resource with a member.
func findShare(ctx context.Context, s *session, kind api.ShareKind, ref, member string) (api.Share, error) {
	var l api.ShareList
	if err := s.cl.get(ctx, "/shares", url.Values{"kind": {string(kind)}, "resource": {ref}}, &l); err != nil {
		return api.Share{}, err
	}
	var ml api.MemberList
	if member != api.ShareAllMembers {
		if err := s.cl.get(ctx, "/members", nil, &ml); err != nil {
			return api.Share{}, err
		}
	}
	for _, sh := range l.Items {
		if sh.Member == member {
			return sh, nil
		}
		for _, m := range ml.Items {
			if m.ID == sh.Member && (m.Login == member || m.ID == member) {
				return sh, nil
			}
		}
	}
	return api.Share{}, clientErr(domain.CodeNotFound, "%s %s is not shared with %s", kind, ref, member)
}

// runUnshare is `unshare [VM] MEMBER` (or --snapshot / --environment).
func runUnshare(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) == 0 || len(args) > 2 {
		return usageErr("usage: %s unshare [ID] MEMBER|all [--snapshot S | --environment E]", buildinfo.Name)
	}
	kind, ref, err := shareTarget(s, o, args[:len(args)-1])
	if err != nil {
		return err
	}
	sh, err := findShare(ctx, s, kind, ref, memberRef(args[len(args)-1]))
	if err != nil {
		return err
	}
	if err := s.cl.do(ctx, http.MethodDelete, "/shares/"+url.PathEscape(sh.ID), nil, nil, nil, nil); err != nil {
		return err
	}
	s.out.human("unshared %s %s from %s; its live sessions there were cut", kind, ref, args[len(args)-1])
	s.out.result("", map[string]any{"shareId": sh.ID, "deleted": true})
	return nil
}

// runShares lists shares: all, or of a VM / --snapshot / --environment.
func runShares(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) > 1 {
		return usageErr("usage: %s shares [ID] [--snapshot S | --environment E]", buildinfo.Name)
	}
	q := url.Values{}
	if len(args) == 1 || o.share.snapshot != "" || o.share.environment != "" {
		kind, ref, err := shareTarget(s, o, args)
		if err != nil {
			return err
		}
		q = url.Values{"kind": {string(kind)}, "resource": {ref}}
	}
	var l api.ShareList
	if err := s.cl.get(ctx, "/shares", q, &l); err != nil {
		return err
	}
	for _, sh := range l.Items {
		s.out.emit("item", "", 0, sh, nil)
		showShare(s, sh)
	}
	if len(l.Items) == 0 {
		s.out.human("nothing shared")
	}
	s.out.result("", map[string]any{"count": len(l.Items)})
	return nil
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
