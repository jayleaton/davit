package preflight

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
	hp "davit/internal/runtime/mac/helperproto"
	"davit/internal/storage"
)

func TestClassifyKVM(t *testing.T) {
	denied := KVMFacts{Exists: true, Permission: true, OpenErr: os.ErrPermission, Group: "kvm"}
	member := denied
	member.Member = true
	inGroup := denied
	inGroup.InGroup = true
	for name, tc := range map[string]struct {
		f        KVMFacts
		required bool
		want     Status
	}{
		"missing, firecracker": {KVMFacts{}, true, Fail},
		"missing, fake":        {KVMFacts{}, false, Warn},
		"module not loaded":    {KVMFacts{VirtFlag: "svm"}, true, Fail},
		"no group":             {denied, true, Fail},
		"stale session":        {member, true, Fail},
		"device mode":          {inGroup, true, Fail},
		"bad api":              {KVMFacts{Exists: true, APIVersion: 11}, true, Fail},
		"ok":                   {KVMFacts{Exists: true, APIVersion: 12}, true, Pass},
	} {
		c := ClassifyKVM(tc.f, tc.required)
		if c.Status != tc.want || (c.Status != Pass && c.Hint == "") {
			t.Errorf("%s: %+v", name, c)
		}
	}
	if c := ClassifyKVM(member, true); c.Hint == ClassifyKVM(denied, true).Hint || !strings.Contains(c.Hint, "newgrp kvm") {
		t.Error("a user already in the group needs a re-login hint, not usermod")
	}
	for flag, mod := range map[string]string{"vmx": "kvm_intel", "svm": "kvm_amd", "": ""} {
		c := ClassifyKVM(KVMFacts{VirtFlag: flag}, true)
		if want := []string{"sudo modprobe " + mod}; mod == "" && c.Sudo != nil || mod != "" && !slices.Equal(c.Sudo, want) {
			t.Errorf("missing /dev/kvm with CPU flag %q: sudo %q", flag, c.Sudo)
		}
	}

	// The group fix restarts the user manager only when one runs and this
	// process is outside it; otherwise the hint says to reboot.
	restart := "sudo systemctl restart user@1000.service"
	for name, tc := range map[string]struct {
		u        UserFacts
		f        KVMFacts
		sudo     []string
		hintPart string
	}{
		"ssh session":      {UserFacts{Name: "alice", UID: 1000, ManagerRunning: true}, denied, []string{"sudo usermod -aG kvm alice", restart}, "user@1000.service"},
		"desktop terminal": {UserFacts{Name: "alice", UID: 1000, ManagerRunning: true, InManager: true}, denied, []string{"sudo usermod -aG kvm alice"}, "reboot"},
		"no manager":       {UserFacts{Name: "alice", UID: 1000}, denied, []string{"sudo usermod -aG kvm alice"}, "log in again"},
		"odd name":         {UserFacts{Name: "a b", UID: 1000}, denied, []string{"sudo usermod -aG kvm 'a b'"}, "log in again"},
		"unknown name":     {UserFacts{UID: 1000, ManagerRunning: true}, denied, nil, "usermod"},
		"stale, ssh":       {UserFacts{Name: "alice", UID: 1000, ManagerRunning: true}, member, []string{restart}, "newgrp kvm"},
		"stale, desktop":   {UserFacts{Name: "alice", UID: 1000, ManagerRunning: true, InManager: true}, member, nil, "reboot"},
	} {
		tc.f.User = tc.u
		c := ClassifyKVM(tc.f, true)
		if !slices.Equal(c.Sudo, tc.sudo) || !strings.Contains(c.Hint, tc.hintPart) {
			t.Errorf("%s: sudo %q, hint %q", name, c.Sudo, c.Hint)
		}
	}
}

func TestInUserManager(t *testing.T) {
	for cg, want := range map[string]bool{
		"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-foot.scope\n": true,
		"0::/user.slice/user-1000.slice/user@1000.service\n":                          true,
		"0::/user.slice/user-1000.slice/session-4.scope\n":                            false,
		"0::/user.slice/user-1001.slice/user@10001.service/app.slice\n":               false,
		"12:pids:/user.slice/user-1000.slice/user@1000.service\n0::/init.scope\n":     true,
		"": false,
	} {
		if got := InUserManager(cg, 1000); got != want {
			t.Errorf("%q: %v", cg, got)
		}
	}
}

func TestClassifyCPU(t *testing.T) {
	virt, hv := ParseCPUInfo("processor\t: 0\nflags\t\t: fpu vme vmx hypervisor sse\n\nflags : ignored svm\n")
	if virt != "vmx" || !hv {
		t.Fatalf("parse: %q %v", virt, hv)
	}
	for name, tc := range map[string]struct {
		f    CPUFacts
		want Status
	}{
		"bare metal":       {CPUFacts{Arch: "amd64", VirtFlag: "svm"}, Pass},
		"nested ok":        {CPUFacts{Arch: "amd64", VirtFlag: "vmx", Hypervisor: true}, Pass},
		"vm without nest":  {CPUFacts{Arch: "amd64", Hypervisor: true}, Fail},
		"disabled in bios": {CPUFacts{Arch: "amd64"}, Fail},
		"arm64":            {CPUFacts{Arch: "arm64"}, Skip},
		"riscv":            {CPUFacts{Arch: "riscv64"}, Fail},
	} {
		if c := ClassifyCPU(tc.f, true); c.Status != tc.want {
			t.Errorf("%s: %+v", name, c)
		}
	}
}

func TestClassifyUserns(t *testing.T) {
	ok := UsernsFacts{UnprivilegedClone: -1, MaxUserNamespaces: 1000, AppArmorRestriction: -1}
	if c := ClassifyUserns(ok, true); c.Status != Pass {
		t.Fatalf("mainline kernel: %+v", c)
	}
	ubuntu := ok
	ubuntu.AppArmorRestriction = 1 // the probe succeeds there, but without capabilities
	if c := ClassifyUserns(ubuntu, true); c.Status != Fail || c.Hint == "" {
		t.Fatalf("Ubuntu 24.04 AppArmor restriction: %+v", c)
	}
	if c := ClassifyUserns(ubuntu, false); c.Status != Warn {
		t.Fatalf("fake runtime must only warn: %+v", c)
	}
	if c := ClassifyUserns(ubuntu, true); !strings.Contains(c.Hint, "init --apparmor") {
		t.Fatalf("restriction hint must point to init --apparmor: %+v", c)
	}
	profiled := ubuntu
	profiled.AppArmorAllowed = true
	if c := ClassifyUserns(profiled, true); c.Status != Pass {
		t.Fatalf("restriction lifted by the davit profile: %+v", c)
	}
	profiled.ProbeErr = errors.New("operation not permitted")
	if c := ClassifyUserns(profiled, true); c.Status != Fail {
		t.Fatalf("profile attached but the probe fails: %+v", c)
	}
	both := ClassifyUserns(UsernsFacts{UnprivilegedClone: 0, MaxUserNamespaces: 0, AppArmorRestriction: -1}, true)
	if want := []string{
		"sudo sysctl -w kernel.unprivileged_userns_clone=1",
		"sudo sysctl -w user.max_user_namespaces=15000",
		`printf '%s\n' kernel.unprivileged_userns_clone=1 user.max_user_namespaces=15000 | sudo tee /etc/sysctl.d/60-davit-userns.conf >/dev/null`,
	}; !slices.Equal(both.Sudo, want) {
		t.Errorf("userns sysctl fix: %q", both.Sudo)
	}
	if c := ClassifyUserns(ubuntu, true); c.Sudo != nil {
		t.Errorf("the AppArmor restriction is fixed by the apparmor check, not here: %q", c.Sudo)
	}
	for _, f := range []UsernsFacts{
		{UnprivilegedClone: 0, MaxUserNamespaces: 1000, AppArmorRestriction: -1},
		{UnprivilegedClone: -1, MaxUserNamespaces: 0, AppArmorRestriction: -1},
		{UnprivilegedClone: 1, MaxUserNamespaces: 1000, AppArmorRestriction: 0, ProbeErr: errors.New("operation not permitted")},
	} {
		if c := ClassifyUserns(f, true); c.Status != Fail {
			t.Errorf("%+v: %+v", f, c)
		}
	}
}

func TestClassifyResources(t *testing.T) {
	m := ParseMeminfo("MemTotal:       16000000 kB\nMemFree:  1 kB\nMemAvailable:    8000000 kB\n")
	if m.Total != 16000000*1024 || m.Available != 8000000*1024 {
		t.Fatalf("meminfo %+v", m)
	}
	if c := ClassifyMemory(m, 0); c.Status != Pass {
		t.Fatalf("%+v", c)
	}
	if c := ClassifyMemory(m, 64*domain.GiB); c.Status != Warn {
		t.Fatalf("limit above RAM: %+v", c)
	}
	fs := func(free, total int64) domain.DiskUsage { return domain.DiskUsage{FreeBytes: free, TotalBytes: total} }
	if s, _, _ := ClassifyDiskSpace(fs(50*domain.GiB, 0), domain.Limits{DiskBytes: 100 * domain.GiB, MinFreeDiskBytes: -1}); s != Warn {
		t.Fatal("free below limit must warn")
	}
	if s, _, _ := ClassifyDiskSpace(fs(50*domain.GiB, 200*domain.GiB), domain.Limits{}); s != Pass {
		t.Fatal("plenty of space")
	}
	// The default floor on a 200 GiB disk is 20 GiB (10%).
	if s, d, h := ClassifyDiskSpace(fs(30*domain.GiB, 200*domain.GiB), domain.Limits{}); s != Warn || h == "" || !strings.Contains(d, "floor 20.0GiB") {
		t.Fatalf("free space below twice the floor must warn: %s %q", s, d)
	}
	if s, _, h := ClassifyDiskSpace(fs(500*domain.MiB, 200*domain.GiB), domain.Limits{}); s != Fail || h == "" {
		t.Fatal("free space below the admission floor must fail with a hint")
	}
}

func TestTailnetChecks(t *testing.T) {
	dir := t.TempDir()
	o := Options{TailnetStateDir: filepath.Join(dir, "tailnet"), AuthKeyEnv: "DAVIT_TEST_NO_SUCH_ENV"}
	// The gateway is optional: while it is off nothing about it warns.
	for _, c := range append([]Check{tailnetStateCheck(o), tailnetAuthCheck(o), tailnetOwnerCheck(o)}, GatewayChecks(o, time.Now())...) {
		if c.Status != Skip || c.Detail != tailnetOff {
			t.Errorf("gateway off: %+v", c)
		}
	}
	o.TailnetEnabled = true
	if c := tailnetStateCheck(o); c.Status != Pass {
		t.Fatalf("not yet created: %+v", c)
	}
	if c := tailnetAuthCheck(o); c.Status != Warn {
		t.Fatalf("no key: %+v", c)
	}
	os.Mkdir(o.TailnetStateDir, 0o755)
	if c := tailnetStateCheck(o); c.Status != Fail {
		t.Fatalf("world-readable state dir: %+v", c)
	}
	os.Chmod(o.TailnetStateDir, 0o700)
	if c := tailnetStateCheck(o); c.Status != Pass {
		t.Fatalf("private state dir: %+v", c)
	}
	o.AuthKeyFile = filepath.Join(dir, "key")
	os.WriteFile(o.AuthKeyFile, []byte("tskey-auth-secret"), 0o644)
	if c := tailnetAuthCheck(o); c.Status != Fail {
		t.Fatalf("readable key file: %+v", c)
	}
	os.Chmod(o.AuthKeyFile, 0o600)
	c := tailnetAuthCheck(o)
	if c.Status != Pass {
		t.Fatalf("private key file: %+v", c)
	}
	t.Setenv("DAVIT_TEST_KEY", "tskey-auth-secret")
	o.AuthKeyFile, o.AuthKeyEnv = "", "DAVIT_TEST_KEY"
	if c := tailnetAuthCheck(o); c.Status != Pass {
		t.Fatalf("env key: %+v", c)
	}
	os.WriteFile(filepath.Join(o.TailnetStateDir, "tailscaled.state"), []byte("{}"), 0o600)
	t.Setenv("DAVIT_TEST_KEY", "")
	if c := tailnetAuthCheck(o); c.Status != Pass {
		t.Fatalf("enrolled node needs no key: %+v", c)
	}
	o.TailnetEnabled = false
	if c := tailnetOwnerCheck(o); c.Status != Skip || c.Hint == "" {
		t.Fatalf("disabled gateway: %+v", c)
	}
	o.TailnetEnabled = true
	if c := tailnetOwnerCheck(o); c.Status != Fail || c.Hint == "" {
		t.Fatalf("enabled without owner: %+v", c)
	}
	o.TailnetOwners = 1
	if c := tailnetOwnerCheck(o); c.Status != Pass {
		t.Fatalf("owner configured: %+v", c)
	}
	for _, c := range []Check{tailnetAuthCheck(o), tailnetStateCheck(o)} {
		if contains(c.Detail+c.Hint, "tskey") {
			t.Fatalf("secret leaked into report: %+v", c)
		}
	}
}

func TestStorageCheckIsHonest(t *testing.T) {
	dir := t.TempDir()
	p, _ := storage.ProbeDir(dir, "")
	c := storageCheck(Options{DataDir: dir})
	if p.FastClone != (c.Status == Pass) {
		t.Fatalf("storage check %+v disagrees with probe %+v", c, p)
	}
	if c := storageCheck(Options{DataDir: dir, StorageMode: "copy"}); c.Status != Warn {
		t.Fatalf("forced copy: %+v", c)
	}
	if !p.FastClone {
		if c := storageCheck(Options{DataDir: dir, StorageMode: "reflink"}); c.Status != Fail {
			t.Fatalf("reflink requested on a non-reflink fs: %+v", c)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("probe left files behind: %v", entries)
	}
}

func TestRunReportsEveryCheck(t *testing.T) {
	dir := t.TempDir()
	r := Run(context.Background(), Options{Runtime: "fake", DataDir: filepath.Join(dir, "data"), TailnetStateDir: filepath.Join(dir, "data", "tailnet")})
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if c.ID == "" || c.Title == "" || c.Status == "" || c.Detail == "" || seen[c.ID] {
			t.Errorf("malformed or duplicate check %+v", c)
		}
		seen[c.ID] = true
		if c.Status == Fail && c.Hint == "" {
			t.Errorf("failed check without a fix hint: %+v", c)
		}
		t.Logf("%-14s %-4s %s", c.ID, c.Status, c.Detail)
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); err == nil {
		t.Fatal("preflight must not create the data dir")
	}
}

// TestEveryProblemHasAFix sweeps the classifiers' failing and warning
// inputs: each result needs a one-line fix, since doctor prints only that.
func TestEveryProblemHasAFix(t *testing.T) {
	perm := KVMFacts{Exists: true, Permission: true, OpenErr: os.ErrPermission, Group: "kvm", User: UserFacts{Name: "u", UID: 1, ManagerRunning: true}}
	var checks []Check
	for _, req := range []bool{true, false} {
		member, inGroup := perm, perm
		member.Member, inGroup.InGroup = true, true
		for _, f := range []KVMFacts{{}, {VirtFlag: "vmx"}, perm, member, inGroup, {Exists: true, OpenErr: errors.New("x")}, {Exists: true, APIVersion: 11}} {
			checks = append(checks, ClassifyKVM(f, req))
		}
		for _, f := range []CPUFacts{{Arch: "riscv64"}, {Arch: "amd64"}, {Arch: "amd64", Hypervisor: true}} {
			checks = append(checks, ClassifyCPU(f, req))
		}
		for _, f := range []UsernsFacts{
			{UnprivilegedClone: 0}, {UnprivilegedClone: -1, MaxUserNamespaces: 0},
			{UnprivilegedClone: -1, MaxUserNamespaces: -1, AppArmorRestriction: 1},
			{UnprivilegedClone: -1, MaxUserNamespaces: -1, ProbeErr: errors.New("x")},
		} {
			checks = append(checks, ClassifyUserns(f, req))
		}
		for _, f := range []AppArmorFacts{{Restriction: 1, Profile: "davit", Loaded: -1}, {Restriction: 1, Profile: "davit", Label: "unconfined", Loaded: 0}} {
			checks = append(checks, ClassifyAppArmor(f, req))
		}
		checks = append(checks, ClassifyImages(nil, req), ClassifyImages([]ImageFacts{{Err: errors.New("x")}}, req),
			ClassifyImages([]ImageFacts{{Name: "a", Compat: guestclient.CompatDegraded}}, req), ClassifyImages([]ImageFacts{{Name: "a"}}, req))
		checks = append(checks, MacOSCheck("amd64", "15.0", req), MacOSCheck("arm64", "", req), MacOSCheck("arm64", "13.1", req),
			MacSignatureCheck(errors.New("x"), "", nil, "", req), MacSignatureCheck(nil, "", errors.New("x"), "", req))
		checks = append(checks, MacHelperChecks("/h", hp.ProbeResult{}, errors.New("x"), req)...)
		checks = append(checks, MacHelperChecks("/h", hp.ProbeResult{MinVersion: 99, MaxVersion: 99}, nil, req)...)
		checks = append(checks, MacHelperChecks("/h", hp.ProbeResult{MinVersion: 1, MaxVersion: 1, VZSupported: true}, nil, req)...)
	}
	for _, m := range []MemFacts{{}, {Total: domain.GiB, Available: domain.MiB}} {
		checks = append(checks, ClassifyMemory(m, 0))
	}
	checks = append(checks, ClassifyMemory(MemFacts{Total: domain.GiB, Available: domain.GiB}, 2*domain.GiB))
	for _, d := range []struct {
		free, total int64
		l           domain.Limits
	}{{domain.MiB, 0, domain.Limits{}}, {20 * domain.GiB, 0, domain.Limits{DiskBytes: 50 * domain.GiB, MinFreeDiskBytes: -1}},
		{domain.GiB, 0, domain.Limits{MinFreeDiskBytes: -1}}, {7 * domain.GiB, 0, domain.Limits{}}} {
		s, detail, hint := ClassifyDiskSpace(domain.DiskUsage{FreeBytes: d.free, TotalBytes: d.total}, d.l)
		checks = append(checks, Check{ID: "disk_space", Status: s, Detail: detail, Hint: hint})
	}
	dir := t.TempDir()
	o := Options{TailnetEnabled: true, TailnetStateDir: filepath.Join(dir, "tailnet"), AuthKeyEnv: "DAVIT_TEST_NO_SUCH_ENV"}
	checks = append(checks, tailnetAuthCheck(o), tailnetOwnerCheck(o))
	checks = append(checks, GatewayChecks(o, time.Now())...)
	for _, c := range checks {
		if (c.Status == Fail || c.Status == Warn) && (c.Hint == "" || strings.Contains(c.Hint, "\n")) {
			t.Errorf("%s %s without a one-line fix: %+v", c.ID, c.Status, c)
		}
		for _, l := range c.Sudo {
			if !strings.Contains(l, "sudo ") || strings.Contains(l, "\n") {
				t.Errorf("%s: sudo line %q", c.ID, l)
			}
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestImagesCheck(t *testing.T) {
	dir := t.TempDir()
	blobs := filepath.Join(dir, "blobs")
	os.MkdirAll(filepath.Join(blobs, "sha256"), 0o700)
	put := func(name, content string) string {
		os.WriteFile(filepath.Join(blobs, "sha256", name), []byte(content), 0o600)
		return "sha256:" + name
	}
	k, r := put("aa", "kern"), put("bb", "rootfs!")
	write := func(name, agent string, protocol ...string) string {
		proto := "null"
		if len(protocol) > 0 {
			proto = protocol[0]
		}
		m := fmt.Sprintf(`{"name":%q,"kernel":{"digest":%q,"size":4},"rootfs":{"digest":%q,"size":7},"guest":{"agent":{"binary":%s,"protocol":%s}}}`, name, k, r, agent, proto)
		p := filepath.Join(dir, name+".json")
		os.WriteFile(p, []byte(m), 0o600)
		return p
	}
	agent := `{"digest":"sha256:cc"}`
	current, _ := json.Marshal(guestclient.AgentProtocol{Min: guestproto.MinProtocolVersion, Max: guestproto.ProtocolVersion, Features: guestclient.Features})
	good := inspectImage(config.ImageSource{Manifest: write("with-agent", agent, string(current)), BlobDir: blobs})
	// Protocol skew: no common version (major) refuses; missing features (minor)
	// or an untracked protocol warns.
	major := inspectImage(config.ImageSource{Manifest: write("future-agent", agent, `{"min":99,"max":99,"features":["exec"]}`), BlobDir: blobs})
	minor := inspectImage(config.ImageSource{Manifest: write("old-agent", agent, `{"min":1,"max":1,"features":["exec"]}`), BlobDir: blobs})
	legacy := inspectImage(config.ImageSource{Manifest: write("legacy-agent", agent), BlobDir: blobs})
	bare := inspectImage(config.ImageSource{Manifest: write("no-agent", "null"), BlobDir: blobs})
	missing := inspectImage(config.ImageSource{Manifest: write("elsewhere", "null"), BlobDir: dir})
	if good.Err != nil || !good.Agent || good.Compat != guestclient.CompatOK || bare.Err != nil || bare.Agent || missing.Err == nil {
		t.Fatalf("inspect: %+v %+v %+v", good, bare, missing)
	}
	if major.Compat != guestclient.CompatIncompatible || minor.Compat != guestclient.CompatDegraded ||
		!strings.Contains(minor.CompatMsg, guestproto.FeatureTCP) || legacy.Compat != guestclient.CompatDegraded {
		t.Fatalf("compat: major %+v minor %+v legacy %+v", major, minor, legacy)
	}
	for name, tc := range map[string]struct {
		facts []ImageFacts
		want  Status
	}{
		"none":           {nil, Fail},
		"usable":         {[]ImageFacts{good}, Pass},
		"agentless only": {[]ImageFacts{bare}, Warn},
		"mixed":          {[]ImageFacts{good, bare}, Pass},
		"broken":         {[]ImageFacts{good, missing}, Fail},
		"major skew":     {[]ImageFacts{good, major}, Fail},
		"minor skew":     {[]ImageFacts{good, minor}, Warn},
		"untracked":      {[]ImageFacts{legacy}, Warn},
	} {
		if c := ClassifyImages(tc.facts, true); c.Status != tc.want || (c.Status != Pass && c.Hint == "") {
			t.Errorf("%s: %+v", name, c)
		}
	}
	if c := imagesCheck(Options{Runtime: "fake"}); c.Status != Skip {
		t.Fatalf("fake runtime: %+v", c)
	}
}

func TestIOPressure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "io")
	os.WriteFile(p, []byte("some avg10=31.50 avg60=4.00 avg300=1.00 total=123\nfull avg10=2.00 avg60=0.00 avg300=0.00 total=9\n"), 0o600)
	if v, ok := ioPressure(p); !ok || v != 31.5 {
		t.Fatalf("ioPressure = %v %v", v, ok)
	}
	if _, ok := ioPressure(filepath.Join(t.TempDir(), "missing")); ok {
		t.Fatal("a missing PSI file reads as a value")
	}
}
