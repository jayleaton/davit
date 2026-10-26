package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
	"davit/internal/lifecycle"
)

// TestExtendRenameTypeAndNotice drives the TTL and profile verbs through the
// CLI: --type picks a profile, extend restarts the countdown (and sets the
// TTL when given), rename renames, and info warns on stderr inside the
// auto-stop warning window.
func TestExtendRenameTypeAndNotice(t *testing.T) {
	serveWith(t, func(c *lifecycle.Config) {
		c.Profiles = []domain.Profile{{Name: "big", Shape: domain.Shape{CPUs: 4}}}
		c.TTLWarning = 2 * time.Hour // every VM with the default 1h TTL is inside the window
	})
	if r := runCLI(t, false, "new", "box", "--type", "big"); r.code != 0 || r.data["vm"].(map[string]any)["shape"].(map[string]any)["cpus"] != float64(4) {
		t.Fatalf("new --type big: exit %d %+v", r.code, r.data)
	}
	if r := runCLI(t, false, "new", "nope", "--type", "huge"); r.code != ExitUsage {
		t.Fatalf("unknown profile: exit %d", r.code)
	}

	before := time.Now()
	r := runCLI(t, false, "extend", "box", "3h")
	exp, _ := time.Parse(time.RFC3339Nano, r.data["expiresAt"].(string))
	if r.code != 0 || r.data["ttlSeconds"] != float64(3*3600) || exp.Before(before.Add(3*time.Hour-time.Minute)) {
		t.Fatalf("extend 3h: exit %d %+v", r.code, r.data)
	}
	t.Setenv("DAVIT_CURRENT_ID", "box")
	if r := runCLI(t, false, "extend", "30m"); r.code != 0 || r.data["ttlSeconds"] != float64(1800) {
		t.Fatalf("extend DURATION with the current VM: exit %d %+v", r.code, r.data)
	}
	t.Setenv("DAVIT_CURRENT_ID", "")
	if r := runCLI(t, false, "extend", "box", "-5m"); r.code != ExitUsage {
		t.Fatalf("negative duration: exit %d", r.code)
	}

	if r := runCLI(t, false, "rename", "box", "box2"); r.code != 0 || r.data["name"] != "box2" {
		t.Fatalf("rename: exit %d %+v", r.code, r.data)
	}

	var stdout, stderr bytes.Buffer
	code := Main([]string{"info", "box2"}, Stdio{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
	if code != 0 || !strings.Contains(stderr.String(), "notice: box2 auto-stops in") || !strings.Contains(stderr.String(), "extend box2") {
		t.Fatalf("info inside the warning window: exit %d stderr %q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"--json", "info", "box2"}, Stdio{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr}); code != 0 || strings.Contains(stdout.String(), "notice:") {
		t.Fatalf("--json stdout must stay JSONL: %q", stdout.String())
	}
}
