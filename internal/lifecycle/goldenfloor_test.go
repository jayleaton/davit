package lifecycle

import (
	"context"
	"encoding/json"
	"testing"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/runtime/fake"
	"davit/internal/state"
	"davit/internal/storage"
)

// TestGoldenEvictionEvents: the engine's golden floor is enforced at start
// and on sweeps; each eviction is a golden.evicted event (a project image's
// golden to its snapshot's owner, an image's to the host owner); the next
// create from an evicted snapshot captures its golden again and restores it.
func TestGoldenEvictionEvents(t *testing.T) {
	const host = domain.PrincipalID("local:host")
	e := newEnv(t, fake.Options{Golden: true}, func(c *Config) { c.HostOwner = host })
	ctx := context.Background()
	if e.rt.FloorChecks() == 0 {
		t.Fatal("golden floor not enforced at start")
	}
	img, err := e.svc.cfg.Images.Resolve(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "builder"}))
	e.mustSucceed(e.svc.Stop(ctx, e.req(""), "builder", false))
	e.mustSucceed(e.svc.Snapshot(ctx, e.req(e.newKey()), "builder", api.SnapshotVMRequest{Name: "img1"}))
	e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "a", SnapshotID: "img1"}))
	want := e.diskContent(t, "a")
	shape := e.vm("a").Shape

	snap, err := e.svc.GetSnapshot(ctx, owner, "img1")
	if err != nil {
		t.Fatal(err)
	}
	var commitID string
	e.db.Tx(ctx, func(tx *state.Tx) error {
		s, err := tx.GetSnapshot(snap.ID)
		if err == nil {
			commitID = s.CommitID
		}
		return err
	})
	c, err := e.svc.cfg.Storage.GetCommit(ctx, storage.CommitID(commitID))
	if err != nil {
		t.Fatal(err)
	}
	base := img
	base.Rootfs = c.Path
	e.rt.EvictGolden(base, shape) // the project image's golden
	e.rt.EvictGolden(img, shape)  // the plain image's golden

	events := func(o domain.PrincipalID) []state.EventRow {
		var rows []state.EventRow
		e.db.Tx(ctx, func(tx *state.Tx) (err error) {
			rows, err = tx.ListEvents(o, 0, 1000, state.EventFilter{Types: []string{api.EventGoldenEvicted}})
			return err
		})
		return rows
	}
	mine := events(owner)
	if len(mine) != 1 || mine[0].Subject != string(snap.ID) {
		t.Fatalf("owner's golden.evicted events %+v", mine)
	}
	var data map[string]any
	json.Unmarshal(mine[0].Data, &data)
	if data["snapshotId"] != string(snap.ID) || data["goldenId"] == "" || data["reason"] != "test" {
		t.Fatalf("event data %v", data)
	}
	if hostEv := events(host); len(hostEv) != 1 || hostEv[0].Subject == string(snap.ID) {
		t.Fatalf("host's golden.evicted events %+v", hostEv)
	}

	// The next instance recaptures the snapshot's golden and restores it.
	op := e.mustSucceed(e.svc.Create(ctx, e.req(e.newKey()), api.CreateVMRequest{Name: "b", SnapshotID: "img1"}))
	if op.Result.BootMode != domain.BootModeGoldenRestore || e.diskContent(t, "b") != want {
		t.Fatalf("after eviction: %s %q, want golden restore of %q", op.Result.BootMode, e.diskContent(t, "b"), want)
	}
	// Sweeps enforce the floor too.
	before := e.rt.FloorChecks()
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	e.svc.enforceGoldenFloor(ctx)
	if e.rt.FloorChecks() <= before {
		t.Fatal("floor not enforced")
	}
}
