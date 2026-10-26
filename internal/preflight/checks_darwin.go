//go:build darwin

package preflight

import (
	"context"
	"os/exec"
	goruntime "runtime"
	"time"

	"golang.org/x/sys/unix"

	"davit/internal/runtime/mac"
)

// platformChecks gathers macOS facts: OS version and architecture, the VZ
// helper's own probe (protocol, Virtualization.framework support, its
// entitlement) and its code signature. They fail only for the mac runtime;
// the fake runtime merely warns.
func platformChecks(ctx context.Context, o Options) []Check {
	need := o.Runtime == "mac"
	version, _ := unix.Sysctl("kern.osproductversion")
	checks := []Check{MacOSCheck(goruntime.GOARCH, version, need)}
	if o.MacHelper == "" {
		return checks
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := mac.Probe(pctx, o.MacHelper, nil)
	checks = append(checks, MacHelperChecks(o.MacHelper, p, err, need)...)
	if err == nil {
		vout, verr := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", o.MacHelper).CombinedOutput()
		sout, serr := exec.CommandContext(ctx, "/usr/sbin/spctl", "--assess", "--type", "execute", o.MacHelper).CombinedOutput()
		checks = append(checks, MacSignatureCheck(verr, string(vout), serr, string(sout), need))
	}
	return checks
}

// KVM reports that no KVM exists on macOS.
func KVM() Check {
	return Check{ID: "kvm", Title: "KVM access", Status: Skip,
		Detail: "macOS has no KVM; the mac runtime uses Virtualization.framework (see the virtualization check)"}
}
