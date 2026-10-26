package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
	"davit/internal/wsock"
)

// Exec exit codes beyond the guest's own status (SPEC §3).
const (
	ExitDeadline  = 124 // the process deadline killed it
	ExitInterrupt = 130 // interrupted at the client
	ExitLost      = 255 // transport lost; outcome unknown
)

// exitStatus makes Main exit with a status instead of an error record (the
// command already printed its terminal line). msg, if set, goes to stderr
// in human mode.
type exitStatus struct {
	code int
	msg  string
}

func (e *exitStatus) Error() string { return "exit status " + strconv.Itoa(e.code) }

// execOpts are exec's flags.
type execOpts struct {
	cwd                string
	env                multiFlag
	user               string
	timeout            secondsFlag
	detach, stdin, tty bool
	stream             bool
	status, attach     string
	kill, sig          string
	after              int64
	argv               []string // after "--"
	key                string
}

// secondsFlag accepts 30 (seconds) or a duration like 2m.
type secondsFlag struct{ v int64 }

func (f *secondsFlag) String() string { return strconv.FormatInt(f.v, 10) }

func (f *secondsFlag) Set(s string) error {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		f.v = n
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Second {
		return errors.New("want seconds (30) or a duration of at least 1s (2m)")
	}
	f.v = int64((d + time.Second - 1) / time.Second)
	return nil
}

func execFlags(fs *flag.FlagSet, o *opts) {
	e := &o.exec
	fs.StringVar(&e.cwd, "cwd", "", "working directory in the guest")
	fs.Var(&e.env, "env", "KEY=VALUE (repeatable)")
	fs.Var(&e.env, "e", "KEY=VALUE (repeatable)")
	fs.StringVar(&e.user, "user", "", "guest user (default: the image's)")
	fs.Var(&e.timeout, "timeout", "process deadline: seconds or a duration; exit 124 when it fires")
	fs.BoolVar(&e.detach, "detach", false, "start in the background and print the process ID")
	fs.BoolVar(&e.stdin, "stdin", false, "forward this terminal's stdin to the process")
	fs.BoolVar(&e.tty, "tty", false, "allocate a terminal (not supported; use ssh)")
	fs.BoolVar(&e.stream, "stream", false, "stream output events (implied by --json)")
	fs.StringVar(&e.status, "status", "", "show process PID")
	fs.StringVar(&e.attach, "attach", "", "follow process PID's output")
	fs.Int64Var(&e.after, "after", 0, "with --attach: replay output after this seq")
	fs.StringVar(&e.kill, "kill", "", "signal process PID's group (--signal, default TERM)")
	fs.StringVar(&e.sig, "signal", "TERM", "signal for --kill")
	fs.StringVar(&e.key, "idempotency-key", "", "deduplicate separate invocations")
}

func procPath(vm, pid string) string { return vmPath(vm) + "/processes/" + url.PathEscape(pid) }

func runExec(ctx context.Context, s *session, o *opts, args []string) error {
	e := &o.exec
	modes := 0
	for _, m := range []string{e.status, e.attach, e.kill} {
		if m != "" {
			modes++
		}
	}
	if modes > 1 {
		return usageErr("--status, --attach and --kill are mutually exclusive")
	}
	if err := maxArgs(args, 1, "exec [ID] -- COMMAND..."); err != nil {
		return err
	}
	id, err := s.currentID(args)
	if err != nil {
		return err
	}
	switch {
	case e.status != "":
		var p api.Process
		if err := s.cl.get(ctx, procPath(id, e.status), nil, &p); err != nil {
			return err
		}
		s.printProcess(p)
		return nil
	case e.kill != "":
		var p api.Process
		if err := s.cl.do(ctx, http.MethodDelete, procPath(id, e.kill), url.Values{"signal": {e.sig}}, nil, nil, &p); err != nil {
			return err
		}
		s.printProcess(p)
		return nil
	case e.attach != "":
		return s.followProcess(ctx, id, e.attach, e.after, false, nil)
	}

	if len(e.argv) == 0 {
		return usageErr("exec needs a command after --: %s exec [ID] -- COMMAND...", buildinfo.Name)
	}
	if e.tty {
		return clientErr(domain.CodeUnsupportedCapability, "--tty is not supported by exec; use `%s ssh`", buildinfo.Name)
	}
	body := api.ExecRequest{Argv: e.argv, Cwd: e.cwd, User: e.user, DeadlineSeconds: e.timeout.v, Detached: e.detach, Stdin: e.stdin}
	for _, kv := range e.env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return usageErr("--env wants KEY=VALUE, got %q", kv)
		}
		if body.Env == nil {
			body.Env = map[string]string{}
		}
		body.Env[k] = v
	}
	key := e.key
	if key == "" {
		key = newIdempotencyKey()
	}
	// Catch Ctrl-C from before the process starts: a signal that arrives
	// before the stream is attached is buffered and forwarded once it is,
	// instead of killing the CLI and leaving the guest process running.
	var sigc chan os.Signal
	if !e.detach {
		sigc = make(chan os.Signal, 2)
		signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigc)
	}
	var p api.Process
	if err := s.cl.do(ctx, http.MethodPost, vmPath(id)+"/processes", nil, body, map[string]string{api.HeaderIdempotencyKey: key}, &p); err != nil {
		return err
	}
	s.out.emit("process.started", "", 0, p, nil)
	if e.detach {
		s.out.result("", p)
		s.out.human("%s", p.ID)
		s.out.diag("detached; follow with: %s exec %s --attach %s", buildinfo.Name, id, p.ID)
		return nil
	}
	return s.followProcess(ctx, id, p.ID, 0, e.stdin, sigc)
}

func (s *session) printProcess(p api.Process) {
	s.out.result("", p)
	line := p.ID + " " + string(p.Status)
	if p.ExitCode != nil {
		line += " exit=" + strconv.Itoa(*p.ExitCode)
	}
	if p.Signal != "" {
		line += " signal=" + p.Signal
	}
	if p.TimedOut {
		line += " (deadline)"
	}
	s.out.human("%s", line)
}

// followProcess attaches to pid's stream and mirrors it until exit, returning the
// process's exit status as an *exitStatus.
// sigc, if non-nil, is already registered for SIGINT/SIGTERM by the caller.
func (s *session) followProcess(ctx context.Context, vm, pid string, after int64, stdin bool, sigc chan os.Signal) error {
	// Ctrl-C forwards SIGINT; a second one sends KILL.
	if sigc == nil {
		sigc = make(chan os.Signal, 2)
		signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigc)
	}
	interrupted := make(chan struct{})
	exit, err := s.streamProcess(ctx, vm, pid, after, stdin, sigc, func(n int, ws *wsock.Conn) {
		sig := "INT"
		if n > 0 {
			sig = "KILL"
		}
		if n == 0 {
			close(interrupted)
		}
		b, _ := json.Marshal(api.ProcessStreamMessage{Type: api.StreamSignal, Signal: sig})
		_ = ws.WriteMessage(wsock.TextMessage, b)
	})
	var lost *streamLost
	if errors.As(err, &lost) {
		e := clientErr(domain.CodeUnavailable, "lost the process stream: %v; query it with `%s exec %s --status %s`", lost.err, buildinfo.Name, vm, pid)
		s.out.fail(e)
		return &exitStatus{code: ExitLost}
	}
	if err != nil {
		return err
	}
	if exit.Process == nil {
		return &exitStatus{code: ExitLost}
	}
	s.out.emit("process.exited", "", exit.Seq, exit.Process, nil)
	select {
	case <-interrupted:
		return &exitStatus{code: ExitInterrupt}
	default:
	}
	return &exitStatus{code: processExit(*exit.Process)}
}

// streamLost is a process stream that broke before the exit message.
type streamLost struct{ err error }

func (e *streamLost) Error() string { return "lost the process stream: " + e.err.Error() }

// streamProcess attaches to pid's stream and mirrors its output (stdout and
// stderr, or stdout/stderr events in --json mode) until the exit message,
// which it returns. Each signal on sigc (nil: none) calls onSignal with
// its 0-based count. Ending ctx closes the stream (*streamLost).
func (s *session) streamProcess(ctx context.Context, vm, pid string, after int64, stdin bool, sigc chan os.Signal,
	onSignal func(n int, ws *wsock.Conn)) (*api.ProcessStreamMessage, error) {
	path := api.BasePath + procPath(vm, pid) + "/stream?outputCursor=" + strconv.FormatInt(after, 10)
	ws, err := s.cl.websocket(ctx, path)
	if err != nil {
		return nil, err
	}
	defer ws.Close(wsock.CloseNormal, "")
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			ws.Close(wsock.CloseNormal, "")
		case <-done:
		}
	}()
	if sigc != nil {
		go func() {
			n := 0
			for range sigc {
				onSignal(n, ws)
				n++
			}
		}()
	}
	if stdin && s.io.Stdin != nil {
		go pumpStdin(ws, s.io.Stdin)
	}

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return nil, &streamLost{err}
		}
		var msg api.ProcessStreamMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case api.StreamStdout, api.StreamStderr:
			b, err := base64.StdEncoding.DecodeString(msg.Data)
			if err != nil {
				continue
			}
			if s.g.json {
				chunk := map[string]string{"processId": pid}
				if utf8.Valid(b) {
					chunk["text"] = string(b)
				} else {
					chunk["base64"] = msg.Data
				}
				s.out.emit(msg.Type, "", msg.Seq, chunk, nil)
			} else if msg.Type == api.StreamStdout {
				s.io.Stdout.Write(b)
			} else {
				s.io.Stderr.Write(b)
			}
		case api.StreamError:
			if msg.Error != nil {
				return nil, &cliError{ErrorBody{Code: msg.Error.Code, Message: msg.Error.Message, Retryable: msg.Error.Retryable, Origin: "server"}}
			}
		case api.StreamExit:
			return &msg, nil
		}
	}
}

// processExit maps a final process to the CLI exit status: the guest's
// code, 124 for the deadline, 128+n for a signal, 255 when unknown.
func processExit(p api.Process) int {
	switch {
	case p.TimedOut:
		return ExitDeadline
	case p.ExitCode != nil:
		return *p.ExitCode & 0xff
	case p.Signal != "":
		if n, ok := signalNumbers[p.Signal]; ok {
			return 128 + n
		}
	}
	return ExitLost
}

var signalNumbers = map[string]int{"HUP": 1, "INT": 2, "QUIT": 3, "KILL": 9, "USR1": 10, "SEGV": 11, "USR2": 12,
	"PIPE": 13, "ALRM": 14, "TERM": 15, "ABRT": 6, "BUS": 7, "FPE": 8, "ILL": 4, "TRAP": 5, "SYS": 31}

func pumpStdin(ws *wsock.Conn, r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b, _ := json.Marshal(api.ProcessStreamMessage{Type: api.StreamStdin, Data: base64.StdEncoding.EncodeToString(buf[:n])})
			if ws.WriteMessage(wsock.TextMessage, b) != nil {
				return
			}
		}
		if err != nil {
			b, _ := json.Marshal(api.ProcessStreamMessage{Type: api.StreamStdinClose})
			_ = ws.WriteMessage(wsock.TextMessage, b)
			return
		}
	}
}
