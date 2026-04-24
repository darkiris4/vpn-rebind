package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"

	"github.com/mikechambers/vpn-rebind/internal/config"
)

// Rebirder performs the stop → remove → recreate → start cycle for dependent containers.
type Rebirder struct {
	client *client.Client
	log    *slog.Logger
}

// NewRebirder constructs a Rebirder.
func NewRebirder(c *client.Client, log *slog.Logger) *Rebirder {
	return &Rebirder{client: c, log: log}
}

// RebindGroup discovers all dependents for the group and recreates each one
// so that it re-attaches to the provider's current network namespace.
func (r *Rebirder) RebindGroup(ctx context.Context, g config.GroupConfig) error {
	r.log.Info("rebinding group", "group", g.Name, "provider", g.Provider)

	dependents, err := r.resolveDependents(ctx, g)
	if err != nil {
		return fmt.Errorf("resolving dependents: %w", err)
	}

	if len(dependents) == 0 {
		r.log.Warn("no dependents found — nothing to rebind", "group", g.Name)
		return nil
	}

	r.log.Info("rebinding dependents",
		"group", g.Name,
		"count", len(dependents),
		"containers", dependents,
	)

	var firstErr error
	for _, name := range dependents {
		if err := r.rebindContainer(ctx, name, g.Provider); err != nil {
			r.log.Error("failed to rebind container",
				"group", g.Name,
				"container", name,
				"error", err,
			)
			if firstErr == nil {
				firstErr = err
			}
			// Continue so other dependents are still rebound.
		}
	}

	return firstErr
}

// resolveDependents merges the explicit dependents list with any containers
// discovered via the group's label selector. Duplicates are deduplicated.
func (r *Rebirder) resolveDependents(ctx context.Context, g config.GroupConfig) ([]string, error) {
	seen := make(map[string]bool)
	var result []string

	for _, name := range g.Dependents {
		if name = strings.TrimSpace(name); name != "" && !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}

	if len(g.LabelSelector) > 0 {
		discovered, err := r.discoverByLabels(ctx, g.LabelSelector)
		if err != nil {
			return nil, fmt.Errorf("label discovery: %w", err)
		}
		for _, name := range discovered {
			if !seen[name] {
				seen[name] = true
				result = append(result, name)
			}
		}
	}

	return result, nil
}

// discoverByLabels returns the names of all containers (running or stopped)
// that match every label in the selector map.
func (r *Rebirder) discoverByLabels(ctx context.Context, selector map[string]string) ([]string, error) {
	args := filters.NewArgs()
	for k, v := range selector {
		if v == "" {
			args.Add("label", k)
		} else {
			args.Add("label", k+"="+v)
		}
	}

	containers, err := r.client.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: args,
	})
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(containers))
	for _, c := range containers {
		if len(c.Names) > 0 {
			names = append(names, stripSlash(c.Names[0]))
		}
	}
	return names, nil
}

// rebindContainer stops, removes, recreates, and starts a single container.
// The provider name is used to normalise the NetworkMode so that the recreated
// container attaches to the provider's *current* network namespace by name.
func (r *Rebirder) rebindContainer(ctx context.Context, containerName, providerName string) error {
	log := r.log.With("container", containerName)

	// 1. Inspect to capture the full config before we touch anything.
	info, err := r.client.ContainerInspect(ctx, containerName)
	if err != nil {
		if client.IsErrNotFound(err) {
			log.Warn("container not found — skipping")
			return nil
		}
		return fmt.Errorf("inspect: %w", err)
	}

	log.Info("rebinding container", "image", info.Config.Image)

	hostCfg := cloneHostConfig(info.HostConfig, providerName)
	netCfg := buildNetworkingConfig(info)
	containerCfg := cloneContainerConfig(info.Config, hostCfg)
	name := stripSlash(info.Name)

	log.Debug("container config prepared",
		"hostname", containerCfg.Hostname,
		"domainname", containerCfg.Domainname,
		"network_mode", hostCfg.NetworkMode,
	)

	// 2. Stop gracefully.
	timeout := 10
	if err := r.client.ContainerStop(ctx, info.ID, container.StopOptions{Timeout: &timeout}); err != nil {
		// Not fatal — container may already be stopped/exited.
		log.Warn("stop returned error (proceeding)", "error", err)
	}

	// 3. Remove (force in case it's still stuck).
	if err := r.client.ContainerRemove(ctx, info.ID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove: %w", err)
	}

	// 4. Recreate with the same config.
	resp, err := r.client.ContainerCreate(ctx, containerCfg, hostCfg, netCfg, nil, name)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if len(resp.Warnings) > 0 {
		log.Warn("container create warnings", "warnings", resp.Warnings)
	}

	// 5. Start.
	if err := r.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	log.Info("container rebound successfully", "new_id", resp.ID[:12])
	return nil
}

// cloneContainerConfig returns a shallow copy of cfg. When the network mode is
// container:*, fields that Docker rejects for shared-namespace containers are cleared:
// Hostname, Domainname, and ExposedPorts (networking is owned by the provider).
func cloneContainerConfig(cfg *container.Config, hc *container.HostConfig) *container.Config {
	if cfg == nil {
		return nil
	}
	copy := *cfg
	if hc != nil && strings.HasPrefix(string(hc.NetworkMode), "container:") {
		copy.Hostname = ""
		copy.Domainname = ""
		copy.ExposedPorts = nil
	}
	return &copy
}

// cloneHostConfig returns a shallow copy of hc with the NetworkMode normalised
// to use the provider's name rather than a stale container ID.
//
// Docker stores network_mode as "container:<id>" after creation. When we
// recreate, we switch to "container:<providerName>" so Docker resolves it to
// the provider's current running instance.
func cloneHostConfig(hc *container.HostConfig, providerName string) *container.HostConfig {
	if hc == nil {
		return nil
	}
	copy := *hc
	if strings.HasPrefix(string(copy.NetworkMode), "container:") {
		copy.NetworkMode = container.NetworkMode("container:" + providerName)
		copy.PortBindings = nil
	}
	return &copy
}

// buildNetworkingConfig constructs a NetworkingConfig for the recreated container.
// For containers using network_mode container:*, the networking is controlled
// entirely by the HostConfig, so we return an empty config.
func buildNetworkingConfig(info dockertypes.ContainerJSON) *network.NetworkingConfig {
	hc := info.HostConfig
	if hc != nil && strings.HasPrefix(string(hc.NetworkMode), "container:") {
		// Shared namespace — no independent network endpoints to configure.
		return &network.NetworkingConfig{}
	}

	// For non-namespace-sharing containers, reconstruct endpoint configs from
	// the current network settings so the recreated container joins the same
	// user-defined networks.
	if info.NetworkSettings == nil || len(info.NetworkSettings.Networks) == 0 {
		return &network.NetworkingConfig{}
	}

	endpoints := make(map[string]*network.EndpointSettings, len(info.NetworkSettings.Networks))
	for netName, ep := range info.NetworkSettings.Networks {
		endpoints[netName] = &network.EndpointSettings{
			Aliases:   ep.Aliases,
			NetworkID: ep.NetworkID,
			IPAMConfig: func() *network.EndpointIPAMConfig {
				if ep.IPAMConfig != nil {
					return &network.EndpointIPAMConfig{
						IPv4Address: ep.IPAMConfig.IPv4Address,
						IPv6Address: ep.IPAMConfig.IPv6Address,
					}
				}
				return nil
			}(),
		}
	}
	return &network.NetworkingConfig{EndpointsConfig: endpoints}
}
