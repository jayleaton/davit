//go:build linux

package main_test

import (
	"strings"
	"testing"

	"davit/internal/config"
	"davit/internal/domain"
	"davit/internal/runtime/firecracker/fctest"
)

// TestBakedGuestAgent: this daemon still talks to the guest agent baked
// into the host's image as it was built (the other real-VM tests inject
// this checkout's agent). It guards the frozen guest protocol: a host
// upgrade must not lock out the VMs of images built by an earlier release.
func TestBakedGuestAgent(t *testing.T) {
	fx := fctest.Setup(t) // built once per process; newFCHost uses the same
	h := newFCHost(t, func(c *config.Daemon) {
		c.Images = []config.ImageSource{{Manifest: fx.SourceManifest, BlobDir: fx.SourceBlobDir}}
	})
	r := h.davit("new", "baked")
	if r.code != 0 {
		if e := r.last().Error; e != nil && strings.Contains(e.Message, "rebuild the image") {
			t.Skipf("the host's image predates features this daemon requires: %s", e.Message)
		}
		t.Fatalf("new from the baked image: exit %d %+v", r.code, r.last().Error)
	}
	h.waited(r, domain.OpCreate)
	if out := h.sh("baked", "echo ok"); out != "ok" {
		t.Fatalf("exec through the baked agent: %q", out)
	}
	h.waited(h.cli("delete", "baked", "--yes"), domain.OpDelete)
}
