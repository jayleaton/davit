//go:build linux

package guestagent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"davit/internal/guestproto"
)

// Processes. Output is a per-process log of stream frames (Stdout, Stderr,
// then ProcessExited) with one seq space. Each session following a process
// (the Exec stream or a ProcessAttach stream) is a subscriber with its own
// cursor. The log keeps at most RetainOutput bytes: with a live subscriber
// the reader pumps block until it catches up (backpressure reaches the
// process through its pipes); with none the oldest frames are evicted, so
// a later attach from an evicted cursor fails rather than silently skipping.
//
// Non-detached processes belong to the session that started them and are
// killed (whole process group) when it closes. Detached processes survive
// sessions. After a process exits its pipes are drained for up to
// drainGrace; a background child still holding them open does not hold the
// exit report hostage.
//
// Signals go to the whole process group, by its ID (the leader's PID). That
// ID must never reach a stranger: once the leader is reaped and the group is
// empty, the kernel may hand the number to a new process, which may lead a
// group of its own. So the exited leader stays an unreaped zombie, which
// keeps its PID and process-group ID reserved, for as long as anything else
// is in its group; its exit is reported at once (waitid WNOWAIT). The leader
// is reaped only when it is alone, and from then on kill does nothing.

const (
	readChunk     = 32 << 10
	drainGrace    = 250 * time.Millisecond
	maxExitedKept = 256
	defaultPath   = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

type procTable struct {
	mu     sync.Mutex
	byID   map[string]*proc
	exited []string // FIFO of exited IDs for bounded retention
	// handles indexes managed jobs by Exec.Handle (see jobs.go);
	// handleOrder is its insertion order, for bounded retention.
	handles     map[string]*job
	handleOrder []string
}

func (t *procTable) init() { t.byID, t.handles = map[string]*proc{}, map[string]*job{} }

func (t *procTable) add(p *proc) {
	t.mu.Lock()
	t.byID[p.id] = p
	t.mu.Unlock()
}

func (t *procTable) get(id string) *proc {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.byID[id]
}

func (t *procTable) markExited(p *proc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.exited = append(t.exited, p.id)
	for len(t.exited) > maxExitedKept {
		if q := t.byID[t.exited[0]]; q != nil {
			t.evicted(q)
		}
		delete(t.byID, t.exited[0])
		t.exited = t.exited[1:]
	}
}

// sessionClosed kills the closing session's non-detached processes.
func (t *procTable) sessionClosed(s *session) {
	t.mu.Lock()
	var kill []*proc
	for _, p := range t.byID {
		if p.owner == s {
			kill = append(kill, p)
		}
	}
	t.mu.Unlock()
	for _, p := range kill {
		p.kill(unix.SIGKILL)
	}
}

type proc struct {
	id     string
	pid    int
	handle string   // Exec.Handle, if any
	owner  *session // nil when detached
	limit  int
	stdinQ chan stdinItem
	stdinD chan struct{} // closed when the stdin writer stops

	mu     sync.Mutex
	cond   *sync.Cond
	log    []*guestproto.Frame // frames with Seq set; log[0].Seq == first
	first  uint64
	bytes  int
	seq    uint64
	outSeq uint64 // seq of the last stdout/stderr chunk
	subs   map[*subscriber]struct{}
	exited *guestproto.ProcessExited
	// exitedCh is closed once the exit is recorded.
	exitedCh chan struct{}
	timeout  bool
	// reaped: the leader was reaped, so its PID may name a stranger now.
	reaped bool
}

type stdinItem struct {
	data  []byte
	close bool
}

type subscriber struct {
	cursor uint64 // last seq delivered
	done   bool
}

func newProcID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "proc_" + hex.EncodeToString(b[:])
}

// exec starts req and answers ProcessStarted; output follows on stream id.
func (s *session) exec(id uint64, req guestproto.Exec) {
	if req.TTY {
		s.fail(id, guestproto.Errorf(guestproto.CodeUnsupported, "tty exec is not supported by this agent; use ssh"))
		return
	}
	var j *job
	if req.Handle != "" {
		var own bool
		for {
			if j, own = s.a.procs.reserveHandle(req.Handle); own {
				break
			}
			select {
			case <-j.ready:
			case <-s.ctx.Done():
				return
			}
			if !j.failed {
				s.execAgain(id, j, req)
				return
			}
		}
	}
	p, l, err := s.start(req, j)
	if j != nil {
		s.a.procs.resolveHandle(j, p)
	}
	if err != nil {
		s.fail(id, err)
		return
	}
	s.started(id, p, l, req)
}

// execAgain answers an Exec whose handle names a process already started
// this boot with that process's ProcessStarted, without running anything.
// Unless NoFollow, the stream replays the retained output from the start
// (or the oldest retained frame) and follows it. Stdin stays with the
// session that started the process.
func (s *session) execAgain(id uint64, j *job, req guestproto.Exec) {
	p := s.a.procs.jobView(j)
	var sub *subscriber
	if p != nil && !req.NoFollow {
		p.mu.Lock()
		after := uint64(0)
		if p.first > 1 {
			after = p.first - 1
		}
		sub = &subscriber{cursor: after}
		p.subs[sub] = struct{}{}
		p.mu.Unlock()
	}
	s.send(id, guestproto.ProcessStarted{ProcessID: j.id, PID: j.pid, Handle: j.handle})
	switch {
	case sub != nil:
		go p.follow(s, id, sub)
	case !req.NoFollow:
		// Forgotten or from an earlier agent: only the outcome is known.
		st := s.a.procs.jobStatus(j)
		f, _ := guestproto.EncodeStream(id, 1, guestproto.ProcessExited{Status: st.Status, ExitCode: st.ExitCode, Signal: st.Signal, TimedOut: st.TimedOut})
		s.sendStream(f)
	}
}

// launch is a started command's parent-side ends, handed from start to
// started.
type launch struct {
	cmd        *exec.Cmd
	outR, errR *os.File
	stdinW     io.WriteCloser
}

// start launches req; on failure nothing ran. With a job it journals the
// handle first (write-ahead: an agent that dies around the start must not
// let a successor run it again) and refuses to start if it cannot.
func (s *session) start(req guestproto.Exec, j *job) (p *proc, l launch, err error) {
	cmd, err := s.a.command(req)
	if err != nil {
		return nil, launch{}, err
	}
	p = &proc{id: newProcID(), handle: req.Handle, limit: s.a.cfg.RetainOutput, subs: map[*subscriber]struct{}{}, exitedCh: make(chan struct{})}
	if j != nil {
		s.a.procs.mu.Lock()
		j.id = p.id // journal compaction may run before the start resolves
		s.a.procs.mu.Unlock()
		if err := s.a.journalJob(jobLine{Handle: j.handle, ProcessID: p.id}); err != nil {
			return nil, launch{}, internal("journal job handle", err)
		}
		defer func() {
			jl := jobLine{Handle: j.handle, Failed: true}
			if err == nil {
				jl = jobLine{Handle: j.handle, ProcessID: p.id, PID: p.pid}
			}
			if jerr := s.a.journalJob(jl); jerr != nil {
				s.a.cfg.Log.Warn("journal job handle", "process", p.id, "err", jerr)
			}
		}()
	}
	p.cond = sync.NewCond(&p.mu)
	if !req.Detached {
		p.owner = s
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, launch{}, internal("pipe", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, launch{}, internal("pipe", err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	var stdinW io.WriteCloser
	if req.Stdin {
		r, w, err := os.Pipe()
		if err != nil {
			outR.Close()
			outW.Close()
			errR.Close()
			errW.Close()
			return nil, launch{}, internal("pipe", err)
		}
		cmd.Stdin, stdinW = r, w
		defer r.Close()
	}
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		if stdinW != nil {
			stdinW.Close()
		}
		return nil, launch{}, guestproto.Errorf(guestproto.CodeNotFound, "start %s: %v", req.Argv[0], unwrapExecErr(err))
	}
	p.pid = cmd.Process.Pid
	s.a.procs.add(p)
	return p, launch{cmd: cmd, outR: outR, errR: errR, stdinW: stdinW}, nil
}

// started answers ProcessStarted for a process start launched and begins
// streaming and supervising it.
func (s *session) started(id uint64, p *proc, l launch, req guestproto.Exec) {
	cmd, outR, errR, stdinW := l.cmd, l.outR, l.errR, l.stdinW

	// Subscribe before ProcessStarted is sent so no output can precede it.
	var sub *subscriber
	if !req.NoFollow {
		sub = p.subscribe(0)
	}
	if stdinW != nil {
		p.stdinQ, p.stdinD = make(chan stdinItem, 256), make(chan struct{})
		s.mu.Lock()
		s.stdin[id] = p
		s.mu.Unlock()
		go p.stdinLoop(stdinW, p.exitedCh)
	}
	s.send(id, guestproto.ProcessStarted{ProcessID: p.id, PID: p.pid, Handle: p.handle})
	if sub != nil {
		go p.follow(s, id, sub)
	}
	go p.run(cmd, outR, errR, req.DeadlineSeconds, func() { s.a.procs.markExited(p) })
}

func unwrapExecErr(err error) error {
	var ee *exec.Error
	if errors.As(err, &ee) {
		return ee.Err
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// command builds the exec.Cmd for req as its guest user.
func (a *Agent) command(req guestproto.Exec) (*exec.Cmd, error) {
	name := req.User
	if name == "" {
		name = a.cfg.DefaultUser
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	home, uname, shell := "/", "", "/bin/sh"
	if name != "" {
		u, err := user.Lookup(name)
		if err != nil {
			return nil, guestproto.Errorf(guestproto.CodeNotFound, "unknown user %q", name)
		}
		cred, err := credential(u)
		if err != nil {
			return nil, err
		}
		cmd.SysProcAttr.Credential = cred
		home, uname = u.HomeDir, u.Username
	} else if u, err := user.Current(); err == nil {
		home, uname = u.HomeDir, u.Username
	}
	env := []string{"PATH=" + defaultPath, "HOME=" + home, "USER=" + uname, "LOGNAME=" + uname, "SHELL=" + shell}
	cmd.Env = append(env, req.Env...) // later entries win in os/exec
	cmd.Dir = req.Cwd
	if cmd.Dir == "" {
		cmd.Dir = home
		if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
			cmd.Dir = "/"
		}
	}
	return cmd, nil
}

// credential returns the process credential for u. An unprivileged agent
// (tests, development) can only run as itself.
func credential(u *user.User) (*syscall.Credential, error) {
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil {
		return nil, guestproto.Errorf(guestproto.CodeInternal, "user %q has non-numeric ids", u.Username)
	}
	if os.Geteuid() != 0 {
		if int(uid) != os.Geteuid() {
			return nil, guestproto.Errorf(guestproto.CodeInvalidArgument, "agent is unprivileged and cannot run as %q", u.Username)
		}
		return nil, nil
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	gids, err := u.GroupIds()
	if err == nil {
		for _, g := range gids {
			if n, err := strconv.ParseUint(g, 10, 32); err == nil {
				cred.Groups = append(cred.Groups, uint32(n))
			}
		}
	}
	return cred, nil
}

// run pumps output, enforces the deadline and records the exit.
func (p *proc) run(cmd *exec.Cmd, outR, errR *os.File, deadline int64, onExit func()) {
	var pumps sync.WaitGroup
	pumps.Add(2)
	go p.pump(outR, true, &pumps)
	go p.pump(errR, false, &pumps)

	if deadline > 0 {
		t := time.AfterFunc(time.Duration(deadline)*time.Second, func() {
			p.mu.Lock()
			p.timeout = true
			p.mu.Unlock()
			p.kill(unix.SIGKILL)
		})
		defer t.Stop()
	}
	// Learn the exit without reaping: the zombie keeps the group ID ours.
	ex := waitExited(p.pid)
	go p.reap(cmd)

	drained := make(chan struct{})
	go func() { pumps.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(drainGrace):
		outR.Close() // unblock pumps held open by a lingering child
		errR.Close()
		<-drained
	}
	outR.Close()
	errR.Close()

	p.mu.Lock()
	if p.timeout {
		ex.Status, ex.TimedOut = guestproto.StatusKilled, true
	}
	p.mu.Unlock()
	p.appendFrame(ex)
	p.mu.Lock()
	p.exited = &ex
	p.cond.Broadcast()
	p.mu.Unlock()
	close(p.exitedCh)
	onExit()
}

// sigchldInfo is the kernel's siginfo_t for SIGCHLD on 64-bit Linux
// (unix.Siginfo hides these fields): si_pid, si_uid and si_status follow the
// three int header and its alignment padding.
type sigchldInfo struct {
	Signo  int32
	Errno  int32
	Code   int32
	_      int32
	Pid    int32
	UID    uint32
	Status int32
	_      [100]byte
}

// si_code values for SIGCHLD (uapi asm-generic/siginfo.h); x/sys has none.
const (
	cldExited = 1
	cldKilled = 2
	cldDumped = 3
)

// waitExited blocks until pid (a child) exits and returns how, leaving it a
// zombie (WNOWAIT): it is reaped later, by reap.
func waitExited(pid int) guestproto.ProcessExited {
	var info sigchldInfo
	for {
		err := unix.Waitid(unix.P_PID, pid, (*unix.Siginfo)(unsafe.Pointer(&info)), unix.WEXITED|unix.WNOWAIT, nil)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EINTR) {
			return guestproto.ProcessExited{Status: guestproto.StatusLost}
		}
	}
	switch info.Code {
	case cldExited:
		code := int(info.Status)
		return guestproto.ProcessExited{Status: guestproto.StatusExited, ExitCode: &code}
	case cldKilled, cldDumped:
		return guestproto.ProcessExited{Status: guestproto.StatusKilled, Signal: signalName(syscall.Signal(info.Status))}
	}
	return guestproto.ProcessExited{Status: guestproto.StatusLost}
}

// reap reaps the exited leader once nothing else is left in its process
// group (polling, as there is no event for that), so kill can keep
// signalling lingering group members by group ID until then.
func (p *proc) reap(cmd *exec.Cmd) {
	for wait := 50 * time.Millisecond; groupHasOthers(p.pid); wait = min(2*wait, 2*time.Second) {
		time.Sleep(wait)
	}
	p.mu.Lock()
	p.reaped = true // before the reap: kill holds mu while it signals
	p.mu.Unlock()
	_ = cmd.Wait()
}

// groupHasOthers reports whether any process other than pgid itself is in
// process group pgid.
func groupHasOthers(pgid int) bool {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == pgid {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // gone meanwhile
		}
		// Fields after the last ')': state, ppid, pgrp, ...
		s := string(b)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
		if len(f) > 2 && f[2] == strconv.Itoa(pgid) {
			return true
		}
	}
	return false
}

var signalNames = map[string]unix.Signal{
	"HUP": unix.SIGHUP, "INT": unix.SIGINT, "QUIT": unix.SIGQUIT, "KILL": unix.SIGKILL, "TERM": unix.SIGTERM,
	"USR1": unix.SIGUSR1, "USR2": unix.SIGUSR2, "CONT": unix.SIGCONT, "STOP": unix.SIGSTOP, "WINCH": unix.SIGWINCH,
}

func signalName(sig syscall.Signal) string {
	for n, s := range signalNames {
		if s == sig {
			return n
		}
	}
	if n := unix.SignalName(sig); n != "" {
		return n[len("SIG"):]
	}
	return strconv.Itoa(int(sig))
}

// kill signals p's whole process group (Setpgid), unless the leader has
// been reaped: then the group is empty and its ID may be a stranger's (see
// the package comment above). mu is held across the signal so reap cannot
// free the ID in between.
func (p *proc) kill(sig unix.Signal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pid > 0 && !p.reaped {
		_ = killGroup(-p.pid, sig)
	}
}

// killGroup is unix.Kill; tests replace it to watch signals.
var killGroup = unix.Kill

func (p *proc) pump(r *os.File, stdout bool, wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, readChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if stdout {
				p.appendFrame(guestproto.Stdout{Data: data})
			} else {
				p.appendFrame(guestproto.Stderr{Data: data})
			}
		}
		if err != nil {
			return
		}
	}
}

// appendFrame adds m to the log, blocking while a live subscriber still
// needs frames that would have to be evicted.
func (p *proc) appendFrame(m guestproto.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	if m.MsgType() != guestproto.TypeProcessExited {
		p.outSeq = p.seq
	}
	f, _ := guestproto.EncodeStream(1, p.seq, m) // stream rewritten per subscriber
	size := len(f.Body)
	if p.first == 0 {
		p.first = p.seq
	}
	p.log = append(p.log, f)
	p.bytes += size
	p.cond.Broadcast()
	for p.bytes > p.limit && len(p.log) > 1 {
		oldest := p.log[0].Seq
		if p.neededBySub(oldest) {
			p.cond.Wait()
			continue
		}
		p.bytes -= len(p.log[0].Body)
		p.log = p.log[1:]
		p.first = p.log[0].Seq
	}
}

func (p *proc) neededBySub(seq uint64) bool {
	for s := range p.subs {
		if !s.done && s.cursor < seq {
			return true
		}
	}
	return false
}

// subscribe registers a reader that has delivered everything up to after.
func (p *proc) subscribe(after uint64) *subscriber {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := &subscriber{cursor: after}
	p.subs[s] = struct{}{}
	return s
}

// follow streams the log from sub's cursor to the session on stream until
// ProcessExited has been sent or the session ends.
func (p *proc) follow(s *session, stream uint64, sub *subscriber) {
	defer func() {
		p.mu.Lock()
		sub.done = true
		delete(p.subs, sub)
		p.cond.Broadcast()
		p.mu.Unlock()
	}()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-s.ctx.Done():
			p.mu.Lock()
			sub.done = true
			p.cond.Broadcast()
			p.mu.Unlock()
		case <-stop:
		}
	}()
	for {
		p.mu.Lock()
		for !sub.done && p.seq == sub.cursor {
			p.cond.Wait()
		}
		if sub.done {
			p.mu.Unlock()
			return
		}
		start := int(sub.cursor + 1 - p.first)
		batch := append([]*guestproto.Frame(nil), p.log[start:]...)
		p.mu.Unlock()
		for _, f := range batch {
			out := *f
			out.Stream = stream
			if !s.sendStream(&out) {
				return
			}
			p.mu.Lock()
			sub.cursor = f.Seq
			p.cond.Broadcast()
			p.mu.Unlock()
			if f.Type == guestproto.TypeProcessExited {
				return
			}
		}
	}
}

// stdinLoop writes queued input; it stops at stdin.close, a write error
// (the process closed stdin) or process exit.
func (p *proc) stdinLoop(w io.WriteCloser, exited <-chan struct{}) {
	defer close(p.stdinD)
	defer w.Close()
	for {
		select {
		case it := <-p.stdinQ:
			if len(it.data) > 0 {
				if _, err := w.Write(it.data); err != nil {
					return
				}
			}
			if it.close {
				return
			}
		case <-exited:
			return
		}
	}
}

// feed queues stdin for p. It blocks (backpressure on the session) only
// while the writer is alive; nil-safe for unknown streams.
func (p *proc) feed(data []byte, closeAfter bool) {
	if p == nil || p.stdinQ == nil {
		return
	}
	select {
	case p.stdinQ <- stdinItem{data: data, close: closeAfter}:
	case <-p.stdinD:
	}
}

// stdinFor returns the process reading stream's stdin; done drops the
// route (after stdin.close).
func (s *session) stdinFor(stream uint64, done bool) *proc {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.stdin[stream]
	if done {
		delete(s.stdin, stream)
	}
	return p
}

// attach replays retained output after AfterSeq on this request's stream.
func (s *session) attach(id uint64, req guestproto.ProcessAttach) {
	p := s.a.procs.get(req.ProcessID)
	if p == nil {
		s.fail(id, guestproto.Errorf(guestproto.CodeNotFound, "no process %s", req.ProcessID))
		return
	}
	p.mu.Lock()
	// The cursor names output already handled; the exit frame always
	// follows it, so a cursor at or past the exit frame is invalid.
	if req.AfterSeq > p.outSeq || (p.first > 0 && req.AfterSeq+1 < p.first) {
		first, last := p.first, p.outSeq
		p.mu.Unlock()
		s.fail(id, guestproto.Errorf(guestproto.CodeInvalidArgument, "afterSeq %d outside retained output [%d,%d]", req.AfterSeq, first, last))
		return
	}
	sub := &subscriber{cursor: req.AfterSeq}
	p.subs[sub] = struct{}{}
	p.mu.Unlock()
	s.send(id, guestproto.Ack{})
	go p.follow(s, id, sub)
}

func (s *session) signal(id uint64, req guestproto.ProcessSignal) {
	p := s.a.procs.get(req.ProcessID)
	if p == nil {
		s.fail(id, guestproto.Errorf(guestproto.CodeNotFound, "no process %s", req.ProcessID))
		return
	}
	p.kill(signalNames[req.Signal])
	s.send(id, guestproto.Ack{})
}

func (s *session) status(id uint64, req guestproto.ProcessStatusRequest) {
	p := s.a.procs.get(req.ProcessID)
	if p == nil {
		s.fail(id, guestproto.Errorf(guestproto.CodeNotFound, "no process %s", req.ProcessID))
		return
	}
	s.send(id, p.status())
}

// lookup answers ProcessLookup: the status of the job carrying the handle.
func (s *session) lookup(id uint64, req guestproto.ProcessLookup) {
	st, ok := s.a.procs.lookupHandle(req.Handle)
	if !ok {
		s.fail(id, guestproto.Errorf(guestproto.CodeNotFound, "no process with handle %s this boot", req.Handle))
		return
	}
	s.send(id, st)
}

func (p *proc) status() guestproto.ProcessStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := guestproto.ProcessStatus{ProcessID: p.id, Status: guestproto.StatusRunning, OutputSeq: p.outSeq, Handle: p.handle}
	if p.exited != nil {
		st.Status, st.ExitCode, st.Signal, st.TimedOut = p.exited.Status, p.exited.ExitCode, p.exited.Signal, p.exited.TimedOut
	}
	return st
}

func (p *proc) exitedNow() *guestproto.ProcessExited {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exited
}

func (p *proc) outSeqNow() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outSeq
}

// String helps logs.
func (p *proc) String() string { return fmt.Sprintf("%s(pid %d)", p.id, p.pid) }
