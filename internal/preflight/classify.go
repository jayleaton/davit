package preflight

import (
	"fmt"
	"strconv"
	"strings"

	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/packaging"
)

// KVMFacts is what the KVM check observed.
type KVMFacts struct {
	Exists     bool
	OpenErr    error // from opening /dev/kvm read-write
	Permission bool  // OpenErr is EACCES/EPERM
	APIVersion int   // KVM_GET_API_VERSION; 0 if not queried
	Group      string
	InGroup    bool // the process's groups include /dev/kvm's group
	// Member: the user's configured groups (/etc/group) include it, so a
	// session without it predates the group change.
	Member bool
	// VirtFlag is the CPU's "vmx" or "svm" flag ("" if none), which names
	// the module to load when /dev/kvm is missing.
	VirtFlag string
	User     UserFacts
}

// UserFacts describe this user and its systemd --user manager, which runs
// the daemon's service with the groups it had when it started.
type UserFacts struct {
	Name string
	UID  int
	// ManagerRunning: /run/user/<uid>/systemd exists.
	ManagerRunning bool
	// InManager: this process runs inside user@<uid>.service (a desktop
	// terminal's app.slice), so restarting the manager would kill it.
	InManager bool
}

// InUserManager reports whether a /proc/self/cgroup listing places the
// process inside user@<uid>.service.
func InUserManager(cgroup string, uid int) bool {
	unit := fmt.Sprintf("/user@%d.service", uid)
	for _, line := range strings.Split(cgroup, "\n") {
		// hierarchy-ID:controllers:path (v2 has "0::path")
		if f := strings.SplitN(line, ":", 3); len(f) == 3 && strings.Contains(f[2]+"/", unit+"/") {
			return true
		}
	}
	return false
}

// groupFix says how a group the user was just given reaches this shell and
// the user's services, and returns the root line that restarts the user
// manager when that is safe (a manager runs and this process is outside it).
func (u UserFacts) groupFix(group string) (string, []string) {
	shell := fmt.Sprintf("log in again (or run `newgrp %s`) for this shell", group)
	switch {
	case !u.ManagerRunning:
		return shell, nil
	case u.InManager:
		return shell + ", and reboot (or log out of every session) so user services such as " + buildinfo.Name + ".service get the group", nil
	}
	return shell + "; restarting user@" + strconv.Itoa(u.UID) + ".service gives user services such as " + buildinfo.Name + ".service the group",
		[]string{fmt.Sprintf("sudo systemctl restart user@%d.service", u.UID)}
}

// kvmModule is the module that provides /dev/kvm for a CPU flag.
var kvmModule = map[string]string{"vmx": "kvm_intel", "svm": "kvm_amd"}

// ClassifyKVM grades /dev/kvm access.
func ClassifyKVM(f KVMFacts, required bool) Check {
	c := Check{ID: "kvm", Title: "KVM access"}
	sev := severity(required)
	switch {
	case !f.Exists && kvmModule[f.VirtFlag] != "":
		mod := kvmModule[f.VirtFlag]
		c.Status, c.Detail = sev, fmt.Sprintf("/dev/kvm does not exist although the CPU has %s: %s is not loaded", f.VirtFlag, mod)
		c.Hint = "load " + mod + "; if that fails, enable VT-x/AMD-V in firmware"
		c.Sudo = []string{"sudo modprobe " + mod}
	case !f.Exists:
		c.Status, c.Detail = sev, "/dev/kvm does not exist"
		c.Hint = "enable VT-x/AMD-V in firmware and load kvm_intel or kvm_amd; cloud VMs need an instance type with nested virtualization"
	case f.Permission && f.InGroup:
		c.Status, c.Detail = sev, fmt.Sprintf("/dev/kvm is not writable although this process is in group %s", f.Group)
		c.Hint = fmt.Sprintf("check /dev/kvm's mode (want crw-rw---- root:%s, which udev normally sets)", f.Group)
	case f.Permission && f.Member:
		c.Status, c.Detail = sev, fmt.Sprintf("/dev/kvm is not writable: you are in group %s, but this login session predates it", f.Group)
		c.Hint, c.Sudo = f.User.groupFix(f.Group)
	case f.Permission:
		c.Status, c.Detail = sev, fmt.Sprintf("/dev/kvm is not writable by this user (group %s)", f.Group)
		who := f.User.Name
		if who == "" {
			c.Hint = fmt.Sprintf("add your user to group %s (sudo usermod -aG %s NAME), then log in again", f.Group, f.Group)
			break
		}
		hint, restart := f.User.groupFix(f.Group)
		c.Hint = fmt.Sprintf("add %s to group %s, then %s", who, f.Group, hint)
		c.Sudo = append([]string{fmt.Sprintf("sudo usermod -aG %s %s", packaging.ShellQuote(f.Group), packaging.ShellQuote(who))}, restart...)
	case f.OpenErr != nil:
		c.Status, c.Detail, c.Hint = sev, "cannot open /dev/kvm: "+f.OpenErr.Error(), "check that the kvm module loaded cleanly (dmesg)"
	case f.APIVersion != 12:
		c.Status, c.Detail, c.Hint = sev, fmt.Sprintf("unexpected KVM API version %d (want 12)", f.APIVersion), "update the host kernel"
	default:
		c.Status, c.Detail = Pass, "/dev/kvm is read-write, KVM API 12"
	}
	return c
}

// CPUFacts summarizes /proc/cpuinfo on x86.
type CPUFacts struct {
	Arch          string
	VirtFlag      string // "vmx", "svm" or ""
	Hypervisor    bool   // this host is itself a VM
	NestedAllowed string // kvm_intel/kvm_amd "nested" parameter, if readable
}

// ParseCPUInfo extracts virtualization flags from /proc/cpuinfo.
func ParseCPUInfo(cpuinfo string) (virt string, hypervisor bool) {
	for _, line := range strings.Split(cpuinfo, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "flags" {
			continue
		}
		for _, fl := range strings.Fields(v) {
			switch fl {
			case "vmx", "svm":
				virt = fl
			case "hypervisor":
				hypervisor = true
			}
		}
		break
	}
	return virt, hypervisor
}

// ClassifyCPU grades hardware virtualization support, including nesting.
func ClassifyCPU(f CPUFacts, required bool) Check {
	c := Check{ID: "cpu_virt", Title: "CPU virtualization"}
	switch f.Arch {
	case "amd64":
	case "arm64":
		c.Status, c.Detail = Skip, "arm64 exposes no CPU flag; the kvm check decides"
		return c
	default:
		c.Status, c.Detail = Fail, "unsupported architecture "+f.Arch
		c.Hint = "davit supports amd64 and arm64 hosts"
		return c
	}
	name := map[string]string{"vmx": "Intel VT-x", "svm": "AMD-V"}[f.VirtFlag]
	switch {
	case f.VirtFlag != "" && f.Hypervisor:
		c.Status = Pass
		c.Detail = name + " available; this host is itself a VM (nested virtualization: expect slower VM exits and boots)"
	case f.VirtFlag != "":
		c.Status, c.Detail = Pass, name+" available on bare metal"
	case f.Hypervisor:
		c.Status, c.Detail = severity(required), "running inside a VM without nested virtualization"
		c.Hint = "enable nested virtualization on the outer hypervisor or cloud instance"
	default:
		c.Status, c.Detail = severity(required), "CPU virtualization extensions are disabled or missing"
		c.Hint = "enable VT-x/AMD-V (SVM) in firmware settings"
	}
	if f.NestedAllowed != "" {
		c.Detail += fmt.Sprintf("; kvm nested=%s for guests of this host", f.NestedAllowed)
	}
	return c
}

// UsernsFacts is what the namespace check observed. Sysctl values are -1
// when the knob does not exist on this kernel.
type UsernsFacts struct {
	UnprivilegedClone   int // kernel.unprivileged_userns_clone (Debian/Arch)
	MaxUserNamespaces   int // user.max_user_namespaces
	AppArmorRestriction int // kernel.apparmor_restrict_unprivileged_userns (Ubuntu 23.10+)
	// AppArmorAllowed: this process runs under an AppArmor profile that
	// grants userns (AppArmorFacts.Attached), which lifts the restriction.
	AppArmorAllowed bool
	ProbeErr        error // creating a user+net namespace as this user
}

// ClassifyUserns grades rootless user+net namespace support, which the
// Firecracker runtime uses to give each VM its own network namespace.
func ClassifyUserns(f UsernsFacts, required bool) Check {
	c := Check{ID: "userns", Title: "User + network namespaces"}
	sev := severity(required)
	switch {
	case f.UnprivilegedClone == 0:
		c.Status, c.Detail = sev, "unprivileged user namespaces are disabled (kernel.unprivileged_userns_clone=0)"
		c.Hint = "set kernel.unprivileged_userns_clone=1 and persist it in " + UsernsSysctlFile
		c.Sudo = usernsSysctlFix(f)
	case f.MaxUserNamespaces == 0:
		c.Status, c.Detail = sev, "user.max_user_namespaces=0"
		c.Hint = "set user.max_user_namespaces=15000 and persist it in " + UsernsSysctlFile
		c.Sudo = usernsSysctlFix(f)
	case f.AppArmorRestriction == 1 && !f.AppArmorAllowed:
		c.Status = sev
		c.Detail = "AppArmor restricts unprivileged user namespaces (Ubuntu 23.10+ default): namespaces are created without capabilities"
		c.Hint = "`" + buildinfo.Name + " init --apparmor` prints a profile scoped to this binary and the one-time root commands to load it; do not disable kernel.apparmor_restrict_unprivileged_userns, which weakens the whole host"
	case f.ProbeErr != nil:
		c.Status, c.Detail = sev, "cannot create a user+network namespace: "+f.ProbeErr.Error()
		c.Hint = "check sysctls kernel.unprivileged_userns_clone and user.max_user_namespaces, and any LSM policy"
	case f.AppArmorRestriction == 1:
		c.Status, c.Detail = Pass, "unprivileged user+network namespaces work (allowed by this binary's AppArmor profile)"
	default:
		c.Status, c.Detail = Pass, "unprivileged user+network namespaces work"
	}
	return c
}

// UsernsSysctlFile persists the userns sysctls the fix sets.
const UsernsSysctlFile = "/etc/sysctl.d/60-" + buildinfo.Name + "-userns.conf"

// usernsSysctlFix sets every userns sysctl that is off, then persists them.
func usernsSysctlFix(f UsernsFacts) []string {
	var set []string
	if f.UnprivilegedClone == 0 {
		set = append(set, "kernel.unprivileged_userns_clone=1")
	}
	if f.MaxUserNamespaces == 0 {
		set = append(set, "user.max_user_namespaces=15000")
	}
	var lines []string
	for _, kv := range set {
		lines = append(lines, "sudo sysctl -w "+kv)
	}
	return append(lines, "printf '%s\\n' "+strings.Join(set, " ")+" | sudo tee "+UsernsSysctlFile+" >/dev/null")
}

// MemFacts is /proc/meminfo in bytes.
type MemFacts struct{ Total, Available int64 }

// ParseMeminfo reads MemTotal and MemAvailable.
func ParseMeminfo(s string) MemFacts {
	var m MemFacts
	for _, line := range strings.Split(s, "\n") {
		var key string
		var kb int64
		if n, _ := fmt.Sscanf(line, "%s %d kB", &key, &kb); n != 2 {
			continue
		}
		switch key {
		case "MemTotal:":
			m.Total = kb * 1024
		case "MemAvailable:":
			m.Available = kb * 1024
		}
	}
	return m
}

// ClassifyMemory grades RAM against the configured memory limit.
func ClassifyMemory(m MemFacts, limit int64) Check {
	c := Check{ID: "memory", Title: "Memory"}
	c.Detail = fmt.Sprintf("%s total, %s available", humanBytes(m.Total), humanBytes(m.Available))
	switch {
	case m.Total == 0:
		c.Status, c.Detail, c.Hint = Warn, "cannot read /proc/meminfo", "check that /proc is mounted"
	case limit > m.Total:
		c.Status = Warn
		c.Detail += fmt.Sprintf("; configured memory limit %s exceeds physical RAM", humanBytes(limit))
		c.Hint = "lower limits.memoryBytes below physical RAM to avoid host OOM kills"
	case m.Available < domain.GiB:
		c.Status, c.Hint = Warn, "under 1GiB available: free memory before starting VMs"
	default:
		c.Status = Pass
	}
	return c
}
