package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"

	"github.com/mikechambers/vpn-rebind/internal/config"
)

// Controller watches Docker events for VPN namespace provider containers and
// triggers a rebind of all registered dependents when a provider restarts.
type Controller struct {
	docker  *client.Client
	cfg     *config.Config
	log     *slog.Logger
	rebirder *Rebirder

	// groupStates holds per-group reconciliation state.
	mu          sync.Mutex
	groupStates map[string]*groupState
}

// groupState tracks per-group debounce/rebind state.
type groupState struct {
	group       config.GroupConfig
	needsRebind bool     // set to true when we observe the provider die
	timer       *time.Timer
}

// New creates a Controller with the given Docker client and configuration.
func New(docker *client.Client, cfg *config.Config, log *slog.Logger) *Controller {
	states := make(map[string]*groupState, len(cfg.Groups))
	for _, g := range cfg.Groups {
		states[g.Provider] = &groupState{group: g}
	}

	return &Controller{
		docker:      docker,
		cfg:         cfg,
		log:         log,
		rebirder:    NewRebirder(docker, log),
		groupStates: states,
	}
}

// Run subscribes to Docker container events and drives the reconciliation loop
// until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	c.log.Info("controller starting",
		"groups", len(c.cfg.Groups),
		"rebind_delay", c.cfg.RebindDelay,
	)
	for _, g := range c.cfg.Groups {
		c.log.Info("watching group",
			"group", g.Name,
			"provider", g.Provider,
			"explicit_dependents", len(g.Dependents),
			"label_selector", g.LabelSelector,
		)
	}

	// Build a filter set so we only receive container lifecycle events.
	f := filters.NewArgs(
		filters.Arg("type", string(events.ContainerEventType)),
	)
	// Restrict to just the actions we care about.
	for _, action := range []string{"die", "kill", "stop", "start"} {
		f.Add("event", action)
	}

	eventCh, errCh := c.docker.Events(ctx, events.ListOptions{Filters: f})

	for {
		select {
		case <-ctx.Done():
			c.log.Info("controller shutting down")
			c.cancelAllTimers()
			return nil

		case err, ok := <-errCh:
			if !ok {
				// Channel closed, Docker daemon gone or ctx cancelled.
				return nil
			}
			return fmt.Errorf("docker events stream error: %w", err)

		case ev, ok := <-eventCh:
			if !ok {
				return nil
			}
			c.handleEvent(ctx, ev)
		}
	}
}

// handleEvent processes a single Docker container event.
func (c *Controller) handleEvent(ctx context.Context, ev events.Message) {
	// Container name comes through as an attribute; fall back to actor ID.
	containerName := ev.Actor.Attributes["name"]
	if containerName == "" {
		return
	}

	c.mu.Lock()
	state, isProvider := c.groupStates[containerName]
	c.mu.Unlock()

	if !isProvider {
		return
	}

	action := string(ev.Action)
	c.log.Debug("provider event",
		"group", state.group.Name,
		"provider", containerName,
		"action", action,
	)

	switch {
	case action == "die" || action == "kill" || action == "stop":
		c.onProviderDown(state, containerName)

	case action == "start":
		c.onProviderStart(ctx, state, containerName)
	}
}

// onProviderDown marks a group as needing a rebind and cancels any scheduled
// rebind timer (the provider went away, so there is nothing to rebind to yet).
func (c *Controller) onProviderDown(state *groupState, containerName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	state.needsRebind = true
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	c.log.Info("provider down — dependents will rebind on next start",
		"group", state.group.Name,
		"provider", containerName,
	)
}

// onProviderStart schedules a debounced rebind if the group was previously
// marked as needing one (i.e. we saw the provider go down first).
func (c *Controller) onProviderStart(ctx context.Context, state *groupState, containerName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !state.needsRebind {
		// Provider started fresh (controller cold-start or first boot); no rebind needed.
		c.log.Debug("provider started without prior down event — skipping rebind",
			"group", state.group.Name,
			"provider", containerName,
		)
		return
	}

	// Cancel any previously scheduled rebind (handles rapid restart loops).
	if state.timer != nil {
		state.timer.Stop()
	}

	delay := c.cfg.RebindDelay
	c.log.Info("provider back up — scheduling rebind",
		"group", state.group.Name,
		"provider", containerName,
		"delay", delay,
	)

	state.timer = time.AfterFunc(delay, func() {
		c.mu.Lock()
		state.needsRebind = false
		state.timer = nil
		c.mu.Unlock()

		if err := c.rebirder.RebindGroup(ctx, state.group); err != nil {
			c.log.Error("rebind failed",
				"group", state.group.Name,
				"error", err,
			)
		}
	})
}

// cancelAllTimers stops any pending rebind timers, called on shutdown.
func (c *Controller) cancelAllTimers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, state := range c.groupStates {
		if state.timer != nil {
			state.timer.Stop()
			state.timer = nil
		}
	}
}

// stripSlash removes the leading "/" that Docker prepends to container names.
func stripSlash(name string) string {
	return strings.TrimPrefix(name, "/")
}
