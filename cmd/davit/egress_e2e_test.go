//go:build linux

package main_test

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
)

// TestFirecrackerEgress proves guest internet egress through whichever
// backend the host gets (the built-in userspace stack where pasta and
// slirp4netns are missing) and that the deny list holds. It needs a real
// internet connection on top of the Firecracker tier.
func TestFirecrackerEgress(t *testing.T) {
	h := newFCHost(t, nil)
	others := egressPIDs() // stacks of other VMs on this host
	h.waited(h.davit("new", "net"), domain.OpCreate)

	// Internet: HTTPS, git over HTTPS, a registry pull.
	if out := h.sh("net", "curl -sSI --max-time 20 https://example.com | head -1"); !strings.HasPrefix(out, "HTTP/") {
		t.Fatalf("curl https://example.com: %q", out)
	}
	if out := h.sh("net", "git ls-remote https://github.com/git/git HEAD"); !strings.HasSuffix(out, "HEAD") || len(out) < 45 {
		t.Fatalf("git ls-remote: %q", out)
	}
	if out := h.sh("net", "docker pull -q hello-world && docker run --rm hello-world | grep -c 'Hello from Docker'"); !strings.HasSuffix(out, "1") {
		t.Fatalf("docker pull/run hello-world: %q", out)
	}

	// Deny list: a host LAN address with a live server on it, cloud
	// metadata, and a tailnet CGNAT address.
	lan := hostLANAddr(t)
	l, err := net.Listen("tcp", net.JoinHostPort(lan.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "host secret") })}
	go srv.Serve(l)
	defer srv.Close()
	if resp, err := http.Get("http://" + l.Addr().String()); err != nil {
		t.Fatalf("host cannot reach its own server: %v", err)
	} else {
		resp.Body.Close()
	}
	for _, target := range []string{"http://" + l.Addr().String(), "http://169.254.169.254/latest/meta-data/", "http://100.100.100.100/"} {
		script := fmt.Sprintf("curl -s --max-time 4 -o /dev/null -w '%%{http_code}' %s; echo \" exit=$?\"", target)
		out := h.sh("net", script)
		if !strings.HasPrefix(out, "000 ") || strings.HasSuffix(out, "exit=0") {
			t.Errorf("guest reached %s: %q", target, out)
		}
	}

	// Throughput: a 100 MiB public test file, in the guest and, for context,
	// on the host; plus the egress stack's CPU for the guest run.
	const url, size = "https://proof.ovh.net/files/100Mb.dat", "104857600"
	egress := egressPID(t, others)
	cpu0 := procCPU(t, egress)
	t0 := time.Now()
	out := h.sh("net", "curl -sSL --max-time 300 -o /dev/null -w '%{size_download} %{speed_download}' '"+url+"'")
	wall := time.Since(t0)
	cpu := procCPU(t, egress) - cpu0
	f := strings.Fields(out)
	if len(f) != 2 || f[0] != size {
		t.Fatalf("guest download: %q", out)
	}
	guestBps, _ := strconv.ParseFloat(f[1], 64)
	hostOut, err := exec.Command("curl", "-sSL", "--max-time", "300", "-o", "/dev/null", "-w", "%{speed_download}", url).Output()
	hostBps, _ := strconv.ParseFloat(strings.TrimSpace(string(hostOut)), 64)
	if err != nil {
		t.Logf("host download failed: %v", err)
	}
	load, _ := os.ReadFile("/proc/loadavg")
	t.Logf("EGRESS guest %.1f MB/s, host %.1f MB/s, egress stack CPU %.2fs over %.2fs wall (%.0f%% of one core), load %s",
		guestBps/1e6, hostBps/1e6, cpu.Seconds(), wall.Seconds(), 100*cpu.Seconds()/wall.Seconds(), strings.Fields(string(load))[0])

	// Deleting the VM ends its egress stack too.
	h.waited(h.cli("delete", "net", "--yes"), domain.OpDelete)
	if egress != 0 {
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat("/proc/" + strconv.Itoa(egress)); err != nil {
				return
			}
		}
		t.Errorf("egress stack %d survived the VM's delete", egress)
	}
}

// hostLANAddr is a non-loopback IPv4 address of this host (guests must
// never reach it).
func hostLANAddr(t *testing.T) netip.Addr {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() && !ip.IsLoopback() {
				return ip.Unmap()
			}
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return netip.Addr{}
}

// egressPIDs lists running userspace egress stacks.
func egressPIDs() map[int]bool {
	out := map[int]bool{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, _ := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if args := strings.Split(string(b), "\x00"); len(args) > 1 && args[1] == "netstack-egress" {
			out[pid] = true
		}
	}
	return out
}

// egressPID is the egress stack started since others was taken (this test
// runs one VM).
func egressPID(t *testing.T, others map[int]bool) int {
	for pid := range egressPIDs() {
		if !others[pid] {
			return pid
		}
	}
	t.Log("no new netstack-egress process (another egress backend is in use)")
	return 0
}

// procCPU is a process's user+system CPU time.
func procCPU(t *testing.T, pid int) time.Duration {
	if pid == 0 {
		return 0
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	ut, _ := strconv.ParseInt(f[11], 10, 64)        // field 14
	st, _ := strconv.ParseInt(f[12], 10, 64)        // field 15
	return time.Duration(ut+st) * time.Second / 100 // USER_HZ = 100
}

// TestFirecrackerEgressPolicy: an allowlist VM reaches only its listed
// domains, and its policy changes live (no restart) to off and filtered.
func TestFirecrackerEgressPolicy(t *testing.T) {
	h := newFCHost(t, nil)
	r := h.davit("new", "pol", "--egress", "allowlist", "--allow", "github.com")
	vm := h.waited(r, domain.OpCreate)
	if vm.Egress.Mode != domain.EgressAllowlist || strings.Join(vm.Egress.Allow, ",") != "github.com" {
		t.Fatalf("created egress %+v", vm.Egress)
	}
	gen := vm.InstanceGeneration
	try := func(script string) string {
		t.Helper()
		return h.sh("pol", script+"; echo \" exit=$?\"")
	}
	gitOK := "timeout 30 git ls-remote https://github.com/git/git HEAD >/dev/null 2>&1"
	if out := try(gitOK); !strings.HasSuffix(out, "exit=0") {
		t.Fatalf("allowlist: git ls-remote github.com: %q", out)
	}
	if out := try("curl -s --max-time 8 -o /dev/null https://example.com"); strings.HasSuffix(out, "exit=0") {
		t.Fatalf("allowlist: example.com reachable: %q", out)
	}
	if out := try("getent hosts example.com"); strings.HasSuffix(out, "exit=0") {
		t.Fatalf("allowlist: example.com resolved: %q", out)
	}
	if out := try("curl -s --max-time 8 -o /dev/null https://1.1.1.1"); strings.HasSuffix(out, "exit=0") {
		t.Fatalf("allowlist: an unlisted IP is reachable: %q", out)
	}

	// Live: off blocks everything, filtered opens the internet again.
	if r := h.cli("egress", "pol", "off"); r.code != 0 {
		t.Fatalf("egress off: exit %d %+v", r.code, r.last().Error)
	}
	if out := try(gitOK); strings.HasSuffix(out, "exit=0") {
		t.Fatalf("off: github.com still reachable: %q", out)
	}
	if r := h.cli("egress", "pol", "filtered"); r.code != 0 {
		t.Fatalf("egress filtered: exit %d %+v", r.code, r.last().Error)
	}
	if out := try("curl -s --max-time 20 -o /dev/null https://example.com"); !strings.HasSuffix(out, "exit=0") {
		t.Fatalf("filtered: example.com unreachable: %q", out)
	}
	if got := h.info("pol"); got.InstanceGeneration != gen || got.Egress.Mode != domain.EgressFiltered {
		t.Fatalf("policy change restarted the VM or was not recorded: gen %d->%d %+v", gen, got.InstanceGeneration, got.Egress)
	}
	h.waited(h.cli("delete", "pol", "--yes"), domain.OpDelete)
}
