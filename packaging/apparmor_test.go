package packaging_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"davit/packaging"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Fatalf("%s differs from the rendered profile:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

func TestAppArmorProfileGolden(t *testing.T) {
	for file, bin := range map[string]string{
		"apparmor-davit.golden":       "/usr/local/bin/davit",
		"apparmor-davit-space.golden": "/home/me/my tools/davit",
	} {
		p, err := packaging.AppArmorProfile(packaging.AppArmorParams{Name: packaging.AppArmorProfileName("davit", bin), Command: "davit", Binary: bin})
		if err != nil {
			t.Fatal(err)
		}
		golden(t, file, p)
		parseOnly(t, p)
	}
}

func TestAppArmorProfileName(t *testing.T) {
	a := packaging.AppArmorProfileName("davit", "/usr/local/bin/davit")
	// Pinned: a name that changes across releases orphans installed profiles.
	if a != "davit-778380318dd9" || a == packaging.AppArmorProfileName("davit", "/home/bob/.local/bin/davit") {
		t.Fatalf("profile names: %s", a)
	}
	if _, err := packaging.AppArmorProfile(packaging.AppArmorParams{Name: a, Binary: "/usr/local/bin/davit"}); err != nil {
		t.Fatalf("a derived name must be a valid profile name: %v", err)
	}
}

func TestAppArmorAttachment(t *testing.T) {
	for bin, want := range map[string]string{
		"/usr/local/bin/davit":          "/usr/local/bin/davit",
		"/home/me/bin with space/davit": `"/home/me/bin with space/davit"`,
		"/opt/slip-way_1.2+x/davit":     "/opt/slip-way_1.2+x/davit",
	} {
		got, err := packaging.AppArmorAttachment(bin)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", bin, got, err, want)
		}
	}
	for _, bin := range []string{
		"", "davit", "bin/davit", "./davit", "/usr/bin/../bin/davit", "/usr/bin/",
		`/a"b/davit`, `/a\b/davit`, "/a*/davit", "/a?/davit", "/a[1]/davit",
		"/a{b,c}/davit", "/a^b/davit", "/@{HOME}/davit", "/a#b/davit", "/a,b/davit",
		"/a\nb/davit", "/a\x00/davit", "/é/davit",
	} {
		if got, err := packaging.AppArmorAttachment(bin); err == nil {
			t.Errorf("%q accepted as %q", bin, got)
		}
	}
	if _, err := packaging.AppArmorProfile(packaging.AppArmorParams{Name: "Slip way", Binary: "/usr/bin/davit"}); err == nil {
		t.Error("invalid profile name accepted")
	}
}

func TestAppArmorCommands(t *testing.T) {
	install, remove := packaging.AppArmorCommands("/home/me/.config/davit/apparmor/davit", "davit")
	want := []string{
		"sudo install -m 0644 -o root -g root /home/me/.config/davit/apparmor/davit /etc/apparmor.d/davit",
		"sudo apparmor_parser -r /etc/apparmor.d/davit",
	}
	if strings.Join(install, "\n") != strings.Join(want, "\n") {
		t.Fatalf("install commands:\n%s", strings.Join(install, "\n"))
	}
	if strings.Join(remove, "\n") != "sudo apparmor_parser -R /etc/apparmor.d/davit\nsudo rm /etc/apparmor.d/davit" {
		t.Fatalf("remove commands:\n%s", strings.Join(remove, "\n"))
	}
	install, _ = packaging.AppArmorCommands("/home/o'neil/my dir/davit", "davit")
	if !strings.Contains(install[0], `'/home/o'\''neil/my dir/davit'`) {
		t.Fatalf("source path not shell-quoted: %s", install[0])
	}
}

// parseOnly compiles the profile with apparmor_parser without loading it
// into the kernel (-Q) or touching the cache (-K), when the parser exists.
func parseOnly(t *testing.T, profile string) {
	t.Helper()
	parser, err := exec.LookPath("apparmor_parser")
	if err != nil {
		t.Log("apparmor_parser not on PATH; profile checked against the golden file only")
		return
	}
	if _, err := os.Stat("/etc/apparmor.d/tunables/global"); err != nil {
		t.Log("apparmor_parser found but /etc/apparmor.d/tunables/global is missing; parse check skipped")
		return
	}
	f := filepath.Join(t.TempDir(), "davit")
	if err := os.WriteFile(f, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(parser, "-Q", "-K", f).CombinedOutput(); err != nil {
		t.Fatalf("apparmor_parser -Q -K: %v\n%s", err, out)
	}
}
