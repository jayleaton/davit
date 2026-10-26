package firecracker

import (
	"testing"
	"time"

	"davit/internal/domain"
)

// TestGoldenBudget: the capture deadline grows with guest memory (the
// snapshot write), and an explicit timeout wins.
func TestGoldenBudget(t *testing.T) {
	for _, tc := range []struct {
		explicit time.Duration
		mem      int64
		want     time.Duration
	}{
		{0, 512 * domain.MiB, 80 * time.Second},
		{0, 2 * domain.GiB, 100 * time.Second},
		{0, 3 * domain.GiB, 120 * time.Second},
		{0, 3*domain.GiB + 1, 140 * time.Second},
		{45 * time.Second, 8 * domain.GiB, 45 * time.Second},
	} {
		if got := goldenBudget(tc.explicit, domain.Shape{MemoryBytes: tc.mem}); got != tc.want {
			t.Errorf("goldenBudget(%v, %d) = %v, want %v", tc.explicit, tc.mem, got, tc.want)
		}
	}
}
