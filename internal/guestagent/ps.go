//go:build linux

package guestagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"davit/internal/guestproto"
)

// Process samples (feature ps) read /proc (Config.ProcRoot). A sample is
// bounded twice: at most guestproto.MaxProcessSamples entries and at most
// maxSampleBytes of encoded entries, so the answer always fits one frame;
// Truncated says either bound cut it. Each process's args are cut to
// maxSampleArgs entries and maxSampleArgBytes bytes.

const (
	maxSampleBytes    = 768 << 10
	maxSampleArgs     = 64
	maxSampleArgBytes = 1024
	// clockTicks is USER_HZ, the unit of /proc/<pid>/stat times; Linux
	// fixes it at 100 for user space on every architecture.
	clockTicks = 100
)

// processList answers ProcessListRequest.
func (s *session) processList(id uint64) {
	handles := s.a.procs.handlesByPID()
	l, err := sampleProcesses(s.a.cfg.ProcRoot, handles, guestproto.MaxProcessSamples, maxSampleBytes)
	if err != nil {
		s.fail(id, internal("sample processes", err))
		return
	}
	s.send(id, l)
}

// handlesByPID maps each running managed job's PID to its handle.
func (t *procTable) handlesByPID() map[int]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[int]string{}
	for _, j := range t.handles {
		if j.proc != nil && j.proc.exitedNow() == nil {
			out[j.pid] = j.handle
		}
	}
	return out
}

// sampleProcesses reads every numeric entry of procRoot, in PID order.
func sampleProcesses(procRoot string, handles map[int]string, maxN, maxBytes int) (guestproto.ProcessList, error) {
	out := guestproto.ProcessList{SampledAtUnixNano: time.Now().UnixNano(), Processes: []guestproto.GuestProcess{}}
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return out, err
	}
	var pids []int
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids) // a truncated sample keeps the oldest PIDs (init first)
	bootTime := readBootTime(procRoot)
	page := int64(os.Getpagesize())
	users := map[string]string{}
	size := 0
	for _, pid := range pids {
		g, ok := readProcess(filepath.Join(procRoot, strconv.Itoa(pid)), pid, bootTime, page, users)
		if !ok {
			continue // exited meanwhile, or unreadable
		}
		g.Handle = handles[pid]
		raw, _ := json.Marshal(g)
		if len(out.Processes) >= maxN || size+len(raw) > maxBytes {
			out.Truncated = true
			break
		}
		size += len(raw) + 1
		out.Processes = append(out.Processes, g)
	}
	return out, nil
}

// readProcess parses one /proc/<pid> directory.
func readProcess(dir string, pid int, bootTime int64, page int64, users map[string]string) (guestproto.GuestProcess, bool) {
	g := guestproto.GuestProcess{PID: pid}
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return g, false
	}
	// comm may hold spaces and parentheses: fields resume after the last ')'.
	s := string(stat)
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return g, false
	}
	comm := s[open+1 : end]
	f := strings.Fields(s[end+1:])
	// f[0] state, f[1] ppid, f[11] utime, f[12] stime, f[19] starttime, f[21] rss.
	if len(f) < 22 {
		return g, false
	}
	g.State = f[0]
	g.PPID, _ = strconv.Atoi(f[1])
	utime, _ := strconv.ParseInt(f[11], 10, 64)
	stime, _ := strconv.ParseInt(f[12], 10, 64)
	g.CPUTimeMillis = (utime + stime) * 1000 / clockTicks
	if start, err := strconv.ParseInt(f[19], 10, 64); err == nil && bootTime > 0 {
		g.StartedAtUnixNano = bootTime*int64(time.Second) + start*int64(time.Second)/clockTicks
	}
	if rss, err := strconv.ParseInt(f[21], 10, 64); err == nil && rss > 0 {
		g.RSSBytes = rss * page
	}
	g.Args = readArgs(filepath.Join(dir, "cmdline"))
	if len(g.Args) == 0 && comm != "" {
		g.Args = []string{"[" + comm + "]"} // kernel thread or zombie
	}
	g.User = processUser(filepath.Join(dir, "status"), users)
	return g, true
}

// readArgs returns the NUL-separated cmdline, bounded.
func readArgs(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, maxSampleArgBytes)
	n, _ := f.Read(buf)
	raw := bytes.TrimRight(buf[:n], "\x00")
	if len(raw) == 0 {
		return nil
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) > maxSampleArgs {
		parts = parts[:maxSampleArgs]
	}
	return parts
}

// processUser returns the name of the process's real uid (or the uid).
func processUser(path string, cache map[string]string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "Uid:")
		if !ok {
			continue
		}
		fs := strings.Fields(rest)
		if len(fs) == 0 {
			return ""
		}
		uid := fs[0]
		if name, ok := cache[uid]; ok {
			return name
		}
		name := uid
		if u, err := user.LookupId(uid); err == nil {
			name = u.Username
		}
		cache[uid] = name
		return name
	}
	return ""
}

// readBootTime returns btime from <procRoot>/stat (unix seconds), or 0.
func readBootTime(procRoot string) int64 {
	f, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}
