//go:build !windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSSHConnWriteAfterExit: a session that ended before any data (here a
// host without davit: the remote shell's "not found", exit 127) explains
// itself on the first write too, not only on a read. The client writes its
// request first, so when the remote exits before that write lands it would
// otherwise surface as a bare "broken pipe".
func TestSSHConnWriteAfterExit(t *testing.T) {
	ssh := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(ssh, []byte("#!/bin/sh\necho 'sh: 1: davit: not found' >&2\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	conn, err := dialSSH(context.Background(), ssh, nil, "u@h.invalid")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-conn.(*sshConn).exited:
	case <-time.After(10 * time.Second):
		t.Fatal("fake ssh did not exit")
	}
	_, err = conn.Write([]byte("GET /api/v1/vms HTTP/1.1\r\nHost: local\r\n\r\n"))
	if err == nil || !strings.Contains(err.Error(), "davit is not installed on u@h.invalid") {
		t.Fatalf("write after the session ended: %v", err)
	}
}
