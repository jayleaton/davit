//go:build linux

package main_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"davit/internal/config"
	"davit/internal/domain"
)

// TestFirecrackerColdTier: on real VMs whose data dir is a small XFS with
// reflink (DAVIT_TEST_SMALL_FS, vm-tier.sh --small-fs) and whose cold
// storage tier is another (DAVIT_TEST_COLD_FS, vm-tier.sh --cold-fs;
// docs/vm-tier.md):
//
//  1. a stopped box with 1 GiB of its own data goes cold at a sweep
//     (COLD-DEMOTE: what the move copied and how long it took), its commit
//     only on the cold tier;
//  2. box up restores it (COLD-PROMOTE: the restore's bytes and time from
//     the daemon log), the box is hot again and the file written inside
//     the guest before the move has the same sha256;
//  3. a box kept running across several sweeps never goes cold;
//  4. a daemon SIGKILLed mid-move (a box with 1.5 GiB of its own data,
//     copied at 32 MiB/s) recovers: no partial copy left in the cold tmp/,
//     the box's commit on exactly one tier (the one its record names), and
//     it resumes with its file intact.
func TestFirecrackerColdTier(t *testing.T) {
	small, coldFS := os.Getenv("DAVIT_TEST_SMALL_FS"), os.Getenv("DAVIT_TEST_COLD_FS")
	if small == "" || coldFS == "" {
		if os.Getenv("DAVIT_TEST_FIRECRACKER") == "1" {
			t.Skip("no hot and cold filesystems: set DAVIT_TEST_SMALL_FS and DAVIT_TEST_COLD_FS (vm-tier.sh --small-fs, --cold-fs; docs/vm-tier.md)")
		}
		t.Skip("real Firecracker tests disabled")
	}
	gib := int64(domain.GiB)
	const mib = int64(1 << 20)
	var hs, cs unix.Statfs_t
	if err := unix.Statfs(small, &hs); err != nil {
		t.Fatal(err)
	}
	if err := unix.Statfs(coldFS, &cs); err != nil {
		t.Fatal(err)
	}
	// Two dedicated XFS filesystems: the move must cross filesystems.
	if hs.Type != unix.XFS_SUPER_MAGIC || cs.Type != unix.XFS_SUPER_MAGIC || hs.Fsid == cs.Fsid {
		t.Fatalf("DAVIT_TEST_SMALL_FS %s (type %#x) and DAVIT_TEST_COLD_FS %s (type %#x): want two different XFS filesystems", small, hs.Type, coldFS, cs.Type)
	}
	root, err := os.MkdirTemp(small, "davit-cold-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	cold, err := os.MkdirTemp(coldFS, "davit-cold-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(cold) })

	var data string
	h := newFCHostIn(t, root, func(c *config.Daemon) {
		data = c.DataDir
		c.Limits.MinFreeDiskBytes, c.Limits.MinFreeDiskPercent = 2*gib, -1
		c.Limits.ReclaimIdleAfterSeconds = -1 // the test stops boxes itself
		c.ColdStorage = config.ColdStorage{Dir: cold, After: config.Duration(time.Second), MinBytes: 256 * mib,
			BytesPerSecond: -1, IOBusyPercent: -1}
		// The image moves into the data dir's own catalog, on the hot
		// filesystem: disks reflink from its rootfs, restores rebase on it.
		copyImage(t, c.Images[0], filepath.Join(c.DataDir, "images"))
		c.Images = nil
	})
	env := append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=", "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"))
	tree := filepath.Join(h.dir, "proj")
	gitTree(t, tree, "main")
	box := func(args ...string) (string, string) {
		t.Helper()
		start := time.Now()
		so, se, code := human(t, env, tree, append([]string{"box"}, args...)...)
		if code != 0 {
			t.Fatalf("box %v: exit %d\nstdout %s\nstderr %s\ndaemon log tail:\n%s", args, code, so, se, tail(h.log.String(), 4000))
		}
		t.Logf("box %s: %s", strings.Join(args, " "), time.Since(start).Round(time.Millisecond))
		return so, se
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
	// fill writes n MiB of random data to file in the box, synced, and
	// returns its sha256.
	fill := func(ref, file string, n int64) string {
		t.Helper()
		so, _ := box("run", ref, "--no-sync", "--", "sh", "-c", "head -c "+strconv.FormatInt(n*mib, 10)+" /dev/urandom > "+file+" && sync && sha256sum "+file)
		return sha(t, so)
	}
	sum := func(ref, file string) string {
		t.Helper()
		so, _ := box("run", ref, "--no-sync", "--", "sha256sum", file)
		return sha(t, so)
	}
	tier := func(name string) domain.StorageTier {
		if vm := h.info(name); vm.Tier != "" {
			return vm.Tier
		}
		return domain.TierHot
	}
	hotCommits := func() []string { return dirNames(filepath.Join(data, "storage", "commits")) }
	coldCommits := func() []string { return dirNames(filepath.Join(cold, "commits")) }
	// oneCopy fails when a commit is published on both tiers (a move
	// that GC did not finish).
	oneCopy := func() {
		t.Helper()
		hot := hotCommits()
		for _, c := range coldCommits() {
			if slices.Contains(hot, c) {
				t.Fatalf("commit %s on both tiers\nhot %v\ncold %v", c, hot, coldCommits())
			}
		}
	}
	logLine := func(re *regexp.Regexp) []string {
		return re.FindStringSubmatch(h.log.String())
	}

	box("image", "build", "--setup-cmd", "true", "--cpus", "1", "--memory", "1GiB")
	box("up", "proj/live", "--cpus", "1", "--memory", "1GiB")
	liveUp := time.Now()
	box("up", "proj/a", "--cpus", "1", "--memory", "1GiB")
	sumA := fill("proj/a", "cold.bin", 1024)
	t.Logf("proj/a: 1 GiB written, sha256 %s", sumA)

	// 1. Demote: the next sweep after the stop moves a.
	hot0, cold0 := hotCommits(), coldCommits()
	box("down", "proj/a")
	waitFor("proj/a cold", 5*time.Minute, func() bool { return tier("proj--a") == domain.TierCold })
	moved := regexp.MustCompile(`msg="moved vm to cold storage" .*name=proj--a .*coldBytes=(\d+) ownBytes=(\d+) hotFreedBytes=(\d+) seconds=([0-9.]+)`)
	waitFor("moved log line", 30*time.Second, func() bool { return logLine(moved) != nil })
	m := logLine(moved)
	n, _ := strconv.ParseInt(m[1], 10, 64)
	secs, _ := strconv.ParseFloat(m[4], 64)
	t.Logf("COLD-DEMOTE bytes=%d seconds=%.2f mib_per_s=%.1f own_bytes=%s hot_freed_bytes=%s (copy and read-back verify, unlimited rate, idle I/O class)",
		n, secs, float64(n)/float64(mib)/max(secs, 0.05), m[2], m[3])
	if a := h.info("proj--a"); a.ColdBytes < gib || a.State != domain.StateStopped {
		t.Fatalf("proj/a on cold storage: %s, %d cold bytes", a.State, a.ColdBytes)
	}
	var movedTo []string
	for _, c := range coldCommits() {
		if !slices.Contains(cold0, c) {
			movedTo = append(movedTo, c)
		}
	}
	if len(movedTo) != 1 || slices.Contains(hotCommits(), movedTo[0]) {
		t.Fatalf("after the move: new cold commits %v; hot commits before %v, now %v", movedTo, hot0, hotCommits())
	}
	oneCopy()
	if left := dirNames(filepath.Join(cold, "tmp")); len(left) != 0 {
		t.Fatalf("cold tmp after the move: %v", left)
	}
	t.Logf("item 1 (demote): ok; commit %s only on the cold tier", movedTo[0])

	// 2. Promote: box up restores the disk onto its hot base.
	start := time.Now()
	so, se := box("up", "proj/a")
	upSecs := time.Since(start).Seconds()
	if !strings.Contains(se, "restoring from cold storage (") || !strings.Contains(se, "storage.restore") || !strings.Contains(so, "proj/a resumed") {
		t.Fatalf("box up of a cold box: stdout %q stderr %q", so, se)
	}
	restored := regexp.MustCompile(`msg="restored vm from cold storage" .*name=proj--a bytes=(\d+) seconds=([0-9.]+) mibPerSecond=(\d+) rebased=(\w+)`)
	m = logLine(restored)
	if m == nil {
		t.Fatalf("no restored log line\ndaemon log tail:\n%s", tail(h.log.String(), 4000))
	}
	n, _ = strconv.ParseInt(m[1], 10, 64)
	secs, _ = strconv.ParseFloat(m[2], 64)
	gibs := float64(n) / float64(gib)
	t.Logf("COLD-PROMOTE bytes=%d seconds=%.2f mib_per_s=%.1f s_per_gib=%.2f rebased=%s box_up_s=%.2f",
		n, secs, float64(n)/float64(mib)/max(secs, 0.05), secs/max(gibs, 1e-9), m[4], upSecs)
	if tr := tier("proj--a"); tr != domain.TierHot {
		t.Fatalf("proj/a after box up: %s", tr)
	}
	if got := sum("proj/a", "cold.bin"); got != sumA {
		t.Fatalf("cold.bin after the round trip: sha256 %s, want %s", got, sumA)
	}
	t.Logf("item 2 (promote): ok; cold.bin sha256 matches")

	// 3. The running box, across several sweeps (every 15 s).
	time.Sleep(time.Until(liveUp.Add(50 * time.Second)))
	if vm := h.info("proj--live"); vm.State != domain.StateReady || tier("proj--live") != domain.TierHot {
		t.Fatalf("running box after %s: %s on %s", time.Since(liveUp).Round(time.Second), vm.State, vm.Tier)
	}
	if strings.Contains(h.log.String(), "name=proj--live reason=") {
		t.Fatalf("the running box was moved:\n%s", tail(h.log.String(), 4000))
	}
	t.Logf("item 3 (running box stays hot): ok after %s", time.Since(liveUp).Round(time.Second))

	// 4. A kill mid-move: slow moves, a large box.
	h.stopDaemon()
	h.cfg.ColdStorage.BytesPerSecond = 32 * mib
	h.startDaemon()
	box("up", "proj/big", "--cpus", "1", "--memory", "1GiB")
	// What the guest's root filesystem has room for.
	sumBig := fill("proj/big", "big.bin", 1536)
	t.Logf("proj/big: 1.5 GiB written, sha256 %s", sumBig)
	hot0, cold0 = hotCommits(), coldCommits()
	box("down", "proj/big")
	movedBig := regexp.MustCompile(`msg="moved vm to cold storage" .*name=proj--big `)
	var partial string
	waitFor("proj/big move in progress", 2*time.Minute, func() bool {
		for _, e := range dirNames(filepath.Join(cold, "tmp")) {
			if strings.HasPrefix(e, "demote-") {
				partial = e
				return true
			}
		}
		return false
	})
	time.Sleep(10 * time.Second) // well into the copy (32 MiB/s: about 320 MiB)
	if logLine(movedBig) != nil {
		t.Fatal("proj/big moved before the kill: the move was too fast to interrupt")
	}
	partialBytes := allocatedUnder(filepath.Join(cold, "tmp", partial))
	h.kill()
	t.Logf("daemon SIGKILLed mid-move: cold tmp/%s held %s", partial, domain.HumanBytes(partialBytes))
	if logLine(movedBig) != nil {
		t.Fatal("proj/big moved before the kill landed")
	}
	// Restarted with no moves due, so what is left is the recovery's.
	h.cfg.ColdStorage.After = config.Duration(time.Hour)
	h.startDaemon()
	waitFor("cold tmp emptied by the restart's GC", time.Minute, func() bool { return len(dirNames(filepath.Join(cold, "tmp"))) == 0 })
	var newHot, newCold []string
	for _, c := range hotCommits() {
		if !slices.Contains(hot0, c) {
			newHot = append(newHot, c)
		}
	}
	for _, c := range coldCommits() {
		if !slices.Contains(cold0, c) {
			newCold = append(newCold, c)
		}
	}
	tr := tier("proj--big")
	t.Logf("after the restart: proj/big %s; its new commits hot %v, cold %v", tr, newHot, newCold)
	switch {
	case tr == domain.TierHot && len(newHot) == 1 && len(newCold) == 0:
	case tr == domain.TierCold && len(newHot) == 0 && len(newCold) == 1:
	default:
		t.Fatalf("after the restart: proj/big on %s with new commits hot %v, cold %v", tr, newHot, newCold)
	}
	oneCopy()
	box("up", "proj/big")
	if got := sum("proj/big", "big.bin"); got != sumBig {
		t.Fatalf("big.bin after the interrupted move: sha256 %s, want %s", got, sumBig)
	}
	if left := dirNames(filepath.Join(cold, "tmp")); len(left) != 0 {
		t.Fatalf("cold tmp after the resume: %v", left)
	}
	t.Logf("item 4 (kill mid-move): ok; proj/big resumed from %s, big.bin sha256 matches", tr)
}

// sha is the digest of sha256sum's output.
func sha(t *testing.T, out string) string {
	t.Helper()
	f := strings.Fields(out)
	if len(f) < 2 || len(f[0]) != 64 {
		t.Fatalf("sha256sum: %q", out)
	}
	return f[0]
}

// dirNames lists dir's entries (none if it does not exist).
func dirNames(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// allocatedUnder sums the allocated bytes of the files under dir.
func allocatedUnder(dir string) int64 {
	var n int64
	filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				n += st.Blocks * 512
			}
		}
		return nil
	})
	return n
}
