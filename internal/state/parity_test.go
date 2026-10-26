package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
)

// TestParityStores: environments keep immutable numbered revisions with
// unique names per owner; deleting a VM revokes its active grants only;
// retained output pages after a cursor; the latest guest sample wins.
func TestParityStores(t *testing.T) {
	db := openT(t, filepath.Join(t.TempDir(), "s.db"))
	now := time.Unix(2000, 0).UTC()
	err := db.Tx(context.Background(), func(tx *Tx) error {
		env := &domain.Environment{ID: "env_a", Owner: "owner", Name: "web", CreatedAt: now, UpdatedAt: now}
		if err := tx.PutEnvironment(env); err != nil {
			return err
		}
		if err := tx.PutEnvironment(&domain.Environment{ID: "env_b", Owner: "owner", Name: "web", CreatedAt: now}); !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Errorf("duplicate environment name: %v", err)
		}
		m := domain.EnvManifest{Repos: []domain.RepoSpec{{URL: "https://example.com/r.git", Path: "/work/r"}}}
		for want := int64(1); want <= 2; want++ {
			rev, err := tx.AddEnvironmentRevision(env, m, now, "", "")
			if err != nil || rev.Revision != want {
				t.Fatalf("revision %d: %+v %v", want, rev, err)
			}
		}
		if _, err := tx.AddEnvironmentRevision(env, domain.EnvManifest{Repos: []domain.RepoSpec{{URL: "x", Path: "rel"}}}, now, "", ""); !domain.IsCode(err, domain.CodeInvalidArgument) {
			t.Errorf("invalid manifest stored: %v", err)
		}
		got, err := tx.GetEnvironment("owner", "web")
		if err != nil || got.LatestRevision != 2 || got.ID != "env_a" {
			t.Fatalf("by name: %+v %v", got, err)
		}
		if _, err := tx.GetEnvironment("other", "env_a"); !domain.IsCode(err, domain.CodeNotFound) {
			t.Errorf("another owner sees the environment: %v", err)
		}
		if r, err := tx.GetEnvironmentRevision("env_a", 1); err != nil || r.Manifest.Repos[0].Path != "/work/r" {
			t.Fatalf("revision 1: %+v %v", r, err)
		}

		if err := tx.PutVM(testVM("vm-a", now)); err != nil {
			return err
		}
		for i, st := range []domain.GrantState{domain.GrantActive, domain.GrantActive, domain.GrantRevoked} {
			g := &domain.CredentialGrant{ID: "g" + string(rune('0'+i)), VMID: "vm-a", ProfileID: "cp_1", Credential: "anthropic",
				State: st, Placeholder: "ph" + string(rune('0'+i)), CreatedAt: now.Add(time.Duration(i))}
			if err := tx.PutGrant("owner", g); err != nil {
				return err
			}
		}
		if n, err := tx.RevokeVMGrants("owner", "vm-a", domain.RevokeVMDeleted, now); err != nil || n != 2 {
			t.Fatalf("revoked %d, %v", n, err)
		}
		gs, _ := tx.Grants("vm-a")
		for _, g := range gs {
			if g.State != domain.GrantRevoked || g.ID != "g2" && g.RevokeReason != domain.RevokeVMDeleted {
				t.Errorf("grant after delete: %+v", g)
			}
		}

		if err := tx.PutProcess(&ProcessRecord{ID: "p1", VMID: "vm-a", Owner: "owner", Generation: 1, CreatedAt: now, Status: "exited"}); err != nil {
			return err
		}
		for i, stream := range []string{api.StreamStdout, api.StreamStderr, api.StreamStdout} {
			if err := tx.AppendOutput("p1", OutputChunk{Seq: int64(i + 1), Stream: stream, Data: []byte{byte('a' + i)}}); err != nil {
				return err
			}
		}
		if cs, err := tx.ReadOutput("p1", 1, 10); err != nil || len(cs) != 2 || cs[0].Stream != api.StreamStderr || string(cs[1].Data) != "c" {
			t.Fatalf("output after 1: %+v %v", cs, err)
		}

		for _, n := range []int{1, 2} {
			if err := tx.PutGuestSample("vm-a", &api.GuestProcessList{SampledAt: now.Add(time.Duration(n)), Items: make([]api.GuestProcess, n)}); err != nil {
				return err
			}
		}
		if s, err := tx.GuestSample("vm-a"); err != nil || len(s.Items) != 2 {
			t.Fatalf("latest sample: %+v %v", s, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
