package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Quickstart":                                             "quickstart",
		"5. Use it":                                              "5-use-it",
		"Who am I, tokens and updates":                           "who-am-i-tokens-and-updates",
		"Tailnet gateway (`internal/gateway`)":                   "tailnet-gateway-internalgateway",
		"8. Share with other people — not captured":              "8-share-with-other-people--not-captured",
		"Desktop: the VM's display in a browser":                 "desktop-the-vms-display-in-a-browser",
		"snake_case and kebab-case":                              "snake_case-and-kebab-case",
		"A [linked](other.md#x) heading":                         "a-linked-heading",
		"Über <code>größe</code>":                                "über-größe",
		"`box ls`: what the host line means":                     "box-ls-what-the-host-line-means",
		"Error: `no davit host configured`":                      "error-no-davit-host-configured",
		"Prompts and credentials":                                "prompts-and-credentials",
		"Ephemeral and private VMs":                              "ephemeral-and-private-vms",
		"Remote access: one configuration for CLI, SSH and SDKs": "remote-access-one-configuration-for-cli-ssh-and-sdks",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("b.md", "# Title\n\n## Part\n\n## Part\n\n<a id=\"pinned\"></a>\n")
	write("a.md", "# A\n\n"+
		"[ok](b.md) [ok](b.md#part-1) [ok](b.md#pinned) [ok](#a) [ok](https://x.example/nope)\n"+
		"`[code](missing.md)` ``[code `x`](missing.md)``\n"+
		"```sh\n[fenced](missing.md)\n```\n"+
		"[bad](missing.md) [bad](b.md#part-2) [bad](#nope)\n"+
		"<img src=\"gone.png\">\n")
	c := &checker{anchors: map[string]map[string]bool{}}
	c.checkFile(dir, filepath.Join(dir, "a.md"))
	if c.broken != 4 {
		t.Fatalf("broken = %d, want 4", c.broken)
	}
}
