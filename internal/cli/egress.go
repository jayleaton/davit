package cli

import (
	"context"
	"flag"
	"net/http"
	"strings"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

func egressFlags(fs *flag.FlagSet, o *opts) {
	fs.StringVar(&o.egress, "egress", "", "egress policy: off, filtered (default) or allowlist")
	fs.Var(&o.allow, "allow", "with --egress allowlist: domains (github.com covers subdomains) and IPv4 CIDRs; comma-separated or repeated")
}

// allowEntries splits repeated, comma-separated --allow values.
func (o *opts) allowEntries() []string {
	var out []string
	for _, v := range o.allow {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, e)
			}
		}
	}
	return out
}

// egressSpec is the --egress/--allow request field, or nil when neither is
// given (host default / inherited).
func (o *opts) egressSpec() (*api.Egress, error) {
	allow := o.allowEntries()
	if o.egress == "" && len(allow) == 0 {
		return nil, nil
	}
	mode := domain.EgressMode(o.egress)
	if mode == "" {
		mode = domain.EgressAllowlist // --allow alone means an allowlist
	}
	e, err := domain.Egress{Mode: mode, Allow: allow}.Normalize()
	if err != nil {
		return nil, usageErr("%s", domain.AsError(err).Message)
	}
	return &e, nil
}

// runEgress sets a VM's egress policy (live if it is running).
func runEgress(ctx context.Context, s *session, o *opts, args []string) error {
	var ref, mode string
	switch len(args) {
	case 1:
		mode = args[0]
	case 2:
		ref, mode = args[0], args[1]
	default:
		return usageErr("usage: %s egress [ID] off|filtered|allowlist [--allow a,b]", buildinfo.Name)
	}
	if ref == "" {
		id, err := s.currentID(nil)
		if err != nil {
			return err
		}
		ref = id
	}
	o.egress = mode
	eg, err := o.egressSpec()
	if err != nil {
		return err
	}
	var vm api.VM
	if err := s.cl.do(ctx, http.MethodPatch, vmPath(ref), nil, api.UpdateVMRequest{Egress: eg}, nil, &vm); err != nil {
		return err
	}
	s.out.result("", vm)
	line := string(vm.Egress.Mode)
	if len(vm.Egress.Allow) > 0 {
		line += " " + strings.Join(vm.Egress.Allow, ",")
	}
	s.out.human("%s egress: %s", vm.Name, line)
	return nil
}
