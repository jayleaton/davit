package fctest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"davit/internal/runtime/firecracker"
)

// fakeVMMName is the basename a test binary is executed under to act as
// Firecracker (see FakeVMM).
const fakeVMMName = "fake-firecracker"

// FakeVMM lets a test binary stand in for the Firecracker binary, so the
// adapter's process supervision (spawn, Stop, Kill, Destroy, stray reaping)
// runs hermetically, without KVM guests. Call it first in TestMain: when the
// binary was executed as FakeBinary it acts as the VMM and never returns.
//
// The fake answers `--version`, serves the API socket (GET / reports
// Running or Paused, PATCH /vm pauses, PUT /actions) and the hybrid vsock
// socket ("CONNECT <port>" then "OK 1"; a "poweroff" line is the guest
// agent's power-off, "disk-write <text>" writes text at the start of the
// root drive and answers "ok", "disk-read <n>" answers the drive's first n
// bytes: a guest's view of its disk). How the "guest" reacts is set by the image boot args,
// the only input the adapter passes through (it clears the environment):
//
//	fake.cad=reboot|halt|ignore       Ctrl+Alt+Del (default reboot)
//	fake.poweroff=halt|reboot|ignore  agent power-off (default halt)
//	fake.delay=<duration>             before either takes effect (default 0)
//
// reboot exits 0 (reboot=k ends the VMM); halt prints the kernel's
// "reboot: System halted" on the console and keeps the VMM running, as a
// powered-off Firecracker guest does.
func FakeVMM() {
	if filepath.Base(os.Args[0]) != fakeVMMName {
		return
	}
	os.Exit(fakeVMM(os.Args[1:]))
}

// FakeBinary returns a path that executes this test binary as the fake VMM.
// It is a symlink, so the VMM's /proc/<pid>/exe is the test binary, which is
// what the adapter resolves the configured binary to.
func FakeBinary(t testing.TB) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), fakeVMMName)
	if err := os.Symlink(exe, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func fakeVMM(args []string) int {
	var apiSock, config string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--version":
			fmt.Println("Firecracker " + firecracker.FirecrackerVersion)
			return 0
		case "--api-sock":
			i++
			apiSock = args[i]
		case "--config-file":
			i++
			config = args[i]
		}
	}
	behave := map[string]string{"fake.cad": "reboot", "fake.poweroff": "halt", "fake.delay": "0s"}
	var root string
	if config != "" {
		var c struct {
			Boot struct {
				Args string `json:"boot_args"`
			} `json:"boot-source"`
			Drives []struct {
				Path string `json:"path_on_host"`
				Root bool   `json:"is_root_device"`
			} `json:"drives"`
		}
		b, err := os.ReadFile(config)
		if err == nil {
			err = json.Unmarshal(b, &c)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake firecracker:", err)
			return 1
		}
		for _, d := range c.Drives {
			if d.Root {
				root = d.Path
			}
		}
		for _, f := range strings.Fields(c.Boot.Args) {
			if k, v, ok := strings.Cut(f, "="); ok && strings.HasPrefix(k, "fake.") {
				behave[k] = v
			}
		}
	}
	delay, err := time.ParseDuration(behave["fake.delay"])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake firecracker:", err)
		return 1
	}
	var mu sync.Mutex
	state := "Running"
	act := func(what string) {
		go func() {
			time.Sleep(delay)
			mu.Lock() // one console writer; exit and halt never interleave
			defer mu.Unlock()
			switch what {
			case "reboot":
				os.Exit(0)
			case "halt":
				fmt.Println("reboot: System halted")
			}
		}()
	}

	api, err := net.Listen("unix", apiSock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake firecracker:", err)
		return 1
	}
	vsock, err := net.Listen("unix", "vsock.sock")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake firecracker:", err)
		return 1
	}
	go func() {
		for {
			c, err := vsock.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				if l, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(l, "CONNECT ") {
					return
				}
				fmt.Fprint(c, "OK 1\n")
				for {
					l, err := br.ReadString('\n')
					if err != nil {
						return
					}
					cmd, arg, _ := strings.Cut(strings.TrimSpace(l), " ")
					switch cmd {
					case "poweroff":
						act(behave["fake.poweroff"])
						return
					case "disk-write", "disk-read":
						fmt.Fprintln(c, fakeDisk(root, cmd, arg))
					default:
						return
					}
				}
			}()
		}
	}()
	http.Serve(api, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]string
		json.NewDecoder(req.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/":
			json.NewEncoder(w).Encode(map[string]string{"state": state})
		case req.Method == http.MethodPatch && req.URL.Path == "/vm":
			state = map[string]string{"Paused": "Paused", "Resumed": "Running"}[body["state"]]
			w.WriteHeader(http.StatusNoContent)
		case req.Method == http.MethodPut && req.URL.Path == "/actions" && body["action_type"] == "SendCtrlAltDel":
			act(behave["fake.cad"])
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"fault_message": "fake firecracker: unsupported " + req.Method + " " + req.URL.Path})
		}
	}))
	return 1
}

// fakeDisk is the guest's root drive access: "disk-write" puts text at
// offset 0 (not synced, like an unflushed guest write), "disk-read" returns
// the first n bytes.
func fakeDisk(root, cmd, arg string) string {
	if root == "" {
		return "error: no root drive"
	}
	if cmd == "disk-write" {
		f, err := os.OpenFile(root, os.O_WRONLY, 0)
		if err == nil {
			_, err = f.WriteAt([]byte(arg), 0)
			f.Close()
		}
		if err != nil {
			return "error: " + err.Error()
		}
		return "ok"
	}
	var n int
	if _, err := fmt.Sscan(arg, &n); err != nil || n <= 0 || n > 4096 {
		return "error: bad length"
	}
	f, err := os.Open(root)
	if err != nil {
		return "error: " + err.Error()
	}
	defer f.Close()
	b := make([]byte, n)
	m, _ := f.ReadAt(b, 0)
	return string(b[:m])
}
