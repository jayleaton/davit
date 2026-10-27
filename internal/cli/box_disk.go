package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/preflight"
)

// Host disk space for boxes (docs/project-boxes.md#when-the-host-is-low-on-disk).
// Admission refuses or queues a box create or resume that would leave the
// host's data disk below its floor; when it is below, the host collects its
// garbage, stops idle boxes and finally auto-deletes boxes unused for
// days, least recently used first, unless they are pinned (`box pin`).
// `box ls` shows the disk next to memory and names recently auto-deleted
// boxes; `box ls --deleted` lists them with why.

// autoDeleteWindow is how long box ls and doctor name auto-deleted boxes
// (the host keeps events a week).
const autoDeleteWindow = 7 * 24 * time.Hour

// autoDeleted lists the caller's boxes the host auto-deleted within
// autoDeleteWindow, newest first (of project, or all). A host without the
// event stream reports none.
func (s *session) autoDeleted(ctx context.Context, cl *client, project string) []api.AutoDeletion {
	q := url.Values{"types": {api.EventVMAutoDeleted}, "limit": {"200"}}
	since := time.Now().Add(-autoDeleteWindow)
	var out []api.AutoDeletion
	for cursor := ""; ; {
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var l api.EventList
		if err := cl.get(ctx, "/events", q, &l); err != nil {
			return out
		}
		for _, ev := range l.Items {
			var d api.AutoDeletion
			b, err := json.Marshal(ev.Data)
			if err != nil || json.Unmarshal(b, &d) != nil || d.DeletedAt.Before(since) {
				continue
			}
			if project != "" && d.Box != project && !strings.HasPrefix(d.Box, project+"/") {
				continue
			}
			out = append(out, d)
		}
		if cursor = l.NextCursor; cursor == "" || len(l.Items) == 0 {
			break
		}
	}
	slices.SortFunc(out, func(a, b api.AutoDeletion) int { return b.DeletedAt.Compare(a.DeletedAt) })
	return out
}

// autoDeletedLine is the one-line form of an auto-deletion.
func autoDeletedLine(d api.AutoDeletion) string {
	return fmt.Sprintf("%s %s auto-deleted %s ago (unused %s, freed %s)", d.Kind, d.Box,
		roundAge(time.Since(d.DeletedAt)), roundAge(time.Duration(d.UnusedSeconds)*time.Second), humanBytes(d.FreedBytes))
}

// roundAge shortens a duration for people: days and hours, or minutes.
func roundAge(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", d/(24*time.Hour), (d%(24*time.Hour))/time.Hour)
	case d >= time.Hour:
		return d.Round(time.Minute).String()
	}
	return d.Round(time.Second).String()
}

// boxDeleted is `box ls --deleted`: the boxes the host auto-deleted for
// disk space recently, with why.
func (s *session) boxDeleted(ctx context.Context, project string) error {
	items := s.autoDeleted(ctx, s.cl, project)
	if items == nil {
		items = []api.AutoDeletion{}
	}
	s.out.result("", map[string]any{"items": items})
	for _, d := range items {
		s.out.human("%s\n  %s", autoDeletedLine(d), d.Reason)
	}
	if len(items) == 0 {
		s.out.human("no box was auto-deleted in the last %d days (the host deletes unused boxes only when its disk is low; `%s box pin` keeps one)",
			int(autoDeleteWindow/(24*time.Hour)), buildinfo.Name)
	}
	return nil
}

// boxPin pins (or unpins) an instance, or with INSTANCE "image" the
// project image: the host never auto-deletes a pinned box. hot also keeps
// it on hot storage (never moved to the cold tier); unpin clears both.
func (s *session) boxPin(ctx context.Context, arg string, keep, hot bool) error {
	if arg == "" {
		arg = defaultProject()
	} else if arg[0] == '/' {
		arg = defaultProject() + arg
	}
	p, inst, _ := strings.Cut(arg, "/")
	var name, project, label string
	if inst == imageInstance {
		pr, err := boxName(p)
		if err != nil {
			return err
		}
		name, project, label = imageVM(pr), pr, pr+" (image)"
	} else {
		ref, err := parseBoxRef(arg)
		if err != nil {
			return err
		}
		name, project, label = ref.vm(), ref.project, ref.project+"/"+ref.instance
	}
	vm, err := s.boxVM(ctx, name, project)
	if err != nil {
		return err
	}
	body := api.UpdateVMRequest{Keep: &keep}
	if hot || !keep {
		keepHot := keep
		body.KeepHot = &keepHot
	}
	var out api.VM
	if err := s.cl.do(ctx, http.MethodPatch, vmPath(string(vm.ID)), nil, body, nil, &out); err != nil {
		return err
	}
	s.out.result("", map[string]any{"box": label, "vm": out.Name, "vmId": out.ID, "keep": out.Keep, "keepHot": out.KeepHot})
	switch {
	case keep && out.KeepHot:
		s.out.human("box %s pinned hot: never auto-deleted when the host is low on disk, never moved to cold storage (`%s box unpin` undoes it)", label, buildinfo.Name)
	case keep:
		s.out.human("box %s pinned: never auto-deleted when the host is low on disk; may still move to cold storage when unused (`box pin --hot` keeps it hot; `%s box unpin` undoes it)", label, buildinfo.Name)
	default:
		s.out.human("box %s unpinned: may move to cold storage when unused, and be auto-deleted once unused for days while the host is low on disk", label)
	}
	return nil
}

// imagePrune is `box image prune`: remove the host's image blobs no
// image uses (host owner only).
func (s *session) imagePrune(ctx context.Context, o *opts) error {
	var res api.PruneImagesResult
	if err := s.cl.do(ctx, http.MethodPost, "/images/prune", nil, api.PruneImagesRequest{DryRun: o.box.dryRun}, nil, &res); err != nil {
		return err
	}
	s.out.result("", res)
	verb := "removed"
	if res.DryRun {
		verb = "would remove"
	}
	for _, b := range res.Removed {
		s.out.human("%s %.19s  %8s  unchanged since %s", verb, b.Digest, humanBytes(b.Bytes), b.ChangedAt.Local().Format(time.DateTime))
	}
	if len(res.Removed) == 0 {
		s.out.human("no unreferenced image blobs (%d in use or changed too recently to prove unused)", res.Kept)
		return nil
	}
	s.out.human("%s %d image blobs, %s; %d kept", verb, len(res.Removed), humanBytes(res.Bytes), res.Kept)
	return nil
}

// diskLine is the disk part of the `box ls` host line: the data disk
// ("hot" with a cold storage tier), then the cold tier.
func (h *boxHost) diskLine() string {
	if h.DiskFloorBytes == 0 && h.FreeDiskBytes == 0 {
		return ""
	}
	var b strings.Builder
	if h.Cold != nil {
		b.WriteString("hot ")
	}
	fmt.Fprintf(&b, "disk %s free", humanBytes(h.FreeDiskBytes))
	if h.TotalDiskBytes > 0 {
		fmt.Fprintf(&b, " of %s", humanBytes(h.TotalDiskBytes))
	}
	if h.DiskFloorBytes > 0 {
		fmt.Fprintf(&b, ", floor %s", humanBytes(h.DiskFloorBytes))
	}
	r := h.DiskReservedBytes
	if h.SaveReserveArmed {
		r += h.SaveReserveBytes
	}
	if r > 0 {
		fmt.Fprintf(&b, ", reserved %s", humanBytes(r))
	}
	if h.SaveReserveBytes > 0 && !h.SaveReserveArmed {
		b.WriteString(", save reserve in use")
	}
	switch {
	case h.DiskFloorBytes > 0 && h.FreeDiskBytes < h.DiskFloorBytes:
		fmt.Fprintf(&b, " (LOW: new boxes wait; unused boxes may be auto-deleted; `%s box gc`, `%s box rm`)", buildinfo.Name, buildinfo.Name)
	case h.DiskFloorBytes > 0 && h.FreeDiskBytes < 2*h.DiskFloorBytes:
		b.WriteString(" (getting low)")
	}
	if c := h.Cold; c != nil {
		b.WriteString("; " + coldLine(c))
	}
	return b.String()
}

// coldLine describes the cold storage tier.
func coldLine(c *api.ColdCapacity) string {
	var b strings.Builder
	if c.Unavailable != "" {
		fmt.Fprintf(&b, "cold storage UNAVAILABLE: %d boxes on it cannot resume (%s)", c.VMs, c.Unavailable)
		return b.String()
	}
	fmt.Fprintf(&b, "cold %s free", humanBytes(c.FreeBytes))
	if c.TotalBytes > 0 {
		fmt.Fprintf(&b, " of %s", humanBytes(c.TotalBytes))
	}
	if c.FloorBytes > 0 {
		fmt.Fprintf(&b, ", floor %s", humanBytes(c.FloorBytes))
	}
	fmt.Fprintf(&b, ", %d boxes (%s) moved after %s unused", c.VMs, humanBytes(c.Bytes), roundAge(time.Duration(c.AfterSeconds)*time.Second))
	if c.Draining {
		b.WriteString(" (draining: boxes move back, none go cold)")
	}
	if c.FloorBytes > 0 && c.FreeBytes < c.FloorBytes {
		b.WriteString(" (LOW: unused cold boxes may be auto-deleted)")
	}
	return b.String()
}

// coldCheck is doctor's view of the cold storage tier: its filesystem
// clones (XFS reflink=1 or btrfs, like the data dir) and is not the data
// dir's own, and its free space against its floor.
func coldCheck(caps api.HostCapabilities) (preflight.Check, bool) {
	if caps.Capacity == nil || caps.Capacity.Cold == nil {
		return preflight.Check{}, false
	}
	c := caps.Capacity.Cold
	ch := preflight.Check{ID: "cold_storage", Title: "Cold storage", Status: preflight.Pass, Detail: coldLine(c) + "; " + c.Detail}
	switch {
	case c.Unavailable != "":
		ch.Status, ch.Detail = preflight.Fail, coldLine(c)
		ch.Hint = "mount the cold disk (or restore coldStorage.dir) and restart the daemon (docs/storage-tiers.md#when-the-cold-disk-is-missing)"
	case c.SameFilesystem:
		ch.Status, ch.Hint = preflight.Warn, "coldStorage.dir is on the data dir's filesystem: moving boxes there frees nothing; point it at another disk (docs/storage-tiers.md)"
	case !c.FastClone:
		ch.Status, ch.Hint = preflight.Warn, "the cold filesystem cannot reflink: format it XFS with reflink=1 (mkfs.xfs -m reflink=1) or btrfs (docs/storage-tiers.md)"
	case c.FloorBytes > 0 && c.FreeBytes < c.FloorBytes:
		ch.Status, ch.Hint = preflight.Fail, "cold storage below its floor: unused cold boxes are being auto-deleted; `"+buildinfo.Name+" box rm` frees it, or give it a larger disk"
	case c.FloorBytes > 0 && c.FreeBytes < 2*c.FloorBytes:
		ch.Status, ch.Hint = preflight.Warn, "cold storage getting low: `"+buildinfo.Name+" box rm` boxes you no longer need"
	}
	return ch, true
}

// diskWaitLine describes a request queued for disk space.
func diskWaitLine(c *api.HostCapacity, w api.CapacityWaiter, ahead int) string {
	n := buildinfo.Name
	return fmt.Sprintf("waiting for host disk space (%d ahead): host disk low: %s free, need %s; `%s box gc` stops idle boxes, `%s box rm` deletes ones you no longer need",
		ahead, humanBytes(c.FreeDiskBytes), humanBytes(w.DiskBytes), n, n)
}

// diskCheck is doctor's view of the daemon's disk admission: free space
// against the floor and what is reserved, warning below twice the floor
// and naming boxes auto-deleted recently.
func (s *session) diskCheck(ctx context.Context, cl *client, caps api.HostCapabilities) (preflight.Check, bool) {
	c := caps.Capacity
	if c == nil || c.DiskFloorBytes == 0 {
		return preflight.Check{}, false
	}
	h := &boxHost{FreeDiskBytes: c.FreeDiskBytes, TotalDiskBytes: c.TotalDiskBytes, DiskFloorBytes: c.DiskFloorBytes,
		DiskReservedBytes: c.DiskReservedBytes, SaveReserveBytes: c.SaveReserveBytes, SaveReserveArmed: c.SaveReserveArmed}
	ch := preflight.Check{ID: "disk_admission", Title: "Disk admission", Status: preflight.Pass, Detail: strings.TrimPrefix(h.diskLine(), "disk ")}
	n := buildinfo.Name
	fix := "free space: `" + n + " box gc` stops idle boxes, `" + n + " box rm` deletes boxes, `" + n + " box image prune` removes unused image blobs"
	switch {
	case c.FreeDiskBytes < c.DiskFloorBytes:
		ch.Status, ch.Hint = preflight.Fail, fix
	case c.FreeDiskBytes < 2*c.DiskFloorBytes:
		ch.Status, ch.Hint = preflight.Warn, fix
	}
	if c.SaveReserveBytes > 0 && !c.SaveReserveArmed {
		if ch.Status == preflight.Pass {
			ch.Status, ch.Hint = preflight.Warn, fix
		}
	}
	if gone := s.autoDeleted(ctx, cl, ""); len(gone) > 0 {
		var names []string
		for _, d := range gone {
			names = append(names, autoDeletedLine(d))
		}
		ch.Detail += "; auto-deleted for disk space: " + strings.Join(names, "; ")
		if ch.Status == preflight.Pass {
			ch.Status, ch.Hint = preflight.Warn, "`"+n+" box ls --deleted` says why; `"+n+" box pin` keeps a box"
		}
	}
	return ch, true
}
