package spoke

import (
	"testing"

	"github.com/clems4ever/llmbox/internal/shared/boxconfig"
)

// TestBoxLimitsConvertsUnits checks the operator-friendly box config units
// (mebibytes, fractional CPUs) are converted to the raw byte / nano-CPU counts
// the Docker API expects, and that zero stays zero (unlimited).
func TestBoxLimitsConvertsUnits(t *testing.T) {
	got := BoxLimits(boxconfig.BoxConfig{MemoryMB: 2048, CPUs: 1.5, PidsLimit: 512, MaxBoxes: 7, DiskGB: 10, MaxDiskGB: 100})
	if got.MemoryBytes != 2048*1024*1024 {
		t.Errorf("MemoryBytes = %d, want %d", got.MemoryBytes, 2048*1024*1024)
	}
	if got.DiskBytes != 10*boxconfig.GiB {
		t.Errorf("DiskBytes = %d, want %d", got.DiskBytes, int64(10*boxconfig.GiB))
	}
	if got.MaxDiskBytes != 100*boxconfig.GiB {
		t.Errorf("MaxDiskBytes = %d, want %d", got.MaxDiskBytes, int64(100*boxconfig.GiB))
	}
	if got.NanoCPUs != 1_500_000_000 {
		t.Errorf("NanoCPUs = %d, want 1500000000", got.NanoCPUs)
	}
	if got.PidsLimit != 512 {
		t.Errorf("PidsLimit = %d, want 512", got.PidsLimit)
	}
	if got.MaxBoxes != 7 {
		t.Errorf("MaxBoxes = %d, want 7", got.MaxBoxes)
	}
	if zero := BoxLimits(boxconfig.BoxConfig{}); zero.MemoryBytes != 0 || zero.NanoCPUs != 0 || zero.PidsLimit != 0 || zero.MaxBoxes != 0 || zero.DiskBytes != 0 || zero.MaxDiskBytes != 0 {
		t.Errorf("zero config should stay unlimited, got %+v", zero)
	}
}
