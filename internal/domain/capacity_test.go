package domain

import "testing"

func TestMemoryFloor(t *testing.T) {
	for _, c := range []struct{ set, want int64 }{
		{0, DefaultMinFreeMemoryBytes}, {-1, 0}, {GiB, GiB},
	} {
		if got := (Limits{MinFreeMemoryBytes: c.set}).MemoryFloor(); got != c.want {
			t.Errorf("floor(%d) = %d, want %d", c.set, got, c.want)
		}
	}
}

func TestAdmitMemory(t *testing.T) {
	l := Limits{MinFreeMemoryBytes: GiB}
	if err := l.AdmitMemory(3*GiB, 2*GiB); err != nil {
		t.Fatalf("exactly at the floor: %v", err)
	}
	err := l.AdmitMemory(3*GiB, 2*GiB+1)
	de := AsError(err)
	if de.Code != CodeResourceExhausted || !de.Retryable || de.Details["resource"] != "memory" ||
		de.Details["freeBytes"] != int64(3*GiB) || de.Details["needBytes"] != int64(3*GiB+1) || de.Details["floorBytes"] != int64(GiB) {
		t.Fatalf("below the floor: %+v", de)
	}
	if err := (Limits{MinFreeMemoryBytes: -1}).AdmitMemory(0, 64*GiB); err != nil {
		t.Fatalf("disabled floor: %v", err)
	}
	if err := (Limits{}).AdmitMemory(DefaultMinFreeMemoryBytes+GiB-1, GiB); err == nil {
		t.Fatal("default floor not applied")
	}
}

func TestAdmitVMs(t *testing.T) {
	if err := (Limits{}).AdmitVMs(10000); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	l := Limits{VMs: 2}
	if err := l.AdmitVMs(1); err != nil {
		t.Fatal(err)
	}
	de := AsError(l.AdmitVMs(2))
	if de.Code != CodeResourceExhausted || !de.Retryable || de.Details["resource"] != "vms" ||
		de.Details["live"] != 2 || de.Details["limit"] != 2 {
		t.Fatalf("at the limit: %+v", de)
	}
}

func TestSlots(t *testing.T) {
	sh := Shape{CPUs: 2, MemoryBytes: 2 * GiB, DiskBytes: 8 * GiB}
	none := Limits{MinFreeMemoryBytes: -1, MinFreeDiskBytes: -1}
	for _, c := range []struct {
		name      string
		l         Limits
		used      Shape
		live      int
		mem, disk int64
		want      int
	}{
		{"unlimited, unmeasured", none, Shape{}, 0, -1, -1, MaxSlots},
		{"vm limit", Limits{VMs: 5, MinFreeMemoryBytes: -1}, Shape{}, 3, -1, -1, 2},
		{"vm limit exceeded", Limits{VMs: 2}, Shape{}, 3, -1, -1, 0},
		{"cpu", Limits{CPUs: 7}, Shape{CPUs: 2}, 1, -1, -1, 2},
		{"memory limit", Limits{MemoryBytes: 9 * GiB, MemoryOvercommit: 1}, Shape{}, 0, -1, -1, 4},
		{"memory limit, default overcommit 1.5", Limits{MemoryBytes: 9 * GiB}, Shape{}, 0, -1, -1, 6},
		{"disk limit", Limits{DiskBytes: 30 * GiB}, Shape{DiskBytes: 8 * GiB}, 0, -1, -1, 2},
		{"memory floor", Limits{MinFreeMemoryBytes: GiB}, Shape{}, 0, 8 * GiB, -1, 3},
		{"memory below floor", Limits{MinFreeMemoryBytes: GiB}, Shape{}, 0, GiB / 2, -1, 0},
		{"disk floor", Limits{MinFreeDiskBytes: 2 * GiB}, Shape{}, 0, -1, GiB, 0},
		{"disk above floor", Limits{MinFreeDiskBytes: 2 * GiB, MinFreeMemoryBytes: -1}, Shape{}, 0, -1, 3 * GiB, MaxSlots},
		{"min of all", Limits{VMs: 10, CPUs: 64, MinFreeMemoryBytes: GiB}, Shape{}, 0, 5 * GiB, -1, 2},
	} {
		if got := c.l.Slots(c.used, c.live, c.mem, c.disk, sh); got != c.want {
			t.Errorf("%s: %d slots, want %d", c.name, got, c.want)
		}
	}
}
