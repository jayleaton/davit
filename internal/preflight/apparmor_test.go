package preflight

import (
	"io/fs"
	"strings"
	"testing"
)

// fakeFS serves fixed file contents; anything else is fs.ErrNotExist, and
// paths mapped to nil fail with fs.ErrPermission (a root-only file).
func fakeFS(files map[string]*string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		v, ok := files[p]
		switch {
		case !ok:
			return nil, fs.ErrNotExist
		case v == nil:
			return nil, fs.ErrPermission
		}
		return []byte(*v), nil
	}
}

func str(s string) *string { return &s }

func TestParseAppArmorLabel(t *testing.T) {
	for in, want := range map[string]string{
		"davit (unconfined)\n":   "davit",
		"unconfined\n":           "unconfined",
		"unconfined":             "unconfined",
		"snap.lxd.lxc (enforce)": "snap.lxd.lxc",
		"davit (unconfined)\x00": "davit",
		"":                       "",
	} {
		if got := ParseAppArmorLabel(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestAppArmorCheck(t *testing.T) {
	const bin = "/usr/local/bin/davit"
	name := AppArmorProfileFor(bin)
	if name != "davit-778380318dd9" || AppArmorProfileFor("") != "" {
		t.Fatalf("profile name %q", name)
	}
	label := str(name + " (unconfined)\n")
	// Another user's binary on the same host has its own profile.
	profiles := "unprivileged_userns (enforce)\nchrome (unconfined)\n" + AppArmorProfileFor("/home/bob/.local/bin/davit") + " (unconfined)\ndavit (unconfined)\n"
	cases := []struct {
		name     string
		files    map[string]*string
		required bool
		want     Status
		allowed  bool
		detail   string
	}{
		{"mainline kernel", map[string]*string{appArmorSelfLabel: str("unconfined\n")}, true, Skip, false, "no AppArmor user namespace restriction"},
		{"restriction off", map[string]*string{AppArmorUsernsSysctl: str("0\n")}, true, Skip, false, "=0"},
		{"attached", map[string]*string{
			AppArmorUsernsSysctl: str("1\n"), appArmorSelfLabel: label, appArmorProfiles: nil,
		}, true, Pass, true, "runs under AppArmor profile"},
		{"attached, old kernel label file", map[string]*string{
			AppArmorUsernsSysctl: str("1\n"), appArmorSelfLabelOld: label,
		}, true, Pass, true, bin},
		{"unconfined, list root-only", map[string]*string{
			AppArmorUsernsSysctl: str("1\n"), appArmorSelfLabel: str("unconfined\n"), appArmorProfiles: nil,
		}, true, Fail, false, "unknown"},
		{"unconfined, fake runtime warns", map[string]*string{
			AppArmorUsernsSysctl: str("1\n"), appArmorSelfLabel: str("unconfined\n"),
		}, false, Warn, false, "unknown"},
		{"not loaded", map[string]*string{
			AppArmorUsernsSysctl: str("1\n"), appArmorSelfLabel: str("unconfined\n"), appArmorProfiles: str(profiles),
		}, true, Fail, false, `no profile named "` + name + `" is loaded`},
		{"loaded after this process started", map[string]*string{
			AppArmorUsernsSysctl: str("1\n"), appArmorSelfLabel: str("unconfined\n"), appArmorProfiles: str(profiles + name + " (unconfined)\n"),
		}, true, Fail, false, "restart the daemon"},
		{"another binary's profile", map[string]*string{
			AppArmorUsernsSysctl: str("1\n"), appArmorSelfLabel: str("davit (unconfined)\n"),
		}, true, Fail, false, `runs as "davit"`},
		{"label unreadable", map[string]*string{AppArmorUsernsSysctl: str("1\n")}, true, Fail, false, "unreadable"},
	}
	for _, tc := range cases {
		f := ReadAppArmorFacts(fakeFS(tc.files), bin, name)
		c := ClassifyAppArmor(f, tc.required)
		if c.Status != tc.want || f.Attached() != tc.allowed || !strings.Contains(c.Detail, tc.detail) {
			t.Errorf("%s: attached=%v %+v", tc.name, f.Attached(), c)
		}
		if (c.Status == Fail || c.Status == Warn) && !strings.Contains(c.Hint, "init --apparmor") {
			t.Errorf("%s: hint must point to init --apparmor: %q", tc.name, c.Hint)
		}
		// The userns check agrees: restricted is a failure unless attached.
		u := ClassifyUserns(UsernsFacts{UnprivilegedClone: -1, MaxUserNamespaces: 1000,
			AppArmorRestriction: f.Restriction, AppArmorAllowed: f.Attached()}, tc.required)
		if wantU := map[bool]Status{true: Pass, false: tc.want}[tc.want == Skip || tc.want == Pass]; u.Status != wantU {
			t.Errorf("%s: userns %+v, want %s", tc.name, u, wantU)
		}
	}
}
