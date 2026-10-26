package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"davit/api"
)

// The connect tests run this test binary as the "ssh" client
// (DAVIT_SSH=os.Args[0]); fakeSSHMain plays OpenSSH plus the remote
// shell, driven by files in $DAVIT_FAKESSH_DIR:
//
//	authorized_keys  public keys the host accepts (the -i key's .pub)
//	unreachable      every connection is refused
//	no-davit         the remote shell has no davit
//	no-daemon        davit runs but its daemon is down
//	sock             the daemon socket dial-stdio splices to
//	args             each invocation's argv, appended
const fakeSSHDir = "DAVIT_FAKESSH_DIR"

func TestMain(m *testing.M) {
	if os.Getenv(fakeSSHDir) != "" && os.Getenv("DAVIT_FAKESSH") == "1" {
		os.Exit(fakeSSHMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeSSHMain(args []string) int {
	dir := os.Getenv(fakeSSHDir)
	has := func(f string) bool { _, err := os.Stat(filepath.Join(dir, f)); return err == nil }
	if f, err := os.OpenFile(filepath.Join(dir, "args"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.WriteString(strings.Join(args, " ") + "\n")
		f.Close()
	}
	var key, remote string
	for i := 0; i < len(args); i++ {
		if args[i] == "-i" && i+1 < len(args) {
			key = args[i+1]
		}
		if args[i] == "--" && i+1 < len(args) {
			remote = strings.Join(args[i+1:], " ")
			break
		}
	}
	if has("unreachable") {
		os.Stderr.WriteString("ssh: connect to host h.invalid port 22: Connection refused\n")
		return 255
	}
	authorized, _ := os.ReadFile(filepath.Join(dir, "authorized_keys"))
	pub, _ := os.ReadFile(key + ".pub")
	if key == "" || len(bytes.TrimSpace(pub)) == 0 || !bytes.Contains(authorized, bytes.TrimSpace(pub)) {
		os.Stderr.WriteString("u@h.invalid: Permission denied (publickey).\n")
		return 255
	}
	if has("no-davit") {
		os.Stderr.WriteString("sh: 1: exec: davit: not found\n")
		return 127
	}
	switch {
	case strings.Contains(remote, "version --json"):
		os.Stdout.WriteString(`{"schemaVersion":1,"event":"result","data":{"name":"davit","version":"v9.9"}}` + "\n")
		return 0
	case strings.Contains(remote, "dial-stdio"):
		if has("no-daemon") {
			os.Stderr.WriteString("error: the daemon on this host is not reachable at /run/davit.sock: no such file (unavailable)\n")
			return 7
		}
		sock, _ := os.ReadFile(filepath.Join(dir, "sock"))
		c, err := net.Dial("unix", string(sock))
		if err != nil {
			return 1
		}
		go func() { io.Copy(c, os.Stdin); c.(*net.UnixConn).CloseWrite() }()
		io.Copy(os.Stdout, c)
		return 0
	}
	return 2
}

type connectEnv struct {
	t           *testing.T
	dir, home   string
	sshDir, cfg string
}

func newConnectEnv(t *testing.T) *connectEnv {
	dir := t.TempDir()
	e := &connectEnv{t: t, dir: dir, home: filepath.Join(dir, "home"), sshDir: filepath.Join(dir, "fakessh"),
		cfg: filepath.Join(dir, "config")}
	os.MkdirAll(e.home, 0o700)
	os.MkdirAll(e.sshDir, 0o700)
	t.Setenv("HOME", e.home)
	t.Setenv("XDG_CONFIG_HOME", e.cfg)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(dir, "run"))
	t.Setenv("DAVIT_API_URL", "")
	t.Setenv("DAVIT_HOST", "")
	t.Setenv("DAVIT_SSH", os.Args[0])
	t.Setenv("DAVIT_FAKESSH", "1")
	t.Setenv(fakeSSHDir, e.sshDir)
	old := connectRetry
	connectRetry = 20 * time.Millisecond
	t.Cleanup(func() { connectRetry = old })

	// A daemon that answers /capabilities.
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(api.BasePath+"/capabilities", func(w http.ResponseWriter, _ *http.Request) {
		var c api.HostCapabilities
		c.Version, c.Runtime.Engine = "v9.9", "fake"
		json.NewEncoder(w).Encode(c)
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	e.touch("sock", sock)
	return e
}

func (e *connectEnv) touch(name, content string) {
	if err := os.WriteFile(filepath.Join(e.sshDir, name), []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *connectEnv) rm(name string) { os.Remove(filepath.Join(e.sshDir, name)) }

func (e *connectEnv) authorize() {
	pub, err := os.ReadFile(filepath.Join(e.home, ".ssh", "id_ed25519.pub"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.touch("authorized_keys", string(pub))
}

func (e *connectEnv) run(args ...string) (stdout, stderr string, code int) {
	var o, er bytes.Buffer
	code = Main(args, Stdio{Stdin: strings.NewReader(""), Stdout: &o, Stderr: &er})
	return o.String(), er.String(), code
}

func (e *connectEnv) savedHost() string {
	b, err := os.ReadFile(filepath.Join(e.cfg, "davit", "client.json"))
	if err != nil {
		return ""
	}
	var c clientConfig
	json.Unmarshal(b, &c)
	return c.Host
}

// TestConnectFreshMachine: a machine with no key gets one, is told how to
// authorize it (and nothing is saved), and connects once it is authorized;
// the key is never replaced.
func TestConnectFreshMachine(t *testing.T) {
	e := newConnectEnv(t)
	key := filepath.Join(e.home, ".ssh", "id_ed25519")

	so, se, code := e.run("connect", "u@h.invalid")
	if code != ExitAuthn {
		t.Fatalf("denied: exit %d\n%s\n%s", code, so, se)
	}
	fi, err := os.Stat(key)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key not created 0600: %v %v", fi, err)
	}
	pub, _ := os.ReadFile(key + ".pub")
	for _, want := range []string{"(created)", "then run `davit connect u@h.invalid` again", "ssh-copy-id -i " + key + ".pub u@h.invalid", strings.TrimSpace(string(pub)) + "' >> ~/.ssh/authorized_keys"} {
		if !strings.Contains(so, want) {
			t.Errorf("denied output lacks %q:\n%s", want, so)
		}
	}
	if !strings.Contains(se, "does not accept this machine's SSH key") || e.savedHost() != "" {
		t.Fatalf("denied: stderr %q, saved %q", se, e.savedHost())
	}

	before, _ := os.ReadFile(key)
	if _, _, code := e.run("connect", "u@h.invalid"); code != ExitAuthn {
		t.Fatalf("still denied: exit %d", code)
	}
	if after, _ := os.ReadFile(key); !bytes.Equal(before, after) {
		t.Fatal("the existing key was replaced")
	}

	e.authorize()
	so, se, code = e.run("connect", "u@h.invalid")
	if code != 0 || !strings.Contains(so, "✓ connected to u@h.invalid (davit v9.9, runtime fake)") ||
		!strings.Contains(so, "next: cd your-repo && davit box run -- make test") {
		t.Fatalf("authorized: exit %d\n%s\n%s", code, so, se)
	}
	if want := "ssh://u@h.invalid?key=" + key; e.savedHost() != want {
		t.Fatalf("saved %q, want %q", e.savedHost(), want)
	}
	// Every later command uses the saved host.
	if _, se, code := e.run("capabilities"); code != 0 {
		t.Fatalf("capabilities through the saved host: exit %d %s", code, se)
	}
	if so, se, code := e.run("status"); code != 0 || !strings.Contains(so, "remote u@h.invalid over ssh") {
		t.Fatalf("status of the saved host: exit %d\n%s\n%s", code, so, se)
	}
	if so, se, code := e.run("connect", "--check"); code != 0 || !strings.Contains(so, "✓ connected") || strings.Contains(so, "next:") {
		t.Fatalf("--check: exit %d\n%s\n%s", code, so, se)
	}
}

// TestConnectWaitsForAuthorization: in wait mode connect retries until the
// key is authorized.
func TestConnectWaitsForAuthorization(t *testing.T) {
	e := newConnectEnv(t)
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(filepath.Join(e.home, ".ssh", "id_ed25519.pub")); err == nil {
				time.Sleep(50 * time.Millisecond)
				e.authorize()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	so, se, code := e.run("connect", "u@h.invalid", "--wait", "30s")
	if code != 0 || !strings.Contains(so, "waiting up to 30s") || !strings.Contains(so, "✓ connected") {
		t.Fatalf("wait: exit %d\n%s\n%s", code, so, se)
	}
}

// TestConnectFailuresSayWhatToDo: each first-run failure names its fix,
// from connect and from any command over the saved host.
func TestConnectFailuresSayWhatToDo(t *testing.T) {
	e := newConnectEnv(t)
	if _, se, code := e.run("list"); code != ExitTransport || !strings.Contains(se, "no davit host configured: run `davit connect user@host`") {
		t.Fatalf("no host: exit %d %q", code, se)
	}
	e.run("connect", "u@h.invalid") // creates the key
	e.authorize()
	if _, _, code := e.run("connect", "u@h.invalid"); code != 0 {
		t.Fatal("connect")
	}
	for _, tc := range []struct {
		file, want string
		exit       int
	}{
		{"unreachable", "cannot reach u@h.invalid over ssh: ssh: connect to host h.invalid port 22: Connection refused", ExitTransport},
		{"no-davit", "davit is not installed on u@h.invalid (or not on its PATH)", ExitConflict},
		{"no-daemon", "davit is installed on u@h.invalid but its daemon is not running", ExitTransport},
	} {
		e.touch(tc.file, "")
		if _, se, code := e.run("connect", "--check"); code != tc.exit || !strings.Contains(se, tc.want) {
			t.Errorf("connect --check, %s: exit %d %q", tc.file, code, se)
		}
		if _, se, code := e.run("box", "ls"); code == 0 || !strings.Contains(se, tc.want) {
			t.Errorf("box ls, %s: exit %d %q", tc.file, code, se)
		}
		e.rm(tc.file)
	}
	e.rm("authorized_keys")
	if _, se, code := e.run("list"); code != ExitAuthn || !strings.Contains(se, "does not accept this machine's SSH key: run `davit connect u@h.invalid`") {
		t.Errorf("revoked key: exit %d %q", code, se)
	}
}

// TestConnectViaAndJSON: --via is ProxyJump on every session, saved with
// the host; --json is envelopes only.
func TestConnectViaAndJSON(t *testing.T) {
	e := newConnectEnv(t)
	e.run("connect", "u@h.invalid")
	e.authorize()
	os.Remove(filepath.Join(e.sshDir, "args"))
	so, se, code := e.run("connect", "u@h.invalid:2222", "--via", "jump@bastion.invalid", "--json")
	if code != 0 {
		t.Fatalf("exit %d %s %s", code, so, se)
	}
	var last Envelope
	for _, l := range strings.Split(strings.TrimSpace(so), "\n") {
		if err := json.Unmarshal([]byte(l), &last); err != nil {
			t.Fatalf("non-JSON stdout line %q", l)
		}
	}
	data, _ := last.Data.(map[string]any)
	if last.Event != "result" || !strings.HasPrefix(data["host"].(string), "ssh://u@h.invalid:2222?jump=jump@bastion.invalid&key=") {
		t.Fatalf("result %+v", last)
	}
	args, _ := os.ReadFile(filepath.Join(e.sshDir, "args"))
	for _, l := range strings.Split(strings.TrimSpace(string(args)), "\n") {
		if !strings.Contains(l, "-p 2222") || !strings.Contains(l, "-J jump@bastion.invalid") {
			t.Errorf("session without the port or jump: %s", l)
		}
	}
}

func TestHostURLForms(t *testing.T) {
	for in, want := range map[string]string{
		"u@h":                 "ssh://u@h",
		"ssh://u@h:22/s.sock": "ssh://u@h:22/s.sock",
		"box-host":            "http://box-host",
		"https://h.ts.net/":   "https://h.ts.net",
	} {
		if got, err := hostURL(in); err != nil || got != want {
			t.Errorf("hostURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestSSHTargetRoundTrip: String parses back to the same target.
func TestSSHTargetRoundTrip(t *testing.T) {
	in := sshTarget{user: "u", host: "h", port: "2222", socket: "/run/d.sock", jump: "j@b:22", key: "/k dir/a&b=c%"}
	u, err := url.Parse(in.String())
	if err != nil {
		t.Fatal(err)
	}
	out, err := parseSSHTarget(u)
	if err != nil || out != in {
		t.Fatalf("%s -> %+v, %v", in.String(), out, err)
	}
}
