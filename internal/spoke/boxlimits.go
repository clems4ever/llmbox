package spoke

import (
	"github.com/clems4ever/llmbox/internal/shared/boxconfig"
	"github.com/clems4ever/llmbox/internal/shared/sandbox"
)

// BoxLimits converts a box config block into the per-box sandbox.Limits,
// translating the operator-friendly units (mebibytes, fractional CPUs) into the
// raw byte / nano-CPU counts the Docker API expects. A zero field stays zero
// (unlimited) so the conversion preserves "no limit" semantics. It lives on the
// spoke because only the spoke turns a box config into the sandbox limits its
// backend consumes.
//
// @arg b The box resource configuration (from YAML on the hub, or flags on the spoke).
// @return sandbox.Limits The equivalent per-box caps and max-box ceiling.
//
// @testcase TestBoxLimitsConvertsUnits converts mebibytes and CPUs to bytes and nano-CPUs.
func BoxLimits(b boxconfig.BoxConfig) sandbox.Limits {
	return sandbox.Limits{
		MemoryBytes: int64(b.MemoryMB) * 1024 * 1024,
		NanoCPUs:    int64(b.CPUs * 1e9),
		PidsLimit:   b.PidsLimit,
		MaxBoxes:    b.MaxBoxes,
	}
}
