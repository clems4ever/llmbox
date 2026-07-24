package firecracker

import (
	"github.com/clems4ever/llmbox/internal/spoke/microvm/mvmnet"
)

// The Firecracker backend's egress networking (the pool of host TAP devices plus the
// shared NAT/isolation rules) lives in the VMM-agnostic internal/spoke/microvm/mvmnet
// package, shared with the Cloud Hypervisor backend. This file binds that shared
// layer to Firecracker's historical addressing so nothing about a deployment's host
// interfaces or box IPs changes: the same "llmboxfc" TAP prefix and 172.16.0.0/16
// space as before. Only the box NIC MAC stays Firecracker-local (macForIndex), since
// its AA:FC:.. scheme predates the shared layer.

// fcNet is Firecracker's egress addressing: the historical llmboxfc TAP prefix and
// 172.16.0.0/16 guest space, kept byte-identical to the pre-extraction backend.
var fcNet = mvmnet.Config{TapPrefix: "llmboxfc", SubnetBase: "172.16"}

// The backend keeps its established local names for the shared types and modes, so
// the provisioner, register, operator, and CLI read exactly as before.
type (
	boxNet     = mvmnet.BoxNet
	egress     = mvmnet.Egress
	hostEgress = mvmnet.HostEgress
	egressMode = mvmnet.EgressMode
)

const (
	egressManaged  = mvmnet.EgressManaged
	egressExternal = mvmnet.EgressExternal
	egressDisabled = mvmnet.EgressDisabled
)

// netFor derives the addressing for a pool slot on Firecracker's TAP prefix/subnet.
//
// @arg index The pool slot.
// @return boxNet The slot's TAP name and host/guest addresses.
//
// @testcase TestNetFor derives distinct, well-formed /30s for different slots.
func netFor(index int) boxNet { return fcNet.NetFor(index) }

// tapName is the deterministic pool TAP device name for a slot on Firecracker's
// prefix.
//
// @arg index The pool slot.
// @return string The TAP device name for the slot.
//
// @testcase TestNetFor derives the pool TAP name for a slot.
func tapName(index int) string { return fcNet.TapName(index) }

// parseEgressMode resolves an --egress-mode value to a mode (empty = managed).
//
// @arg s The flag value.
// @return egressMode The parsed mode.
// @error error if s names no known mode.
//
// @testcase TestParseEgressMode accepts every mode and rejects an unknown one.
func parseEgressMode(s string) (egressMode, error) { return mvmnet.ParseEgressMode(s) }

// newHostEgress builds a host egress on Firecracker's addressing, optionally
// overriding the uplink and the owning TAP GID (0 keeps them unset). It is the single
// place the backend constructs the shared HostEgress, so every call carries fcNet's
// prefix/subnet.
//
// @arg uplink The uplink interface to masquerade out of; empty resolves the default route.
// @arg tapGroup The owning GID for created TAP devices; 0 leaves them root-owned.
// @return *hostEgress A host egress bound to Firecracker's addressing.
//
// @testcase TestSetupNetworkPoolRequiresRoot builds a host egress through the pool setup.
func newHostEgress(uplink string, tapGroup int) *hostEgress {
	cfg := fcNet
	cfg.Uplink = uplink
	cfg.TapGroup = tapGroup
	return mvmnet.NewHostEgress(cfg)
}
