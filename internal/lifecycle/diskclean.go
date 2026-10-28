package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"davit/api"
	"davit/internal/domain"
	"davit/internal/state"
)

// The low-disk cleanup (docs/vm-host.md#disk-space-floor-reservation-and-auto-delete).
// It runs only while the data dir's free space is below the floor (the
// sweeper checks every tick) or below what the request at the head of the
// capacity queue needs (reclaim.go), and stops as soon as free space is
// back at that level plus AutoDeletePolicy.MarginBytes, so that it does not
// run again for the next create. Above the floor, for a queued request, it
// stops and deletes only the requester's own boxes, and never when the
// request needs more than the whole filesystem. A run that freed nothing is
// stuck: it is not repeated until the journal changes (a box stopped,
// unpinned or deleted), or for stuckEvery, and while stuck the cleanup
// logs what blocks it at most every stuckEvery. In order, cheapest loss
// first:
//
//  1. davit-owned garbage: storage not referenced by the journal (disks and
//     commits of deleted VMs and snapshots), goldens that can never be
//     restored again, goldens above the golden floor (least recently used,
//     never an image's newest), and image blobs no manifest or served image
//     uses (blobprune.go);
//  2. idle boxes are stopped, as admission does to make room (reclaim.go),
//     with a reflink store only: their commits share blocks, and the
//     snapshots they supersede are freed;
//  3. with a cold storage tier, stopped VMs move there, least recently
//     used first (coldtier.go): nothing is lost but a fast first resume.
//     A move the cold tier has no room for first runs the cold cleanup,
//     which auto-deletes cold boxes unused for the cold age;
//  4. auto-delete of unused boxes, least recently used first: project
//     images that have no instances (with their generations and the goldens
//     captured from them), then stopped instances still on hot storage
//     (cold ones free nothing here; the cold tier's own cleanup deletes
//     them when it is full);
//  5. under pressure (the host below its floor, not only a request's need,
//     and the tiers above left the target unmet), the age limits are
//     waived down to the pressure age: tier 3 again, its cold cleanup
//     deleting cold boxes least recently used first, then tier 4.
//
// A box is auto-deleted only when it has been unused for the policy's
// minimum age (under pressure, the pressure age), is stopped with no
// operation, is not pinned (VM.keep; a pinned builder pins its image), is
// not being moved between tiers, runs nothing and holds no session or
// prompt lease, and is not the target of a queued request. Only box VMs
// (label davit.box) are ever auto-deleted; other VMs never are. Each
// deletion is logged at warn level, recorded as the VM's deleteReason and
// as a vm.autodeleted event (box ls, doctor and `davit events` show it).

// Auto-delete defaults (AutoDeletePolicy zero values). A stopped instance
// holds work: three days keep it across a weekend away. A project image
// with no instances holds nothing a rebuild (`box image build`) cannot
// make again: one day keeps it from one working day to the next. A cold
// instance has already been unused for coldStorage.after and is the
// cheapest way to make room for the next move: half a day.
const (
	DefaultAutoDeleteInstanceAge     = 72 * time.Hour
	DefaultAutoDeleteImageAge        = 24 * time.Hour
	DefaultAutoDeleteColdInstanceAge = 12 * time.Hour
	// PressureDeleteAge is the least a box must have been unused before
	// the cleanup deletes it under pressure (never more than the age it
	// waives): not one in use a moment ago.
	PressureDeleteAge = time.Hour
	// DefaultDiskMargin is the cleanup's hysteresis: one default box's
	// growth headroom (25% of 8 GiB), so that the create it made room for
	// does not start it again.
	DefaultDiskMargin = 2 * domain.GiB
)

// garbageEvery bounds how often the cleanup collects garbage (it walks the
// store and the blob directory). stuckEvery is how long a run that freed
// nothing holds back the next (unless the journal changes), and how often
// a stuck cleanup logs what blocks it.
const (
	garbageEvery = time.Minute
	stuckEvery   = 10 * time.Minute
)

// AutoDeletePolicy configures the low-disk cleanup.
type AutoDeletePolicy struct {
	// Disabled turns auto-delete off (garbage is still collected).
	Disabled bool
	// InstanceMinAge and ImageMinAge are how long a stopped instance, or a
	// project image without instances, must have been unused (0: the
	// defaults). ColdInstanceMinAge is InstanceMinAge for an instance on
	// the cold tier, which the cold cleanup deletes.
	InstanceMinAge     time.Duration
	ImageMinAge        time.Duration
	ColdInstanceMinAge time.Duration
	// MarginBytes is how far above the floor the cleanup frees (0:
	// DefaultDiskMargin).
	MarginBytes int64
}

func (p AutoDeletePolicy) instanceAge() time.Duration {
	return cmpOr(p.InstanceMinAge, DefaultAutoDeleteInstanceAge)
}

func (p AutoDeletePolicy) imageAge() time.Duration {
	return cmpOr(p.ImageMinAge, DefaultAutoDeleteImageAge)
}

func (p AutoDeletePolicy) coldInstanceAge() time.Duration {
	return cmpOr(p.ColdInstanceMinAge, DefaultAutoDeleteColdInstanceAge)
}

func (p AutoDeletePolicy) margin() int64 { return cmpOr(p.MarginBytes, DefaultDiskMargin) }

func cmpOr[T int64 | time.Duration](v, def T) T {
	if v <= 0 {
		return def
	}
	return v
}

// cleanup is one run of freeDisk, or of the cold tier's cleanup
// (coldtier.go).
type cleanup struct {
	s             *Service
	free, total   int64
	floor, target int64
	// owner, when set, limits stops and deletes to that owner's boxes (a
	// queued request's cleanup while the host is above the floor).
	owner domain.PrincipalID
	// where names the filesystem ("" is the host's data disk); usage and
	// sync measure and flush it (nil: the data disk's). With tier set,
	// only boxes on that storage tier are deleted.
	where string
	usage func() (domain.DiskUsage, bool, error)
	sync  func() error
	tier  domain.StorageTier
	// pressure waives the age limits down to PressureDeleteAge (tier 5).
	pressure bool
	// blocked is what kept a step from freeing space, for the stuck log.
	blocked []string
}

// block records why a step freed nothing.
func (c *cleanup) block(format string, args ...any) {
	why := fmt.Sprintf(format, args...)
	if !slices.Contains(c.blocked, why) {
		c.blocked = append(c.blocked, why)
	}
}

// ages are the least time unused before an instance and an image may be
// deleted in this run: the policy's (on cold, its cold instance age), or
// under pressure PressureDeleteAge, never above the policy's.
func (c *cleanup) ages() (instance, image time.Duration) {
	p := c.s.cfg.AutoDelete
	instance, image = p.instanceAge(), p.imageAge()
	if c.tier == domain.TierCold {
		instance = p.coldInstanceAge()
	}
	if c.pressure {
		instance, image = min(instance, PressureDeleteAge), min(image, PressureDeleteAge)
	}
	return instance, image
}

func (c *cleanup) place() string { return cmpOrS(c.where, "host disk") }

func cmpOrS(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// measure refreshes c.free after flushing the filesystem (so space a
// delete freed is visible) and reports whether the target is met.
func (c *cleanup) measure() bool {
	usage, sync := c.usage, c.sync
	if usage == nil {
		usage, sync = c.s.diskUsage, c.s.cfg.SyncDisk
	}
	if sync != nil {
		if err := sync(); err != nil {
			c.s.cfg.Log.Warn("low-disk cleanup: sync", "where", c.place(), "err", err)
		}
	}
	if d, ok, err := usage(); err == nil && ok {
		c.free, c.total = d.FreeBytes, d.TotalBytes
	}
	return c.done()
}

func (c *cleanup) done() bool { return c.free >= c.target }

// lowDisk reports whether free space is below the floor.
func (s *Service) lowDisk() bool {
	d, ok, err := s.diskUsage()
	if err != nil || !ok {
		return false
	}
	floor := s.cfg.Limits.DiskFloor(d.TotalBytes)
	return floor > 0 && d.FreeBytes < floor
}

// freeDisk runs the low-disk cleanup when free space is below the floor,
// or below need (a queued admission's free-space need, floor included, for
// requester), until it is at least the larger of the two plus the margin.
// Only one runs at a time; a call while one runs returns at once.
func (s *Service) freeDisk(ctx context.Context, need int64, requester domain.PrincipalID) {
	if !s.cleanMu.TryLock() {
		return
	}
	defer s.cleanMu.Unlock()
	d, ok, err := s.diskUsage()
	if err != nil || !ok {
		return
	}
	floor := s.cfg.Limits.DiskFloor(d.TotalBytes)
	if floor == 0 || d.FreeBytes >= max(floor, need) {
		return
	}
	now := s.now()
	c := &cleanup{s: s, free: d.FreeBytes, total: d.TotalBytes, floor: floor, target: max(floor, need) + s.cfg.AutoDelete.margin()}
	if _, _, ok := s.coldTier(); ok {
		c.tier = domain.TierHot
	}
	if d.FreeBytes >= floor {
		c.owner = requester // only the requester's need, not the host's
	}
	// A stuck run holds back the next one that could do no more (the same
	// boxes or fewer) until the journal changes or stuckEvery passes.
	if s.fruitless != nil && now.Sub(s.fruitlessAt) < stuckEvery && (s.fruitlessOwner == "" || s.fruitlessOwner == c.owner) {
		select {
		case <-s.fruitless:
		default:
			return // nothing changed since the last run freed nothing
		}
	}
	changed := s.watch()
	stuck := s.fruitless != nil // the last run freed nothing
	start := c.free
	lvl := slog.LevelInfo
	if stuck {
		lvl = slog.LevelDebug
	}
	s.cfg.Log.Log(ctx, lvl, "host disk low: cleaning up", "freeBytes", c.free, "floorBytes", floor, "needBytes", need, "targetBytes", c.target)
	defer func() {
		s.fruitless, s.fruitlessAt, s.fruitlessOwner = nil, now, ""
		if c.free > start {
			s.cfg.Log.Warn("host disk low: cleanup done", "freeBytes", c.free, "freedBytes", c.free-start,
				"targetBytes", c.target, "met", c.done(), "pressure", c.pressure)
			s.stuckLogAt = time.Time{}
			return
		}
		s.fruitless, s.fruitlessOwner = changed, c.owner
		if !s.stuckLogAt.IsZero() && now.Sub(s.stuckLogAt) < stuckEvery {
			s.cfg.Log.Debug("host disk low: cleanup still stuck", "freeBytes", c.free, "targetBytes", c.target)
			return
		}
		s.stuckLogAt = now
		s.cfg.Log.Warn("host disk low: cleanup freed nothing; retrying when a box changes, or in "+shortAge(stuckEvery),
			"freeBytes", c.free, "floorBytes", c.floor, "targetBytes", c.target, "owner", string(c.owner),
			"blockedBy", strings.Join(append(c.blocked, s.cleanupCensus(ctx, c.owner)...), "; "))
	}()
	if c.garbage(ctx) || c.stopIdle(ctx) || c.demoteForSpace(ctx) {
		return
	}
	if d.TotalBytes > 0 && need > d.TotalBytes {
		c.block("the request needs %s, more than the whole disk (%s)", domain.HumanBytes(need), domain.HumanBytes(d.TotalBytes))
		return // no deletion can make that much room
	}
	if c.autoDelete(ctx) || c.owner != "" {
		return // above the floor, a request frees only its owner's boxes at the usual ages
	}
	// Tier 5: below the floor with the target unmet, waive the ages.
	c.pressure = true
	if !c.demoteForSpace(ctx) {
		c.autoDelete(ctx)
	}
}

// cleanupCensus says why the boxes (of owner, or all) that a cleanup left
// were not candidates: one clause per reason, with up to three names.
func (s *Service) cleanupCensus(ctx context.Context, owner domain.PrincipalID) []string {
	now := s.now()
	q := s.queuedRefs()
	type group struct {
		names []string
		n     int
	}
	groups := map[string]*group{}
	var order []string
	add := func(why, name string) {
		g := groups[why]
		if g == nil {
			g = &group{}
			groups[why] = g
			order = append(order, why)
		}
		if g.n++; len(g.names) < 3 {
			g.names = append(g.names, name)
		}
	}
	var stopped []domain.VM
	_ = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		all, err := tx.AllVMs()
		for _, r := range all {
			v := r.VM
			if v.State == domain.StateDeleted || v.State == domain.StateDeleting || owner != "" && v.Owner != owner {
				continue
			}
			switch {
			case v.Labels[domain.LabelBox] == "":
				add("not boxes (never auto-deleted)", v.Name)
			case v.State == domain.StateReady || v.State == domain.StateRunning:
				add("running", v.Name)
			case v.State != domain.StateStopped:
				add(string(v.State), v.Name)
			case v.Keep:
				add("pinned", v.Name)
			case v.ActiveOperationID != "":
				add("with an operation in progress", v.Name)
			case q.vm(v):
				add("queued for", v.Name)
			default:
				stopped = append(stopped, v)
			}
		}
		return err
	})
	for _, v := range stopped {
		switch {
		case s.moving(v.ID):
			add("moving between storage tiers", v.Name)
		case s.busy(ctx, v, now):
			add("with a session or lease", v.Name)
		case now.Sub(s.lastActivity(v)) < PressureDeleteAge:
			add("used in the last "+shortAge(PressureDeleteAge), v.Name)
		default:
			add("younger than the auto-delete age, or on cold storage", v.Name)
		}
	}
	if s.cfg.AutoDelete.Disabled {
		order = append([]string{"autoDelete.enabled is false"}, order...)
	}
	out := make([]string, 0, len(order))
	for _, why := range order {
		g := groups[why]
		if g == nil {
			out = append(out, why)
			continue
		}
		more := ""
		if g.n > len(g.names) {
			more = ", …"
		}
		out = append(out, fmt.Sprintf("%d %s (%s%s)", g.n, why, strings.Join(g.names, ", "), more))
	}
	return out
}

// garbage collects davit-owned garbage (at most every garbageEvery).
func (c *cleanup) garbage(ctx context.Context) bool {
	s := c.s
	now := s.now()
	if now.Sub(s.lastGarbage) < garbageEvery {
		return false
	}
	s.lastGarbage = now
	before := c.free
	if err := s.collect(ctx); err != nil {
		s.cfg.Log.Warn("low-disk cleanup: collect", "err", err)
	}
	s.enforceGoldenFloor(ctx)
	if res, err := s.pruneBlobs(ctx, false); err != nil {
		s.cfg.Log.Warn("low-disk cleanup: prune image blobs", "err", err)
	} else if len(res.Removed) > 0 {
		s.cfg.Log.Warn("low-disk cleanup: pruned unreferenced image blobs", "blobs", len(res.Removed), "bytes", res.Bytes)
	}
	done := c.measure()
	if c.free > before {
		s.cfg.Log.Warn("low-disk cleanup: garbage collected", "freedBytes", c.free-before, "freeBytes", c.free)
	}
	return done
}

// stopIdle stops reclaimable idle VMs, oldest activity first, re-measuring
// after each (reflink store only: a copy store's commit copies the disk).
func (c *cleanup) stopIdle(ctx context.Context) bool {
	s := c.s
	after := s.cfg.Limits.ReclaimIdleAfter()
	if after == 0 || !s.probe.FastClone {
		return false
	}
	for n := 0; n < maxReclaims; n++ {
		vms, err := s.reclaimable(ctx, reclaimQuery{idle: after, needTimeout: true, owner: c.owner})
		if err != nil || len(vms) == 0 {
			if err != nil {
				s.cfg.Log.Warn("low-disk cleanup: list idle VMs", "err", err)
			}
			return false
		}
		if err := s.reclaimStop(ctx, vms[0]); err != nil {
			s.cfg.Log.Warn("low-disk cleanup: stop", "vm", vms[0].ID, "err", err)
			return false
		}
		if c.measure() {
			return true
		}
	}
	return false
}

// candidate is a box the cleanup may auto-delete.
type candidate struct {
	kind     string // "image" or "instance"
	vm       domain.VM
	project  string
	gens     []domain.SnapshotID // an image's generations
	lastUsed time.Time
}

func (c candidate) box() string {
	if c.kind == "image" {
		return c.project
	}
	return c.project + "/" + c.vm.Labels[domain.LabelBoxInstance]
}

// autoDelete deletes candidates, images before instances, each least
// recently used first, until the target is met.
func (c *cleanup) autoDelete(ctx context.Context) bool {
	s := c.s
	if s.cfg.AutoDelete.Disabled {
		return false
	}
	instanceAge, imageAge := c.ages()
	cands, err := s.autoDeleteCandidates(ctx, c.owner, c.tier, instanceAge, imageAge)
	if err != nil {
		s.cfg.Log.Warn("low-disk cleanup: list unused boxes", "err", err)
		return false
	}
	for _, cand := range cands {
		if ctx.Err() != nil {
			return false
		}
		if err := c.deleteBox(ctx, cand); err != nil {
			s.cfg.Log.Info("low-disk cleanup: box not deleted", "box", cand.box(), "vm", cand.vm.ID, "err", err)
			continue
		}
		if c.done() {
			return true
		}
	}
	return false
}

type projectKey struct {
	owner   domain.PrincipalID
	project string
}

func isBuilder(vm domain.VM, project string) bool {
	return vm.Labels[domain.LabelBoxRole] == domain.BoxRoleImage && vm.Name == domain.BoxImageVM(project)
}

// autoDeleteCandidates lists the boxes (of owner, or anyone's, on storage
// tier, or either) that may be auto-deleted now, unused for at least
// instanceAge or imageAge: images without instances, then stopped
// instances, each oldest last use first.
func (s *Service) autoDeleteCandidates(ctx context.Context, owner domain.PrincipalID, tier domain.StorageTier,
	instanceAge, imageAge time.Duration) ([]candidate, error) {
	now := s.now()
	queued := s.queuedRefs()
	var images, instances []candidate
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		all, err := tx.AllVMs()
		if err != nil {
			return err
		}
		lastUse := map[projectKey]time.Time{}
		instancesOf := map[projectKey]int{}
		for _, r := range all {
			v := r.VM
			p := v.Labels[domain.LabelBox]
			if p == "" {
				continue
			}
			k := projectKey{v.Owner, p}
			t := s.lastActivity(v)
			if v.DeletedAt != nil && v.DeletedAt.After(t) {
				t = *v.DeletedAt
			}
			if t.After(lastUse[k]) {
				lastUse[k] = t
			}
			if !isBuilder(v, p) && v.State != domain.StateDeleted && v.State != domain.StateDeleting {
				instancesOf[k]++
			}
		}
		for _, r := range all {
			v := r.VM
			p := v.Labels[domain.LabelBox]
			if p == "" || owner != "" && v.Owner != owner || !deletable(v, queued) || tier != "" && tierOf(v) != tier {
				continue
			}
			if isBuilder(v, p) {
				k := projectKey{v.Owner, p}
				if instancesOf[k] > 0 {
					continue
				}
				snaps, err := tx.SnapshotsOfVM(v.ID)
				if err != nil {
					return err
				}
				use, err := snapshotsInUse(tx, v.ID) // the builder goes with them
				if err != nil {
					return err
				}
				c := candidate{kind: "image", vm: v, project: p, lastUsed: lastUse[k]}
				busy := false
				for _, sn := range snaps {
					if !strings.HasPrefix(sn.Name, domain.BoxImageSnapshotPrefix(p)) {
						continue
					}
					if _, ok := use[sn.ID]; ok || queued.snapshot(sn) {
						busy = true // another VM boots from it, or a queued create will
					}
					c.gens = append(c.gens, sn.ID)
					if sn.CreatedAt.After(c.lastUsed) {
						c.lastUsed = sn.CreatedAt
					}
				}
				if !busy && now.Sub(c.lastUsed) >= imageAge {
					images = append(images, c)
				}
				continue
			}
			if v.Labels[domain.LabelBoxInstance] == "" {
				continue
			}
			if c := (candidate{kind: "instance", vm: v, project: p, lastUsed: s.lastActivity(v)}); now.Sub(c.lastUsed) >= instanceAge {
				instances = append(instances, c)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Outside the transaction: a prompt-turn lease is read from the journal.
	notBusy := func(c candidate) bool { return s.moving(c.vm.ID) || s.busy(ctx, c.vm, now) }
	images, instances = slices.DeleteFunc(images, notBusy), slices.DeleteFunc(instances, notBusy)
	lru := func(a, b candidate) int {
		if c := a.lastUsed.Compare(b.lastUsed); c != 0 {
			return c
		}
		return strings.Compare(string(a.vm.ID), string(b.vm.ID))
	}
	slices.SortFunc(images, lru)
	slices.SortFunc(instances, lru)
	return append(images, instances...), nil
}

// deletable reports whether v may be auto-deleted at all, its age and
// live activity (busy) aside: stopped with no operation, not pinned, not
// ephemeral (its stop deleted it) and not queued for.
func deletable(v domain.VM, q queued) bool {
	return v.State == domain.StateStopped && v.ActiveOperationID == "" && !v.Keep && !v.Privacy.Ephemeral && !q.vm(v)
}

// busy reports live activity on v: a session, stream or process
// (Config.Busy), or a prompt-turn lease.
func (s *Service) busy(ctx context.Context, v domain.VM, now time.Time) bool {
	if s.cfg.Busy != nil && s.cfg.Busy(v.ID, v.InstanceGeneration) {
		return true
	}
	return s.leased(ctx, v.ID, now)
}

// shortAge formats d without trailing zero units ("72h", "1h30m", "45s").
func shortAge(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// moving reports a move of vm between storage tiers in flight.
func (s *Service) moving(vm domain.VMID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.demoting[vm] != nil
}

// deleteBox auto-deletes one candidate and records it.
func (c *cleanup) deleteBox(ctx context.Context, cand candidate) error {
	s := c.s
	now := s.now()
	unused := now.Sub(cand.lastUsed).Round(time.Minute)
	why := fmt.Sprintf("auto-deleted: unused for %s (last used %s); %s low (%s free, floor %s)",
		unused, cand.lastUsed.UTC().Format(time.RFC3339), c.place(), domain.HumanBytes(c.free), domain.HumanBytes(c.floor))
	if c.pressure {
		age := c.s.cfg.AutoDelete.instanceAge()
		if c.tier == domain.TierCold {
			age = c.s.cfg.AutoDelete.coldInstanceAge()
		}
		if cand.kind == "image" {
			age = c.s.cfg.AutoDelete.imageAge()
		}
		why += fmt.Sprintf("; under pressure (host disk below its floor with nothing else to free): "+
			"least recently used first, the %s age limit waived", shortAge(age))
	}
	req := Request{
		Principal:      cand.vm.Owner,
		IdempotencyKey: fmt.Sprintf("autodelete:%s:%d", cand.vm.ID, cand.vm.StateRevision),
		Method:         "DELETE", Path: "internal/auto-delete",
	}
	// Re-checked in the delete's transaction: the box may have been used,
	// pinned or given an instance since it was listed.
	check := func(tx *state.Tx, rec *state.VMRecord) error {
		v := rec.VM
		q := s.queuedRefs()
		if v.StateRevision != cand.vm.StateRevision || !deletable(v, q) {
			return domain.Errorf(domain.CodeInvalidState, "box %s changed since it was chosen", cand.box())
		}
		if cand.kind != "image" {
			return nil
		}
		for _, id := range cand.gens {
			if sn, err := tx.GetSnapshot(id); err == nil && q.snapshot(sn) {
				return domain.Errorf(domain.CodeInvalidState, "a queued create boots box %s's image", cand.box())
			}
		}
		all, err := tx.AllVMs()
		if err != nil {
			return err
		}
		for _, r := range all {
			if r.VM.Owner == v.Owner && r.VM.Labels[domain.LabelBox] == cand.project && r.VM.ID != v.ID &&
				r.VM.State != domain.StateDeleted && r.VM.State != domain.StateDeleting {
				return domain.Errorf(domain.CodeInvalidState, "box %s has an instance again", cand.box())
			}
		}
		return nil
	}
	before := c.free
	acc, err := s.deleteVM(ctx, req, string(cand.vm.ID), "auto-delete", why, check)
	if err != nil {
		return err
	}
	if err := s.waitOperation(ctx, acc.Operation.ID); err != nil {
		return err
	}
	if len(cand.gens) > 0 {
		s.deleteGenerations(ctx, cand)
	}
	c.measure()
	rec := domain.AutoDeletion{
		VMID: cand.vm.ID, Name: cand.vm.Name, Box: cand.box(), Kind: cand.kind,
		LastUsedAt: cand.lastUsed.UTC(), UnusedSeconds: int64(now.Sub(cand.lastUsed) / time.Second),
		FreedBytes: max(0, c.free-before), DeletedAt: s.now(), Reason: why,
	}
	s.cfg.Log.Warn("auto-deleted unused box to free disk space", "box", rec.Box, "kind", rec.Kind, "vm", rec.VMID, "name", rec.Name,
		"unused", unused.String(), "lastUsedAt", rec.LastUsedAt.Format(time.RFC3339), "freedBytes", rec.FreedBytes,
		"freeBytes", c.free, "floorBytes", c.floor, "operation", acc.Operation.ID)
	var data map[string]any
	if b, err := json.Marshal(rec); err == nil {
		_ = json.Unmarshal(b, &data)
	}
	if err := s.cfg.DB.Tx(context.WithoutCancel(ctx), func(tx *state.Tx) error {
		return tx.AppendHostEvent(cand.vm.Owner, api.EventVMAutoDeleted, string(cand.vm.ID), string(cand.vm.ID), data)
	}); err != nil {
		s.cfg.Log.Warn("vm.autodeleted event", "vm", cand.vm.ID, "err", err)
	}
	s.notify()
	return nil
}

// deleteGenerations removes an auto-deleted image's generation snapshots
// that nothing uses, then collects their storage (and the goldens captured
// from them).
func (s *Service) deleteGenerations(ctx context.Context, cand candidate) {
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		use, err := snapshotsInUse(tx, cand.vm.ID)
		if err != nil {
			return err
		}
		for _, id := range cand.gens {
			if _, busy := use[id]; busy {
				continue
			}
			if err := tx.DeleteSnapshot(id); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		err = s.collect(ctx)
	}
	if err != nil {
		s.cfg.Log.Warn("auto-delete: image generations", "box", cand.project, "err", err)
	}
}
