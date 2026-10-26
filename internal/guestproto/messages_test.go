package guestproto

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func intp(i int) *int { return &i }

// samples holds one populated, valid value per registered message type.
func samples() []Message {
	nonce := bytes.Repeat([]byte{1}, NonceSize)
	return []Message{
		&Hello{Versions: VersionRange{1, 1}, Features: []string{"pty"}, VMID: "vm_abc", Generation: 3,
			Challenge: nonce, BootstrapSecret: bytes.Repeat([]byte{9}, 32)},
		&HelloAck{Version: 1, Features: []string{"pty"}, AgentVersion: "0.1.0", GuestNonce: nonce,
			Proof: bytes.Repeat([]byte{2}, ProofSize)},
		&Ack{},
		&Ping{},
		&Pong{},
		&ReadinessProbe{Services: []string{"sshd"}},
		&Readiness{Agent: true, SSH: true, Services: map[string]ServiceState{"sshd": ServiceReady}},
		&SetIdentity{VMID: "vm_abc", Generation: 4, Hostname: "dev-1", WallClockUnixNano: 1_760_000_000_000_000_000,
			RotateMachineID: true, RotateSSHHostKeys: true, NewAgentSecret: bytes.Repeat([]byte{7}, 32),
			SSHUserCAKeys: []string{"ssh-ed25519 AAAA ca"}},
		&IdentityResult{VMID: "vm_abc", Generation: 4, Hostname: "dev-1", MachineID: "0123456789abcdef0123456789abcdef",
			SSHHostKeyFingerprints: []string{"SHA256:abc"}, BootID: "b-1"},
		&Exec{Argv: []string{"sh", "-lc", "echo $A"}, Cwd: "/home/user", Env: []string{"A=1", "EMPTY="},
			DeadlineSeconds: 30, Detached: true, Stdin: true, TTY: true, User: "user", NoFollow: true,
			Handle: "job_A-1.b:2"},
		&ProcessStarted{ProcessID: "proc_1", PID: 42, Handle: "job_A-1.b:2"},
		&ProcessAttach{ProcessID: "proc_1", AfterSeq: 17},
		&ProcessSignal{ProcessID: "proc_1", Signal: "TERM"},
		&ProcessStatusRequest{ProcessID: "proc_1"},
		&ProcessStatus{ProcessID: "proc_1", Status: StatusExited, ExitCode: intp(0), OutputSeq: 9, Handle: "job_1"},
		&ProcessLookup{Handle: "job_1"},
		&ProcessListRequest{},
		&ProcessList{SampledAtUnixNano: 1_760_000_000_000_000_000, Truncated: true, Processes: []GuestProcess{
			{PID: 1, User: "root", Args: []string{"/sbin/init"}, State: "S", CPUTimeMillis: 12, RSSBytes: 4096,
				StartedAtUnixNano: 1_759_999_999_000_000_000},
			{PID: 42, PPID: 1, User: "user", Args: []string{"sleep", "60"}, State: "R", Handle: "job_1"},
		}},
		&Stdout{Data: []byte("out\x00\xff")},
		&Stderr{Data: []byte("err")},
		&Stdin{Data: []byte("in")},
		&StdinClose{},
		&ProcessExited{Status: StatusKilled, Signal: "KILL", TimedOut: true},
		&ReadFile{Path: "/etc/hostname", User: "user", Offset: 1, Limit: 10},
		&ReadFile{Path: "/home/user/src", Archive: ArchiveTar},
		&FileReadResult{Size: 10, Mode: 0o644},
		&WriteFile{Path: "/home/user/a.txt", User: "user", Mode: 0o600, Archive: ArchiveTar},
		&FileChunk{Data: []byte("abc"), EOF: true},
		&FileCommit{Size: 3, SHA256: bytes.Repeat([]byte{3}, 32)},
		&FileWriteResult{Size: 3},
		&Shutdown{GraceSeconds: 10},
		&DialTCP{Port: 8080},
		&Listen{Port: 0},
		&ListenStarted{ListenerID: "lst_1", Port: 3000},
		&ListenAccepted{ListenerID: "lst_1", ConnID: "conn_1"},
		&ReverseConnect{ListenerID: "lst_1", ConnID: "conn_1"},
		&Unlisten{ListenerID: "lst_1"},
		&WorkspacePrepare{Revision: "env_x@3",
			Repos: []RepoCheckout{{URL: "https://example.com/r.git", Ref: "main", Path: "/home/user/r"}},
			Files: []WorkspaceFile{{Path: "/home/user/.env", Mode: 0o600, Data: []byte("TOKEN=x\x00\xff")}},
			Env:   []string{"GIT_TOKEN=placeholder"},
			Setup: []SetupStep{{Name: "install", Argv: []string{"npm", "ci"}, Cwd: "/home/user/r", User: "user",
				TimeoutSeconds: 600}, {Name: "build", Argv: []string{"make"}}}},
		&WorkspaceProgress{Step: "install", Phase: WorkspacePhaseSetup, State: WorkspaceStepFailed, Message: "exit 1"},
		&WorkspaceResult{Revision: "env_x@3", State: WorkspaceFailed, FailedStep: "install", ExitCode: intp(1),
			Message: "npm ci failed"},
		&WorkspaceResult{Revision: "env_x@3", State: WorkspaceReady},
	}
}

func TestRoundTripEveryMessage(t *testing.T) {
	seen := map[MsgType]bool{}
	for i, m := range samples() {
		mt := m.MsgType()
		seen[mt] = true
		t.Run(string(mt), func(t *testing.T) {
			var buf bytes.Buffer
			c := NewConn(&buf)
			if err := c.Send(uint64(i+1), m); err != nil {
				t.Fatal(err)
			}
			f, err := c.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			if f.ID != uint64(i+1) || f.Type != mt {
				t.Fatalf("envelope = %+v", f)
			}
			got, err := DecodeMessage(f)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, m) {
				t.Fatalf("got %#v\nwant %#v", got, m)
			}
		})
	}
	for _, mt := range Types() {
		if !seen[mt] {
			t.Errorf("registered type %q has no round-trip sample", mt)
		}
	}
}

func TestDecodeGeneric(t *testing.T) {
	f, err := EncodeStream(5, 2, Stderr{Data: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode[Stderr](f)
	if err != nil || string(got.Data) != "x" || f.Stream != 5 || f.Seq != 2 || f.ID != 0 {
		t.Fatalf("got %+v %+v %v", got, f, err)
	}
}

func TestValidateRejects(t *testing.T) {
	long := "/" + strings.Repeat("a", MaxPathLen)
	tests := []struct {
		name string
		v    Validator
	}{
		{"exec nil argv", Exec{}},
		{"exec empty argv0", Exec{Argv: []string{""}}},
		{"exec NUL arg", Exec{Argv: []string{"echo", "a\x00b"}}},
		{"exec env no =", Exec{Argv: []string{"x"}, Env: []string{"NOPE"}}},
		{"exec env empty key", Exec{Argv: []string{"x"}, Env: []string{"=v"}}},
		{"exec env NUL", Exec{Argv: []string{"x"}, Env: []string{"A=\x00"}}},
		{"exec relative cwd", Exec{Argv: []string{"x"}, Cwd: "tmp"}},
		{"exec dotdot cwd", Exec{Argv: []string{"x"}, Cwd: "/a/../b"}},
		{"exec negative deadline", Exec{Argv: []string{"x"}, DeadlineSeconds: -1}},
		{"exec bad user", Exec{Argv: []string{"x"}, User: "../root"}},
		{"exec too many args", Exec{Argv: append([]string{"x"}, make([]string, MaxArgs)...)}},
		{"read relative", ReadFile{Path: "etc/passwd", User: "user"}},
		{"read dotdot", ReadFile{Path: "/home/../etc/shadow", User: "user"}},
		{"read trailing dotdot", ReadFile{Path: "/home/..", User: "user"}},
		{"read NUL", ReadFile{Path: "/etc/x\x00", User: "user"}},
		{"read unclean", ReadFile{Path: "/etc//passwd", User: "user"}},
		{"read trailing slash", ReadFile{Path: "/etc/", User: "user"}},
		{"read too long", ReadFile{Path: long, User: "user"}},
		{"read archive with offset", ReadFile{Path: "/etc", Archive: ArchiveTar, Offset: 1}},
		{"read bad archive", ReadFile{Path: "/etc", Archive: "zip"}},
		{"read negative offset", ReadFile{Path: "/etc/hostname", User: "user", Offset: -1}},
		{"write root", WriteFile{Path: "/", User: "user"}},
		{"write bad mode", WriteFile{Path: "/a", User: "user", Mode: 0o170000}},
		{"write empty path", WriteFile{User: "user"}},
		{"commit bad hash", FileCommit{Size: 1, SHA256: []byte{1}}},
		{"commit negative", FileCommit{Size: -1}},
		{"chunk too big", FileChunk{Data: make([]byte, MaxChunkSize+1)}},
		{"stdin too big", Stdin{Data: make([]byte, MaxChunkSize+1)}},
		{"signal unknown", ProcessSignal{ProcessID: "p", Signal: "SEGV"}},
		{"signal no id", ProcessSignal{Signal: "TERM"}},
		{"status no id", ProcessStatusRequest{}},
		{"attach no id", ProcessAttach{}},
		{"identity no vm", SetIdentity{Generation: 1, Hostname: "h", WallClockUnixNano: 1}},
		{"identity gen 0", SetIdentity{VMID: "vm", Hostname: "h", WallClockUnixNano: 1}},
		{"identity bad host", SetIdentity{VMID: "vm", Generation: 1, Hostname: "-h", WallClockUnixNano: 1}},
		{"identity no clock", SetIdentity{VMID: "vm", Generation: 1, Hostname: "h"}},
		{"identity short secret", SetIdentity{VMID: "vm", Generation: 1, Hostname: "h", WallClockUnixNano: 1,
			NewAgentSecret: []byte("short")}},
		{"shutdown negative", Shutdown{GraceSeconds: -1}},
		{"tcp port 0", DialTCP{}},
		{"exec bad handle", Exec{Argv: []string{"x"}, Handle: "a/b"}},
		{"exec handle space", Exec{Argv: []string{"x"}, Handle: "a b"}},
		{"exec handle too long", Exec{Argv: []string{"x"}, Handle: strings.Repeat("h", MaxIDLen+1)}},
		{"lookup no handle", ProcessLookup{}},
		{"lookup bad handle", ProcessLookup{Handle: "job\x00"}},
		{"ps too many", ProcessList{Processes: make([]GuestProcess, MaxProcessSamples+1)}},
		{"ps pid 0", ProcessList{Processes: []GuestProcess{{}}}},
		{"ps negative ppid", ProcessList{Processes: []GuestProcess{{PID: 2, PPID: -1}}}},
		{"ps bad handle", ProcessList{Processes: []GuestProcess{{PID: 2, Handle: "a b"}}}},
		{"listen negative port", Listen{Port: -1}},
		{"listen port too big", Listen{Port: 65536}},
		{"listen started no id", ListenStarted{Port: 80}},
		{"listen started port 0", ListenStarted{ListenerID: "l", Port: 0}},
		{"listen started port too big", ListenStarted{ListenerID: "l", Port: 65536}},
		{"accepted no conn", ListenAccepted{ListenerID: "l"}},
		{"accepted no listener", ListenAccepted{ConnID: "c"}},
		{"reverse no conn", ReverseConnect{ListenerID: "l"}},
		{"reverse no listener", ReverseConnect{ConnID: "c"}},
		{"reverse bad conn", ReverseConnect{ListenerID: "l", ConnID: "a/b"}},
		{"unlisten no id", Unlisten{}},
		{"workspace no revision", WorkspacePrepare{}},
		{"workspace bad revision", WorkspacePrepare{Revision: "env x"}},
		{"workspace repo no url", WorkspacePrepare{Revision: "r", Repos: []RepoCheckout{{Path: "/w"}}}},
		{"workspace repo relative", WorkspacePrepare{Revision: "r", Repos: []RepoCheckout{{URL: "u", Path: "w"}}}},
		{"workspace repo NUL ref", WorkspacePrepare{Revision: "r", Repos: []RepoCheckout{{URL: "u", Ref: "\x00", Path: "/w"}}}},
		{"workspace too many repos", WorkspacePrepare{Revision: "r", Repos: make([]RepoCheckout, MaxWorkspaceItems+1)}},
		{"workspace file relative", WorkspacePrepare{Revision: "r", Files: []WorkspaceFile{{Path: "a"}}}},
		{"workspace file root", WorkspacePrepare{Revision: "r", Files: []WorkspaceFile{{Path: "/"}}}},
		{"workspace file bad mode", WorkspacePrepare{Revision: "r", Files: []WorkspaceFile{{Path: "/a", Mode: 0o170000}}}},
		{"workspace file too big", WorkspacePrepare{Revision: "r",
			Files: []WorkspaceFile{{Path: "/a", Data: make([]byte, MaxChunkSize+1)}}}},
		{"workspace env no =", WorkspacePrepare{Revision: "r", Env: []string{"NOPE"}}},
		{"workspace setup no name", WorkspacePrepare{Revision: "r", Setup: []SetupStep{{Argv: []string{"x"}}}}},
		{"workspace setup dup name", WorkspacePrepare{Revision: "r",
			Setup: []SetupStep{{Name: "a", Argv: []string{"x"}}, {Name: "a", Argv: []string{"y"}}}}},
		{"workspace setup no argv", WorkspacePrepare{Revision: "r", Setup: []SetupStep{{Name: "a"}}}},
		{"workspace setup relative cwd", WorkspacePrepare{Revision: "r",
			Setup: []SetupStep{{Name: "a", Argv: []string{"x"}, Cwd: "tmp"}}}},
		{"workspace setup bad user", WorkspacePrepare{Revision: "r",
			Setup: []SetupStep{{Name: "a", Argv: []string{"x"}, User: "../root"}}}},
		{"workspace setup negative timeout", WorkspacePrepare{Revision: "r",
			Setup: []SetupStep{{Name: "a", Argv: []string{"x"}, TimeoutSeconds: -1}}}},
		{"progress no step", WorkspaceProgress{Phase: WorkspacePhaseClone, State: WorkspaceStepRunning}},
		{"progress bad phase", WorkspaceProgress{Step: "s", Phase: "build", State: WorkspaceStepRunning}},
		{"progress bad state", WorkspaceProgress{Step: "s", Phase: WorkspacePhaseFile, State: "ready"}},
		{"result no revision", WorkspaceResult{State: WorkspaceReady}},
		{"result bad state", WorkspaceResult{Revision: "r", State: "done"}},
		{"result ready with failed step", WorkspaceResult{Revision: "r", State: WorkspaceReady, FailedStep: "s"}},
		{"result ready with exit code", WorkspaceResult{Revision: "r", State: WorkspaceReady, ExitCode: intp(0)}},
		{"hello short challenge", Hello{Versions: SupportedVersions, VMID: "vm", Generation: 1, Challenge: []byte{1}}},
		{"hello gen 0", Hello{Versions: SupportedVersions, VMID: "vm", Challenge: NewChallenge()}},
		{"hello bad range", Hello{Versions: VersionRange{2, 1}, VMID: "vm", Generation: 1, Challenge: NewChallenge()}},
		{"ack short proof", HelloAck{Version: 1, GuestNonce: NewChallenge(), Proof: []byte{1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.v.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() = %v, want invalid_argument", err)
			}
		})
	}
}

func TestValidateAcceptsSamples(t *testing.T) {
	for _, m := range samples() {
		if v, ok := m.(Validator); ok {
			if err := v.Validate(); err != nil {
				t.Errorf("%s: %v", m.MsgType(), err)
			}
		}
	}
}

func TestDialTCPDecode(t *testing.T) {
	for body, want := range map[string]error{
		`{"port":22}`:    nil,
		`{"port":0}`:     ErrInvalid,
		`{}`:             ErrInvalid,
		`{"port":65536}`: ErrMalformedFrame,
		`{"port":-1}`:    ErrMalformedFrame,
	} {
		f := &Frame{V: ProtocolVersion, ID: 1, Type: TypeDialTCP, Body: []byte(body)}
		m, err := DecodeMessage(f)
		if want == nil {
			if d, ok := m.(*DialTCP); err != nil || !ok || d.Port != 22 {
				t.Errorf("%s: got %#v, %v", body, m, err)
			}
		} else if !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", body, err, want)
		}
	}
}
