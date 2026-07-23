package netfw

import (
	"net/netip"
	"sort"
	"sync"
)

// RecordingProgrammer is an in-memory Programmer that records the egress policy it
// was asked to apply instead of touching a real firewall. It is the fake the
// enforcement engine is unit-tested against (no root, no nft), and the reference
// for what a real Programmer must end up with: per box, the DNS resolver address
// and the current allow set. Safe for concurrent use.
type RecordingProgrammer struct {
	mu    sync.Mutex
	boxes map[string]*boxState
}

type boxState struct {
	spec  BoxSpec
	allow map[netip.Addr]bool
}

// NewRecordingProgrammer builds an empty recording programmer.
//
// @return *RecordingProgrammer A ready fake.
func NewRecordingProgrammer() *RecordingProgrammer {
	return &RecordingProgrammer{boxes: map[string]*boxState{}}
}

// Baseline records the box's spec and starts it with an empty allow set.
//
// @arg boxID The box.
// @arg spec The box's source prefix and DNS resolver.
// @error error Always nil.
func (r *RecordingProgrammer) Baseline(boxID string, spec BoxSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.boxes[boxID] = &boxState{spec: spec, allow: map[netip.Addr]bool{}}
	return nil
}

// Spec returns the recorded spec for a box, for assertions.
//
// @arg boxID The box.
// @return BoxSpec The recorded spec (zero when the box is unknown).
// @return bool True when the box has a baseline.
func (r *RecordingProgrammer) Spec(boxID string) (BoxSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.boxes[boxID]
	if b == nil {
		return BoxSpec{}, false
	}
	return b.spec, true
}

// Allow records ip as open for the box.
//
// @arg boxID The box.
// @arg ip The IP to open.
// @error error Always nil.
func (r *RecordingProgrammer) Allow(boxID string, ip netip.Addr) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.boxes[boxID] == nil {
		r.boxes[boxID] = &boxState{allow: map[netip.Addr]bool{}}
	}
	r.boxes[boxID].allow[ip] = true
	return nil
}

// Revoke records ip as closed for the box.
//
// @arg boxID The box.
// @arg ip The IP to close.
// @error error Always nil.
func (r *RecordingProgrammer) Revoke(boxID string, ip netip.Addr) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b := r.boxes[boxID]; b != nil {
		delete(b.allow, ip)
	}
	return nil
}

// Teardown forgets the box entirely.
//
// @arg boxID The box.
// @error error Always nil.
func (r *RecordingProgrammer) Teardown(boxID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.boxes, boxID)
	return nil
}

// Allowed reports whether ip is currently open for the box — the assertion hook
// tests use to check the firewall ended up in the expected state.
//
// @arg boxID The box.
// @arg ip The IP to check.
// @return bool True when ip is in the box's allow set.
func (r *RecordingProgrammer) Allowed(boxID string, ip netip.Addr) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.boxes[boxID]
	return b != nil && b.allow[ip]
}

// AllowList returns the box's current allow set, sorted, for assertions.
//
// @arg boxID The box.
// @return []netip.Addr The sorted open IPs.
func (r *RecordingProgrammer) AllowList(boxID string) []netip.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.boxes[boxID]
	if b == nil {
		return nil
	}
	out := make([]netip.Addr, 0, len(b.allow))
	for ip := range b.allow {
		out = append(out, ip)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}
