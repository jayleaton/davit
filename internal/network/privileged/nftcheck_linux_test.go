package privileged

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"davit/internal/domain"
)

// inUserNetNS runs script steps inside a throwaway rootless user+net
// namespace (never the host's), skipping when that is unavailable.
func inUserNetNS(t *testing.T, stdin string, argv ...string) (string, error) {
	t.Helper()
	for _, tool := range []string{"unshare", "nft"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not found in PATH")
		}
	}
	if out, err := exec.Command("unshare", "--user", "--map-root-user", "--net", "--", "true").CombinedOutput(); err != nil {
		t.Skipf("unprivileged user+net namespaces unavailable: %v: %s", err, out)
	}
	cmd := exec.Command("unshare", append([]string{"--user", "--map-root-user", "--net", "--"}, argv...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestRenderedRulesetsLoad validates both rendered rulesets with the real
// nft parser and kernel in a user-owned netns: the host ruleset loads, a VM
// element is added (as PlanCreate does), and re-applying the host ruleset
// (EnsureHost on helper restart) keeps that element.
func TestRenderedRulesetsLoad(t *testing.T) {
	cfg := testConfig()
	for _, e := range []domain.Egress{
		{Mode: domain.EgressOff}, {Mode: domain.EgressFiltered},
		{Mode: domain.EgressAllowlist, Allow: []string{"203.0.113.0/24", "example.com"}},
	} {
		plan, err := PlanSetEgress(SetVMEgress{VMID: "vm_example", Generation: 1, Index: 1, Egress: e}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		// Loaded twice: the replace form must work with and without a table.
		rs := plan[0].Stdin
		if out, err := inUserNetNS(t, rs+rs, "nft", "-f", "-"); err != nil {
			t.Fatalf("%s guest ruleset rejected: %v\n%s", e.Mode, err, out)
		}
	}
	host := RenderHostRuleset("eth0", cfg.Egress)
	create, err := PlanCreate(CreateVMNet{VMID: "vm_example", Generation: 1, Index: 1}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	addElem := create[len(create)-1].Stdin
	destroy, _ := PlanDestroy(DestroyVMNet{VMID: "vm_example", Generation: 1, Index: 1})
	delElem := destroy[0].Stdin
	// One nft process per step, so each is its own transaction like the
	// helper's separate commands. (A test-only shell inside the throwaway
	// namespace; the helper itself never uses a shell.)
	steps := []string{host, addElem, host}
	var script strings.Builder
	for i, s := range steps {
		fmt.Fprintf(&script, "nft -f - <<'EOF%d'\n%sEOF%d\n", i, s, i)
	}
	script.WriteString("nft list set inet davit vm_src\n")
	fmt.Fprintf(&script, "nft -f - <<'EOFD'\n%sEOFD\n", delElem)
	script.WriteString("nft list set inet davit vm_src\n")
	out, err := inUserNetNS(t, script.String(), "sh", "-e")
	if err != nil {
		t.Fatalf("host ruleset sequence failed: %v\n%s", err, out)
	}
	n := NamesFor("vm_example", 1)
	if c := strings.Count(out, `"`+n.HostVeth+`" . 10.200.0.6`); c != 1 {
		t.Errorf("vm_src element seen %d times, want once (kept across re-apply, gone after delete):\n%s", c, out)
	}
}
