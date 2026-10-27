package cli

import (
	"context"
	"os"
	"syscall"
	"testing"

	"davit/internal/domain"
	"davit/internal/lifecycle"
	"davit/internal/storage"
)

// noSpaceStore fails every disk write with ENOSPC, like a full host.
type noSpaceStore struct{ storage.Store }

func (noSpaceStore) CreateWritable(context.Context, storage.DiskID, string, int64) (storage.Disk, error) {
	return storage.Disk{}, &os.PathError{Op: "write", Path: "disk.img", Err: syscall.ENOSPC}
}

// TestDiskFullExitCode: both ways a full disk surfaces end in exit 8 with a
// retryable resource_exhausted JSON error — refused up front below the
// free-space floor, and an operation that hits ENOSPC while running.
func TestDiskFullExitCode(t *testing.T) {
	t.Run("below floor", func(t *testing.T) {
		serveWith(t, func(c *lifecycle.Config) {
			c.DiskSpace = func() (domain.DiskUsage, error) { return domain.DiskUsage{FreeBytes: 10 * domain.MiB}, nil }
		})
		r := runCLI(t, false, "new", "web")
		if r.code != ExitExhaustion || r.last.Error == nil || r.last.Error.Code != domain.CodeResourceExhausted ||
			!r.last.Error.Retryable || r.last.Error.Origin != "server" {
			t.Fatalf("exit %d %+v", r.code, r.last.Error)
		}
	})
	t.Run("ENOSPC during the operation", func(t *testing.T) {
		serveWith(t, func(c *lifecycle.Config) { c.Storage = noSpaceStore{c.Storage} })
		r := runCLI(t, false, "new", "web")
		if r.code != ExitExhaustion || r.last.Error == nil || r.last.Error.Code != domain.CodeResourceExhausted ||
			r.last.Error.Origin != "operation" || r.last.Error.OperationID == "" {
			t.Fatalf("exit %d %+v", r.code, r.last.Error)
		}
		// The daemon keeps serving: the failed VM is listed in error state.
		if vm := runCLI(t, false, "info", "web"); vm.code != 0 || vm.data["state"] != "error" {
			t.Fatalf("info after ENOSPC: exit %d %+v", vm.code, vm.data)
		}
	})
}
