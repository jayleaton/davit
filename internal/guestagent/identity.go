//go:build linux

package guestagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"davit/internal/guestproto"
)

// System is the guest operating system seen by the agent. The production
// implementation (NewSystem) uses syscalls and systemctl; tests substitute
// a recorder.
type System interface {
	// SetClock steps CLOCK_REALTIME.
	SetClock(t time.Time) error
	// SetHostname sets the kernel hostname (the file is written separately).
	SetHostname(name string) error
	// RestartUnits restarts (or starts) systemd units, in order.
	RestartUnits(ctx context.Context, units []string) error
	// SystemState is the overall systemd state (is-system-running).
	SystemState(ctx context.Context) guestproto.ServiceState
	// UnitState is one unit's state (is-active).
	UnitState(ctx context.Context, unit string) guestproto.ServiceState
	// SSHReady reports whether sshd answers with an SSH banner.
	SSHReady(ctx context.Context) bool
	// PowerOff shuts the guest down.
	PowerOff(ctx context.Context) error
}

// Identity file locations relative to Config.Root, and the in-guest paths
// sshd is configured with.
const (
	machineIDPath    = "etc/machine-id"
	dbusMachineID    = "var/lib/dbus/machine-id"
	hostnamePath     = "etc/hostname"
	hostsPath        = "etc/hosts"
	sshDir           = "etc/ssh"
	sshDropIn        = "etc/ssh/sshd_config.d/50-davit.conf"
	hostKeyName      = "ssh_host_ed25519_key"
	userCAName       = "davit_user_ca.pub"
	stampPath        = "var/lib/davit/identity.json"
	bootIDPath       = "proc/sys/kernel/random/boot_id"
	guestSSHDir      = "/etc/ssh/"
	hostsLoopbackTag = "127.0.1.1"
)

// stamp records which VM the disk's identity (machine-id, host keys)
// belongs to. It lives on the disk, so a disk cloned from another VM (fork,
// golden, or a resume from a source snapshot) carries the other VM's stamp
// and is rotated even if the host did not ask.
type stamp struct {
	VMID string `json:"vmId"`
}

func (a *Agent) path(rel string) string { return filepath.Join(a.cfg.Root, rel) }

// runBarrier is SPEC §5 steps 2-4. Readiness flips only after every step,
// including the secret rotation, has succeeded.
func (a *Agent) runBarrier(ctx context.Context, req guestproto.SetIdentity) (guestproto.IdentityResult, error) {
	a.barrier.Lock()
	defer a.barrier.Unlock()
	lap := newLaps()

	// Step 2: trusted wall clock before anything time-sensitive.
	if err := a.cfg.System.SetClock(time.Unix(0, req.WallClockUnixNano)); err != nil {
		return guestproto.IdentityResult{}, internal("set clock", err)
	}
	lap.mark("clock")

	// Step 3: hostname, machine-id, SSH host keys.
	if err := a.setHostname(req.Hostname); err != nil {
		return guestproto.IdentityResult{}, internal("set hostname", err)
	}
	lap.mark("hostname")
	var st stamp
	if raw, err := os.ReadFile(a.path(stampPath)); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	foreign := st.VMID != req.VMID
	var restart []string

	mid, err := readMachineID(a.path(machineIDPath))
	midRotated := req.RotateMachineID || foreign || err != nil
	if midRotated {
		if mid, err = a.rotateMachineID(); err != nil {
			return guestproto.IdentityResult{}, internal("rotate machine-id", err)
		}
		restart = append(restart, a.cfg.MachineIDUnits...)
	}
	lap.mark("machineId")

	sshChanged := false
	if _, err := os.Stat(a.path(filepath.Join(sshDir, hostKeyName))); req.RotateSSHHostKeys || foreign || err != nil {
		if err := a.rotateHostKeys(); err != nil {
			return guestproto.IdentityResult{}, internal("rotate ssh host keys", err)
		}
		sshChanged = true
	}
	lap.mark("hostKeys")
	// Step 4 (SSH part): trusted user CA and pinned host key.
	changed, err := a.writeSSHConfig(req.SSHUserCAKeys)
	if err != nil {
		return guestproto.IdentityResult{}, internal("write sshd config", err)
	}
	sshRestart := (sshChanged || changed) && a.cfg.SSHUnit != "-"
	lap.mark("sshConfig")
	// Restarts run off the critical path, started as soon as the keys and
	// sshd config are in place (sshd reads them from the page cache).
	// machine-id units only refresh cached IDs (journald's _MACHINE_ID).
	// sshd must not serve the old host keys: SSH readiness stays false
	// until its restart finished.
	a.restartAsync(restart, sshRestart)

	// Identity files were written without per-file fsyncs; one syncfs makes
	// them durable before the stamp that declares them this VM's.
	if err := syncFS(a.path("etc")); err != nil {
		return guestproto.IdentityResult{}, internal("sync identity files", err)
	}
	if err := os.MkdirAll(filepath.Dir(a.path(stampPath)), 0o755); err != nil {
		return guestproto.IdentityResult{}, internal("write identity stamp", err)
	}
	raw, _ := json.Marshal(stamp{VMID: req.VMID})
	if err := writeAtomic(a.path(stampPath), raw, 0o644); err != nil {
		return guestproto.IdentityResult{}, internal("write identity stamp", err)
	}
	lap.mark("syncAndStamp")

	fps, err := a.hostKeyFingerprints()
	if err != nil {
		return guestproto.IdentityResult{}, internal("read ssh host keys", err)
	}
	// Step 4 (agent credential) last: readiness flips with it.
	if err := a.rotate(req.VMID, req.Generation, req.NewAgentSecret); err != nil {
		return guestproto.IdentityResult{}, err
	}
	lap.mark("secret")
	bootID, _ := os.ReadFile(a.path(bootIDPath))
	a.cfg.Log.Info("restore barrier complete", append([]any{"vm", req.VMID, "generation", req.Generation,
		"rotatedMachineId", midRotated, "rotatedHostKeys", sshChanged, "restarting", restart, "sshRestart", sshRestart}, lap.attrs()...)...)
	return guestproto.IdentityResult{
		VMID: req.VMID, Generation: req.Generation, Hostname: req.Hostname,
		MachineID: mid, SSHHostKeyFingerprints: fps, BootID: strings.TrimSpace(string(bootID)),
	}, nil
}

func internal(what string, err error) error {
	return guestproto.Errorf(guestproto.CodeInternal, "%s: %v", what, err)
}

func (a *Agent) setHostname(name string) error {
	if err := os.MkdirAll(a.path("etc"), 0o755); err != nil {
		return err
	}
	if err := writeNoSync(a.path(hostnamePath), []byte(name+"\n"), 0o644); err != nil {
		return err
	}
	// Keep "127.0.1.1 <hostname>" so sudo and friends resolve the name.
	hosts, err := os.ReadFile(a.path(hostsPath))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(hosts), "\n"), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == hostsLoopbackTag {
			continue
		}
		if line != "" || len(out) > 0 {
			out = append(out, line)
		}
	}
	out = append(out, hostsLoopbackTag+"\t"+name)
	if err := writeNoSync(a.path(hostsPath), []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		return err
	}
	return a.cfg.System.SetHostname(name)
}

func readMachineID(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if b, err := hex.DecodeString(id); err != nil || len(b) != 16 || strings.ToLower(id) != id || strings.Trim(id, "0") == "" {
		return "", fmt.Errorf("invalid machine-id %q", id)
	}
	return id, nil
}

// rotateMachineID writes a fresh random ID (UUID v4 layout, as
// systemd-id128 new). PID 1 and dbus-daemon keep the old ID cached in
// memory; golden preparation must run before anything depends on it.
func (a *Agent) rotateMachineID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	id := hex.EncodeToString(b[:])
	if err := os.MkdirAll(a.path("etc"), 0o755); err != nil {
		return "", err
	}
	if err := writeNoSync(a.path(machineIDPath), []byte(id+"\n"), 0o444); err != nil {
		return "", err
	}
	// Older images keep a separate D-Bus copy; a symlink follows /etc.
	if fi, err := os.Lstat(a.path(dbusMachineID)); err == nil && fi.Mode().IsRegular() {
		if err := writeNoSync(a.path(dbusMachineID), []byte(id+"\n"), 0o444); err != nil {
			return "", err
		}
	}
	return id, nil
}

// rotateHostKeys removes every existing host key (so no key type is shared
// with the source VM) and generates a new ed25519 key.
func (a *Agent) rotateHostKeys() error {
	dir := a.path(sshDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	old, err := filepath.Glob(filepath.Join(dir, "ssh_host_*_key*"))
	if err != nil {
		return err
	}
	// ed25519 and ECDSA always (images configure both, and an image unit
	// that finds one missing would run ssh-keygen on the critical path); RSA
	// only if the image shipped one, since 3072-bit keygen is slow.
	kinds := []string{keyEd25519, keyECDSA}
	for _, p := range old {
		if filepath.Base(p) == "ssh_host_rsa_key" {
			kinds = append(kinds, keyRSA)
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	comment := "root@" + a.hostnameOr("localhost")
	for _, kind := range kinds {
		priv, pub, err := newHostKey(kind, comment)
		if err != nil {
			return err
		}
		name := "ssh_host_" + kind + "_key"
		if err := writeNoSync(filepath.Join(dir, name), priv, 0o600); err != nil {
			return err
		}
		if err := writeNoSync(filepath.Join(dir, name+".pub"), pub, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) hostnameOr(def string) string {
	raw, err := os.ReadFile(a.path(hostnamePath))
	if h := strings.TrimSpace(string(raw)); err == nil && h != "" {
		return h
	}
	return def
}

// writeSSHConfig pins sshd to the rotated host key and installs the user CA
// keys. It reports whether anything changed.
func (a *Agent) writeSSHConfig(caKeys []string) (bool, error) {
	var conf bytes.Buffer
	conf.WriteString("# Managed by the davit guest agent; rewritten by every restore barrier.\n")
	for _, kind := range []string{keyEd25519, keyECDSA, keyRSA} {
		name := "ssh_host_" + kind + "_key"
		if _, err := os.Stat(a.path(filepath.Join(sshDir, name))); err == nil {
			conf.WriteString("HostKey " + guestSSHDir + name + "\n")
		}
	}
	caPath := a.path(filepath.Join(sshDir, userCAName))
	changed := false
	if len(caKeys) > 0 {
		var ca bytes.Buffer
		for _, k := range caKeys {
			if strings.ContainsAny(k, "\r\n") {
				return false, fmt.Errorf("user CA key contains a newline")
			}
			ca.WriteString(strings.TrimSpace(k) + "\n")
		}
		c, err := writeIfChanged(caPath, ca.Bytes(), 0o644)
		if err != nil {
			return false, err
		}
		changed = changed || c
		conf.WriteString("TrustedUserCAKeys " + guestSSHDir + userCAName + "\n")
	} else if err := os.Remove(caPath); err == nil {
		changed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(a.path(sshDropIn)), 0o755); err != nil {
		return false, err
	}
	c, err := writeIfChanged(a.path(sshDropIn), conf.Bytes(), 0o644)
	return changed || c, err
}

func writeIfChanged(path string, data []byte, mode os.FileMode) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return false, nil
	}
	return true, writeNoSync(path, data, mode)
}

// hostKeyFingerprints returns the sorted SHA256 fingerprints of every host
// public key, in ssh-keygen -l form ("SHA256:<base64>").
func (a *Agent) hostKeyFingerprints() ([]string, error) {
	pubs, err := filepath.Glob(filepath.Join(a.path(sshDir), "ssh_host_*_key.pub"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range pubs {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		f := strings.Fields(string(raw))
		if len(f) < 2 {
			return nil, fmt.Errorf("malformed public key %s", filepath.Base(p))
		}
		blob, err := base64.StdEncoding.DecodeString(f[1])
		if err != nil {
			return nil, fmt.Errorf("malformed public key %s", filepath.Base(p))
		}
		sum := sha256.Sum256(blob)
		out = append(out, "SHA256:"+base64.RawStdEncoding.EncodeToString(sum[:]))
	}
	sort.Strings(out)
	return out, nil
}

// readiness answers ReadinessProbe. Agent is true only once the barrier has
// completed for the session's generation.
func (a *Agent) readiness(ctx context.Context, s *session, services []string) guestproto.Readiness {
	fresh, failed := a.sshState()
	r := guestproto.Readiness{
		Agent:    a.barrierDone(s.vmID, s.gen),
		SSH:      fresh && a.cfg.System.SSHReady(ctx),
		Services: map[string]guestproto.ServiceState{},
	}
	if failed {
		r.Services[guestproto.ServiceSSHRestart] = guestproto.ServiceFailed
	}
	for _, name := range services {
		if name == "systemd" { // systemctl is-system-running costs a fork; only on request
			r.Services[name] = a.cfg.System.SystemState(ctx)
			continue
		}
		r.Services[name] = a.cfg.System.UnitState(ctx, name)
	}
	return r
}

// laps times barrier steps for the completion log (milliseconds).
type laps struct {
	start, last time.Time
	attr        []any
}

func newLaps() *laps { now := time.Now(); return &laps{start: now, last: now} }

func (l *laps) mark(step string) {
	now := time.Now()
	l.attr = append(l.attr, step+"Ms", float64(now.Sub(l.last).Microseconds())/1000)
	l.last = now
}

func (l *laps) attrs() []any {
	return append(l.attr, "totalMs", float64(time.Since(l.start).Microseconds())/1000)
}

// restartAsync restarts machine-id units and (if sshRestart) the SSH unit
// in the background. Each barrier that needs an sshd restart starts a new
// epoch; SSH counts as fresh only once the latest epoch's restart finished.
func (a *Agent) restartAsync(units []string, sshRestart bool) {
	if len(units) > 0 {
		go func() {
			if err := a.cfg.System.RestartUnits(context.Background(), units); err != nil {
				a.cfg.Log.Warn("restart machine-id units", "units", units, "err", err)
			}
		}()
	}
	a.mu.Lock()
	if !sshRestart {
		a.sshDone, a.sshFailed = a.sshEpoch, false
		a.mu.Unlock()
		return
	}
	a.sshEpoch++
	epoch := a.sshEpoch
	a.sshFailed = false
	a.mu.Unlock()
	go func() {
		t0 := time.Now()
		err := a.cfg.System.RestartUnits(context.Background(), []string{a.cfg.SSHUnit})
		a.mu.Lock()
		if epoch == a.sshEpoch {
			if err == nil {
				a.sshDone = epoch
			} else {
				a.sshFailed = true
			}
		}
		a.mu.Unlock()
		if err != nil {
			a.cfg.Log.Error("restart sshd after identity change", "err", err)
		} else {
			a.cfg.Log.Info("sshd restarted with this VM's host keys", "ms", float64(time.Since(t0).Microseconds())/1000)
		}
	}()
}

// sshState reports whether sshd runs this barrier's keys and config, and
// whether its restart failed.
func (a *Agent) sshState() (fresh, failed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sshDone == a.sshEpoch, a.sshFailed
}
