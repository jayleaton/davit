package gateway

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"davit/api"
)

// TestDesktopProfileMatchesGateway: the image profile's VNC server is where
// the gateway looks for it, on loopback only and without a password (the
// gateway is the authentication).
func TestDesktopProfileMatchesGateway(t *testing.T) {
	b, err := os.ReadFile("../../images/manifests/profiles/ubuntu-24.04-desktop.json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Kind         string
		Capabilities struct {
			VNC struct{ Port int } `json:"desktop.vnc"`
		}
		Units map[string]struct{ File string }
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.Kind != "profile" || p.Capabilities.VNC.Port != api.DefaultDesktopPort {
		t.Fatalf("profile %+v", p)
	}
	unit, err := os.ReadFile("../../images/manifests/profiles/" + p.Units["davit-desktop.service"].File)
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../images/manifests/profiles/" + p.Units["davit-desktop"].File)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-localhost", "-nopw", "-shared", "DAVIT_DESKTOP_PORT:-5900"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("desktop script lacks %q", want)
		}
	}
	if !strings.Contains(string(unit), "ExecStart=/usr/local/libexec/davit-desktop") ||
		!strings.Contains(string(unit), "DAVIT_DESKTOP_PORT=5900") || strings.Contains(string(unit), "davit-guest-agent") {
		t.Errorf("unit %s", unit)
	}
}
