package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMacRuntimeConfig(t *testing.T) {
	c := Defaults()
	c.DataDir, c.Socket, c.Runtime = "/data", "/run/s.sock", "mac"
	if err := c.Validate(); err != nil {
		t.Fatalf("mac runtime: %v", err)
	}
	if got := c.MacHelperPath("/opt/davit/bin/davit"); got != "/opt/davit/bin/"+MacHelperName {
		t.Errorf("default helper %s", got)
	}
	if got := c.MacRuntimeDir(); got != filepath.Join("/data", "runtime", "mac") {
		t.Errorf("default runtime dir %s", got)
	}
	c.Mac = Mac{Helper: "/h/helper", RuntimeDir: "/r"}
	if c.MacHelperPath("/x/davit") != "/h/helper" || c.MacRuntimeDir() != "/r" {
		t.Errorf("explicit paths ignored: %+v", c.Mac)
	}
	c.Mac.Helper = "relative/helper"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "mac.helper") {
		t.Errorf("relative helper: %v", err)
	}
}
