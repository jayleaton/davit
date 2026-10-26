package jobs

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

type fakeVMs map[string]*domain.VM

func (f fakeVMs) GetVM(_ context.Context, p domain.PrincipalID, ref string) (*domain.VM, error) {
	if vm, ok := f[ref]; ok && vm.Owner == p {
		return vm, nil
	}
	return nil, domain.Errorf(domain.CodeNotFound, "vm %s not found", ref)
}

// fakeSampler stands in for guestexec.Manager.GuestProcesses: it returns
// list and records it, as the manager does.
type fakeSampler struct {
	db   *state.DB
	vm   domain.VMID
	list api.GuestProcessList
}

func (f fakeSampler) GuestProcesses(ctx context.Context, _ domain.PrincipalID, _ string) (api.GuestProcessList, error) {
	l := f.list
	return l, f.db.Tx(ctx, func(tx *state.Tx) error { return tx.PutGuestSample(f.vm, &l) })
}

func setup(t *testing.T) (*Service, *state.DB, fakeVMs) {
	t.Helper()
	db, err := state.Open(context.Background(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Unix(5000, 0).UTC()
	vm := &domain.VM{ID: "vm_a", Owner: "owner", Name: "box", State: domain.StateStopped, StateRevision: 1, InstanceGeneration: 2, CreatedAt: now, UpdatedAt: now}
	err = db.Tx(context.Background(), func(tx *state.Tx) error {
		if err := tx.PutVM(&state.VMRecord{VM: *vm, DiskID: "d"}); err != nil {
			return err
		}
		for i, st := range []string{"exited", "lost", "running"} {
			p := &state.ProcessRecord{ID: "p" + string(rune('1'+i)), VMID: "vm_a", Owner: "owner", Generation: int64(i/2 + 1),
				Argv: []string{"job"}, CreatedAt: now.Add(time.Duration(i) * time.Second), Status: st, Handle: "pstart_" + string(rune('1'+i))}
			if err := tx.PutProcess(p); err != nil {
				return err
			}
		}
		for seq, s := range []string{"stdout", "stderr", "stdout"} {
			if err := tx.AppendOutput("p1", state.OutputChunk{Seq: int64(seq + 1), Stream: s, Data: []byte{'a' + byte(seq)}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	vms := fakeVMs{"box": vm, "vm_a": vm}
	return New(Config{DB: db, VMs: vms}), db, vms
}

// TestHistoryAndOutput: jobs list newest first across generations with
// retained sizes; output pages by seq, filters a stream, is readable while
// the VM is stopped, and is complete once the job ended.
func TestHistoryAndOutput(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	h, err := s.History(ctx, "owner", "box", 2, "")
	if err != nil || len(h.Items) != 2 || h.Items[0].Process.ID != "p3" || h.NextCursor == "" {
		t.Fatalf("page 1: %+v %v", h, err)
	}
	h2, err := s.History(ctx, "owner", "box", 2, h.NextCursor)
	if err != nil || len(h2.Items) != 1 || h2.Items[0].Process.ID != "p1" || h2.Items[0].StdoutBytes != 2 || h2.Items[0].StderrBytes != 1 ||
		h2.Items[0].Handle != "pstart_1" {
		t.Fatalf("page 2: %+v %v", h2, err)
	}
	if _, err := s.History(ctx, "intruder", "box", 0, ""); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("other principal: %v", err)
	}

	o, err := s.Output(ctx, "owner", "box", "p1", "", "both", 2)
	if err != nil || len(o.Items) != 2 || o.NextCursor != "2" || o.Complete {
		t.Fatalf("output page 1: %+v %v", o, err)
	}
	o, err = s.Output(ctx, "owner", "box", "p1", o.NextCursor, "", 2)
	if b, _ := base64.StdEncoding.DecodeString(o.Items[0].Data); err != nil || len(o.Items) != 1 || string(b) != "c" || !o.Complete {
		t.Fatalf("output page 2: %+v %v", o, err)
	}
	if o, _ := s.Output(ctx, "owner", "box", "p1", "", "stderr", 0); len(o.Items) != 1 || o.Items[0].Type != api.StreamStderr {
		t.Fatalf("stderr only: %+v", o)
	}
	if o, _ := s.Output(ctx, "owner", "box", "p3", "", "", 0); o.Complete {
		t.Fatal("a running job's output is complete")
	}
	if _, err := s.Output(ctx, "owner", "box", "p1", "x", "", 0); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("bad cursor: %v", err)
	}
	if _, err := s.Output(ctx, "owner", "box", "nope", "", "", 0); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("unknown process: %v", err)
	}
}

// TestPS: a running VM is sampled live (and the sample kept); a stopped VM
// returns its last sample as stale; without a lister a running VM's sample
// is not implemented.
func TestPS(t *testing.T) {
	s, db, vms := setup(t)
	ctx := context.Background()
	if _, err := s.PS(ctx, "owner", "box"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("stopped VM without a sample: %v", err)
	}
	vms["box"].State = domain.StateRunning
	if _, err := s.PS(ctx, "owner", "box"); !domain.IsCode(err, domain.CodeNotImplemented) {
		t.Fatalf("no lister: %v", err)
	}
	at := time.Unix(6000, 0)
	s.cfg.Sampler = fakeSampler{db: db, vm: "vm_a", list: api.GuestProcessList{SampledAt: at.UTC(), Items: []api.GuestProcess{
		{PID: 1, User: "root", Args: []string{"/sbin/init"}, State: "S"}, {PID: 42, PPID: 1, Args: []string{"job"}, ProcessID: "p3"}}}}
	l, err := s.PS(ctx, "owner", "box")
	if err != nil || len(l.Items) != 2 || l.Items[1].ProcessID != "p3" || !l.SampledAt.Equal(at) || l.Stale {
		t.Fatalf("live sample: %+v %v", l, err)
	}
	vms["box"].State = domain.StateStopped
	if l, err := s.PS(ctx, "owner", "box"); err != nil || !l.Stale || len(l.Items) != 2 {
		t.Fatalf("last sample: %+v %v", l, err)
	}
}
