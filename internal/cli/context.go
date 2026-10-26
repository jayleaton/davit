package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"davit/api"
	"davit/internal/buildinfo"
	"davit/internal/domain"
)

// Saved context (SPEC §3): a per-user, per-host default VM for interactive
// terminals. Resolution order for a VM argument is explicit ID, then
// DAVIT_CURRENT_ID, then saved context — and saved context only when stdin
// is a terminal, so scripts never depend on another shell's mutable state.

// savedContext is one host's entry.
type savedContext struct {
	VMID  domain.VMID `json:"vmId"`
	Name  string      `json:"name"`
	SetAt time.Time   `json:"setAt"`
}

type contextFile struct {
	Hosts map[string]savedContext `json:"hosts"`
}

// contextPath is $XDG_CONFIG_HOME/davit/context.json (or the OS user
// config dir).
func contextPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", clientErr(domain.CodeInternal, "no user config directory: %v", err)
	}
	return filepath.Join(dir, buildinfo.Name, "context.json"), nil
}

func readContexts() (*contextFile, string, error) {
	path, err := contextPath()
	if err != nil {
		return nil, "", err
	}
	f := &contextFile{Hosts: map[string]savedContext{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, path, nil
	}
	if err == nil {
		err = json.Unmarshal(b, f)
	}
	if err != nil {
		return nil, "", clientErr(domain.CodeInternal, "reading %s: %v", path, err)
	}
	if f.Hosts == nil {
		f.Hosts = map[string]savedContext{}
	}
	return f, path, nil
}

// updateContext applies fn to the host's entry (nil clears it) and writes the
// file atomically with owner-only permissions.
func updateContext(host string, fn func(cur *savedContext) *savedContext) error {
	f, path, err := readContexts()
	if err != nil {
		return err
	}
	var cur *savedContext
	if c, ok := f.Hosts[host]; ok {
		cur = &c
	}
	next := fn(cur)
	if next == nil {
		if cur == nil {
			return nil
		}
		delete(f.Hosts, host)
	} else {
		f.Hosts[host] = *next
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// hostKey scopes saved context to the host the CLI talks to.
func (s *session) hostKey() string { return s.g.apiURL }

// currentID resolves the VM argument: explicit, then DAVIT_CURRENT_ID,
// then (interactive terminals only) the saved context for this host.
func (s *session) currentID(args []string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		return args[0], nil
	}
	if id := os.Getenv(buildinfo.Env("CURRENT_ID")); id != "" {
		return id, nil
	}
	if s.io.Interactive {
		f, _, err := readContexts()
		if err != nil {
			return "", err
		}
		if c, ok := f.Hosts[s.hostKey()]; ok {
			s.out.diag("using context %s (%s)", c.Name, c.VMID)
			return string(c.VMID), nil
		}
	}
	return "", usageErr("no VM given: pass an ID or name, set %s, or (interactive terminals only) run `%s context use ID`",
		buildinfo.Env("CURRENT_ID"), buildinfo.Name)
}

// selectContext saves vm as this host's context after an interactive new or
// fork; scripts never change it implicitly.
func (s *session) selectContext(vm *api.VM) {
	if !s.io.Interactive || vm == nil || vm.ID == "" {
		return
	}
	if err := updateContext(s.hostKey(), func(*savedContext) *savedContext {
		return &savedContext{VMID: vm.ID, Name: vm.Name, SetAt: time.Now().UTC()}
	}); err == nil {
		s.out.diag("context: %s (%s)", vm.Name, vm.ID)
	}
}

// forgetContext clears the saved context if it names vm (after deletion).
func (s *session) forgetContext(vm domain.VMID) {
	updateContext(s.hostKey(), func(cur *savedContext) *savedContext {
		if cur != nil && cur.VMID == vm {
			return nil
		}
		return cur
	})
}

func runContext(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) == 0 {
		return usageErr("usage: %s context use ID | show | clear", buildinfo.Name)
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "use":
		if len(rest) != 1 {
			return usageErr("usage: %s context use ID", buildinfo.Name)
		}
		var vm api.VM
		if err := s.cl.get(ctx, vmPath(rest[0]), nil, &vm); err != nil {
			return err
		}
		if vm.State == domain.StateDeleted {
			return clientErr(domain.CodeNotFound, "vm %s is deleted", vm.ID)
		}
		c := savedContext{VMID: vm.ID, Name: vm.Name, SetAt: time.Now().UTC()}
		if err := updateContext(s.hostKey(), func(*savedContext) *savedContext { return &c }); err != nil {
			return err
		}
		s.out.result("", map[string]any{"host": s.hostKey(), "context": c})
		s.out.human("context: %s (%s)", c.Name, c.VMID)
		return nil
	case "show":
		if len(rest) != 0 {
			return usageErr("usage: %s context show", buildinfo.Name)
		}
		f, _, err := readContexts()
		if err != nil {
			return err
		}
		res := map[string]any{"host": s.hostKey(), "interactive": s.io.Interactive, "source": "none", "current": ""}
		saved, hasSaved := f.Hosts[s.hostKey()]
		if hasSaved {
			res["saved"] = saved
		}
		env := os.Getenv(buildinfo.Env("CURRENT_ID"))
		switch {
		case env != "":
			res["source"], res["current"] = "env", env
		case hasSaved && s.io.Interactive:
			res["source"], res["current"] = "saved", saved.VMID
		}
		s.out.result("", res)
		switch {
		case env != "":
			s.out.human("current: %s (from %s)", env, buildinfo.Env("CURRENT_ID"))
		case hasSaved && s.io.Interactive:
			s.out.human("current: %s (%s)", saved.Name, saved.VMID)
		case hasSaved:
			s.out.human("saved context %s (%s) applies to interactive terminals only", saved.Name, saved.VMID)
		default:
			s.out.human("no context")
		}
		return nil
	case "clear":
		if len(rest) != 0 {
			return usageErr("usage: %s context clear", buildinfo.Name)
		}
		if err := updateContext(s.hostKey(), func(*savedContext) *savedContext { return nil }); err != nil {
			return err
		}
		s.out.result("", map[string]any{"host": s.hostKey(), "cleared": true})
		s.out.human("context cleared")
		return nil
	}
	return usageErr("unknown context subcommand %q; use use, show or clear", args[0])
}

func runSnapshot(ctx context.Context, s *session, o *opts, args []string) error {
	if len(args) == 0 {
		return usageErr("usage: %s snapshot create [ID] NAME | snapshot delete SNAPSHOT | snapshot tree|pull|export SNAPSHOT ...", buildinfo.Name)
	}
	if ok, err := snapshotFilesSub(ctx, s, o, args[0], args[1:]); ok {
		return err
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "create":
		var ref, name string
		switch len(rest) {
		case 1:
			id, err := s.currentID(nil)
			if err != nil {
				return err
			}
			ref, name = id, rest[0]
		case 2:
			ref, name = rest[0], rest[1]
		default:
			return usageErr("usage: %s snapshot create [ID] NAME", buildinfo.Name)
		}
		return s.lifecycle(ctx, o, "POST", vmPath(ref)+"/snapshots", api.SnapshotVMRequest{Name: name})
	case "delete":
		if len(rest) != 1 {
			return usageErr("usage: %s snapshot delete SNAPSHOT [--yes]", buildinfo.Name)
		}
		path := "/snapshots/" + url.PathEscape(rest[0])
		var snap api.Snapshot
		if err := s.cl.get(ctx, path, nil, &snap); err != nil {
			return err
		}
		if err := s.confirm(o, "Delete snapshot "+orDash(snap.Name)+" ("+string(snap.ID)+")?"); err != nil {
			return err
		}
		if err := s.cl.do(ctx, "DELETE", "/snapshots/"+url.PathEscape(string(snap.ID)), nil, nil, nil, nil); err != nil {
			return err
		}
		s.out.result("", map[string]any{"deleted": snap})
		s.out.human("deleted snapshot %s (%s)", orDash(snap.Name), snap.ID)
		return nil
	}
	return usageErr("unknown snapshot subcommand %q; use create, delete, tree, pull or export (list with `%s snapshots`)", args[0], buildinfo.Name)
}
