// Package fakehelper is a Go stand-in for the Mac VZ helper that speaks the
// real protocol (helperproto) with a simulated VM, so the Mac runtime
// adapter runs its conformance suite on any OS. A test binary becomes the
// helper when executed as Binary (call Main first in TestMain).
//
// The simulated VM boots after BootDelay and powers off ShutdownDelay after
// a Stop; its vsock port echoes every byte back. Behaviour knobs come from
// the environment (the adapter passes Options.HelperEnv through):
//
//	DAVIT_FAKE_VZ=ignore-shutdown   the guest ignores Stop (grace expires)
//	DAVIT_FAKE_VZ=unentitled        probe reports no virtualization entitlement
package fakehelper

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	hp "davit/internal/runtime/mac/helperproto"
)

const (
	binaryName    = "fake-vz-helper"
	Version       = "fake-0.1"
	BootDelay     = 10 * time.Millisecond
	ShutdownDelay = 10 * time.Millisecond
)

var operations = []hp.MsgType{hp.TypeCreate, hp.TypeBoot, hp.TypeState, hp.TypePause, hp.TypeResume, hp.TypeStop, hp.TypeKill, hp.TypeWatch, hp.TypeDial}

// Main runs the fake helper and exits if this process was started as it.
func Main() {
	if filepath.Base(os.Args[0]) != binaryName {
		return
	}
	os.Exit(run(os.Args[1:]))
}

// Binary returns a path that executes this test binary as the fake helper.
func Binary(t testing.TB) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), binaryName)
	if err := os.Symlink(exe, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func knob(name string) bool {
	for _, k := range strings.Split(os.Getenv("DAVIT_FAKE_VZ"), ",") {
		if k == name {
			return true
		}
	}
	return false
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: helper probe | helper serve --dir DIR --vm ID --generation N")
		return 2
	}
	switch args[0] {
	case "probe":
		p := hp.ProbeResult{
			MinVersion: hp.MinProtocolVersion, MaxVersion: hp.ProtocolVersion, HelperVersion: Version, Build: "test",
			OSVersion: "14.6", OSBuild: "fake", Arch: "arm64", VZSupported: true, Entitled: !knob("unentitled"),
			Limits:     hp.Limits{MaxCPUs: 8, MinMemoryBytes: 128 << 20, MaxMemoryBytes: 16 << 30, MemoryAlignBytes: 1 << 20},
			Operations: operations,
		}
		json.NewEncoder(os.Stdout).Encode(p)
		return 0
	case "serve":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", args[0])
		return 2
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := fs.String("dir", "", "")
	vm := fs.String("vm", "", "")
	gen := fs.Int64("generation", 0, "")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	key := hp.Key{VMID: *vm, Generation: *gen}
	if err := key.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if st, err := os.Lstat(*dir); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		fmt.Fprintf(os.Stderr, "dir %s must be an owner-only directory: %v\n", *dir, err)
		return 2
	}
	h := &helper{dir: *dir, key: key, done: make(chan struct{}), state: hp.StateCreated}
	if err := h.serve(); err != nil {
		fmt.Fprintln(os.Stderr, "fake helper:", err)
		return 1
	}
	return 0
}

type helper struct {
	dir         string
	key         hp.Key
	incarnation string

	mu       sync.Mutex
	state    hp.State
	started  time.Time
	reason   string
	cfg      *hp.VMConfig
	watchers []chan hp.StateResult
	done     chan struct{} // closed on a terminal state
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (h *helper) serve() error {
	var b [16]byte
	rand.Read(b[:])
	h.incarnation = hex.EncodeToString(b[:])
	if err := writeJSON(filepath.Join(h.dir, hp.RecordFile), hp.Record{
		Protocol: hp.ProtocolVersion, Incarnation: h.incarnation, Key: h.key, PID: os.Getpid(), StartedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}
	ctl, err := listen(filepath.Join(h.dir, hp.ControlSocket))
	if err != nil {
		return err
	}
	str, err := listen(filepath.Join(h.dir, hp.StreamSocket))
	if err != nil {
		return err
	}
	go accept(ctl, h.control)
	go accept(str, h.stream)
	<-h.done
	ctl.Close()
	str.Close()
	h.mu.Lock()
	exit := hp.Exit{Incarnation: h.incarnation, State: h.state, Reason: h.reason, At: time.Now().UTC()}
	h.mu.Unlock()
	time.Sleep(20 * time.Millisecond) // let the final replies drain
	return writeJSON(filepath.Join(h.dir, hp.ExitFile), exit)
}

func listen(path string) (net.Listener, error) {
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return l, os.Chmod(path, 0o600)
}

func accept(l net.Listener, handle func(net.Conn)) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			uid, err := hp.PeerUID(c)
			if err != nil || uid != os.Getuid() {
				return
			}
			handle(c)
		}()
	}
}

// hello answers the first frame of a connection.
func (h *helper) hello(c net.Conn) (uint16, bool) {
	f, err := hp.ReadFrame(c)
	if err != nil {
		return 0, false
	}
	var req hp.Hello
	if f.Type != hp.TypeHello {
		hp.Reply(c, hp.MinProtocolVersion, f, nil, hp.Errorf(hp.CodeInvalidState, "hello first"))
		return 0, false
	}
	if err := f.Decode(&req); err != nil {
		hp.Reply(c, hp.MinProtocolVersion, f, nil, err)
		return 0, false
	}
	v, err := hp.Negotiate(req.MinVersion, req.MaxVersion)
	if err != nil {
		hp.Reply(c, hp.MinProtocolVersion, f, nil, err)
		return 0, false
	}
	return v, hp.Reply(c, v, f, hp.HelloResult{Version: v, Incarnation: h.incarnation, Key: h.key, HelperVersion: Version, Build: "test", Operations: operations}, nil) == nil
}

func (h *helper) snapshot() hp.StateResult {
	return hp.StateResult{State: h.state, StartedAt: h.started, Reason: h.reason}
}

// set changes state (h.mu held) and notifies watchers.
func (h *helper) set(s hp.State, reason string) {
	if h.state.Terminal() {
		return
	}
	h.state = s
	if s.Terminal() {
		h.reason = reason
	}
	ev := h.snapshot()
	for _, w := range h.watchers {
		select {
		case w <- ev:
		default: // a slow watcher misses intermediate events, never the connection
		}
	}
	if s.Terminal() {
		for _, w := range h.watchers {
			close(w)
		}
		h.watchers = nil
		close(h.done)
	}
}

func (h *helper) control(c net.Conn) {
	v, ok := h.hello(c)
	if !ok {
		return
	}
	for {
		f, err := hp.ReadFrame(c)
		if err != nil {
			return
		}
		if f.V != v {
			hp.Reply(c, v, f, nil, hp.Errorf(hp.CodeVersion, "frame version %d, negotiated %d", f.V, v))
			return
		}
		if f.Type == hp.TypeWatch {
			h.watch(c, v, f)
			return
		}
		body, err := h.handle(f)
		if hp.Reply(c, v, f, body, err) != nil {
			return
		}
	}
}

func (h *helper) handle(f *hp.Frame) (any, error) {
	switch f.Type {
	case hp.TypeCreate:
		var cfg hp.VMConfig
		if err := f.Decode(&cfg); err != nil {
			return nil, err
		}
		if cfg.Key != h.key {
			return nil, hp.Errorf(hp.CodeInvalidArgument, "config for %v, helper serves %v", cfg.Key, h.key)
		}
		for _, p := range []string{cfg.Kernel, cfg.RootDisk, cfg.Initrd} {
			if p == "" {
				continue
			}
			st, err := os.Lstat(p)
			if err != nil || !st.Mode().IsRegular() {
				return nil, hp.Errorf(hp.CodeInvalidArgument, "%s is not a regular file", p)
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case h.cfg == nil:
			h.cfg = &cfg
		case !sameConfig(*h.cfg, cfg):
			return nil, hp.Errorf(hp.CodeConflict, "already created with another configuration")
		}
		return h.snapshot(), nil
	case hp.TypeBoot:
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case h.cfg == nil:
			return nil, hp.Errorf(hp.CodeInvalidState, "boot before create")
		case h.state == hp.StateCreated:
			h.set(hp.StateStarting, "")
			h.mu.Unlock()
			time.Sleep(BootDelay)
			h.mu.Lock()
			h.started = time.Now().UTC()
			h.set(hp.StateRunning, "")
		}
		for h.state == hp.StateStarting { // another Boot is starting it: wait
			h.mu.Unlock()
			time.Sleep(time.Millisecond)
			h.mu.Lock()
		}
		if h.state.Terminal() {
			return nil, hp.Errorf(hp.CodeInvalidState, "VM %s", h.state)
		}
		return h.snapshot(), nil // running (or paused): idempotent
	case hp.TypeState:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.snapshot(), nil
	case hp.TypePause, hp.TypeResume:
		from, to := hp.StateRunning, hp.StatePaused
		if f.Type == hp.TypeResume {
			from, to = to, from
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		switch h.state {
		case to:
		case from:
			h.set(to, "")
		default:
			return nil, hp.Errorf(hp.CodeInvalidState, "%s while %s", f.Type, h.state)
		}
		return h.snapshot(), nil
	case hp.TypeStop:
		var req hp.Stop
		if err := f.Decode(&req); err != nil {
			return nil, err
		}
		h.mu.Lock()
		if h.state.Terminal() {
			defer h.mu.Unlock()
			return h.snapshot(), nil
		}
		if h.state != hp.StateRunning && h.state != hp.StateStopping {
			defer h.mu.Unlock()
			return nil, hp.Errorf(hp.CodeInvalidState, "stop while %s", h.state)
		}
		h.set(hp.StateStopping, "")
		h.mu.Unlock()
		grace := time.Duration(req.GraceMillis) * time.Millisecond
		if knob("ignore-shutdown") || grace < ShutdownDelay {
			time.Sleep(grace)
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.state == hp.StateStopping {
				h.set(hp.StateRunning, "") // the guest kept running
			}
			e := hp.Errorf(hp.CodeTimeout, "guest did not stop within %s", grace)
			e.Retryable = true
			return nil, e
		}
		time.Sleep(ShutdownDelay)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.set(hp.StateStopped, hp.ReasonShutdown)
		return h.snapshot(), nil
	case hp.TypeKill:
		h.mu.Lock()
		defer h.mu.Unlock()
		h.set(hp.StateStopped, hp.ReasonKilled)
		return h.snapshot(), nil
	}
	return nil, hp.Errorf(hp.CodeUnsupported, "message %q", f.Type)
}

func sameConfig(a, b hp.VMConfig) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func (h *helper) watch(c net.Conn, v uint16, req *hp.Frame) {
	ch := make(chan hp.StateResult, 16)
	h.mu.Lock()
	first := h.snapshot()
	if h.state.Terminal() {
		close(ch)
	} else {
		h.watchers = append(h.watchers, ch)
	}
	h.mu.Unlock()
	seq := uint64(0)
	send := func(ev hp.StateResult) bool {
		seq++
		f, _ := hp.NewFrame(v, req.ID, hp.TypeEvent, ev)
		f.Seq = seq
		return hp.WriteFrame(c, f) == nil
	}
	if !send(first) {
		return
	}
	for ev := range ch {
		if !send(ev) {
			return
		}
	}
}

func (h *helper) stream(c net.Conn) {
	v, ok := h.hello(c)
	if !ok {
		return
	}
	f, err := hp.ReadFrame(c)
	if err != nil {
		return
	}
	var d hp.Dial
	if f.Type != hp.TypeDial {
		hp.Reply(c, v, f, nil, hp.Errorf(hp.CodeInvalidState, "str.sock serves dial only"))
		return
	}
	if err := f.Decode(&d); err != nil {
		hp.Reply(c, v, f, nil, err)
		return
	}
	h.mu.Lock()
	running, port := h.state == hp.StateRunning, uint32(0)
	if h.cfg != nil {
		port = h.cfg.VsockPort
	}
	h.mu.Unlock()
	switch {
	case !running:
		hp.Reply(c, v, f, nil, hp.Errorf(hp.CodeInvalidState, "VM is not running"))
		return
	case d.Port != port:
		hp.Reply(c, v, f, nil, hp.Errorf(hp.CodeRefused, "nothing listens on vsock port %d", d.Port))
		return
	}
	if hp.Reply(c, v, f, struct{}{}, nil) != nil {
		return
	}
	// The simulated guest service echoes until the VM stops or the peer
	// closes; a stopped VM ends the stream.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-h.done:
			c.Close()
		case <-ctx.Done():
		}
	}()
	io.Copy(c, bufio.NewReader(c))
}
