package cli

import (
	"context"
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"davit/api"
	"davit/internal/buildinfo"
)

// `davit history` and `davit ps` (G7).

// runHistory lists a VM's managed jobs across generations, newest first;
// with a process ID it prints that job's host-retained output, which stays
// readable while the VM is stopped.
func runHistory(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) > 2 {
		return usageErr("usage: %s history [ID] [PROCESS_ID] [--stream stdout|stderr]", buildinfo.Name)
	}
	var vm, pid string
	if len(args) == 2 {
		vm, pid = args[0], args[1]
	} else if len(args) == 1 && strings.HasPrefix(args[0], "proc_") {
		pid = args[0]
	}
	if vm == "" {
		var err error
		rest := args
		if pid != "" {
			rest = nil
		}
		if vm, err = s.currentID(rest); err != nil {
			return err
		}
	}
	if pid != "" {
		return s.printOutput(ctx, o, vm, pid)
	}
	q := url.Values{}
	if o.limit != 0 {
		q.Set("limit", strconv.Itoa(o.limit))
	}
	if o.cursor != "" {
		q.Set("cursor", o.cursor)
	}
	var h api.HistoryList
	if err := s.cl.get(ctx, vmPath(vm)+"/history", q, &h); err != nil {
		return err
	}
	for _, e := range h.Items {
		s.out.emit("item", "", 0, e, nil)
		status := string(e.Process.Status)
		if e.Process.ExitCode != nil {
			status += " " + strconv.Itoa(*e.Process.ExitCode)
		} else if e.Process.Signal != "" {
			status += " " + e.Process.Signal
		}
		s.out.human("%-26s gen %-3d %-14s %s  %8s  %s", e.Process.ID, e.Process.Generation, status,
			e.Process.CreatedAt.Local().Format(time.DateTime), historyOutput(e), strings.Join(e.Process.Argv, " "))
	}
	s.out.result("", map[string]any{"count": len(h.Items), "nextCursor": h.NextCursor})
	return nil
}

// printOutput pages through a job's retained output.
func (s *session) printOutput(ctx context.Context, o *opts, vm, pid string) error {
	q := url.Values{}
	if o.stream != "" {
		q.Set("stream", o.stream)
	}
	chunks := 0
	for {
		var page api.ProcessOutput
		if err := s.cl.get(ctx, vmPath(vm)+"/processes/"+url.PathEscape(pid)+"/output", q, &page); err != nil {
			return err
		}
		for _, m := range page.Items {
			b, err := base64.StdEncoding.DecodeString(m.Data)
			if err != nil {
				return err
			}
			if s.g.json {
				chunk := map[string]string{"processId": pid}
				if utf8.Valid(b) {
					chunk["text"] = string(b)
				} else {
					chunk["base64"] = m.Data
				}
				s.out.emit(m.Type, "", m.Seq, chunk, nil)
			} else if m.Type == api.StreamStderr {
				s.io.Stderr.Write(b)
			} else {
				s.io.Stdout.Write(b)
			}
			chunks++
		}
		if page.NextCursor == "" || len(page.Items) == 0 && page.Complete {
			s.out.result("", map[string]any{"processId": pid, "chunks": chunks, "complete": page.Complete})
			return nil
		}
		q.Set("after", page.NextCursor)
	}
}

// runPS samples a VM's guest processes (a stopped VM: its last sample).
func runPS(ctx context.Context, s *session, o *opts, args []string) error {
	vm, err := s.currentID(args)
	if err == nil {
		err = maxArgs(args, 1, "ps [ID]")
	}
	if err != nil {
		return err
	}
	var l api.GuestProcessList
	if err := s.cl.get(ctx, vmPath(vm)+"/ps", nil, &l); err != nil {
		return err
	}
	when := l.SampledAt.Local().Format(time.DateTime)
	if l.Stale {
		when += " (VM not running: last sample)"
	}
	s.out.human("sampled %s", when)
	s.out.human("%7s %7s %-10s %5s %9s %10s  %s", "PID", "PPID", "USER", "STATE", "RSS", "CPU", "COMMAND")
	for _, p := range l.Items {
		s.out.emit("item", "", 0, p, nil)
		cmd := strings.Join(p.Args, " ")
		if p.ProcessID != "" {
			cmd += "  [" + p.ProcessID + "]"
		}
		s.out.human("%7d %7d %-10s %5s %9s %9.1fs  %s", p.PID, p.PPID, p.User, p.State, bytesHuman(p.RSSBytes), float64(p.CPUTimeMillis)/1000, cmd)
	}
	s.out.result("", map[string]any{"count": len(l.Items), "sampledAt": l.SampledAt, "stale": l.Stale, "truncated": l.Truncated})
	return nil
}

// historyOutput is the output column of a history line: the retained size,
// or why the output was not kept (a privacy policy is not truncation).
func historyOutput(e api.HistoryEntry) string {
	n := e.StdoutBytes + e.StderrBytes
	out := bytesHuman(n)
	switch {
	case e.OutputNotRetained != "" && n == 0:
		out = "not retained (" + string(e.OutputNotRetained) + ")"
	case e.OutputNotRetained != "":
		out += " (partly not retained: " + string(e.OutputNotRetained) + ")"
	}
	if e.OutputDropped {
		out += " (oldest dropped)"
	}
	return out
}
