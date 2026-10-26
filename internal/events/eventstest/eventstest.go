// Package eventstest is a real lifecycle host (fake runtime) for tests of
// the event stream and its consumers: VMs created and stopped through it
// produce the events the triggers write.
package eventstest

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/events"
	"davit/internal/lifecycle"
	"davit/internal/runtime/fake"
	"davit/internal/state"
	"davit/internal/storage"
)

// Owner is the principal of every VM the host makes.
const Owner = domain.PrincipalID("owner")

// Host is a lifecycle service over a fresh database in dir.
type Host struct {
	DB     *state.DB
	Svc    *lifecycle.Service
	Events *events.Service
	t      testing.TB
}

// New starts a host in dir (reopen the same dir to model a restart).
func New(t testing.TB, dir string) *Host {
	t.Helper()
	ctx := context.Background()
	db, err := state.Open(ctx, filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	rt, err := fake.New(fake.Options{Dir: filepath.Join(dir, "rt")})
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.Open(filepath.Join(dir, "st"), storage.ModeCopy)
	if err != nil {
		t.Fatal(err)
	}
	img, err := lifecycle.DevImage(filepath.Join(dir, "img"))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := lifecycle.New(lifecycle.Config{DB: db, Runtime: rt, Storage: st, Images: lifecycle.NewStaticCatalog(img),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	h := &Host{DB: db, Svc: svc, Events: &events.Service{DB: db, Changed: svc.Changed, Poll: 20 * time.Millisecond}, t: t}
	t.Cleanup(h.Close)
	return h
}

// Close stops the service and closes the database (idempotent enough for
// t.Cleanup after an explicit Close).
func (h *Host) Close() {
	if h.Svc != nil {
		h.Svc.Close()
		h.DB.Close()
		h.Svc = nil
	}
}

func (h *Host) wait(acc *lifecycle.Accepted, err error) domain.Operation {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
	ctx := context.Background()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		op, err := h.Svc.GetOperation(ctx, Owner, acc.Operation.ID)
		if err != nil {
			h.t.Fatal(err)
		}
		if op.State == domain.OpSucceeded {
			return *op
		}
		if op.State == domain.OpFailed || time.Now().After(deadline) {
			h.t.Fatalf("operation %s: %s %+v", op.ID, op.State, op.Error)
		}
	}
}

// CreateVM creates VM name (with env) and waits until it runs.
func (h *Host) CreateVM(name string, env map[string]string) domain.VMID {
	h.t.Helper()
	acc, err := h.Svc.Create(context.Background(), lifecycle.Request{Principal: Owner, Method: "POST", Path: "/vms", IdempotencyKey: "create-" + name + "-" + domain.NewID("k")}, api.CreateVMRequest{Name: name, Env: env})
	op := h.wait(acc, err)
	return op.VMID
}

// StopVM stops VM name and waits.
func (h *Host) StopVM(name string) {
	h.t.Helper()
	acc, err := h.Svc.Stop(context.Background(), lifecycle.Request{Principal: Owner, Method: "POST", Path: "/vms/" + name + "/stop", IdempotencyKey: domain.NewID("k")}, name, false)
	h.wait(acc, err)
}
