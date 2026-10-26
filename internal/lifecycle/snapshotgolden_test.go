package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
)

// diskContent reads vm's working disk (the fake engine's disks are small
// text files).
func (e *env) diskContent(t *testing.T, ref string) string {
	t.Helper()
	rec := e.record(ref)
	f, err := os.Open(filepath.Join(e.dir, "storage", "disks", rec.DiskID+".img"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, 64<<10) // the text before the sparse tail
	n, _ := f.Read(b)
	s, _, _ := strings.Cut(string(b[:n]), "\x00")
	return s
}

// TestGoldenFromSnapshot: creates from a snapshot (a project image's
// instances) restore a golden of that snapshot's disk, one per snapshot,
// never the plain image's; resume after stop still boots the instance's own
// committed disk.
func TestGoldenFromSnapshot(t *testing.T) {
	e := newEnv(t, fake.Options{Golden: true}, nil)
	ctx := context.Background()
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "builder"}))
	image := e.diskContent(t, "builder") // the image's golden disk
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "builder", false))
	e.mustSucceed(e.svc.Snapshot(ctx, e.req(e.newKey()), "builder", api.SnapshotVMRequest{Name: "img1"}))

	for _, name := range []string{"a", "b"} {
		op := e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: name, SnapshotID: "img1"}))
		if op.Result.BootMode != domain.BootModeGoldenRestore {
			t.Fatalf("%s from a snapshot: %s", name, op.Result.BootMode)
		}
		// The snapshot's disk plus its own golden's boot: not the image's golden.
		if got := e.diskContent(t, name); got != image+"golden\n" {
			t.Fatalf("%s disk %q, want the snapshot's golden %q", name, got, image+"golden\n")
		}
	}
	if a, b := e.vm("a"), e.vm("b"); a.BootMode != domain.BootModeGoldenRestore || b.BootMode != domain.BootModeGoldenRestore {
		t.Fatalf("boot modes %s %s", a.BootMode, b.BootMode)
	}

	// down/up keeps the instance's own disk and cold boots it.
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "a", false))
	if op := e.mustSucceed(e.svc.Resume(ctx, e.req(""), "a", api.ResumeVMRequest{})); op.Result.BootMode != domain.BootModeColdBoot {
		t.Fatalf("resume %s", op.Result.BootMode)
	}
	if got := e.diskContent(t, "a"); got != image+"golden\n" {
		t.Fatalf("resumed disk %q", got)
	}

	// A new generation of the project image gets its own golden.
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "a", false))
	e.mustSucceed(e.svc.Snapshot(ctx, e.req(e.newKey()), "a", api.SnapshotVMRequest{Name: "img2"}))
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "c", SnapshotID: "img2"}))
	if got := e.diskContent(t, "c"); got != image+"golden\ngolden\n" || strings.Count(got, "golden\n") != strings.Count(image, "golden\n")+2 {
		t.Fatalf("second generation disk %q", got)
	}
	// RequireGolden is honoured for snapshots too.
	if op := e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "d", SnapshotID: "img1", RequireGolden: true})); op.Result.BootMode != domain.BootModeGoldenRestore {
		t.Fatalf("require golden from a snapshot: %s", op.Result.BootMode)
	}
}
