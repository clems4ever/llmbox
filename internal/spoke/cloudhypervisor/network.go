package cloudhypervisor

import (
	"github.com/clems4ever/llmbox/internal/spoke/microvm/mvmnet"
)

// The Cloud Hypervisor backend's egress networking (the pool of host TAP devices plus
// the shared NAT/isolation rules) is the same VMM-agnostic layer the Firecracker
// backend uses, in internal/spoke/microvm/mvmnet. This file binds it to Cloud
// Hypervisor's own addressing so the two backends never collide on a shared host:
// Cloud Hypervisor uses the "llmboxch" TAP prefix and the 172.17.0.0/16 space, versus
// Firecracker's "llmboxfc" / 172.16.0.0/16.

// chNet is the Cloud Hypervisor backend's egress addressing.
var chNet = mvmnet.Config{TapPrefix: "llmboxch", SubnetBase: "172.17"}

// The backend uses its own local names for the shared egress modes, matching the
// Firecracker backend's style.
type egressMode = mvmnet.EgressMode

const (
	egressManaged  = mvmnet.EgressManaged
	egressExternal = mvmnet.EgressExternal
	egressDisabled = mvmnet.EgressDisabled
)
