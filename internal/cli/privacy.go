package cli

import (
	"context"
	"flag"
	"net/http"
	"strconv"
	"strings"

	"davit/api"
	"davit/internal/domain"
)

const privacyUsage = "[ID] [--ephemeral[=false]] [--no-snapshot[=false]] [--no-output-retention[=false]] [--no-process-samples[=false]]"

// optBool is a boolean flag that remembers whether it was given, so
// `privacy --ephemeral=false` turns one flag off and leaves the others.
type optBool struct{ set, v bool }

func (b *optBool) String() string   { return strconv.FormatBool(b != nil && b.v) }
func (b *optBool) IsBoolFlag() bool { return true }
func (b *optBool) Set(s string) error {
	v, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	b.set, b.v = true, v
	return nil
}

// privacyOpts are the G17 policy flags of `new` and `privacy`.
type privacyOpts struct {
	ephemeral, noSnapshot, noOutput, noSamples optBool
}

func privacyFlags(fs *flag.FlagSet, o *opts) {
	p := &o.privacy
	fs.Var(&p.ephemeral, "ephemeral", "delete the VM when it stops (explicit, TTL or forced stop); nothing is committed")
	fs.Var(&p.noSnapshot, "no-snapshot", "a stop commits nothing: no resume, no fork, no explicit snapshots")
	fs.Var(&p.noOutput, "no-output-retention", "keep no process output on the host (history lists processes only)")
	fs.Var(&p.noSamples, "no-process-samples", "store no whole-guest process samples (ps still answers live)")
}

// apply sets the given flags on pol and reports whether any was given.
func (p *privacyOpts) apply(pol *domain.PrivacyPolicy) bool {
	given := false
	for _, f := range []struct {
		b   *optBool
		dst *bool
	}{
		{&p.ephemeral, &pol.Ephemeral},
		{&p.noSnapshot, &pol.NoSnapshot},
		{&p.noOutput, &pol.NoOutputRetention},
		{&p.noSamples, &pol.NoProcessSamples},
	} {
		if f.b.set {
			*f.dst, given = f.b.v, true
		}
	}
	return given
}

// privacySpec is new's CreateVMRequest.Privacy: nil (usual retention) unless
// a flag turns something on.
func (o *opts) privacySpec() *api.PrivacyPolicy {
	var pol api.PrivacyPolicy
	o.privacy.apply(&pol)
	if pol == (api.PrivacyPolicy{}) {
		return nil
	}
	return &pol
}

// privacyWords describes a policy, e.g. "ephemeral, no snapshot"; empty
// for the usual retention.
func privacyWords(p domain.PrivacyPolicy) string {
	var w []string
	for _, f := range []struct {
		on   bool
		word string
	}{
		{p.Ephemeral, "ephemeral"},
		{p.NoSnapshot, "no snapshot"},
		{p.NoOutputRetention, "no output retention"},
		{p.NoProcessSamples, "no process samples"},
	} {
		if f.on {
			w = append(w, f.word)
		}
	}
	return strings.Join(w, ", ")
}

// runPrivacy shows a VM's privacy policy or, with flags, changes the flags
// given and keeps the rest: it reads the VM and PATCHes the whole policy.
func runPrivacy(ctx context.Context, s *session, o *opts, args []string) error {
	if err := maxArgs(args, 1, "privacy "+privacyUsage); err != nil {
		return err
	}
	id, err := s.currentID(args)
	if err != nil {
		return err
	}
	var vm api.VM
	if err := s.cl.get(ctx, vmPath(id), nil, &vm); err != nil {
		return err
	}
	pol := vm.Privacy
	if o.privacy.apply(&pol) {
		// A fresh VM: false flags are omitted on the wire, so decoding into
		// the old one would keep them set.
		var updated api.VM
		if err := s.cl.do(ctx, http.MethodPatch, vmPath(string(vm.ID)), nil, api.UpdateVMRequest{Privacy: &pol}, nil, &updated); err != nil {
			return err
		}
		vm = updated
	}
	s.out.result("", vm)
	s.out.human("%s privacy: %s", vm.Name, orDash(privacyWords(vm.Privacy)))
	return nil
}
