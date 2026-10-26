package cli

import (
	"encoding/json"
	"testing"

	"davit/internal/domain"
	"davit/internal/lifecycle"
)

func TestNewCount(t *testing.T) {
	serveWith(t, func(c *lifecycle.Config) { c.Limits = domain.Limits{VMs: 4} })
	type out struct {
		Items  []batchOutcome
		Failed int
	}
	run := func(args ...string) (int, out, []Envelope) {
		t.Helper()
		code, envs, raws := lines(t, args...)
		var o out
		if err := json.Unmarshal(raws[len(raws)-1], &o); err != nil && code == 0 {
			t.Fatal(err)
		}
		if code != 0 && len(raws) > 1 { // the result precedes the error envelope
			json.Unmarshal(raws[len(raws)-2], &o)
		}
		return code, o, envs
	}

	code, o, _ := run("new", "web", "--count", "3")
	if code != 0 || len(o.Items) != 3 || o.Failed != 0 {
		t.Fatalf("new --count 3: exit %d %+v", code, o)
	}
	for i, it := range o.Items {
		want := "web-" + string(rune('1'+i))
		if it.Name != want || it.Final == nil || it.Final.State != domain.OpSucceeded || it.VM == nil ||
			it.VM.Name != want || it.VM.State != domain.StateReady {
			t.Fatalf("item %d: %+v", i+1, it)
		}
	}

	// One slot left: the first item is accepted, the rest refused.
	code, o, envs := run("new", "more", "--count", "3")
	if code != ExitExhaustion || o.Failed != 2 || o.Items[0].VM == nil || o.Items[1].Error == nil ||
		o.Items[2].Error.Code != domain.CodeResourceExhausted {
		t.Fatalf("over the limit: exit %d %+v", code, o)
	}
	if last := envs[len(envs)-1]; last.Error == nil || last.Error.Code != domain.CodeResourceExhausted {
		t.Fatalf("last envelope: %+v", last)
	}

	if code, _, _ := run("new", "x", "--count", "0"); code != ExitUsage {
		t.Fatalf("--count 0: exit %d", code)
	}
	if code, _, _ := run("new", "x", "--count", "101"); code != ExitUsage {
		t.Fatalf("--count 101: exit %d", code)
	}
}
