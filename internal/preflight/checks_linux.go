//go:build linux

package preflight

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"davit/internal/buildinfo"
)

const kvmGetAPIVersion = 0xAE00 // _IO(KVMIO, 0x00)

func platformChecks(ctx context.Context, o Options) []Check {
	req := o.needsVM()
	aa := ReadAppArmorFacts(os.ReadFile, SelfBinary(), AppArmorProfileFor(SelfBinary()))
	uf := usernsFacts(ctx)
	uf.AppArmorAllowed = aa.Attached()
	return []Check{
		platformCheck(),
		ClassifyCPU(cpuFacts(), req),
		ClassifyKVM(kvmFacts(), req),
		ClassifyUserns(uf, req),
		ClassifyAppArmor(aa, req),
		tunCheck(),
		cgroupCheck(),
		vsockCheck(),
		ClassifyMemory(ParseMeminfo(readFile("/proc/meminfo")), o.Limits.MemoryBytes),
		firecrackerCheck(ctx, o),
	}
}

// KVM reports whether this host can boot VMs with KVM right now.
func KVM() Check { return ClassifyKVM(kvmFacts(), true) }

func platformCheck() Check {
	c := Check{ID: "platform", Title: "Host platform", Status: Pass, Detail: "linux/" + goruntime.GOARCH}
	if goruntime.GOARCH != "amd64" && goruntime.GOARCH != "arm64" {
		c.Status, c.Hint = Fail, "davit supports amd64 and arm64 Linux hosts"
	}
	return c
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// sysctlInt reads an integer sysctl, -1 if absent.
func sysctlInt(name string) int {
	v, err := strconv.Atoi(strings.TrimSpace(readFile(filepath.Join("/proc/sys", strings.ReplaceAll(name, ".", "/")))))
	if err != nil {
		return -1
	}
	return v
}

func cpuFacts() CPUFacts {
	f := CPUFacts{Arch: goruntime.GOARCH}
	f.VirtFlag, f.Hypervisor = ParseCPUInfo(readFile("/proc/cpuinfo"))
	for _, mod := range []string{"kvm_intel", "kvm_amd"} {
		if v := strings.TrimSpace(readFile("/sys/module/" + mod + "/parameters/nested")); v != "" {
			f.NestedAllowed = v
		}
	}
	return f
}

// CurrentUser reports this user and whether its systemd --user manager
// runs, and runs this process.
func CurrentUser() UserFacts {
	u := UserFacts{UID: os.Getuid()}
	if cur, err := user.Current(); err == nil {
		u.Name = cur.Username
	}
	if fi, err := os.Stat(fmt.Sprintf("/run/user/%d/systemd", u.UID)); err == nil && fi.IsDir() {
		u.ManagerRunning = true
	}
	u.InManager = InUserManager(readFile("/proc/self/cgroup"), u.UID)
	return u
}

func kvmFacts() KVMFacts {
	const dev = "/dev/kvm"
	f := KVMFacts{User: CurrentUser()}
	f.VirtFlag, _ = ParseCPUInfo(readFile("/proc/cpuinfo"))
	fi, err := os.Stat(dev)
	if err != nil {
		return f
	}
	f.Exists = true
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		gid := strconv.Itoa(int(st.Gid))
		f.Group = gid
		if g, err := user.LookupGroupId(f.Group); err == nil {
			f.Group = g.Name
		}
		groups, _ := os.Getgroups()
		f.InGroup = slices.Contains(groups, int(st.Gid)) || os.Getegid() == int(st.Gid)
		if u, err := user.Current(); err == nil {
			ids, _ := u.GroupIds()
			f.Member = slices.Contains(ids, gid)
		}
	}
	file, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		f.OpenErr = err
		f.Permission = errors.Is(err, os.ErrPermission)
		return f
	}
	defer file.Close()
	v, err := unix.IoctlRetInt(int(file.Fd()), kvmGetAPIVersion)
	if err != nil {
		f.OpenErr = err
		return f
	}
	f.APIVersion = v
	return f
}

func usernsFacts(ctx context.Context) UsernsFacts {
	f := UsernsFacts{
		UnprivilegedClone:   sysctlInt("kernel.unprivileged_userns_clone"),
		MaxUserNamespaces:   sysctlInt("user.max_user_namespaces"),
		AppArmorRestriction: sysctlInt("kernel.apparmor_restrict_unprivileged_userns"),
	}
	f.ProbeErr = probeUserns(ctx)
	return f
}

// probeUserns starts `true` in a fresh user+net namespace mapped to this
// user, which is exactly what the rootless VM launcher needs.
func probeUserns(ctx context.Context) error {
	bin, err := exec.LookPath("true")
	if err != nil {
		return nil // nothing to probe with; the sysctls decide
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	return cmd.Run()
}

func tunCheck() Check {
	c := Check{ID: "tun", Title: "TUN/TAP device"}
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		c.Status, c.Detail = Warn, "cannot open /dev/net/tun: guests get no network interface (vsock access still works)"
		c.Hint = "load the tun module (and at boot)"
		c.Sudo = []string{"sudo modprobe tun", "echo tun | sudo tee /etc/modules-load.d/" + buildinfo.Name + "-tun.conf >/dev/null"}
		return c
	}
	f.Close()
	c.Status, c.Detail = Pass, "/dev/net/tun is usable"
	return c
}

func cgroupCheck() Check {
	c := Check{ID: "cgroup_v2", Title: "cgroup v2"}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		c.Status, c.Detail = Warn, "unified cgroup v2 hierarchy not mounted: per-VM CPU/RAM/PID limits cannot be enforced"
		c.Hint = "boot with systemd.unified_cgroup_hierarchy=1"
		return c
	}
	ctrl := strings.Fields(readFile("/sys/fs/cgroup/cgroup.controllers"))
	c.Status, c.Detail = Pass, "controllers: "+strings.Join(ctrl, " ")
	return c
}

func vsockCheck() Check {
	c := Check{ID: "vsock", Title: "vsock"}
	if _, err := os.Stat("/dev/vhost-vsock"); err == nil {
		c.Status, c.Detail = Pass, "/dev/vhost-vsock present"
		return c
	}
	// Firecracker and Cloud Hypervisor expose guest vsock as a host Unix
	// socket ("hybrid vsock"), so the kernel module is not required.
	c.Status, c.Detail = Pass, "vhost_vsock not loaded; not required (Firecracker uses hybrid vsock over Unix sockets)"
	return c
}

// firecrackerBinary finds the binary the runtime would use: explicit path,
// else the newest verified release under <dataDir>/bin, else $PATH.
func firecrackerBinary(o Options) string {
	if o.FirecrackerBinary != "" {
		return o.FirecrackerBinary
	}
	m, _ := filepath.Glob(filepath.Join(o.DataDir, "bin", "firecracker-v*"))
	m = slices.DeleteFunc(m, func(p string) bool { return strings.HasSuffix(p, ".sha256") })
	if len(m) > 0 {
		sort.Strings(m)
		return m[len(m)-1]
	}
	p, _ := exec.LookPath("firecracker")
	return p
}

func firecrackerCheck(ctx context.Context, o Options) Check {
	c := Check{ID: "firecracker", Title: "Firecracker binary"}
	if o.Runtime != "firecracker" {
		c.Status, c.Detail = Skip, fmt.Sprintf("runtime is %q", o.Runtime)
		return c
	}
	bin := firecrackerBinary(o)
	if bin == "" {
		c.Status = Warn
		c.Detail = "not installed yet; the daemon downloads the pinned, checksum-verified release into " + filepath.Join(o.DataDir, "bin") + " on first start"
		c.Hint = "allow outbound HTTPS to github.com on first start, or set firecracker.binary"
		return c
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		c.Status, c.Detail, c.Hint = Fail, fmt.Sprintf("%s --version failed: %v", bin, err), "reinstall it or fix firecracker.binary"
		return c
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	c.Status, c.Detail = Pass, fmt.Sprintf("%s (%s)", first, bin)
	return c
}
