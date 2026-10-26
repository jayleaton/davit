package domain

import (
	"strings"
	"testing"
)

// built are the parity features already implemented; they start
// unsupported and the daemon reports them available when wired.
var built = map[string]bool{FeatureReverseForward: true, FeatureAppEndpoints: true, FeatureMembers: true, FeatureDesktop: true}

func TestParityFeaturesAreUniqueAndNotImplemented(t *testing.T) {
	names, ops := map[string]bool{}, map[string]string{}
	for _, f := range ParityFeatures() {
		if names[f.Name] {
			t.Errorf("feature %s listed twice", f.Name)
		}
		names[f.Name] = true
		want := FeatureNotImplemented
		if built[f.Name] {
			want = FeatureUnsupported
		}
		if f.Status != want || !strings.HasPrefix(f.Gap, "G") {
			t.Errorf("%s: status %s gap %q", f.Name, f.Status, f.Gap)
		}
		for _, op := range f.Operations {
			if prev, dup := ops[op]; dup {
				t.Errorf("operation %s in features %s and %s", op, prev, f.Name)
			}
			ops[op] = f.Name
		}
	}
	e := NotImplemented(FeatureRuns, "createRun")
	if e.Code != CodeNotImplemented || e.Details["feature"] != FeatureRuns || e.Details["operation"] != "createRun" {
		t.Fatalf("NotImplemented: %+v", e)
	}
}

func TestEnvManifestValidate(t *testing.T) {
	ok := EnvManifest{
		Repos:       []RepoSpec{{URL: "https://github.com/acme/web.git", Ref: "main", Path: "/work/web"}, {URL: "git@github.com:acme/lib.git", Path: "/work/lib"}},
		Variables:   map[string]string{"NODE_ENV": "test"},
		SecretFiles: []SecretFileRef{{Path: "/home/davit/.npmrc", Secret: "npmrc", Mode: "0600"}},
		Setup:       []SetupAction{{Name: "deps", Argv: []string{"npm", "ci"}, Cwd: "/work/web", TimeoutSeconds: 600}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	for name, mut := range map[string]func(*EnvManifest){
		"credentials in url":  func(m *EnvManifest) { m.Repos[0].URL = "https://user:tok@github.com/acme/web.git" },
		"relative repo path":  func(m *EnvManifest) { m.Repos[0].Path = "work/web" },
		"duplicate path":      func(m *EnvManifest) { m.SecretFiles[0].Path = "/work/web" },
		"unclean path":        func(m *EnvManifest) { m.Repos[1].Path = "/work/../lib" },
		"bad variable":        func(m *EnvManifest) { m.Variables["1X"] = "y" },
		"secret without name": func(m *EnvManifest) { m.SecretFiles[0].Secret = "" },
		"bad mode":            func(m *EnvManifest) { m.SecretFiles[0].Mode = "rw" },
		"setup without argv":  func(m *EnvManifest) { m.Setup[0].Argv = nil },
		"duplicate setup": func(m *EnvManifest) {
			m.Setup = append(m.Setup, SetupAction{Name: "deps", Argv: []string{"true"}})
		},
		"negative timeout": func(m *EnvManifest) { m.Setup[0].TimeoutSeconds = -1 },
	} {
		m := ok
		m.Repos = append([]RepoSpec(nil), ok.Repos...)
		m.SecretFiles = append([]SecretFileRef(nil), ok.SecretFiles...)
		m.Setup = append([]SetupAction(nil), ok.Setup...)
		m.Variables = map[string]string{"NODE_ENV": "test"}
		mut(&m)
		if err := m.Validate(); !IsCode(err, CodeInvalidArgument) {
			t.Errorf("%s: %v, want invalid_argument", name, err)
		}
	}
}

// TestGrantRevokeContract pins the grant semantics across lifecycle events.
func TestGrantRevokeContract(t *testing.T) {
	for _, c := range []struct {
		e      GrantEvent
		keep   bool
		reason RevokeReason
	}{
		{GrantOnStop, true, ""},
		{GrantOnResume, true, ""},
		{GrantOnFork, true, ""},
		{GrantOnDelete, false, RevokeVMDeleted},
	} {
		keep, inherit, reason := GrantAfter(c.e)
		if keep != c.keep || inherit || reason != c.reason {
			t.Errorf("%s: keep %v inherit %v reason %q", c.e, keep, inherit, reason)
		}
	}
}
