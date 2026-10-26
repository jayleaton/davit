package cli

import (
	"context"
	"flag"
	"net/http"
	"net/url"
	"strconv"

	"davit/api"
	"davit/internal/buildinfo"
)

// exposeOpts are the flags of expose and endpoints.
type exposeOpts struct {
	name            string
	forwardIdentity bool
}

func exposeFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.expose.name, "name", "", "endpoint name (default port-PORT; a DNS label, unique in the VM)")
	fs.BoolVar(&o.expose.forwardIdentity, "forward-identity", false,
		"send the caller's tailnet login to the app (Tailscale-User-Login); off by default, the guest is untrusted")
}

func appEndpointsPath(vm string) string { return vmPath(vm) + "/app-endpoints" }

func showEndpoint(s *session, ep api.AppEndpoint) {
	url := ep.URL
	if url == "" {
		url = "(no URL yet: the tailnet gateway is not up)"
	}
	s.out.human("%-20s guest :%-5d %s  %s", ep.Name, ep.Port, url, ep.State)
}

// runExpose is `expose [ID] PORT [--name N]`: a stable tailnet URL for the
// app on guest port PORT.
func runExpose(ctx context.Context, s *session, o *opts, args []string) error {
	usage := "expose [ID] PORT [--name NAME] [--forward-identity]"
	if len(args) == 0 || len(args) > 2 {
		return usageErr("usage: %s %s", buildinfo.Name, usage)
	}
	port, err := strconv.Atoi(args[len(args)-1])
	if err != nil || port < 1 || port > 65535 {
		return usageErr("%s needs a guest port 1..65535: %s %s", buildinfo.Name, buildinfo.Name, usage)
	}
	ref, err := s.currentID(args[:len(args)-1])
	if err != nil {
		return err
	}
	var ep api.AppEndpoint
	req := api.CreateAppEndpointRequest{Port: port, Name: o.expose.name, ForwardIdentity: o.expose.forwardIdentity}
	if err := s.cl.do(ctx, http.MethodPost, appEndpointsPath(ref), nil, req, nil, &ep); err != nil {
		return err
	}
	showEndpoint(s, ep)
	s.out.result("", ep)
	return nil
}

// runEndpoints lists a VM's app endpoints, or hides, shows, renames or
// deletes one.
func runEndpoints(ctx context.Context, s *session, o *opts, args []string) error {
	usage := "endpoints [ID] | endpoints hide|show|delete [ID] NAME | endpoints rename [ID] NAME NEW"
	verb := ""
	if len(args) > 0 {
		switch args[0] {
		case "hide", "show", "delete", "rename", "list":
			verb, args = args[0], args[1:]
		}
	}
	if verb == "" || verb == "list" {
		if len(args) > 1 {
			return usageErr("usage: %s %s", buildinfo.Name, usage)
		}
		ref, err := s.currentID(args)
		if err != nil {
			return err
		}
		var l api.AppEndpointList
		if err := s.cl.get(ctx, appEndpointsPath(ref), nil, &l); err != nil {
			return err
		}
		for _, ep := range l.Items {
			s.out.emit("item", "", 0, ep, nil)
			showEndpoint(s, ep)
		}
		if len(l.Items) == 0 {
			s.out.human("no app endpoints (create one with `%s expose [ID] PORT`)", buildinfo.Name)
		}
		s.out.result("", map[string]any{"count": len(l.Items)})
		return nil
	}
	want := 1
	if verb == "rename" {
		want = 2
	}
	if len(args) < want || len(args) > want+1 {
		return usageErr("usage: %s %s", buildinfo.Name, usage)
	}
	ref, err := s.currentID(args[:len(args)-want])
	if err != nil {
		return err
	}
	name := args[len(args)-want]
	path := appEndpointsPath(ref) + "/" + url.PathEscape(name)
	var req api.UpdateAppEndpointRequest
	switch verb {
	case "delete":
		if err := s.cl.do(ctx, http.MethodDelete, path, nil, nil, nil, nil); err != nil {
			return err
		}
		s.out.human("deleted %s", name)
		s.out.result("", map[string]any{"name": name, "deleted": true})
		return nil
	case "hide", "show":
		hidden := verb == "hide"
		req.Hidden = &hidden
	case "rename":
		req.Name = &args[len(args)-1]
	}
	var ep api.AppEndpoint
	if err := s.cl.do(ctx, http.MethodPatch, path, nil, req, nil, &ep); err != nil {
		return err
	}
	showEndpoint(s, ep)
	s.out.result("", ep)
	return nil
}
