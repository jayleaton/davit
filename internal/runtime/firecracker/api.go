//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// apiClient talks to one VMM's API socket.
type apiClient struct {
	c *http.Client
}

func newAPI(sock string) *apiClient {
	return &apiClient{c: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialUnix(ctx, sock)
			},
			DisableKeepAlives: true,
		},
		Timeout: 30 * time.Second,
	}}
}

func (a *apiClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://fc"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var f struct {
			Fault string `json:"fault_message"`
		}
		if json.Unmarshal(b, &f) == nil && f.Fault != "" {
			return fmt.Errorf("firecracker %s %s: %s", method, path, f.Fault)
		}
		return fmt.Errorf("firecracker %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// state returns "Not started", "Running" or "Paused".
func (a *apiClient) state(ctx context.Context) (string, error) {
	var info struct {
		State string `json:"state"`
	}
	err := a.do(ctx, http.MethodGet, "/", nil, &info)
	return info.State, err
}

func (a *apiClient) action(ctx context.Context, typ string) error {
	return a.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": typ}, nil)
}

func (a *apiClient) setPaused(ctx context.Context, paused bool) error {
	s := "Resumed"
	if paused {
		s = "Paused"
	}
	return a.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": s}, nil)
}

func (a *apiClient) snapshotCreate(ctx context.Context, vmstate, mem string) error {
	return a.do(ctx, http.MethodPut, "/snapshot/create", map[string]any{
		"snapshot_type": "Full", "snapshot_path": vmstate, "mem_file_path": mem,
	}, nil)
}

// snapshotCreateNoSync is snapshotCreate without fsyncing the files (the
// data stays in the page cache for same-host restores).
func (a *apiClient) snapshotCreateNoSync(ctx context.Context, vmstate, mem string) error {
	return a.do(ctx, http.MethodPut, "/snapshot/create", map[string]any{
		"snapshot_type": "Full", "snapshot_path": vmstate, "mem_file_path": mem, "sync_snapshot_files": false,
	}, nil)
}

func (a *apiClient) snapshotLoad(ctx context.Context, vmstate, mem string) error {
	return a.do(ctx, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": vmstate,
		"mem_backend":   map[string]string{"backend_type": "File", "backend_path": mem},
		"resume_vm":     true,
	}, nil)
}

// vmConfig is Firecracker's --config-file document.
type vmConfig struct {
	BootSource struct {
		Kernel   string `json:"kernel_image_path"`
		BootArgs string `json:"boot_args"`
	} `json:"boot-source"`
	Drives  []fcDrive `json:"drives"`
	Machine struct {
		VCPUs  int   `json:"vcpu_count"`
		MemMiB int64 `json:"mem_size_mib"`
		SMT    bool  `json:"smt"`
	} `json:"machine-config"`
	NICs  []fcNIC `json:"network-interfaces,omitempty"`
	Vsock struct {
		CID  uint32 `json:"guest_cid"`
		Path string `json:"uds_path"`
	} `json:"vsock"`
}

type fcDrive struct {
	ID       string `json:"drive_id"`
	Path     string `json:"path_on_host"`
	Root     bool   `json:"is_root_device"`
	ReadOnly bool   `json:"is_read_only"`
}

type fcNIC struct {
	ID  string `json:"iface_id"`
	MAC string `json:"guest_mac"`
	Tap string `json:"host_dev_name"`
}
