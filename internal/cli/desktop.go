package cli

import (
	"context"
	"flag"
	"net/http"
	"os/exec"
	goruntime "runtime"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// desktopOpts are the flags of desktop.
type desktopOpts struct {
	port    int
	browser bool
}

func desktopFlags(fs *flag.FlagSet, o *opts) {
	fs.IntVar(&o.desktop.port, "port", 0, "guest VNC server port (open; default 5900)")
	fs.BoolVar(&o.desktop.browser, "browser", false, "open the viewer in this machine's browser (open, url)")
}

func desktopPath(vm string) string { return vmPath(vm) + "/desktop" }

// runDesktop is `desktop open|url|close [ID]`: the VM's VNC desktop as a
// tailnet-private web viewer on the gateway.
func runDesktop(ctx context.Context, s *session, o *opts, args []string) error {
	usage := "desktop open|url|close [ID] [--port N] [--browser]"
	if len(args) == 0 || len(args) > 2 {
		return usageErr("usage: %s %s", buildinfo.Name, usage)
	}
	verb := args[0]
	ref, err := s.currentID(args[1:])
	if err != nil {
		return err
	}
	var ep api.AppEndpoint
	switch verb {
	case "open":
		req := api.OpenDesktopRequest{Port: o.desktop.port}
		if err := s.cl.do(ctx, http.MethodPost, desktopPath(ref), nil, req, nil, &ep); err != nil {
			return err
		}
	case "url":
		if err := s.cl.get(ctx, desktopPath(ref), nil, &ep); err != nil {
			return err
		}
	case "close":
		if err := s.cl.do(ctx, http.MethodDelete, desktopPath(ref), nil, nil, nil, nil); err != nil {
			return err
		}
		s.out.human("closed the desktop of %s; its viewers were cut", ref)
		s.out.result("", map[string]any{"vm": ref, "closed": true})
		return nil
	default:
		return usageErr("usage: %s %s", buildinfo.Name, usage)
	}
	switch {
	case ep.URL == "":
		s.out.human("desktop of %s: no URL yet (the tailnet gateway is not up)", ref)
	case ep.State != api.AppEndpointActive:
		s.out.human("%s  (%s)", ep.URL, ep.State)
	default:
		s.out.human("%s", ep.URL)
	}
	if o.desktop.browser && ep.URL != "" {
		if err := openBrowser(ep.URL); err != nil {
			s.out.diag("could not open a browser: %v", err)
		}
	}
	s.out.result("", ep)
	return nil
}

// openBrowser opens url in this machine's browser.
func openBrowser(url string) error {
	name := "xdg-open"
	switch goruntime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		return clientErr(domain.CodeUnsupportedCapability, "--browser is not supported on Windows; open %s", url)
	}
	return exec.Command(name, url).Start()
}
