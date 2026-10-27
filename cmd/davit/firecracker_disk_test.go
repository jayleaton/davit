//go:build linux

package main_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"davit/api"
	"davit/internal/config"
	"davit/internal/domain"
)

// TestFirecrackerDiskLow: on real VMs whose data dir is a small XFS
// filesystem with reflink (DAVIT_TEST_SMALL_FS, vm-tier.sh --small-fs;
// docs/vm-tier.md), filled with a preallocated filler file as guests
// filling it would:
//
//  1. a box create just above the floor is refused with the disk message,
//     queues with --wait, and gets in once space is back;
//  2. a running box stops (its disk committed) with free space below the
//     save reserve: the stop releases the preallocated reserve, which the
//     sweeper puts back once there is room;
//  3. below the floor the sweeper auto-deletes the least recently used
//     unpinned stopped box past instanceMinAge, stops at the floor plus the
//     margin, and, still below the floor, never deletes the pinned box or
//     the running one;
//  4. the bytes freed it records match what statfs (df) saw;
//  5. box image prune --dry-run lists an unreferenced blob, prune removes
//     it, and the image's own blobs stay.
func TestFirecrackerDiskLow(t *testing.T) {
	small := os.Getenv("DAVIT_TEST_SMALL_FS")
	if small == "" {
		if os.Getenv("DAVIT_TEST_FIRECRACKER") == "1" {
			t.Skip("no small filesystem to fill: set DAVIT_TEST_SMALL_FS (vm-tier.sh --small-fs, docs/vm-tier.md)")
		}
		t.Skip("real Firecracker tests disabled")
	}
	gib := int64(domain.GiB)
	const mib = int64(1 << 20)
	var st unix.Statfs_t
	if err := unix.Statfs(small, &st); err != nil {
		t.Fatal(err)
	}
	// Only ever a dedicated small XFS: the test fills it on purpose.
	if st.Type != unix.XFS_SUPER_MAGIC || int64(st.Blocks)*int64(st.Bsize) > 64*gib {
		t.Fatalf("DAVIT_TEST_SMALL_FS %s: want a dedicated XFS of at most 64 GiB (type %#x, %d GiB)", small, st.Type, int64(st.Blocks)*int64(st.Bsize)/gib)
	}
	root, err := os.MkdirTemp(small, "davit-disklow-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	filler := filepath.Join(root, "filler")
	free := func() int64 {
		t.Helper()
		if fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY, 0); err == nil {
			unix.Syncfs(fd)
			unix.Close(fd)
		}
		var st unix.Statfs_t
		if err := unix.Statfs(root, &st); err != nil {
			t.Fatal(err)
		}
		return int64(st.Bavail) * int64(st.Bsize)
	}
	// fillTo sizes the preallocated filler so that about want bytes stay
	// free (as guests writing would), and returns the free space.
	fillTo := func(want int64) int64 {
		t.Helper()
		f, err := os.OpenFile(filler, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		// One pass: a cleanup the new level starts must not be undone.
		var fs unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &fs); err != nil {
			t.Fatal(err)
		}
		n := max(0, fs.Blocks*512+free()-want)
		if err := f.Truncate(min(n, fs.Size)); err != nil {
			t.Fatal(err)
		}
		// Only the new tail: XFS wants room for the whole range asked.
		if n > fs.Size {
			if err := unix.Fallocate(int(f.Fd()), 0, fs.Size, n-fs.Size); err != nil {
				t.Fatalf("fallocate %d more: %v", n-fs.Size, err)
			}
		}
		got := free()
		t.Logf("filler: %s free (asked %s)", domain.HumanBytes(got), domain.HumanBytes(want))
		return got
	}

	floor, reserve, margin := 4*gib, gib, 512*mib
	const instanceAge = 45 * time.Second
	var data string
	h := newFCHostIn(t, root, func(c *config.Daemon) {
		data = c.DataDir
		c.Limits.MinFreeDiskBytes, c.Limits.MinFreeDiskPercent, c.Limits.SaveReserveBytes = floor, -1, reserve
		c.AutoDelete.InstanceMinAge, c.AutoDelete.MarginBytes = config.Duration(instanceAge), margin
		c.AutoDelete.BlobMinAge = config.Duration(2 * time.Second)
		c.Limits.ReclaimIdleAfterSeconds = -1 // the test stops boxes itself
		// The image moves into the data dir's own catalog, on this
		// filesystem: disks reflink from its rootfs.
		copyImage(t, c.Images[0], filepath.Join(c.DataDir, "images"))
		c.Images = nil
	})
	a := apiConn{t: t, c: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.sock)
	}}}}
	capacity := func() *api.HostCapacity {
		var c api.HostCapabilities
		a.must("GET", "/capabilities", nil, "", &c)
		return c.Capacity
	}
	env := append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=", "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"))
	tree := filepath.Join(h.dir, "proj")
	gitTree(t, tree, "main")
	boxCode := func(args ...string) (string, string, int) {
		t.Helper()
		start := time.Now()
		so, se, code := human(t, env, tree, append([]string{"box"}, args...)...)
		t.Logf("box %s: exit %d in %s", strings.Join(args, " "), code, time.Since(start).Round(time.Millisecond))
		return so, se, code
	}
	box := func(args ...string) string {
		t.Helper()
		so, se, code := boxCode(args...)
		if code != 0 {
			t.Fatalf("box %v: exit %d\nstdout %s\nstderr %s\ndaemon log tail:\n%s", args, code, so, se, tail(h.log.String(), 4000))
		}
		return so
	}
	// write puts n MiB of incompressible data into a box's disk.
	write := func(ref string, n int64) {
		t.Helper()
		box("run", ref, "--no-sync", "--", "sh", "-c", fmt.Sprintf("head -c %d /dev/urandom > fill.bin && sync", n*mib))
	}
	waitFor := func(what string, d time.Duration, ok func() bool) {
		t.Helper()
		start := time.Now()
		for deadline := start.Add(d); !ok(); time.Sleep(time.Second) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within %s\ndaemon log tail:\n%s", what, d, tail(h.log.String(), 6000))
			}
		}
		t.Logf("%s: %s", what, time.Since(start).Round(100*time.Millisecond))
	}
	autoDeleted := func() []api.AutoDeletion {
		var page api.EventList
		a.must("GET", "/events?types="+api.EventVMAutoDeleted, nil, "", &page)
		var out []api.AutoDeletion
		for _, e := range page.Items {
			var d api.AutoDeletion
			b, _ := json.Marshal(e.Data)
			if json.Unmarshal(b, &d) != nil {
				t.Fatalf("event data: %s", b)
			}
			out = append(out, d)
		}
		return out
	}
	state := func(name string) domain.VMState { return h.info(name).State }

	// 5. Prune: an unreferenced blob goes, the image's own stay.
	start := time.Now()
	blobs := filepath.Join(data, "images", "blobs", "sha256")
	stray := make([]byte, 64*mib)
	rand.Read(stray)
	sum := sha256.Sum256(stray)
	strayPath := filepath.Join(blobs, hex.EncodeToString(sum[:]))
	if err := os.WriteFile(strayPath, stray, 0o600); err != nil {
		t.Fatal(err)
	}
	own, _ := filepath.Glob(filepath.Join(blobs, "*"))
	time.Sleep(3 * time.Second) // past autoDelete.blobMinAge
	short := "sha256:" + hex.EncodeToString(sum[:])[:12]
	if so := box("image", "prune", "--dry-run"); !strings.Contains(so, "would remove "+short) || strings.Count(so, "would remove sha256:") != 1 {
		t.Fatalf("prune --dry-run:\n%s", so)
	}
	if _, err := os.Stat(strayPath); err != nil {
		t.Fatalf("dry run removed the blob: %v", err)
	}
	before := free()
	if so := box("image", "prune"); !strings.Contains(so, "removed "+short) || !strings.Contains(so, "removed 1 image blobs") {
		t.Fatalf("prune:\n%s", so)
	}
	if _, err := os.Stat(strayPath); !os.IsNotExist(err) {
		t.Fatalf("pruned blob still there: %v", err)
	}
	for _, p := range own {
		if p == strayPath {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("prune removed the image's blob %s: %v", p, err)
		}
	}
	t.Logf("item 5 (prune): ok in %s; df freed %s for the 64 MiB blob", time.Since(start).Round(time.Second), domain.HumanBytes(free()-before))

	// The boxes: pinned (oldest use, stopped), old (running, 1 GiB
	// written), live (running a process).
	box("image", "build", "--setup-cmd", "true", "--cpus", "1", "--memory", "1GiB")
	box("up", "proj/pinned", "--cpus", "1", "--memory", "1GiB")
	write("proj/pinned", 64)
	box("down", "proj/pinned")
	if so := box("pin", "proj/pinned"); !strings.Contains(so, "pinned") {
		t.Fatalf("pin: %q", so)
	}
	box("up", "proj/old", "--cpus", "1", "--memory", "1GiB")
	write("proj/old", 1024)
	box("up", "proj/live", "--cpus", "1", "--memory", "1GiB")
	var p api.Process
	a.must("POST", "/vms/proj--live/processes", api.ExecRequest{Argv: []string{"sleep", "3600"}, Detached: true}, "k-disklow-sleep", &p)
	c := capacity()
	if c.DiskFloorBytes != floor || c.SaveReserveBytes != reserve || !c.SaveReserveArmed {
		t.Fatalf("capacity: %+v", c)
	}
	reservePath := filepath.Join(data, "save-reserve")
	var rs unix.Stat_t
	if err := unix.Stat(reservePath, &rs); err != nil || rs.Blocks*512 < reserve {
		t.Fatalf("save reserve %s not preallocated: %v, %d bytes allocated", reservePath, err, rs.Blocks*512)
	}

	// 1. Just above the floor: a new box does not fit its headroom.
	start = time.Now()
	fillTo(floor + 256*mib)
	_, se, code := boxCode("up", "proj/c", "--cpus", "1", "--memory", "1GiB", "--wait", "0s")
	if code != 8 || !regexp.MustCompile(`host disk low: [0-9.]+ [GM]iB free, need [0-9.]+ GiB`).MatchString(se) ||
		!strings.Contains(se, "box gc") || !strings.Contains(se, "box rm") {
		t.Fatalf("box up on a low disk: exit %d\n%s", code, se)
	}
	type result struct {
		so, se string
		code   int
	}
	queued := make(chan result, 1)
	go func() {
		so, se, code := human(t, env, tree, "box", "up", "proj/c", "--cpus", "1", "--memory", "1GiB", "--wait", "3m")
		queued <- result{so, se, code}
	}()
	waitFor("box up --wait queued for disk", time.Minute, func() bool {
		w := capacity().Waiting
		return len(w) == 1 && w[0].Resource == "disk" && w[0].DiskBytes > 0
	})
	time.Sleep(5 * time.Second)
	select {
	case r := <-queued:
		t.Fatalf("queued box up answered while the disk was low: exit %d\n%s", r.code, r.se)
	default:
	}
	os.Remove(filler)
	released := time.Now()
	select {
	case r := <-queued:
		if r.code != 0 || !strings.Contains(r.se, "waiting for host disk space") {
			t.Fatalf("queued box up: exit %d\nstdout %s\nstderr %s", r.code, r.so, r.se)
		}
		t.Logf("queued box up admitted and booted %s after space was freed", time.Since(released).Round(100*time.Millisecond))
	case <-time.After(2 * time.Minute):
		t.Fatal("queued box up not admitted after space was freed")
	}
	box("rm", "proj/c", "--yes")
	t.Logf("item 1 (refuse + queue): ok in %s", time.Since(start).Round(time.Second))

	// 2. Below the save reserve: stopping old commits its disk.
	start = time.Now()
	fillTo(reserve / 2)
	box("down", "proj/old")
	if s := state("proj--old"); s != domain.StateStopped {
		t.Fatalf("old after the stop: %s", s)
	}
	if c := capacity(); c.SaveReserveArmed {
		t.Fatalf("save reserve still armed after a stop below its size: %+v", c)
	}
	if _, err := os.Stat(reservePath); !os.IsNotExist(err) {
		t.Fatalf("save reserve file after the release: %v", err)
	}
	t.Logf("stop below the reserve: %s free after it", domain.HumanBytes(free()))
	os.Remove(filler)
	waitFor("save reserve re-armed", time.Minute, func() bool { return capacity().SaveReserveArmed })
	t.Logf("item 2 (save reserve): ok in %s", time.Since(start).Round(time.Second))

	// 3 and 4. newer: stopped after old. Once both are past
	// instanceMinAge, push free space below the floor.
	box("up", "proj/newer", "--cpus", "1", "--memory", "1GiB")
	write("proj/newer", 1024)
	box("down", "proj/newer")
	stopped := time.Now()
	if n := autoDeleted(); len(n) != 0 {
		t.Fatalf("auto-deleted before the test pushed below the floor: %+v", n)
	}
	time.Sleep(time.Until(stopped.Add(instanceAge + 5*time.Second)))
	start = time.Now()
	free0 := fillTo(floor - 256*mib)
	waitFor("auto-delete below the floor", 2*time.Minute, func() bool { return len(autoDeleted()) > 0 })
	waitFor("free space back at the floor", time.Minute, func() bool { return free() >= floor })
	time.Sleep(20 * time.Second) // more sweeps: nothing else goes
	free1 := free()
	del := autoDeleted()
	if len(del) != 1 || del[0].Box != "proj/old" || del[0].Kind != "instance" || del[0].UnusedSeconds < int64(instanceAge/time.Second) {
		t.Fatalf("auto-deleted: %+v", del)
	}
	if free1 < floor+margin-64*mib {
		t.Fatalf("cleanup stopped at %s free, below the floor plus margin %s", domain.HumanBytes(free1), domain.HumanBytes(floor+margin))
	}
	for name, want := range map[string]domain.VMState{"proj--newer": domain.StateStopped, "proj--pinned": domain.StateStopped, "proj--live": domain.StateReady} {
		if s := state(name); s != want {
			t.Fatalf("%s after the cleanup: %s, want %s", name, s, want)
		}
	}
	so := box("ls", "--deleted")
	if !strings.Contains(so, "instance proj/old auto-deleted") {
		t.Fatalf("box ls --deleted:\n%s", so)
	}
	freed, df := del[0].FreedBytes, free1-free0
	t.Logf("item 3 (LRU auto-delete): ok in %s; proj/old freed %s (event), df %s -> %s (%s)", time.Since(start).Round(time.Second),
		domain.HumanBytes(freed), domain.HumanBytes(free0), domain.HumanBytes(free1), domain.HumanBytes(df))
	if freed < 900*mib || freed-df > max(df/10, 128*mib) || df-freed > max(df/10, 128*mib) {
		t.Fatalf("item 4: freed %d bytes by the event, df saw %d", freed, df)
	}
	t.Logf("item 4 (bytes freed vs df): ok; difference %s", domain.HumanBytes(max(freed-df, df-freed)))

	// Still below the floor with newer gone: pinned and live stay.
	start = time.Now()
	fillTo(floor - 2*gib)
	waitFor("second auto-delete", 2*time.Minute, func() bool { return len(autoDeleted()) > 1 })
	time.Sleep(40 * time.Second) // several sweeps below the floor
	del = autoDeleted()
	if len(del) != 2 || del[1].Box != "proj/newer" {
		t.Fatalf("auto-deleted below the floor: %+v", del)
	}
	if free() >= floor {
		t.Fatalf("expected to stay below the floor with only pinned and running boxes left (%s free)", domain.HumanBytes(free()))
	}
	if s := state("proj--pinned"); s != domain.StateStopped {
		t.Fatalf("pinned box: %s", s)
	}
	if s := state("proj--live"); s != domain.StateReady {
		t.Fatalf("running box: %s", s)
	}
	if out := h.sh("proj--live", "pgrep -x sleep >/dev/null && echo running"); out != "running" {
		t.Fatalf("live box's process: %q", out)
	}
	t.Logf("item 3 (pinned + running kept below the floor): ok in %s", time.Since(start).Round(time.Second))
	os.Remove(filler)
}

// copyImage copies image source src's manifest and the blobs it names
// (sparse) into the daemon images dir dst, so that disks reflink from a
// rootfs on dst's filesystem.
func copyImage(t *testing.T, src config.ImageSource, dst string) {
	t.Helper()
	raw, err := os.ReadFile(src.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	blobs := filepath.Join(dst, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "manifests"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`sha256:([0-9a-f]{64})`).FindAllStringSubmatch(string(raw), -1) {
		from := filepath.Join(src.BlobDir, "sha256", m[1])
		if _, err := os.Stat(from); err != nil {
			continue // a digest of something else (the manifest's own id)
		}
		if _, err := os.Stat(filepath.Join(blobs, m[1])); err == nil {
			continue
		}
		if out, err := exec.Command("cp", "--sparse=always", from, filepath.Join(blobs, m[1])).CombinedOutput(); err != nil {
			t.Fatalf("copy blob %s: %v %s", m[1], err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, "manifests", filepath.Base(src.Manifest)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
