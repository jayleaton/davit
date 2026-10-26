package fake_test

import (
	"testing"
	"time"

	"davit/internal/runtime"
	"davit/internal/runtime/fake"
	"davit/internal/runtime/runtimetest"
)

func TestConformance(t *testing.T) {
	open := func(t *testing.T, dir string) runtime.Runtime {
		rt, err := fake.New(fake.Options{Dir: dir, BootDelay: 5 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}
	runtimetest.Run(t, runtimetest.Harness{New: open, Reopen: open, Disk: runtimetest.TempDisk})
	live := func(t *testing.T, dir string) runtime.Runtime {
		rt, err := fake.New(fake.Options{Dir: dir, BootDelay: 5 * time.Millisecond, LiveFork: true})
		if err != nil {
			t.Fatal(err)
		}
		return rt
	}
	t.Run("LiveFork", func(t *testing.T) {
		runtimetest.Run(t, runtimetest.Harness{New: live, Reopen: live, Disk: runtimetest.TempDisk, CloneDisk: runtimetest.CopyDisk})
	})
}
