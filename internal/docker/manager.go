// Package docker wraps the Docker Engine API to manage the lifecycle of
// containers that run the Claude "remote-control" image.
//
// To stay safe, the manager only ever lists or destroys containers it created
// itself. Every container it creates is tagged with the ManagedLabel; list and
// destroy operations are scoped to that label so the server can never stop or
// remove an unrelated container on the host.
package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// dockerAPI is the subset of the Docker client used by Manager. It exists so
// the Docker layer can be faked in tests; *client.Client satisfies it.
type dockerAPI interface {
	ContainerList(ctx context.Context, opts container.ListOptions) ([]container.Summary, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, opts container.StartOptions) error
	ContainerRemove(ctx context.Context, containerID string, opts container.RemoveOptions) error
	Close() error
}

const (
	// ManagedLabel marks every container created by this server. Its value is
	// always "true" for managed containers.
	ManagedLabel = "com.llmbox.managed"

	// DefaultImage is the image launched when the caller does not specify one.
	// It matches the image name produced by Dockerfile.claude / docker-compose.
	DefaultImage = "claude-remote"
)

// Manager talks to the Docker daemon.
type Manager struct {
	cli          dockerAPI
	defaultImage string
}

// Container is a trimmed-down view of a managed container, suitable for
// returning to an MCP client.
type Container struct {
	ID      string   `json:"id" jsonschema:"the short container ID"`
	Name    string   `json:"name" jsonschema:"the container name"`
	Image   string   `json:"image" jsonschema:"the image the container runs"`
	State   string   `json:"state" jsonschema:"the container state, e.g. running or exited"`
	Status  string   `json:"status" jsonschema:"a human readable status string"`
	Created int64    `json:"created" jsonschema:"creation time as a unix timestamp"`
	Ports   []string `json:"ports,omitempty" jsonschema:"published port mappings"`
}

// NewManager creates a Manager using Docker configuration from the
// environment (DOCKER_HOST, etc.). defaultImage overrides DefaultImage when
// non-empty.
func NewManager(defaultImage string) (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}
	if defaultImage == "" {
		defaultImage = DefaultImage
	}
	return &Manager{cli: cli, defaultImage: defaultImage}, nil
}

// Close releases the underlying Docker client.
func (m *Manager) Close() error { return m.cli.Close() }

// managedFilter scopes an operation to containers created by this server.
func managedFilter() filters.Args {
	return filters.NewArgs(filters.Arg("label", ManagedLabel+"=true"))
}

// List returns all containers created by this server, running or not.
func (m *Manager) List(ctx context.Context) ([]Container, error) {
	cs, err := m.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: managedFilter(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	out := make([]Container, 0, len(cs))
	for _, c := range cs {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		ports := make([]string, 0, len(c.Ports))
		for _, p := range c.Ports {
			if p.PublicPort != 0 {
				ports = append(ports, fmt.Sprintf("%s:%d->%d/%s", p.IP, p.PublicPort, p.PrivatePort, p.Type))
			}
		}
		out = append(out, Container{
			ID:      c.ID[:12],
			Name:    name,
			Image:   c.Image,
			State:   c.State,
			Status:  c.Status,
			Created: c.Created,
			Ports:   ports,
		})
	}
	return out, nil
}

// CreateOptions configures a new Claude container.
type CreateOptions struct {
	// Image to launch; defaults to the Manager's default image when empty.
	Image string
	// Name for the container; Docker generates one when empty.
	Name string
	// Env are additional environment variables in KEY=VALUE form. Common keys
	// are CLAUDE_CODE_OAUTH_TOKEN and CLAUDE_REMOTE_ARGS.
	Env []string
}

// Create creates and starts a Claude container, returning the new container's
// ID. The container is tagged with ManagedLabel so it can later be listed and
// destroyed by this server.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (string, error) {
	image := opts.Image
	if image == "" {
		image = m.defaultImage
	}

	resp, err := m.cli.ContainerCreate(ctx,
		&container.Config{
			Image: image,
			Env:   opts.Env,
			// Remote-control needs a TTY to reach the "Ready" state.
			Tty:       true,
			OpenStdin: true,
			Labels:    map[string]string{ManagedLabel: "true"},
		},
		&container.HostConfig{
			// Auto-clean the filesystem layer once the container exits so
			// destroyed/finished sessions don't accumulate.
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		},
		nil, nil, opts.Name,
	)
	if err != nil {
		return "", fmt.Errorf("creating container from image %q: %w", image, err)
	}

	if err := m.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Best effort cleanup so we don't leave a created-but-not-started shell.
		_ = m.cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("starting container %s: %w", resp.ID[:12], err)
	}
	return resp.ID, nil
}

// Destroy stops and removes a managed container identified by ID or name. It
// refuses to touch containers that were not created by this server.
func (m *Manager) Destroy(ctx context.Context, idOrName string) error {
	c, err := m.findManaged(ctx, idOrName)
	if err != nil {
		return err
	}
	if err := m.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{
		Force:         true, // stop if running
		RemoveVolumes: true,
	}); err != nil {
		return fmt.Errorf("removing container %s: %w", idOrName, err)
	}
	return nil
}

// findManaged resolves idOrName to a single container that carries the managed
// label, erroring out otherwise.
func (m *Manager) findManaged(ctx context.Context, idOrName string) (*Container, error) {
	cs, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range cs {
		c := cs[i]
		// Match by name, or by full/short ID prefix in either direction.
		if c.Name == idOrName ||
			strings.HasPrefix(c.ID, idOrName) ||
			strings.HasPrefix(idOrName, c.ID) {
			return &c, nil
		}
	}
	return nil, fmt.Errorf("no managed container matches %q (it may not exist or was not created by this server)", idOrName)
}
