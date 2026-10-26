package preflight

import (
	"fmt"
	"strconv"
	"strings"

	hp "davit/internal/runtime/mac/helperproto"
)

// Mac runtime checks (MAC-BACKEND.md). The facts are gathered on macOS
// (checks_darwin.go); the classification here is pure, so it is tested on
// any host.

// MinMacOSMajor is the oldest supported macOS release (Virtualization.framework
// features the backend relies on; certify exact builds before a release).
const MinMacOSMajor = 14

// MacOSCheck grades the host OS and architecture for the mac runtime.
func MacOSCheck(arch, version string, required bool) Check {
	c := Check{ID: "macos", Title: "macOS version and architecture"}
	major, _ := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	switch {
	case arch != "arm64":
		c.Status, c.Detail = severity(required), "darwin/"+arch+": the Mac backend runs on Apple silicon (arm64) only"
		c.Hint = "use an Apple-silicon Mac"
	case version == "" || major == 0:
		c.Status, c.Detail = severity(required), "cannot read the macOS version (kern.osproductversion)"
		c.Hint = fmt.Sprintf("check `sysctl kern.osproductversion`; the Mac backend needs macOS %d or later", MinMacOSMajor)
	case major < MinMacOSMajor:
		c.Status, c.Detail = severity(required), fmt.Sprintf("macOS %s is older than %d", version, MinMacOSMajor)
		c.Hint = fmt.Sprintf("update to macOS %d or later", MinMacOSMajor)
	default:
		c.Status, c.Detail = Pass, "macOS "+version+" on arm64"
	}
	return c
}

// MacHelperChecks grade the helper from its own probe: presence and
// protocol, then Virtualization.framework support and the entitlement as
// the signed helper process itself reports them.
func MacHelperChecks(path string, p hp.ProbeResult, probeErr error, required bool) []Check {
	helper := Check{ID: "mac_helper", Title: "VZ helper"}
	vz := Check{ID: "virtualization", Title: "Virtualization.framework"}
	if probeErr != nil {
		helper.Status, helper.Detail = severity(required), fmt.Sprintf("%s: %v", path, probeErr)
		helper.Hint = `install davit-vz-helper next to davit, or set "mac": {"helper": "/path/to/davit-vz-helper"}`
		vz.Status, vz.Detail = Skip, "needs the helper"
		return []Check{helper, vz}
	}
	if v, err := hp.Negotiate(p.MinVersion, p.MaxVersion); err != nil {
		helper.Status, helper.Detail = severity(required), fmt.Sprintf("%s %s: %v", path, p.HelperVersion, err)
		helper.Hint = "install the helper from the same davit release"
	} else {
		helper.Status, helper.Detail = Pass, fmt.Sprintf("%s %s (build %s), protocol %d", path, p.HelperVersion, p.Build, v)
	}
	switch {
	case !p.VZSupported:
		vz.Status, vz.Detail = severity(required), "VZVirtualMachine.isSupported is false on this Mac"
		vz.Hint = "virtualization is unavailable here (e.g. inside a VM without nested virtualization)"
	case !p.Entitled:
		vz.Status, vz.Detail = severity(required), "the helper lacks the com.apple.security.virtualization entitlement"
		vz.Hint = "install the signed helper from a release; a local build must be signed with that entitlement"
	default:
		vz.Status = Pass
		vz.Detail = fmt.Sprintf("supported and entitled; up to %d CPUs, %s memory", p.Limits.MaxCPUs, humanBytes(p.Limits.MaxMemoryBytes))
	}
	return []Check{helper, vz}
}

// MacSignatureCheck grades the helper's code signature: codesign --verify
// must pass (a broken signature fails); spctl's Gatekeeper assessment
// (Developer ID, notarized) only warns, because local development builds
// are ad-hoc signed by design and that is not release trust.
func MacSignatureCheck(verifyErr error, verifyOut string, spctlErr error, spctlOut string, required bool) Check {
	c := Check{ID: "mac_helper_signature", Title: "VZ helper signature"}
	switch {
	case verifyErr != nil:
		c.Status = severity(required)
		c.Detail = "codesign --verify failed: " + firstLine(verifyOut, verifyErr)
		c.Hint = "reinstall the helper from a release; never strip signatures or quarantine to make it run"
	case spctlErr != nil:
		c.Status = Warn
		c.Detail = "valid signature, but Gatekeeper does not accept it (ad-hoc or not notarized): " + firstLine(spctlOut, spctlErr)
		c.Hint = "fine for a local development build; releases ship a Developer ID-signed, notarized helper"
	default:
		c.Status, c.Detail = Pass, "valid signature accepted by Gatekeeper"
	}
	return c
}

func firstLine(out string, err error) string {
	if s := strings.TrimSpace(out); s != "" {
		return strings.SplitN(s, "\n", 2)[0]
	}
	return err.Error()
}
