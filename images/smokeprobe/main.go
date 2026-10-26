// Command smokeprobe drives a booted image's baked guest agent from the host with the product's
// own host code, for images/scripts/smoke.py. It is image tooling, not part of the davit binary.
//
//	smokeprobe -uds <firecracker vsock uds> -vm <vmId> -generation N -secrets <dir> [-harness claude,codex]
//
// It runs guestclient.Prober.Barrier (SPEC §5 restore barrier: authenticated session, clock,
// hostname, machine-id and SSH host-key rotation, secret rotation, readiness incl. sshd) over
// Firecracker hybrid vsock ("CONNECT <port>\n"), then opens a second session with the rotated
// secret and execs as root and as the image's default user. It prints one JSON object and exits
// non-zero if any step fails.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"davit/internal/domain"
	"davit/internal/guestclient"
	"davit/internal/guestproto"
	"davit/internal/runtime"
)

type report struct {
	BarrierMs       float64                   `json:"barrierMs"`
	AgentVersion    string                    `json:"agentVersion"`
	ProtocolVersion uint16                    `json:"protocolVersion"`
	HostFeatures    []string                  `json:"hostFeatures"`
	Features        []string                  `json:"features"` // negotiated
	Identity        guestproto.IdentityResult `json:"identity"`
	Readiness       domain.Readiness          `json:"readiness"`
	ExecRootUID     string                    `json:"execRootUid"`
	ExecDefaultUser string                    `json:"execDefaultUser"`
	ClockSkewMs     float64                   `json:"clockSkewMs"`
	Harness         map[string]string         `json:"harness,omitempty"`
	Error           string                    `json:"error,omitempty"`
}

func main() {
	uds := flag.String("uds", "", "Firecracker vsock Unix socket (hybrid vsock)")
	vm := flag.String("vm", "", "VM ID the barrier assigns")
	gen := flag.Int64("generation", 1, "instance generation (1 rotates identity, like a new VM or fork)")
	secrets := flag.String("secrets", "", "directory for per-generation secrets")
	harness := flag.String("harness", "", "comma-separated harness commands to run --version for")
	timeout := flag.Duration("timeout", 90*time.Second, "overall deadline")
	flag.Parse()
	if *uds == "" || *vm == "" || *secrets == "" {
		flag.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	rep, err := run(ctx, *uds, runtime.Key{VMID: domain.VMID(*vm), Generation: *gen}, *secrets, *harness)
	if err != nil {
		rep.Error = err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(rep)
	if err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, uds string, key runtime.Key, secretDir, harness string) (report, error) {
	var rep report
	dial := hybridVsock(uds, guestclient.DefaultPort)
	store := guestclient.DirSecrets{Dir: secretDir}
	p := &guestclient.Prober{
		Secrets: store,
		OnIdentity: func(_ context.Context, _ runtime.Key, id guestproto.IdentityResult) error {
			rep.Identity = id
			return nil
		},
	}
	t0 := time.Now()
	ready, err := p.Barrier(ctx, key, dial, nil)
	rep.BarrierMs = float64(time.Since(t0).Microseconds()) / 1000
	if err != nil {
		return rep, fmt.Errorf("barrier: %w", err)
	}
	rep.Readiness = ready

	// A later session must authenticate with the secret the barrier rotated in.
	cands, err := store.Load(ctx, key)
	if err != nil {
		return rep, err
	}
	c, err := guestclient.Connect(ctx, dial, guestclient.Session{VMID: string(key.VMID), Generation: uint64(key.Generation), Secrets: cands})
	if err != nil {
		return rep, fmt.Errorf("second session: %w", err)
	}
	defer c.Close()
	rep.AgentVersion, rep.ProtocolVersion = c.AgentVersion(), c.Version()
	rep.HostFeatures, rep.Features = guestclient.Features, []string{}
	for _, f := range guestclient.Features {
		if c.Has(f) {
			rep.Features = append(rep.Features, f)
		}
	}

	if rep.ExecRootUID, err = execOut(ctx, c, "root", "id", "-u"); err != nil {
		return rep, err
	}
	if rep.ExecDefaultUser, err = execOut(ctx, c, "", "id", "-un"); err != nil {
		return rep, err
	}
	before := time.Now()
	ns, err := execOut(ctx, c, "", "date", "+%s%N")
	if err != nil {
		return rep, err
	}
	g, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return rep, fmt.Errorf("guest date %q: %w", ns, err)
	}
	mid := before.Add(time.Since(before) / 2)
	rep.ClockSkewMs = float64(g-mid.UnixNano()) / 1e6

	if harness != "" {
		rep.Harness = map[string]string{}
		for _, cmd := range strings.Split(harness, ",") {
			// Same argv shape internal/harness uses for a bare command name (Service.argv).
			out, err := execOut(ctx, c, "", "sh", "-lc", `exec "$0" "$@"`, cmd, "--version")
			if err != nil {
				return rep, fmt.Errorf("harness %s: %w", cmd, err)
			}
			rep.Harness[cmd] = out
		}
	}
	return rep, nil
}

// execOut runs argv in the guest and returns trimmed stdout; a non-zero exit is an error.
func execOut(ctx context.Context, c *guestclient.Client, user string, argv ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	res, err := c.Exec(ctx, guestproto.Exec{Argv: argv, User: user, DeadlineSeconds: 60},
		guestclient.ExecIO{Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return "", fmt.Errorf("exec %v: %w", argv, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exec %v: exit %d (%s): %s", argv, res.ExitCode, res.Status, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// hybridVsock dials Firecracker's host-side vsock socket and asks for the guest port.
func hybridVsock(uds string, port uint32) guestclient.DialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", uds)
		if err != nil {
			return nil, err
		}
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		}
		if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
			conn.Close()
			return nil, err
		}
		line, err := readLine(conn)
		if err != nil || !strings.HasPrefix(line, "OK ") {
			conn.Close()
			return nil, fmt.Errorf("vsock CONNECT %d: %q %v", port, line, err)
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

// readLine reads one '\n'-terminated line a byte at a time, so no protocol bytes are consumed.
func readLine(conn net.Conn) (string, error) {
	var b strings.Builder
	var c [1]byte
	for b.Len() < 64 {
		if _, err := conn.Read(c[:]); err != nil {
			return b.String(), err
		}
		if c[0] == '\n' {
			return b.String(), nil
		}
		b.WriteByte(c[0])
	}
	return b.String(), fmt.Errorf("line too long")
}
