package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"davit/api"
	"davit/internal/domain"
)

// Host capacity for boxes (docs/project-boxes.md#when-the-host-is-full).
// The host stops idle boxes to make room on its own; a box create or
// resume also waits in the host's FIFO queue for up to --wait (default
// DefaultBoxWait), and says so on stderr. `box ls` shows the host's memory
// and reservation; `box gc` stops idle boxes or removes old stopped ones.

// DefaultBoxWait is how long a box create/resume queues for host capacity.
const DefaultBoxWait = 3 * time.Minute

// boxWait is the capacity wait of box create/resume.
func (o *opts) boxWait() time.Duration {
	if o.box.wait.set {
		return o.box.wait.d
	}
	return DefaultBoxWait
}

// capacityMutate is cl.mutate for createVM/resumeVM with a capacity wait:
// while the request is queued, it reports its place on stderr.
func (s *session) capacityMutate(ctx context.Context, path string, body any, wait time.Duration) (*api.OperationAccepted, error) {
	key := newIdempotencyKey()
	var q url.Values
	if wait > 0 {
		q = url.Values{"waitSeconds": {strconv.FormatInt(int64((wait+time.Second-1)/time.Second), 10)}}
		stop := s.reportQueue(ctx, domain.CapacityTicket(key))
		defer stop()
	}
	var out api.OperationAccepted
	err := s.cl.do(ctx, http.MethodPost, path, q, body, map[string]string{api.HeaderIdempotencyKey: key}, &out)
	return &out, err
}

// reportQueue polls the host's capacity report until stop is called and
// prints this request's place in the queue whenever it changes.
func (s *session) reportQueue(ctx context.Context, ticket string) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		last := ""
		t := time.NewTimer(time.Second) // an admission with room answers sooner
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			var caps api.HostCapabilities
			if err := s.cl.get(ctx, "/capabilities", nil, &caps); err == nil && caps.Capacity != nil {
				if msg := queueLine(caps.Capacity, ticket); msg != "" && msg != last {
					s.out.diag("%s", msg)
					last = msg
				}
			}
			t.Reset(2 * time.Second)
		}
	}()
	return func() { cancel(); wg.Wait() }
}

// queueLine describes ticket's place in c's queue ("" if it is not there).
func queueLine(c *api.HostCapacity, ticket string) string {
	i := slices.IndexFunc(c.Waiting, func(w api.CapacityWaiter) bool { return w.Ticket == ticket })
	if i < 0 {
		return ""
	}
	return fmt.Sprintf("waiting for host capacity (%d ahead, %s needed, %s free)", i, humanBytes(c.Waiting[i].MemoryBytes), humanBytes(c.FreeMemoryBytes))
}

// boxHost is the host summary of `box ls`.
type boxHost struct {
	TotalMemoryBytes    int64 `json:"totalMemoryBytes,omitempty"`
	FreeMemoryBytes     int64 `json:"freeMemoryBytes"`
	UsedMemoryBytes     int64 `json:"usedMemoryBytes,omitempty"`
	ReservedMemoryBytes int64 `json:"reservedMemoryBytes"` // declared memory of live VMs
	MemoryBudgetBytes   int64 `json:"memoryBudgetBytes,omitempty"`
	LiveVMs             int   `json:"liveVms"`
	RunningBoxes        int   `json:"runningBoxes"` // of the boxes listed
	Reclaimable         int   `json:"reclaimable"`
	Waiting             int   `json:"waiting"`
}

// hostSummary reads the capacity report for `box ls` (nil when the host
// does not report capacity).
func (s *session) hostSummary(ctx context.Context, rows []boxRow) (*boxHost, error) {
	var caps api.HostCapabilities
	if err := s.cl.get(ctx, "/capabilities", nil, &caps); err != nil {
		return nil, err
	}
	c := caps.Capacity
	if c == nil {
		return nil, nil
	}
	h := &boxHost{TotalMemoryBytes: c.TotalMemoryBytes, FreeMemoryBytes: c.FreeMemoryBytes, ReservedMemoryBytes: c.Allocated.MemoryBytes,
		MemoryBudgetBytes: c.MemoryBudgetBytes, LiveVMs: c.LiveVMs, Reclaimable: c.Reclaimable, Waiting: len(c.Waiting)}
	if c.TotalMemoryBytes > 0 {
		h.UsedMemoryBytes = max(0, c.TotalMemoryBytes-c.FreeMemoryBytes)
	}
	for _, r := range rows {
		if r.State == string(domain.StateReady) || r.State == string(domain.StateRunning) {
			h.RunningBoxes++
		}
	}
	return h, nil
}

// line is the human header of `box ls`.
func (h *boxHost) line() string {
	var b strings.Builder
	b.WriteString("host: memory ")
	if h.TotalMemoryBytes > 0 {
		fmt.Fprintf(&b, "%s used, %s free of %s", humanBytes(h.UsedMemoryBytes), humanBytes(h.FreeMemoryBytes), humanBytes(h.TotalMemoryBytes))
	} else {
		fmt.Fprintf(&b, "%s free", humanBytes(h.FreeMemoryBytes))
	}
	fmt.Fprintf(&b, "; reserved %s", humanBytes(h.ReservedMemoryBytes))
	if h.MemoryBudgetBytes > 0 {
		fmt.Fprintf(&b, " of %s", humanBytes(h.MemoryBudgetBytes))
	}
	fmt.Fprintf(&b, "; %d boxes running (%d VMs live)", h.RunningBoxes, h.LiveVMs)
	if h.Reclaimable > 0 {
		fmt.Fprintf(&b, "; %d idle enough to stop for room", h.Reclaimable)
	}
	if h.Waiting > 0 {
		fmt.Fprintf(&b, "; %d waiting for capacity", h.Waiting)
	}
	return b.String()
}

// gcResult is `box gc`'s JSON result.
type gcResult struct {
	DryRun  bool              `json:"dryRun,omitempty"`
	Stopped []api.ReclaimedVM `json:"stopped"`
	Removed []gcRemoved       `json:"removed"`
}

type gcRemoved struct {
	Box         string             `json:"box"`
	VMID        string             `json:"vmId"`
	StoppedFor  int64              `json:"stoppedSeconds"`
	OperationID domain.OperationID `json:"operationId,omitempty"`
	MemoryBytes int64              `json:"memoryBytes"`
	DiskBytes   int64              `json:"diskBytes"`
}

// boxGC stops the caller's boxes idle at least --idle (default 10m when
// --rm-older-than is not given), and with --rm-older-than deletes the
// instances stopped at least that long (never a project image). project
// limits it to one project.
func (s *session) boxGC(ctx context.Context, o *opts, project string) error {
	idle, rmAfter := o.box.idle, o.box.rmOlder
	if !idle.set && !rmAfter.set {
		idle = optDuration{set: true, d: 10 * time.Minute}
	}
	rows, err := s.boxRows(ctx, project)
	if err != nil {
		return err
	}
	res := gcResult{DryRun: o.box.dryRun, Stopped: []api.ReclaimedVM{}, Removed: []gcRemoved{}}
	if idle.set {
		projects := map[string]bool{}
		for _, r := range rows {
			projects[r.Project] = true
		}
		names := make([]string, 0, len(projects))
		for p := range projects {
			names = append(names, p)
		}
		slices.Sort(names)
		for _, p := range names {
			var out api.ReclaimVMsResult
			body := api.ReclaimVMsRequest{IdleSeconds: int64(idle.d / time.Second), Labels: map[string]string{domain.LabelBox: p}, DryRun: o.box.dryRun}
			if err := s.cl.do(ctx, http.MethodPost, "/vms/reclaim", nil, body, nil, &out); err != nil {
				return err
			}
			res.Stopped = append(res.Stopped, out.Items...)
		}
		for _, it := range res.Stopped {
			if it.OperationID == "" || o.noWait {
				continue
			}
			op := api.Operation{ID: it.OperationID}
			if err := s.await(ctx, o, &op); err != nil {
				return err
			}
		}
	}
	if rmAfter.set {
		for _, r := range rows {
			stoppedFor := time.Since(r.UpdatedAt)
			if r.Kind != "instance" || r.State != string(domain.StateStopped) || stoppedFor < rmAfter.d {
				continue
			}
			rm := gcRemoved{Box: r.Project + "/" + r.Instance, VMID: r.VMID, StoppedFor: int64(stoppedFor / time.Second), MemoryBytes: r.MemoryBytes, DiskBytes: r.DiskBytes}
			if !o.box.dryRun {
				acc, err := s.cl.mutate(ctx, http.MethodDelete, vmPath(r.VMID), nil, newIdempotencyKey())
				if err != nil {
					return err
				}
				rm.OperationID = acc.Operation.ID
				if !o.noWait {
					if err := s.await(ctx, o, &acc.Operation); err != nil {
						return err
					}
				}
			}
			res.Removed = append(res.Removed, rm)
		}
	}
	s.out.result("", res)
	verb, rmVerb := "stopped", "removed"
	if o.box.dryRun {
		verb, rmVerb = "would stop", "would remove"
	}
	for _, it := range res.Stopped {
		s.out.human("%s %-40s idle %s, %s", verb, boxLabel(rows, it.Name), (time.Duration(it.IdleSeconds) * time.Second).String(), humanBytes(it.MemoryBytes))
	}
	for _, rm := range res.Removed {
		s.out.human("%s %-38s stopped %s, disk %s", rmVerb, rm.Box, (time.Duration(rm.StoppedFor) * time.Second).String(), humanBytes(rm.DiskBytes))
	}
	if len(res.Stopped)+len(res.Removed) == 0 {
		s.out.human("nothing to collect (boxes with a running process, an open session or recent activity are never stopped)")
	}
	return nil
}

// boxLabel is PROJECT/INSTANCE for VM name, or the name.
func boxLabel(rows []boxRow, name string) string {
	for _, r := range rows {
		if r.VM == name {
			return r.Project + "/" + r.Instance
		}
	}
	return name
}

// parseAge parses a duration that may also be in days ("7d", "1d12h").
func parseAge(v string) (time.Duration, error) {
	var days time.Duration
	if i := strings.IndexByte(v, 'd'); i >= 0 {
		n, err := strconv.Atoi(v[:i])
		if err != nil || n < 0 {
			return 0, fmt.Errorf("want a duration such as 7d or 12h, got %q", v)
		}
		days, v = time.Duration(n)*24*time.Hour, v[i+1:]
	}
	if v == "" {
		return days, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("want a duration such as 7d or 12h, got %q", v)
	}
	return days + d, nil
}

// ageFlag is an optDuration that also takes days.
type ageFlag struct{ *optDuration }

// String is nil-safe: flag.PrintDefaults calls it on a zero ageFlag.
func (f ageFlag) String() string {
	if f.optDuration == nil {
		return "0s"
	}
	return f.optDuration.String()
}

func (f ageFlag) Set(v string) error {
	d, err := parseAge(v)
	if err != nil {
		return err
	}
	f.set, f.d = true, d
	return nil
}
