package credproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"davit/internal/domain"
	"davit/internal/network"
	"davit/internal/network/nethelper"
)

// TestMain lets the test binary serve the network helpers (the in-netns
// listener) and act as a guest HTTP client.
func TestMain(m *testing.M) {
	nethelper.Serve()
	if len(os.Args) == 4 && os.Args[1] == "guest-get" {
		os.Exit(guestGet(os.Args[2], os.Args[3]))
	}
	os.Exit(m.Run())
}

// guestGet GETs url with X-Api-Key: key and prints "STATUS BODY".
func guestGet(url, key string) int {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-Api-Key", key)
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		fmt.Println("ERR", err)
		return 1
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println(resp.StatusCode, string(b))
	return 0
}

// TestProxyThroughVMNetwork: the whole path on a real rootless VM network.
// A client inside the VM's namespace reaches the proxy at TapIP:GuestPort,
// the daemon-side listener resolves its placeholder for that VM only, and
// the request lands on the upstream (in the host namespace) with the real
// key. The host namespace itself cannot reach that address.
func TestProxyThroughVMNetwork(t *testing.T) {
	cfg := network.DefaultConfig()
	cfg.TapServices = []uint16{GuestPort}
	r, err := network.NewRootless(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range r.Probe(context.Background()).Modes {
		if m.Mode == network.ModeRootless && !m.Usable {
			t.Skip("rootless network namespaces unavailable: " + m.Reason)
		}
	}
	ctx := context.Background()
	nh, err := r.Create(ctx, "vm_a", 1, domain.Egress{Mode: domain.EgressOff})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Destroy(ctx, nh) })

	h, _, _ := newHost(t)
	h.Listen = func(ctx context.Context, nh network.Handle) (net.Listener, error) {
		return r.ListenTCP(ctx, nh, GuestPort)
	}
	up := newUpstream(t)
	g := grantFor(t, h, up.URL)
	h.AttachNetwork(ctx, "vm_a", nh)
	t.Cleanup(func() { h.DetachNetwork("vm_a") })

	exe, _ := os.Executable()
	get := func(key string) string {
		argv := append(network.EnterArgv(nh), exe, "guest-get", GuestBaseURL(cfg)+"/v1/models", key)
		out, _ := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	if got := get(g.Placeholder); got != `200 {"path":"/base/v1/models"}` {
		t.Fatalf("guest request through the VM network: %q", got)
	}
	if rs := up.requests(); len(rs) != 1 || rs[0].Header.Get("X-Api-Key") != realKey {
		t.Fatalf("upstream saw %d requests: %v", len(rs), rs)
	}
	if got := get("sk-guess"); !strings.HasPrefix(got, "401 ") {
		t.Errorf("no placeholder: %q", got)
	}
	if c, err := net.DialTimeout("tcp", net.JoinHostPort(cfg.TapIP.String(), fmt.Sprint(GuestPort)), time.Second); err == nil {
		c.Close()
		t.Error("the host namespace reached the VM's proxy listener")
	}
	h.DetachNetwork("vm_a")
	if got := get(g.Placeholder); !strings.HasPrefix(got, "ERR") {
		t.Errorf("after detach: %q", got)
	}
}
