package domain

import (
	"strings"
	"testing"
)

func TestDiskFloor(t *testing.T) {
	for _, c := range []struct {
		name  string
		l     Limits
		total int64
		want  int64
	}{
		{"default: 5 GiB on a small disk", Limits{}, 20 * GiB, 5 * GiB},
		{"default: 10% of a large disk", Limits{}, 500 * GiB, 50 * GiB},
		{"default, size unknown", Limits{}, 0, 5 * GiB},
		{"bytes above the percent", Limits{MinFreeDiskBytes: 80 * GiB}, 500 * GiB, 80 * GiB},
		{"percent above the bytes", Limits{MinFreeDiskBytes: GiB, MinFreeDiskPercent: 20}, 100 * GiB, 20 * GiB},
		{"bytes only", Limits{MinFreeDiskBytes: GiB, MinFreeDiskPercent: -1}, 500 * GiB, GiB},
		{"disabled", Limits{MinFreeDiskBytes: -1, MinFreeDiskPercent: 50}, 500 * GiB, 0},
	} {
		if got := c.l.DiskFloor(c.total); got != c.want {
			t.Errorf("%s: floor %d, want %d", c.name, got, c.want)
		}
	}
}

func TestDiskHeadroomAndReserve(t *testing.T) {
	sh := Shape{DiskBytes: 8 * GiB}
	if got := (Limits{}).DiskHeadroom(sh); got != 2*GiB {
		t.Errorf("default headroom %d", got)
	}
	if got := (Limits{DiskHeadroomPercent: 100}).DiskHeadroom(sh); got != 8*GiB {
		t.Errorf("full headroom %d", got)
	}
	if got := (Limits{DiskHeadroomPercent: -1}).DiskHeadroom(sh); got != 0 {
		t.Errorf("no headroom %d", got)
	}
	if (Limits{}).SaveReserve() != 2*GiB || (Limits{SaveReserveBytes: GiB}).SaveReserve() != GiB || (Limits{SaveReserveBytes: -1}).SaveReserve() != 0 {
		t.Error("save reserve")
	}
}

// TestAdmitDisk: admitted while free space less the headroom reserved for
// starting VMs and this VM's stays at the floor; the refusal names the
// numbers, suggests box gc / box rm, and queues (a room error), unlike the
// disk allocation limit.
func TestAdmitDisk(t *testing.T) {
	l := Limits{MinFreeDiskBytes: 5 * GiB}
	d := DiskUsage{FreeBytes: 9 * GiB}
	if err := l.AdmitDisk(d, 2*GiB, 2*GiB); err != nil {
		t.Fatalf("exactly at the floor: %v", err)
	}
	err := l.AdmitDisk(d, 2*GiB, 2*GiB+1)
	e := AsError(err)
	if e.Code != CodeResourceExhausted || !e.Retryable || !IsDiskSpaceError(err) || !IsRoomError(err) {
		t.Fatalf("refusal: %+v", e)
	}
	if !strings.Contains(e.Message, "host disk low: 9.0 GiB free, need 9.0 GiB") || !strings.Contains(e.Message, "box gc") ||
		!strings.Contains(e.Message, "box rm") {
		t.Fatalf("message: %s", e.Message)
	}
	if e.Details["needBytes"] != 9*GiB+1 || e.Details["floorBytes"] != 5*GiB || e.Details["reservedBytes"] != 2*GiB {
		t.Fatalf("details: %+v", e.Details)
	}
	if err := (Limits{MinFreeDiskBytes: -1}).AdmitDisk(DiskUsage{}, 0, GiB); err != nil {
		t.Fatalf("disabled floor: %v", err)
	}
	limit := Limits{DiskBytes: GiB}.Admit(Shape{}, Shape{DiskBytes: 2 * GiB})
	if limit == nil || IsRoomError(limit) || IsDiskSpaceError(limit) {
		t.Fatalf("the disk allocation limit must not queue: %v", limit)
	}
}
