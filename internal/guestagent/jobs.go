//go:build linux

package guestagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"davit/internal/guestproto"
)

// Managed-job handles (feature jobs). An Exec with a Handle is recorded in
// the handle index before it starts; a second Exec with that handle waits
// for the first to resolve and is answered with the same ProcessStarted, so
// the command never runs twice. A start that fails releases the handle
// (nothing ran, so a retry may run it).
//
// The index outlives the process table's bounded retention: an evicted
// process keeps a compact summary (ID, PID, exit) under its handle. Each
// started handle is also appended to RunDir/jobs.jsonl (tmpfs, so per
// boot), and a restarted agent loads it: those processes are not its
// children any more, so lookups report them lost, and an Exec with their
// handle is answered with the old ProcessStarted rather than run again.

const (
	jobsFile = "jobs.jsonl"
	// maxHandles bounds the handle index; beyond it the oldest finished jobs
	// are forgotten (a later Exec with such a handle would run again).
	maxHandles = 16384
)

type job struct {
	handle string
	// ready is closed once the start resolved; failed then says nothing ran.
	ready  chan struct{}
	failed bool

	id   string // set (under procTable.mu) before the start
	pid  int
	proc *proc // nil once evicted from the process table or from an earlier agent
	// final is the evicted process's outcome; nil with proc nil means the
	// process belonged to an earlier agent process (outcome lost).
	final  *guestproto.ProcessExited
	outSeq uint64
}

// jobLine is one jobs.jsonl record. A handle is journaled before its
// process starts (PID 0) and again with its PID once it runs; Failed
// releases it (the start failed, nothing ran). The last line per handle
// wins.
type jobLine struct {
	Handle    string `json:"handle"`
	ProcessID string `json:"processId,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
}

// reserveHandle returns the job for handle and whether this caller owns its
// start (true) or must wait for another's (false).
func (t *procTable) reserveHandle(handle string) (*job, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if j := t.handles[handle]; j != nil {
		return j, false
	}
	j := &job{handle: handle, ready: make(chan struct{})}
	t.handles[handle] = j
	t.handleOrder = append(t.handleOrder, handle)
	t.trimHandles()
	return j, true
}

// resolveHandle settles a reserved start: p is nil when it failed.
func (t *procTable) resolveHandle(j *job, p *proc) {
	t.mu.Lock()
	if p == nil {
		j.failed = true
		t.dropHandle(j)
	} else {
		j.pid, j.proc = p.pid, p
	}
	t.mu.Unlock()
	close(j.ready)
}

// dropHandle forgets j. Called with t.mu held.
func (t *procTable) dropHandle(j *job) {
	if t.handles[j.handle] != j {
		return
	}
	delete(t.handles, j.handle)
	for i := len(t.handleOrder) - 1; i >= 0; i-- { // usually among the newest
		if t.handleOrder[i] == j.handle {
			t.handleOrder = append(t.handleOrder[:i], t.handleOrder[i+1:]...)
			break
		}
	}
}

// lookupHandle returns the status of the started job carrying handle.
func (t *procTable) lookupHandle(handle string) (guestproto.ProcessStatus, bool) {
	t.mu.Lock()
	j := t.handles[handle]
	t.mu.Unlock()
	if j == nil {
		return guestproto.ProcessStatus{}, false
	}
	<-j.ready // starts resolve promptly (fork/exec)
	if j.failed {
		return guestproto.ProcessStatus{}, false
	}
	return t.jobStatus(j), true
}

// jobView reads j's process (nil once evicted) under the table lock, which
// eviction holds while it replaces the process with its summary.
func (t *procTable) jobView(j *job) *proc {
	t.mu.Lock()
	defer t.mu.Unlock()
	return j.proc
}

// jobStatus reports j as ProcessStatus (lock order: t.mu, then proc.mu).
func (t *procTable) jobStatus(j *job) guestproto.ProcessStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	if j.proc != nil {
		return j.proc.status()
	}
	st := guestproto.ProcessStatus{ProcessID: j.id, Status: guestproto.StatusLost, Handle: j.handle, OutputSeq: j.outSeq}
	if f := j.final; f != nil {
		st.Status, st.ExitCode, st.Signal, st.TimedOut = f.Status, f.ExitCode, f.Signal, f.TimedOut
	}
	return st
}

// trimHandles forgets the oldest finished jobs beyond maxHandles; jobs of an
// earlier agent process (outcome unknown, maybe still running) are kept.
// Called with t.mu held.
func (t *procTable) trimHandles() {
	for i := 0; len(t.handles) > maxHandles && i < len(t.handleOrder); {
		h := t.handleOrder[i]
		j := t.handles[h]
		if j == nil {
			t.handleOrder = append(t.handleOrder[:i], t.handleOrder[i+1:]...)
			continue
		}
		select {
		case <-j.ready:
		default:
			i++
			continue // still starting
		}
		if j.proc != nil && j.proc.exitedNow() == nil || j.proc == nil && j.final == nil {
			i++
			continue // still running, or an earlier agent's
		}
		delete(t.handles, h)
		t.handleOrder = append(t.handleOrder[:i], t.handleOrder[i+1:]...)
	}
}

// evicted keeps a compact summary of p under its handle once the process
// table forgets p. Called with t.mu held.
func (t *procTable) evicted(p *proc) {
	if p.handle == "" {
		return
	}
	if j := t.handles[p.handle]; j != nil && j.proc == p {
		j.final, j.outSeq = p.exitedNow(), p.outSeqNow()
		j.proc = nil
	}
}

// journalJob appends l to the boot's jobs file.
func (a *Agent) journalJob(l jobLine) error {
	raw, _ := json.Marshal(l)
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	if err := os.MkdirAll(a.cfg.RunDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(a.cfg.RunDir, jobsFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(raw, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if a.jobLines++; a.jobLines > 4*maxHandles {
		a.compactJobs()
	}
	return nil
}

// compactJobs rewrites the jobs file with the handles still indexed. Called
// with jobsMu held.
func (a *Agent) compactJobs() {
	a.procs.mu.Lock()
	var buf bytes.Buffer
	n := 0
	for _, h := range a.procs.handleOrder {
		j := a.procs.handles[h]
		if j == nil {
			continue
		}
		// A start in flight is journaled as starting (its PID line follows).
		raw, _ := json.Marshal(jobLine{Handle: h, ProcessID: j.id, PID: j.pid})
		buf.Write(append(raw, '\n'))
		n++
	}
	a.procs.mu.Unlock()
	if err := writeAtomic(filepath.Join(a.cfg.RunDir, jobsFile), buf.Bytes(), 0o600); err != nil {
		a.cfg.Log.Warn("compact job handles", "err", err)
		return
	}
	a.jobLines = n
}

// loadJobs indexes the handles an earlier agent process journaled this
// boot; their outcome is lost to this one. A handle journaled as starting
// (the agent died around the start) counts as started: it may have run.
func (a *Agent) loadJobs() error {
	f, err := os.Open(filepath.Join(a.cfg.RunDir, jobsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	last := map[string]jobLine{}
	var order []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l jobLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Handle == "" {
			continue // a torn last line
		}
		a.jobLines++
		if _, seen := last[l.Handle]; !seen {
			order = append(order, l.Handle)
		}
		last[l.Handle] = l
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for _, h := range order {
		l := last[h]
		if l.Failed || l.ProcessID == "" {
			continue
		}
		if j, own := a.procs.reserveHandle(h); own {
			j.id, j.pid = l.ProcessID, l.PID
			close(j.ready)
		}
	}
	return nil
}
