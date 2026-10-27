package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/lifecycle"
	"davit/internal/storage"
)

// smallDisk is a small fake data filesystem: free space the test sets (as
// guests writing would), a save reserve that takes its size while armed,
// and commits that need some room.
type smallDisk struct {
	mu         sync.Mutex
	free       int64
	reserve    int64
	armed      bool
	commitNeed int64
}

func (d *smallDisk) set(n int64) { d.mu.Lock(); d.free = n; d.mu.Unlock() }
func (d *smallDisk) get() int64  { d.mu.Lock(); defer d.mu.Unlock(); return d.free }
func (d *smallDisk) Size() int64 { return d.reserve }
func (d *smallDisk) Armed() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.armed }
func (d *smallDisk) Arm(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed, d.free = true, d.free-d.reserve
	return nil
}
func (d *smallDisk) Release() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.armed {
		return false
	}
	d.armed, d.free = false, d.free+d.reserve
	return true
}

func (d *smallDisk) usage() (domain.DiskUsage, error) {
	return domain.DiskUsage{FreeBytes: d.get(), TotalBytes: 40 * domain.GiB}, nil
}

type smallStore struct {
	storage.Store
	d *smallDisk
}

func (s smallStore) Commit(ctx context.Context, disk storage.DiskID, id storage.CommitID) (storage.Commit, error) {
	if s.d.get() < s.d.commitNeed {
		return storage.Commit{}, &os.PathError{Op: "write", Path: "disk.img", Err: syscall.ENOSPC}
	}
	return s.Store.Commit(ctx, disk, id)
}

// runHuman runs the CLI without --json, returning stdout and stderr.
func runHuman(args ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	code := Main(args, Stdio{Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), stderr.String(), code
}

// TestDiskFullBoxes: on a small data filesystem that fills up, a box
// resume and a new VM are refused with the disk message, a box waiting
// with --wait is told it waits for disk space and why, stopping a running
// box still succeeds (it uses the save reserve), and box ls shows the
// disk's free space, floor and reservations; box pin marks a box kept.
func TestDiskFullBoxes(t *testing.T) {
	gib := domain.GiB
	d := &smallDisk{free: 30 * gib, reserve: 2 * gib, commitNeed: 3 * gib / 2}
	serveWith(t, func(c *lifecycle.Config) {
		c.DiskSpace, c.SaveReserve, c.Storage = d.usage, d, smallStore{Store: c.Storage, d: d}
		c.Limits.MinFreeDiskBytes = 5 * gib // above 10% of 40 GiB
	})
	cl, err := newClient(os.Getenv("DAVIT_API_URL"), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, inst := range []string{"one", "two"} {
		no := false
		if _, err := cl.mutate(ctx, "POST", "/vms", api.CreateVMRequest{Name: "demo--" + inst, AutoStop: &no,
			Labels: map[string]string{domain.LabelBox: "demo", labelBoxInstance: inst, labelBoxGeneration: "0"}}, newIdempotencyKey()); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			var vm api.VM
			if err := cl.get(ctx, vmPath("demo--"+inst), nil, &vm); err == nil && vm.State == domain.StateReady {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("box %s did not come up", inst)
			}
		}
	}
	if r := runCLI(t, false, "box", "down", "demo/one"); r.code != 0 {
		t.Fatalf("box down: exit %d %+v", r.code, r.last.Error)
	}
	if !d.Armed() {
		t.Fatal("the save reserve was not set aside")
	}

	// 6 GiB free: a box needs its 2 GiB headroom above the 5 GiB floor.
	d.set(6 * gib)
	const msg = "host disk low: 6.0 GiB free, need 7.0 GiB"
	r := runCLI(t, false, "new", "web")
	if r.code != ExitExhaustion || r.last.Error == nil || !strings.Contains(r.last.Error.Message, msg) ||
		!strings.Contains(r.last.Error.Message, "box gc") || r.last.Error.Details["resource"] != "disk" {
		t.Fatalf("new on a low disk: exit %d %+v", r.code, r.last.Error)
	}
	_, stderr, code := runHuman("box", "up", "demo/one", "--wait", "3s")
	if code != ExitExhaustion || !strings.Contains(stderr, "waiting for host disk space (0 ahead): "+msg) ||
		!strings.Contains(stderr, "box gc") || !strings.Contains(stderr, "box rm") ||
		!strings.Contains(stderr, "waited 3s for host capacity: "+msg) {
		t.Fatalf("box up --wait on a low disk: exit %d\n%s", code, stderr)
	}

	// Guests filled it to 1 GiB, less than a commit needs: the stop
	// releases the save reserve and succeeds.
	d.set(gib)
	if r := runCLI(t, false, "box", "down", "demo/two"); r.code != 0 {
		t.Fatalf("box down on a full disk: exit %d %+v", r.code, r.last.Error)
	}
	if d.Armed() || d.get() != 3*gib {
		t.Fatalf("save reserve after the stop: armed %v, free %d", d.Armed(), d.get())
	}

	if r := runCLI(t, false, "box", "pin", "demo/one"); r.code != 0 || r.data["keep"] != true {
		t.Fatalf("box pin: exit %d %+v %+v", r.code, r.last.Error, r.data)
	}
	r = runCLI(t, false, "box", "ls")
	host, _ := r.data["host"].(map[string]any)
	items, _ := r.data["items"].([]any)
	if r.code != 0 || host == nil || host["freeDiskBytes"] != float64(3*gib) || host["diskFloorBytes"] != float64(5*gib) ||
		host["saveReserveBytes"] != float64(2*gib) || host["saveReserveArmed"] != nil || host["totalDiskBytes"] != float64(40*gib) {
		t.Fatalf("box ls host: exit %d %+v", r.code, host)
	}
	pinned := 0
	for _, it := range items {
		if m := it.(map[string]any); m["keep"] == true && m["instance"] == "one" {
			pinned++
		}
		if m := it.(map[string]any); m["tier"] != "hot" {
			t.Fatalf("box ls item without tier hot: %+v", m)
		}
	}
	if pinned != 1 {
		t.Fatalf("box ls items: %+v", items)
	}
	out, _, code := runHuman("box", "ls")
	if code != 0 || !strings.Contains(out, "disk 3.0 GiB free of 40.0 GiB, floor 5.0 GiB, save reserve in use (LOW") || !strings.Contains(out, "pinned") {
		t.Fatalf("box ls:\n%s", out)
	}
	// --hot keeps it on hot storage too; unpin clears both.
	if r := runCLI(t, false, "box", "pin", "--hot", "demo/one"); r.code != 0 || r.data["keep"] != true || r.data["keepHot"] != true {
		t.Fatalf("box pin --hot: exit %d %+v %+v", r.code, r.last.Error, r.data)
	}
	if out, _, _ := runHuman("box", "ls"); !strings.Contains(out, "pinned hot") {
		t.Fatalf("box ls after pin --hot:\n%s", out)
	}
	if r := runCLI(t, false, "box", "unpin", "demo/one"); r.code != 0 || r.data["keep"] != false || r.data["keepHot"] != false {
		t.Fatalf("box unpin: exit %d %+v", r.code, r.data)
	}
}
