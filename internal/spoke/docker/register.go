package docker

import (
	"context"

	"github.com/clems4ever/llmbox/internal/spoke/box/backend"
)

// init registers the Docker backend so importing this package makes "docker"
// selectable through backend.New.
//
// @testcase TestDockerBackendRegistered checks importing this package registers the docker backend.
func init() {
	backend.Register("docker", newBackend)
}

// newBackend builds a Docker Provisioner from neutral backend options, applying
// the Docker-specific inputs (limits, registry auths, GPUs, namespace) that were
// previously set through individual setters at the composition roots. The
// microVM-only fields in opts are ignored. When a box-port service is
// configured, the box-port API listeners of boxes that survived a spoke
// restart are recovered before the provisioner is handed out.
//
// @arg opts The neutral backend options; Docker reads the common fields plus GPUs and RegistryAuths.
// @return backend.Provisioner A configured Docker provisioner.
// @error error if the Docker client cannot be created or the GPU spec is malformed.
//
// @testcase TestNewBackendConfiguresProvisioner builds a Docker backend and applies the options.
// @testcase TestNewBackendRejectsBadGPUs errors when the GPU spec is malformed.
func newBackend(opts backend.Options) (backend.Provisioner, error) {
	p, err := NewProvisioner(opts.DefaultImage, opts.SocketDir, opts.Peers, opts.BoxPorts)
	if err != nil {
		return nil, err
	}
	p.SetPerBoxLimits(opts.Limits)
	p.SetRegistryAuths(opts.RegistryAuths)
	if err := p.SetBoxGPUs(opts.GPUs); err != nil {
		return nil, err
	}
	p.SetNamespace(opts.Namespace)
	// Boxes persist across spoke restarts (restart policy unless-stopped); their
	// host-side box-port listeners do not. Restart them now, after the namespace
	// is pinned so recovery only sees this spoke's boxes. Best-effort: a daemon
	// hiccup here must not stop the spoke from starting.
	if err := p.RecoverBoxAPIs(context.Background()); err != nil {
		p.logger().Warn("failed to recover box-port APIs", "err", err)
	}
	return p, nil
}
