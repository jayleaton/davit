//go:build !linux && !darwin

package preflight

import (
	"context"
	"errors"
	goruntime "runtime"

	"davit/internal/domain"
)

func platformChecks(context.Context, Options) []Check {
	return []Check{{ID: "platform", Title: "Host platform", Status: Fail,
		Detail: goruntime.GOOS + " is not supported", Hint: "use a Linux host"}}
}

func FreeBytes(string) (int64, error) { return 0, errUnsupported }

func DiskSpace(string) (domain.DiskUsage, error) { return domain.DiskUsage{}, errUnsupported }

func SyncFS(string) error { return errUnsupported }

func SameFilesystem(string, string) bool { return false }

var errUnsupported = errors.New("not supported on this OS")

// KVM reports that this OS has no supported hypervisor.
func KVM() Check {
	return Check{ID: "kvm", Title: "KVM access", Status: Fail, Detail: goruntime.GOOS + " is not supported", Hint: "use a Linux host"}
}
