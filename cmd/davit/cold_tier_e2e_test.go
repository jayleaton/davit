package main_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
)

// TestColdTierEndToEnd drives the cold storage tier through the CLI on
// the fake runtime (coldStorage after 1s, any size, no free-space floor on
// a test machine's disk, never I/O-busy): a
// stopped box goes cold at a sweep while a running one, one pinned --hot
// and the project image's builder stay hot; a daemon kill and restart keep the cold box cold with
// its commit only on the cold tier; box up says it restores, the resume
// reports storage.restore progress, the box is hot again, and data
// written into its disk came back.
func TestColdTierEndToEnd(t *testing.T) {
	h := newHost(t)
	h.guestAgent = true
	cold := filepath.Join(h.dir, "cold") // same filesystem as the data dir: only a warning
	if err := os.MkdirAll(cold, 0o755); err != nil {
		t.Fatal(err)
	}
	h.extraConfig = fmt.Sprintf(`"coldStorage":{"dir":%q,"after":"1s","minBytes":-1,"minFreeBytes":-1,"ioBusyPercent":-1}`, cold)
	h.start(0, 0)
	data := filepath.Join(h.dir, "data")
	// The fake guest runs commands on this host: the box work dir is a
	// test path.
	work := filepath.Join(h.dir, "work")
	h.env = append(h.env, "DAVIT_BOX_WORKDIR="+work)
	env := append(os.Environ(), "DAVIT_API_URL=unix://"+h.sock, "DAVIT_CURRENT_ID=", "XDG_CONFIG_HOME="+filepath.Join(h.dir, "config"),
		"DAVIT_BOX_WORKDIR="+work)

	type row struct {
		Instance, State string
		Tier            domain.StorageTier
		ColdBytes       int64
		KeepHot         bool
	}
	tiers := func() map[string]row {
		t.Helper()
		r := h.cli("box", "ls", "proj")
		var l struct {
			Items []row `json:"items"`
		}
		if r.code != 0 || json.Unmarshal(r.raw[len(r.raw)-1], &l) != nil {
			t.Fatalf("box ls: exit %d %s", r.code, r.stderr)
		}
		out := map[string]row{}
		for _, it := range l.Items {
			out[it.Instance] = it
		}
		return out
	}
	// diskOf is the fake runtime's root disk file of vm's current instance.
	diskOf := func(name string) string {
		t.Helper()
		vm := h.info(name)
		b, err := os.ReadFile(filepath.Join(data, "runtime", fmt.Sprintf("%s.g%d.json", vm.ID, vm.InstanceGeneration)))
		if err != nil {
			t.Fatal(err)
		}
		var rec struct {
			DiskPath string `json:"diskPath"`
		}
		if json.Unmarshal(b, &rec) != nil || rec.DiskPath == "" {
			t.Fatalf("fake runtime record of %s: %s", name, b)
		}
		return rec.DiskPath
	}
	commits := func(root string) []string {
		ents, _ := os.ReadDir(filepath.Join(root, "commits"))
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}
	coldTmp := func() []string {
		ents, _ := os.ReadDir(filepath.Join(cold, "tmp"))
		var out []string
		for _, e := range ents {
			out = append(out, e.Name())
		}
		return out
	}

	tree := filepath.Join(h.dir, "proj")
	gitTree(t, tree, "main")
	if _, se, code := human(t, env, tree, "box", "image", "build", "--setup-cmd", "true"); code != 0 {
		t.Fatalf("image build: exit %d %s", code, se)
	}
	for _, inst := range []string{"proj/a", "proj/live", "proj/pinned"} {
		h.boxUp(inst)
	}
	// What the guest wrote: a random block in a's disk, past the image's
	// data.
	const off = 1 << 20
	mark := make([]byte, 64<<10)
	rand.Read(mark)
	f, err := os.OpenFile(diskOf("proj--a"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(mark, off); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if so, se, code := human(t, env, tree, "box", "pin", "proj/pinned", "--hot"); code != 0 || !strings.Contains(so, "pinned hot") {
		t.Fatalf("box pin --hot: exit %d %q %q", code, so, se)
	}
	for _, inst := range []string{"proj/pinned", "proj/a"} {
		if r := h.cli("box", "down", inst); r.code != 0 {
			t.Fatalf("box down %s: exit %d %s", inst, r.code, r.stderr)
		}
	}

	// The next sweep (every 15 s) moves a.
	start := time.Now()
	for tiers()["a"].Tier != domain.TierCold {
		if time.Since(start) > 40*time.Second {
			t.Fatalf("proj/a not cold within 40 s: %+v", tiers())
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("proj/a went cold %s after its stop", time.Since(start).Round(100*time.Millisecond))
	logged := func(line string) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !strings.Contains(h.log.String(), line); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("no %q log line", line)
			}
		}
	}
	logged("moved vm to cold storage") // logged after the journal switch
	a := h.info("proj--a")
	if a.Tier != domain.TierCold || a.ColdBytes <= 0 || a.ColdAt == nil || a.State != domain.StateStopped {
		t.Fatalf("cold box: %s %s %d bytes at %v", a.State, a.Tier, a.ColdBytes, a.ColdAt)
	}

	// A kill and restart: the move stays, GC leaves one copy of the
	// commit, on the cold tier.
	h.kill()
	h.start(0, 0)
	if a := h.info("proj--a"); a.Tier != domain.TierCold || a.State != domain.StateStopped {
		t.Fatalf("cold box after the restart: %s %s", a.State, a.Tier)
	}
	coldCommits := commits(cold)
	if len(coldCommits) != 1 {
		t.Fatalf("cold commits after the restart: %v", coldCommits)
	}
	for _, c := range commits(filepath.Join(data, "storage")) {
		if c == coldCommits[0] {
			t.Fatalf("commit %s on both tiers after the restart", c)
		}
	}
	if left := coldTmp(); len(left) != 0 {
		t.Fatalf("cold tmp leftovers: %v", left)
	}

	// The sweep that moved a passed over the running box, the one pinned
	// hot and the project image's builder.
	ts := tiers()
	if ts["live"].Tier != domain.TierHot || ts["live"].State != string(domain.StateReady) {
		t.Fatalf("running box: %+v", ts["live"])
	}
	if ts["pinned"].Tier != domain.TierHot || !ts["pinned"].KeepHot || ts["pinned"].State != string(domain.StateStopped) {
		t.Fatalf("box pinned hot: %+v", ts["pinned"])
	}
	if ts["image"].Tier != domain.TierHot || ts["image"].State != string(domain.StateStopped) {
		t.Fatalf("project image builder: %+v", ts["image"])
	}
	if ts["a"].Tier != domain.TierCold || ts["a"].ColdBytes <= 0 {
		t.Fatalf("cold box: %+v", ts["a"])
	}

	// box up restores it (the note names the instance's VM, as the
	// resume note after it does).
	so, se, code := human(t, env, tree, "box", "up", "proj/a")
	if code != 0 || !strings.Contains(se, "box proj--a: restoring from cold storage (") || !strings.Contains(se, "storage.restore") {
		t.Fatalf("box up of a cold box: exit %d %q %q", code, so, se)
	}
	logged("restored vm from cold storage")
	if a := h.info("proj--a"); a.Tier != domain.TierHot && a.Tier != "" || a.State != domain.StateReady || a.ColdBytes != 0 {
		t.Fatalf("restored box: %s %s %d cold bytes", a.State, a.Tier, a.ColdBytes)
	}
	if ts := tiers(); ts["a"].Tier != domain.TierHot {
		t.Fatalf("box ls after the restore: %+v", ts["a"])
	}
	f, err = os.Open(diskOf("proj--a"))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(mark))
	_, err = f.ReadAt(got, off)
	f.Close()
	if err != nil || !bytes.Equal(got, mark) {
		t.Fatalf("restored disk lost the data written before the move: %v", err)
	}
	if left := coldTmp(); len(left) != 0 {
		t.Fatalf("cold tmp leftovers after the restore: %v", left)
	}
}
