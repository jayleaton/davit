package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"davit/api"
	"davit/internal/config"
)

// Migrating a host set up before the slipway -> davit rename
// (docs/migrating-from-slipway.md). The old layout is the new one with
// "slipway" for "davit" in every path: ~/.svm/slipway, ~/.config/slipway
// (or slipway-host), slipway.service. Migrate moves the data dir and the
// config dir, rewrites the config's paths, and retires the old unit; the
// VMs, their disks and snapshots move with the data dir, and the state
// database relabels the boxes itself on first open (migration 9). The
// journal records no absolute paths; what does (golden metadata, the blob
// verification cache) is rebuilt: goldens captured under the old path go
// stale and the daemon's sweep frees them, new ones are captured on the
// next create. It refuses while the old daemon has live VMs, unless StopVMs
// stops them first (a clean stop keeps their disks).

// LegacyName is the product's name before the rename.
const LegacyName = "slipway"

// MigrateOptions are the new paths; the legacy ones are derived from them.
type MigrateOptions struct {
	ConfigPath string // absolute, the davit config
	DataDir    string // absolute, the davit data dir
	StopVMs    bool
	// UnitDir and Systemctl as in Options (defaults alike).
	UnitDir   string
	Systemctl func(ctx context.Context, args ...string) error
	// StopTimeout bounds stopping one live VM (default 3m).
	StopTimeout time.Duration
}

// MigrateResult reports what Migrate did.
type MigrateResult struct {
	Migrated   bool     `json:"migrated"`
	Reason     string   `json:"reason,omitempty"` // why nothing was done
	FromConfig string   `json:"fromConfig,omitempty"`
	ToConfig   string   `json:"toConfig,omitempty"`
	FromData   string   `json:"fromData,omitempty"`
	ToData     string   `json:"toData,omitempty"`
	StoppedVMs []string `json:"stoppedVms,omitempty"`
	OldUnit    string   `json:"oldUnit,omitempty"`
}

// legacyPath is p with the old name for the new one in its last n
// components, the ones the product names (~/.svm/davit: 1;
// ~/.config/davit-host/config.json: 2); parent directories are the user's.
func legacyPath(p string, n int) string {
	parts := strings.Split(p, string(filepath.Separator))
	for i := max(0, len(parts)-n); i < len(parts); i++ {
		parts[i] = strings.ReplaceAll(parts[i], "davit", LegacyName)
	}
	return strings.Join(parts, string(filepath.Separator))
}

// Migrate moves a legacy host layout to o's paths. It does nothing when
// there is no legacy config or the davit config already exists.
func Migrate(ctx context.Context, o MigrateOptions) (*MigrateResult, error) {
	if !filepath.IsAbs(o.ConfigPath) || !filepath.IsAbs(o.DataDir) {
		return nil, fmt.Errorf("migrate: --config and --data-dir must be absolute paths")
	}
	svc := Options{ConfigPath: o.ConfigPath, DataDir: o.DataDir, UnitDir: o.UnitDir, Systemctl: o.Systemctl}
	if err := svc.defaults(); err != nil {
		return nil, err
	}
	if o.StopTimeout == 0 {
		o.StopTimeout = 3 * time.Minute
	}
	oldCfg, oldData := legacyPath(o.ConfigPath, 2), legacyPath(o.DataDir, 1)
	r := &MigrateResult{FromConfig: oldCfg, ToConfig: o.ConfigPath, FromData: oldData, ToData: o.DataDir}
	if _, err := os.Stat(o.ConfigPath); err == nil {
		r.Reason = o.ConfigPath + " exists: already migrated or set up as davit"
		return r, nil
	}
	if _, err := os.Stat(oldCfg); err != nil {
		r.Reason = "no " + LegacyName + " setup at " + oldCfg
		return r, nil
	}
	cfg, err := config.Load(oldCfg)
	if err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", oldCfg, err)
	}
	if cfg.DataDir != oldData {
		return nil, fmt.Errorf("migrate: %s uses data dir %s, not %s; pass --data-dir matching it with davit for %s", oldCfg, cfg.DataDir, oldData, LegacyName)
	}

	// 1. No live VMs (the old daemon stops them with StopVMs).
	stopped, err := stopLegacyVMs(ctx, cfg.Socket, oldData, o)
	r.StoppedVMs = stopped
	if err != nil {
		return r, err
	}
	// 2. Retire the old unit.
	oldUnit := filepath.Join(svc.UnitDir, LegacyName+".service")
	if _, err := os.Stat(oldUnit); err == nil {
		_ = svc.Systemctl(ctx, "disable", "--now", LegacyName+".service")
		if err := os.Remove(oldUnit); err != nil {
			return r, fmt.Errorf("migrate: remove %s: %w", oldUnit, err)
		}
		_ = svc.Systemctl(ctx, "daemon-reload")
		r.OldUnit = oldUnit
	}
	// 3. The data dir.
	if err := moveDir(oldData, o.DataDir); err != nil {
		return r, fmt.Errorf("migrate: move %s to %s: %w", oldData, o.DataDir, err)
	}
	_ = os.Remove(filepath.Join(o.DataDir, "bin", LegacyName)) // the old binary, when it lived there
	// 4. The config dir (the whole dir when it is the old product's own),
	// then the config's paths.
	oldDir, newDir := filepath.Dir(oldCfg), filepath.Dir(o.ConfigPath)
	if oldDir != newDir && strings.Contains(filepath.Base(oldDir), LegacyName) {
		if err := moveDir(oldDir, newDir); err != nil {
			return r, fmt.Errorf("migrate: move %s to %s: %w", oldDir, newDir, err)
		}
		if filepath.Base(oldCfg) != filepath.Base(o.ConfigPath) {
			if err := os.Rename(filepath.Join(newDir, filepath.Base(oldCfg)), o.ConfigPath); err != nil {
				return r, err
			}
		}
	} else {
		if err := os.MkdirAll(newDir, 0o700); err != nil {
			return r, err
		}
		if err := os.Rename(oldCfg, o.ConfigPath); err != nil {
			return r, err
		}
	}
	if err := rewriteConfigPaths(o.ConfigPath, map[string]string{oldData: o.DataDir, oldDir: newDir}); err != nil {
		return r, err
	}
	// The socket's own name too, so clients find it where davit puts it.
	if err := renameSocket(o.ConfigPath); err != nil {
		return r, err
	}
	if _, err := config.Load(o.ConfigPath); err != nil {
		return r, fmt.Errorf("migrate: the moved config does not load: %w", err)
	}
	r.Migrated = true
	return r, nil
}

// moveDir renames src to dst; when dst exists (an install staged its
// binary there already) it moves src's entries into it, one level deep
// for directories both have.
func moveDir(src, dst string) error {
	if _, err := os.Lstat(dst); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.Rename(src, dst)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		st, err := os.Lstat(d)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Rename(s, d); err != nil {
				return err
			}
		case err != nil:
			return err
		case e.IsDir() && st.IsDir():
			sub, err := os.ReadDir(s)
			if err != nil {
				return err
			}
			for _, f := range sub {
				if _, err := os.Lstat(filepath.Join(d, f.Name())); err == nil {
					return fmt.Errorf("%s exists in both", filepath.Join(d, f.Name()))
				}
				if err := os.Rename(filepath.Join(s, f.Name()), filepath.Join(d, f.Name())); err != nil {
					return err
				}
			}
			if err := os.Remove(s); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s exists in both", d)
		}
	}
	return os.Remove(src)
}

// rewriteConfigPaths replaces each old path prefix with its new one in
// every string of the config file (data dir, socket, image manifests and
// blobs, the tailnet state dir and auth key file, ...).
func rewriteConfigPaths(path string, prefixes map[string]string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			for old, nw := range prefixes {
				if t == old || strings.HasPrefix(t, old+string(filepath.Separator)) {
					return nw + strings.TrimPrefix(t, old)
				}
			}
			return t
		case map[string]any:
			for k, x := range t {
				t[k] = walk(x)
			}
		case []any:
			for i, x := range t {
				t[i] = walk(x)
			}
		}
		return v
	}
	out, err := json.MarshalIndent(walk(doc), "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(out, '\n'), 0o600)
}

// renameSocket renames a config's socket named slipway.sock to davit.sock.
func renameSocket(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	sock, _ := doc["socket"].(string)
	if filepath.Base(sock) != LegacyName+".sock" {
		return nil
	}
	doc["socket"] = filepath.Join(filepath.Dir(sock), "davit.sock")
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(out, '\n'), 0o600)
}

// stopLegacyVMs makes sure no VM of the old daemon is live: none, or (with
// StopVMs) each stopped cleanly through it. It returns the VMs it stopped.
func stopLegacyVMs(ctx context.Context, socket, data string, o MigrateOptions) ([]string, error) {
	cl := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
	}}}
	get := func(path string, out any) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://local"+api.BasePath+path, nil)
		resp, err := cl.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
	}
	var l api.VMList
	if err := get("/vms?limit=500", &l); err != nil {
		// The old daemon is not running: its VMMs would be orphans under the
		// data dir, which a move would break.
		if pids := vmmsUnder(data); len(pids) > 0 {
			return nil, fmt.Errorf("migrate: the %s daemon is not answering on %s but %d VMM processes run under %s; start it and stop the VMs first", LegacyName, socket, len(pids), data)
		}
		return nil, nil
	}
	var live []api.VM
	for _, vm := range l.Items {
		if vm.State.Live() {
			live = append(live, vm)
		}
	}
	if len(live) == 0 {
		return nil, nil
	}
	if !o.StopVMs {
		names := make([]string, len(live))
		for i, vm := range live {
			names[i] = vm.Name
		}
		return nil, fmt.Errorf("migrate: %d VMs are live on the %s daemon (%s): stop them (no running processes), or pass --stop-vms to stop them cleanly; disks are kept",
			len(live), LegacyName, strings.Join(names, ", "))
	}
	var stopped []string
	for _, vm := range live {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://local"+api.BasePath+"/vms/"+string(vm.ID)+"/stop", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(api.HeaderIdempotencyKey, "migrate-stop-"+string(vm.ID))
		resp, err := cl.Do(req)
		if err != nil {
			return stopped, fmt.Errorf("migrate: stop %s: %w", vm.Name, err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusConflict {
			return stopped, fmt.Errorf("migrate: stop %s: HTTP %d", vm.Name, resp.StatusCode)
		}
		deadline := time.Now().Add(o.StopTimeout)
		for {
			var cur api.VM
			if err := get("/vms/"+string(vm.ID), &cur); err == nil && !cur.State.Live() {
				break
			}
			if time.Now().After(deadline) {
				return stopped, fmt.Errorf("migrate: %s did not stop within %s", vm.Name, o.StopTimeout)
			}
			select {
			case <-ctx.Done():
				return stopped, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		stopped = append(stopped, vm.Name)
	}
	return stopped, nil
}

// vmmsUnder lists firecracker processes whose working directory is under
// dir (Linux; none elsewhere).
func vmmsUnder(dir string) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range ents {
		var pid int
		if _, err := fmt.Sscanf(e.Name(), "%d", &pid); err != nil {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if !strings.HasPrefix(strings.TrimSpace(string(comm)), "firecracker") {
			continue
		}
		if cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd")); err == nil && strings.HasPrefix(cwd, dir+"/") {
			out = append(out, pid)
		}
	}
	return out
}
