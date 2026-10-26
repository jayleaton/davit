package fctest

import "testing"

// TestFixture builds the real-VM fixture once (pinned Firecracker binary,
// this checkout's agent, the derived test image) without booting a VM.
// `scripts/vm-tier.sh --prepare` runs it to warm a VM host; skipped unless
// DAVIT_TEST_FIRECRACKER=1.
func TestFixture(t *testing.T) {
	fx := Setup(t)
	t.Logf("firecracker %s (%s)", fx.Binary.Version, fx.Binary.Path)
	t.Logf("image %s rootfs %s", fx.Manifest, fx.Image.Rootfs)
}
