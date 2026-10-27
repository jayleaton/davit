package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"davit/internal/domain"
	"davit/internal/state"
	"davit/internal/storage"
)

// The cold storage tier (docs/storage-tiers.md). With a cold
// directory configured, a stopped VM unused for ColdPolicy.After moves its
// committed disk there (demotion): images, goldens, running VMs, recently
// used ones and anything with a session or lease stay on the hot data
// dir. Its next resume copies the disk back (restore) before it boots.
//
// A demotion copies the VM's commits (its last one and its named
// snapshots no other VM needs) with storage.Tiered.Demote: rate-limited,
// in the idle I/O class, verified by reading the copy back. Then one
// transaction re-checks that the VM did not change meanwhile (any resume,
// delete, pin or new snapshot aborts the move) and records it: VM.Tier
// cold, the moved commits (VMRecord.ColdCommits) and no working disk.
// Storage GC then removes the hot copies and the stopped VM's disk. A crash
// anywhere leaves either the hot copy (the journal never switched: GC
// drops the cold one) or both (GC finishes the move); never neither.
//
// Demotions run one at a time: from the sweeper, for VMs unused for After,
// only while the host is not busy (no VM starting, nothing queued for
// capacity, ColdPolicy.HostBusy false); and from the low-disk cleanup, as
// its first tier ahead of auto-delete, for VMs stopped at least
// pressureColdAge ago, whatever the host is doing. A VM whose disk holds
// less than MinBytes of its own data stays hot: moving it would free
// almost nothing and slow its next resume.
//
// The cold tier keeps its own floor (ColdPolicy.MinFreeBytes and
// MinFreePercent, like the data dir's limits). A
// demotion that would take it below runs the cold cleanup first: cold
// boxes are auto-deleted, least recently used first and only after
// AutoDeletePolicy's instance age, until it fits. So cold fills before
// anything is deleted, and the hot cleanup auto-deletes only what cannot
// go cold.
//
// Promotion without a boot is the reverse move (storage.Tiered.Promote,
// the same switch, then GC drops the cold copies): the sweeper promotes
// a cold VM pinned hot (VM.KeepHot), and with ColdPolicy.Drain every cold
// VM, one at a time, while the hot disk keeps its floor plus the margin.
// Drain also stops all demotion, so the tier can be emptied and removed.

// Cold tier defaults (ColdPolicy zero values).
const (
	// DefaultColdAfter: a box stopped over lunch or between meetings stays
	// hot and resumes at once; one stopped for the night (or longer) goes
	// cold, where the next morning's first resume pays one copy back.
	DefaultColdAfter = 6 * time.Hour
	// DefaultColdBytesPerSecond paces a demotion's reads (the copy, then
	// the read-back that verifies it): well under one hard disk's
	// sequential rate, so guests' I/O keeps the disks' attention.
	DefaultColdBytesPerSecond = 100 * domain.MiB
	// DefaultColdMinBytes is the least of its own data a VM's disk must
	// hold to go cold.
	DefaultColdMinBytes = 512 * domain.MiB
	// pressureColdAge is how long a VM must have been unused before the
	// low-disk cleanup moves it early: not a stop-and-start in progress.
	pressureColdAge = 10 * time.Minute
)

// ColdPolicy configures the cold storage tier. The store must be a
// storage.Tiered with its cold tier attached.
type ColdPolicy struct {
	// After is how long a stopped VM must be unused before it goes cold
	// (0: DefaultColdAfter).
	After time.Duration
	// BytesPerSecond caps a demotion's I/O (0: DefaultColdBytesPerSecond,
	// negative: unlimited).
	BytesPerSecond int64
	// MinBytes is the least data of its own a VM's disk must hold to go
	// cold (0: DefaultColdMinBytes, negative: any).
	MinBytes int64
	// DiskSpace measures the cold filesystem; SyncDisk flushes it so a
	// delete's space shows (nil: no sync).
	DiskSpace func() (domain.DiskUsage, error)
	SyncDisk  func() error
	// SameFilesystem reports that the cold directory is on the data dir's
	// filesystem (doctor warns: moving frees nothing).
	SameFilesystem bool
	// HostBusy, if set, reports that the host's disks are busy now; the
	// sweeper then moves nothing.
	HostBusy func() bool
	// MinFreeBytes and MinFreePercent make the cold tier's floor, as
	// limits.minFreeDiskBytes / minFreeDiskPercent do the data dir's (0:
	// 5 GiB and 10%).
	MinFreeBytes   int64
	MinFreePercent int
	// Drain moves nothing to cold storage and brings every cold VM back
	// (to empty the tier before removing it).
	Drain bool
}

// coldFloor is the cold tier's floor for a filesystem of total bytes.
func (s *Service) coldFloor(total int64) int64 {
	return domain.Limits{MinFreeDiskBytes: s.cfg.Cold.MinFreeBytes, MinFreeDiskPercent: s.cfg.Cold.MinFreePercent}.DiskFloor(total)
}

func (p *ColdPolicy) after() time.Duration { return cmpOr(p.After, DefaultColdAfter) }

// AfterOrDefault is After with its default applied.
func (p *ColdPolicy) AfterOrDefault() time.Duration { return p.after() }

func (p *ColdPolicy) rate() int64 {
	if p.BytesPerSecond < 0 {
		return 0
	}
	return cmpOr(p.BytesPerSecond, DefaultColdBytesPerSecond)
}

func (p *ColdPolicy) minBytes() int64 {
	if p.MinBytes < 0 {
		return 0
	}
	return cmpOr(p.MinBytes, DefaultColdMinBytes)
}

// Why a VM stays hot (demote's refusals; not failures).
var (
	errStaysHot   = errors.New("stays hot")
	errColdFull   = errors.New("cold storage full")
	errVMChanged  = errors.New("vm changed while moving")
	errNoColdTier = errors.New("no cold storage tier")
)

// coldTier returns the tiered store and policy; ok is false without one.
func (s *Service) coldTier() (storage.Tiered, *ColdPolicy, bool) {
	if s.cfg.Cold == nil {
		return nil, nil, false
	}
	st, ok := s.cfg.Storage.(storage.Tiered)
	if !ok {
		return nil, nil, false
	}
	if _, attached := st.ColdProbe(); !attached {
		return nil, nil, false
	}
	return st, s.cfg.Cold, true
}

// coldUsage measures the cold filesystem.
func (s *Service) coldUsage() (domain.DiskUsage, bool) {
	_, p, ok := s.coldTier()
	if !ok || p.DiskSpace == nil {
		return domain.DiskUsage{}, false
	}
	d, err := p.DiskSpace()
	if err != nil {
		if !errors.Is(err, errors.ErrUnsupported) {
			s.cfg.Log.Warn("measure cold storage free space", "err", err)
		}
		return d, false
	}
	return d, true
}

func tierOf(v domain.VM) domain.StorageTier {
	if v.Tier == "" {
		return domain.TierHot
	}
	return v.Tier
}

// demotable reports whether v may go cold at all, its age and live
// activity aside: a stopped VM with a committed disk, no operation, not
// kept hot, not queued for, not a project image's builder (images stay
// hot) and not ephemeral.
func demotable(r *state.VMRecord, q queued) bool {
	v := r.VM
	return v.State == domain.StateStopped && v.ActiveOperationID == "" && !v.KeepHot && tierOf(v) == domain.TierHot &&
		v.LastSnapshotID != "" && r.RetainedDiskID == "" && !v.Privacy.Ephemeral && !q.vm(v) &&
		!(v.Labels[domain.LabelBox] != "" && isBuilder(v, v.Labels[domain.LabelBox]))
}

// coldCandidates lists the VMs (of owner, or anyone's) that may go cold
// now, unused for at least minAge, least recently used first.
func (s *Service) coldCandidates(ctx context.Context, minAge time.Duration, owner domain.PrincipalID) ([]domain.VM, error) {
	now := s.now()
	q := s.queuedRefs()
	type cand struct {
		vm   domain.VM
		used time.Time
	}
	var out []cand
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		all, err := tx.AllVMs()
		if err != nil {
			return err
		}
		for _, r := range all {
			if owner != "" && r.VM.Owner != owner || !demotable(r, q) {
				continue
			}
			s.mu.Lock()
			skip, tried := s.coldSkip[r.VM.ID]
			s.mu.Unlock()
			if tried && skip == r.VM.StateRevision {
				continue // measured too small at this revision
			}
			if used := s.lastActivity(r.VM); now.Sub(used) >= minAge {
				out = append(out, cand{r.VM, used})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out = slices.DeleteFunc(out, func(c cand) bool { return s.busy(ctx, c.vm, now) })
	slices.SortFunc(out, func(a, b cand) int {
		if c := a.used.Compare(b.used); c != 0 {
			return c
		}
		return strings.Compare(string(a.vm.ID), string(b.vm.ID))
	})
	vms := make([]domain.VM, len(out))
	for i, c := range out {
		vms[i] = c.vm
	}
	return vms, nil
}

// hotBase is the hot file vm's disk most likely shares blocks with: the
// commit of the snapshot it was created from, else its image's root
// filesystem ("" when neither is on hot storage).
func (s *Service) hotBase(ctx context.Context, vm domain.VM) string {
	if vm.SourceSnapshotID != "" {
		var sn *domain.Snapshot
		err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) { sn, err = tx.GetSnapshot(vm.SourceSnapshotID); return err })
		if err == nil && sn.CommitID != "" {
			if c, err := s.cfg.Storage.GetCommit(ctx, storage.CommitID(sn.CommitID)); err == nil && c.Tier != domain.TierCold {
				return c.Path
			}
		}
		return ""
	}
	if img, err := s.cfg.Images.Get(ctx, vm.ImageID); err == nil {
		return img.Rootfs
	}
	return ""
}

// cancelDemote stops a demotion of vm in flight (a resume, delete or pin
// wins over it; its journal check would abort it anyway).
func (s *Service) cancelDemote(vm domain.VMID) {
	s.mu.Lock()
	cancel := s.demoting[vm]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// cancelDemoteRef is cancelDemote for the VM principal names ref.
func (s *Service) cancelDemoteRef(ctx context.Context, principal domain.PrincipalID, ref string) {
	s.mu.Lock()
	n := len(s.demoting)
	s.mu.Unlock()
	if n == 0 {
		return
	}
	_ = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		if rec, err := lookupVM(tx, principal, ref); err == nil {
			s.cancelDemote(rec.VM.ID)
		}
		return nil
	})
}

// coldRestore is what booting v first copies back from cold storage.
func coldRestore(v domain.VM) int64 {
	if tierOf(v) == domain.TierCold {
		return v.ColdBytes
	}
	return 0
}

// demote moves vm's committed disk to the cold tier. errStaysHot (with
// why) means it is not to be moved; errColdFull that the cold tier has no
// room even after its cleanup.
func (s *Service) demote(ctx context.Context, id domain.VMID, reason string) error {
	st, pol, ok := s.coldTier()
	if !ok {
		return errNoColdTier
	}
	s.demoteMu.Lock()
	defer s.demoteMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.demoting[id] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.demoting, id)
		s.mu.Unlock()
		cancel()
	}()

	var rec *state.VMRecord
	var commits []storage.CommitID
	var last storage.CommitID
	err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) (err error) {
		if rec, err = tx.GetVM(id); err != nil {
			return err
		}
		if !demotable(rec, s.queuedRefs()) {
			return fmt.Errorf("%w: not a stopped vm that may move", errStaysHot)
		}
		others, err := snapshotsInUse(tx, id) // what other VMs need stays hot
		if err != nil {
			return err
		}
		if _, ok := others[rec.VM.LastSnapshotID]; ok {
			return fmt.Errorf("%w: its disk is %s", errStaysHot, others[rec.VM.LastSnapshotID])
		}
		snaps, err := tx.SnapshotsOfVM(id)
		if err != nil {
			return err
		}
		for _, sn := range snaps {
			c := storage.CommitID(sn.CommitID)
			if _, used := others[sn.ID]; used || c == "" || slices.Contains(commits, c) {
				continue
			}
			commits = append(commits, c)
			if sn.ID == rec.VM.LastSnapshotID {
				last = c
			}
		}
		if last == "" {
			return fmt.Errorf("%w: no committed disk", errStaysHot)
		}
		return nil
	})
	if err != nil {
		return err
	}
	start := s.now()
	base := s.hotBase(ctx, rec.VM)
	delta, err := st.DeltaBytes(ctx, last, base)
	if err != nil {
		return err
	}
	if delta < pol.minBytes() {
		s.mu.Lock()
		s.coldSkip[id] = rec.VM.StateRevision
		s.mu.Unlock()
		return fmt.Errorf("%w: only %s of its own data (coldStorage.minBytes %s)", errStaysHot,
			domain.HumanBytes(delta), domain.HumanBytes(pol.minBytes()))
	}
	// A cold copy shares nothing: it takes every allocated byte.
	var need int64
	for _, c := range commits {
		n, err := st.DeltaBytes(ctx, c, "")
		if err != nil {
			return err
		}
		need += n
	}
	if !s.coldRoom(ctx, need) {
		return fmt.Errorf("%w: %s to move", errColdFull, domain.HumanBytes(need))
	}

	hotBefore, _, _ := s.diskUsage()
	var moved []*storage.Move
	defer func() {
		for _, d := range moved {
			d.Release()
		}
	}()
	var lastBytes, total int64
	for _, c := range commits {
		d, err := st.Demote(ctx, c, storage.MoveOptions{BytesPerSecond: pol.rate(), IdleIO: true})
		if err != nil {
			for _, d := range moved {
				d.Release()
			}
			moved = nil
			if cerr := s.collect(context.WithoutCancel(ctx)); cerr != nil { // the copies made so far
				s.cfg.Log.Warn("collect after failed cold storage move", "vm", id, "err", cerr)
			}
			return fmt.Errorf("copy commit %s to cold storage: %w", c, err)
		}
		moved = append(moved, d)
		total += d.Bytes
		if c == last {
			lastBytes = d.Bytes
		}
	}
	now := s.now()
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		cur, err := tx.GetVM(id)
		if err != nil {
			return err
		}
		if cur.VM.StateRevision != rec.VM.StateRevision || cur.VM.LastSnapshotID != rec.VM.LastSnapshotID || !demotable(cur, s.queuedRefs()) {
			return errVMChanged
		}
		cur.VM.Tier, cur.VM.ColdBytes, cur.VM.ColdAt = domain.TierCold, lastBytes, &now
		cur.ColdCommits = cur.ColdCommits[:0]
		for _, c := range commits {
			cur.ColdCommits = append(cur.ColdCommits, string(c))
		}
		cur.DiskID = "" // the stopped VM's disk: its commit holds the same data
		return tx.PutVM(cur)
	})
	for _, d := range moved {
		d.Release()
	}
	moved = nil
	// Removes the hot copies, or the cold ones of an aborted move.
	if cerr := s.collect(context.WithoutCancel(ctx)); cerr != nil {
		s.cfg.Log.Warn("collect after cold storage move", "vm", id, "err", cerr)
	}
	if err != nil {
		return err
	}
	s.notify()
	var freed int64
	if s.cfg.SyncDisk != nil {
		_ = s.cfg.SyncDisk()
	}
	if after, ok, _ := s.diskUsage(); ok {
		freed = max(0, after.FreeBytes-hotBefore.FreeBytes)
	}
	secs := now.Sub(start).Seconds()
	s.cfg.Log.Warn("moved vm to cold storage", "vm", id, "name", rec.VM.Name, "reason", reason,
		"unused", start.Sub(s.lastActivity(rec.VM)).Round(time.Minute).String(), "commits", len(commits),
		"coldBytes", total, "ownBytes", delta, "hotFreedBytes", freed, "seconds", fmt.Sprintf("%.1f", secs))
	return nil
}

// coldRoom reports whether the cold tier can take need bytes above its
// floor, running the cold cleanup first when it cannot.
func (s *Service) coldRoom(ctx context.Context, need int64) bool {
	d, ok := s.coldUsage()
	if !ok {
		return true // not measured: the copy itself fails if it is full
	}
	floor := s.coldFloor(d.TotalBytes)
	if d.FreeBytes-need >= floor {
		return true
	}
	return s.coldCleanup(ctx, need)
}

// coldCleanup auto-deletes cold boxes, least recently used first, until
// the cold tier has need bytes above its floor plus the margin; it reports
// whether need now fits.
func (s *Service) coldCleanup(ctx context.Context, need int64) bool {
	_, pol, ok := s.coldTier()
	d, measured := s.coldUsage()
	if !ok || !measured {
		return false
	}
	floor := s.coldFloor(d.TotalBytes)
	c := &cleanup{s: s, free: d.FreeBytes, total: d.TotalBytes, floor: floor, target: floor + need + s.cfg.AutoDelete.margin(),
		where: "cold storage", usage: func() (domain.DiskUsage, bool, error) {
			u, ok := s.coldUsage()
			return u, ok, nil
		}, sync: pol.SyncDisk, tier: domain.TierCold}
	if c.free-need >= floor {
		return true
	}
	if need > 0 && need > d.TotalBytes-floor {
		return false // no deletion can make that much room
	}
	s.cfg.Log.Info("cold storage low: cleaning up", "freeBytes", c.free, "floorBytes", floor, "needBytes", need)
	c.autoDelete(ctx)
	return c.free-need >= floor
}

// coldSweep runs from the sweeper: the cold cleanup when the cold tier
// is below its floor, then the demotion of VMs unused for
// ColdPolicy.After, one at a time, while the host is not busy.
func (s *Service) coldSweep() {
	if _, _, ok := s.coldTier(); !ok || !s.coldMu.TryLock() {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.coldMu.Unlock()
		ctx := s.ctx
		if d, ok := s.coldUsage(); ok && d.FreeBytes < s.coldFloor(d.TotalBytes) {
			s.coldCleanup(ctx, 0)
		}
		s.promoteSweep(ctx)
		if s.cfg.Cold.Drain {
			return
		}
		tried := map[domain.VMID]bool{}
		for ctx.Err() == nil {
			if s.hostBusy(ctx) {
				return
			}
			vms, err := s.coldCandidates(ctx, s.cfg.Cold.after(), "")
			if err != nil {
				s.cfg.Log.Warn("cold storage: list unused vms", "err", err)
				return
			}
			vms = slices.DeleteFunc(vms, func(v domain.VM) bool { return tried[v.ID] })
			if len(vms) == 0 {
				return
			}
			v := vms[0]
			tried[v.ID] = true
			err = s.demote(ctx, v.ID, "unused")
			switch {
			case err == nil, ctx.Err() != nil:
			case errors.Is(err, errColdFull):
				s.cfg.Log.Warn("cold storage: no room to move unused vms", "vm", v.ID, "err", err)
				return
			case errors.Is(err, errStaysHot), errors.Is(err, errVMChanged), errors.Is(err, context.Canceled):
				s.cfg.Log.Debug("cold storage: vm stays hot", "vm", v.ID, "err", err)
			default:
				s.cfg.Log.Warn("cold storage: move failed", "vm", v.ID, "err", err)
			}
		}
	}()
}

// promoteSweep brings cold VMs back to hot storage without booting them:
// those pinned hot, or with Drain all of them, oldest first, one at a time
// while the host is not busy and the hot disk keeps its floor plus the
// margin.
func (s *Service) promoteSweep(ctx context.Context) {
	tried := map[domain.VMID]bool{}
	for ctx.Err() == nil && !s.hostBusy(ctx) {
		var next *state.VMRecord
		err := s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
			all, err := tx.AllVMs()
			for _, r := range all {
				v := r.VM
				if tierOf(v) == domain.TierCold && v.State == domain.StateStopped && v.ActiveOperationID == "" &&
					(v.KeepHot || s.cfg.Cold.Drain) && !tried[v.ID] {
					next = r
					break
				}
			}
			return err
		})
		if err != nil || next == nil {
			return
		}
		tried[next.VM.ID] = true
		if d, ok, _ := s.diskUsage(); ok {
			if need := next.VM.ColdBytes + s.cfg.Limits.DiskFloor(d.TotalBytes) + s.cfg.AutoDelete.margin(); d.FreeBytes < need {
				s.cfg.Log.Info("cold storage: no hot room to bring a vm back yet", "vm", next.VM.ID, "needBytes", need, "freeBytes", d.FreeBytes)
				return
			}
		}
		if err := s.promote(ctx, next.VM.ID); err != nil && ctx.Err() == nil {
			s.cfg.Log.Warn("cold storage: bringing vm back failed", "vm", next.VM.ID, "err", err)
		}
	}
}

// promote copies vm's cold commits back to hot storage, the reverse of
// demote, with the same journal switch; GC then drops the cold copies.
func (s *Service) promote(ctx context.Context, id domain.VMID) error {
	st, _, ok := s.coldTier()
	if !ok {
		return errNoColdTier
	}
	s.demoteMu.Lock()
	defer s.demoteMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.demoting[id] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.demoting, id)
		s.mu.Unlock()
		cancel()
	}()
	rec, err := s.loadVM(ctx, id)
	if err != nil {
		return err
	}
	if tierOf(rec.VM) != domain.TierCold || rec.VM.State != domain.StateStopped || rec.VM.ActiveOperationID != "" {
		return errVMChanged
	}
	start := time.Now()
	base := s.hotBase(ctx, rec.VM)
	var moved []*storage.Move
	defer func() {
		for _, m := range moved {
			m.Release()
		}
	}()
	for _, c := range rec.ColdCommits {
		m, err := st.Promote(ctx, storage.CommitID(c), storage.RestoreOptions{Base: base})
		if domain.IsCode(err, domain.CodeNotFound) {
			continue // pruned since it went cold
		}
		if err != nil {
			return fmt.Errorf("copy commit %s back from cold storage: %w", c, err)
		}
		moved = append(moved, m)
	}
	err = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		cur, err := tx.GetVM(id)
		if err != nil {
			return err
		}
		if cur.VM.StateRevision != rec.VM.StateRevision || tierOf(cur.VM) != domain.TierCold || cur.VM.ActiveOperationID != "" {
			return errVMChanged
		}
		cur.VM.Tier, cur.VM.ColdBytes, cur.VM.ColdAt, cur.ColdCommits = "", 0, nil, nil
		return tx.PutVM(cur)
	})
	for _, m := range moved {
		m.Release()
	}
	moved = nil
	if cerr := s.collect(context.WithoutCancel(ctx)); cerr != nil {
		s.cfg.Log.Warn("collect after bringing vm back from cold storage", "vm", id, "err", cerr)
	}
	if err != nil {
		return err
	}
	s.notify()
	s.cfg.Log.Warn("brought vm back from cold storage", "vm", id, "name", rec.VM.Name, "bytes", rec.VM.ColdBytes,
		"seconds", fmt.Sprintf("%.1f", time.Since(start).Seconds()), "keptHot", rec.VM.KeepHot)
	return nil
}

// hostBusy reports whether the sweeper should leave the disks alone now:
// a VM starting or stopping, a request queued for capacity, or the
// policy's probe (I/O pressure).
func (s *Service) hostBusy(ctx context.Context) bool {
	if s.queueLen() > 0 || s.cfg.Cold.HostBusy != nil && s.cfg.Cold.HostBusy() {
		return true
	}
	busy := false
	_ = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		all, err := tx.AllVMs()
		for _, r := range all {
			if r.VM.State == domain.StateProvisioning || r.VM.State == domain.StateStopping {
				busy = true
			}
		}
		return err
	})
	return busy
}

// demoteForSpace is the low-disk cleanup's first tier (diskclean.go):
// VMs go cold, least recently used first, until the target is met.
func (c *cleanup) demoteForSpace(ctx context.Context) bool {
	s := c.s
	if _, pol, ok := s.coldTier(); !ok || pol.Drain {
		return false
	}
	vms, err := s.coldCandidates(ctx, pressureColdAge, c.owner)
	if err != nil {
		s.cfg.Log.Warn("low-disk cleanup: list vms for cold storage", "err", err)
		return false
	}
	for _, v := range vms {
		if ctx.Err() != nil {
			return false
		}
		err := s.demote(ctx, v.ID, fmt.Sprintf("host disk low (%s free, floor %s)", domain.HumanBytes(c.free), domain.HumanBytes(c.floor)))
		if errors.Is(err, errColdFull) {
			s.cfg.Log.Warn("low-disk cleanup: cold storage full", "err", err)
			return false
		}
		if err != nil {
			s.cfg.Log.Info("low-disk cleanup: vm not moved to cold storage", "vm", v.ID, "err", err)
			continue
		}
		if c.measure() {
			return true
		}
	}
	return false
}

// cloneSnapshot makes disk from snap's commit for op: a clone, or for a
// commit on the cold tier a restore onto the VM's hot base with progress
// events. A resume that restored makes its VM hot again.
func (s *Service) cloneSnapshot(ctx context.Context, op *domain.Operation, rec *state.VMRecord, snap *domain.Snapshot, disk storage.DiskID) error {
	id := storage.CommitID(snap.CommitID)
	st, ok := s.cfg.Storage.(storage.Tiered)
	c, err := s.cfg.Storage.GetCommit(ctx, id)
	if domain.IsCode(err, domain.CodeNotFound) && tierOf(rec.VM) == domain.TierCold {
		why := s.cfg.ColdUnavailable
		if why == "" {
			why = "its copy is missing there"
		}
		return domain.Errorf(domain.CodeUnavailable, "vm %s is on cold storage, which is unavailable: %s (docs/storage-tiers.md#when-the-cold-disk-is-missing)", rec.VM.Name, why)
	}
	if !ok || err != nil || c.Tier != domain.TierCold {
		_, err = s.cfg.Storage.CloneCommitted(ctx, id, disk, rec.VM.Shape.DiskBytes)
		if err == nil && op.Kind == domain.OpResume && tierOf(rec.VM) == domain.TierCold {
			err = s.markHot(ctx, rec.VM.ID) // its hot copy outlived the move
		}
		return err
	}
	size := rec.VM.ColdBytes
	if size == 0 {
		size = c.SizeBytes
	}
	start := time.Now()
	s.event(ctx, op, "storage.restore", fmt.Sprintf("restoring from cold storage (%s)…", domain.HumanBytes(size)))
	last := start
	progress := func(done, total int64) {
		if time.Since(last) < 2*time.Second && done < total {
			return
		}
		last = time.Now()
		s.event(ctx, op, "storage.restore", fmt.Sprintf("restoring from cold storage: %s of %s (%.0f%%)",
			domain.HumanBytes(done), domain.HumanBytes(total), float64(done)*100/float64(max(total, 1))))
	}
	base := s.hotBase(ctx, rec.VM)
	if _, err := st.Restore(ctx, id, disk, rec.VM.Shape.DiskBytes, storage.RestoreOptions{Base: base, Progress: progress}); err != nil {
		return err
	}
	secs := time.Since(start).Seconds()
	s.cfg.Log.Info("restored vm from cold storage", "vm", rec.VM.ID, "name", rec.VM.Name, "bytes", size,
		"seconds", fmt.Sprintf("%.1f", secs), "mibPerSecond", fmt.Sprintf("%.0f", float64(size)/float64(domain.MiB)/max(secs, 0.001)),
		"rebased", base != "")
	if op.Kind == domain.OpResume {
		return s.markHot(ctx, rec.VM.ID)
	}
	return nil
}

// markHot records that vm's disk is on hot storage again.
func (s *Service) markHot(ctx context.Context, vm domain.VMID) error {
	return s.updateVM(ctx, vm, func(r *state.VMRecord, _ time.Time) error {
		r.VM.Tier, r.VM.ColdBytes, r.VM.ColdAt = "", 0, nil
		return nil
	})
}

// ColdVMs counts the VMs on cold storage in db's journal.
func ColdVMs(ctx context.Context, db *state.DB) (int, error) {
	n := 0
	err := db.Tx(ctx, func(tx *state.Tx) error {
		all, err := tx.AllVMs()
		for _, r := range all {
			if tierOf(r.VM) == domain.TierCold && r.VM.State != domain.StateDeleted {
				n++
			}
		}
		return err
	})
	return n, err
}

// coldCapacity is the capacity report's cold tier; nil without one.
func (s *Service) coldCapacity(ctx context.Context) *domain.ColdCapacity {
	st, pol, ok := s.coldTier()
	c := &domain.ColdCapacity{Unavailable: s.cfg.ColdUnavailable}
	if ok {
		p, _ := st.ColdProbe()
		c.AfterSeconds, c.FastClone, c.Detail = int64(pol.after()/time.Second), p.FastClone, p.Detail
		c.SameFilesystem, c.Draining = pol.SameFilesystem, pol.Drain
	}
	if d, ok := s.coldUsage(); ok {
		c.FreeBytes, c.TotalBytes, c.FloorBytes = d.FreeBytes, d.TotalBytes, s.coldFloor(d.TotalBytes)
	}
	_ = s.cfg.DB.Tx(ctx, func(tx *state.Tx) error {
		all, err := tx.AllVMs()
		for _, r := range all {
			if tierOf(r.VM) == domain.TierCold && r.VM.State != domain.StateDeleted && r.VM.State != domain.StateDeleting {
				c.VMs++
				c.Bytes += r.VM.ColdBytes
			}
		}
		return err
	})
	if !ok && c.VMs == 0 {
		return nil
	}
	if !ok && c.Unavailable == "" {
		c.Unavailable = "coldStorage.dir is not configured"
	}
	return c
}
