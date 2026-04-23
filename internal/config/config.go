package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration structure.
type Config struct {
	// Groups defines one or more VPN namespace groups, each with a provider and dependents.
	Groups []GroupConfig `yaml:"groups"`

	// RebindDelay is how long to wait after the provider starts before rebinding dependents.
	// This gives the provider time to fully initialize its network stack.
	// Defaults to 3 seconds. Override with VPN_REBIND_DELAY env var (e.g. "5s").
	RebindDelay time.Duration `yaml:"rebind_delay"`

	// StopTimeout is the graceful stop timeout sent to each dependent before force-removing it.
	// Defaults to 10 seconds. Override with VPN_REBIND_STOP_TIMEOUT.
	StopTimeout time.Duration `yaml:"stop_timeout"`

	// LogLevel controls verbosity: "debug", "info", "warn", "error".
	// Defaults to "info". Override with VPN_REBIND_LOG_LEVEL.
	LogLevel string `yaml:"log_level"`
}

// GroupConfig represents a single VPN namespace group.
type GroupConfig struct {
	// Name is a human-readable label for this group used in logs.
	Name string `yaml:"name"`

	// Provider is the container name of the VPN namespace provider (e.g. "gluetun").
	// This is the container whose network namespace all dependents attach to.
	Provider string `yaml:"provider"`

	// Dependents is an explicit list of container names that must be rebound
	// whenever the provider restarts.
	Dependents []string `yaml:"dependents"`

	// LabelSelector discovers additional dependents dynamically by matching
	// Docker container labels. All listed key=value pairs must match.
	// Example: {"vpn.required": "true", "vpn.provider": "gluetun"}
	LabelSelector map[string]string `yaml:"label_selector"`
}

// Load reads configuration from a YAML file at path, then applies environment
// variable overrides. If path is empty, only env vars are applied.
func Load(path string) (*Config, error) {
	cfg := &Config{
		RebindDelay: 3 * time.Second,
		StopTimeout: 10 * time.Second,
		LogLevel:    "info",
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config file %q: %w", path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file %q: %w", path, err)
		}
	}

	applyEnv(cfg)

	// Allow a single group to be defined entirely via env vars, useful for
	// simple single-provider deployments without a config file.
	if provider := os.Getenv("VPN_REBIND_PROVIDER"); provider != "" {
		g := GroupConfig{
			Name:     "default",
			Provider: provider,
		}
		if deps := os.Getenv("VPN_REBIND_DEPENDENTS"); deps != "" {
			for _, d := range strings.Split(deps, ",") {
				if name := strings.TrimSpace(d); name != "" {
					g.Dependents = append(g.Dependents, name)
				}
			}
		}
		// Label selector via env: VPN_REBIND_LABEL_SELECTOR="vpn.required=true,vpn.provider=gluetun"
		if ls := os.Getenv("VPN_REBIND_LABEL_SELECTOR"); ls != "" {
			g.LabelSelector = make(map[string]string)
			for _, pair := range strings.Split(ls, ",") {
				pair = strings.TrimSpace(pair)
				if pair == "" {
					continue
				}
				k, v, _ := strings.Cut(pair, "=")
				g.LabelSelector[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		cfg.Groups = append(cfg.Groups, g)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// applyEnv overlays individual settings from environment variables.
func applyEnv(cfg *Config) {
	if v := os.Getenv("VPN_REBIND_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("VPN_REBIND_DELAY"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.RebindDelay = d
		}
	}
	if v := os.Getenv("VPN_REBIND_STOP_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.StopTimeout = d
		}
	}
}

// Validate returns an error if the configuration is semantically invalid.
func (c *Config) Validate() error {
	if len(c.Groups) == 0 {
		return fmt.Errorf("no groups defined: set VPN_REBIND_PROVIDER or provide a config file with at least one group")
	}
	seen := make(map[string]bool)
	for i, g := range c.Groups {
		if g.Name == "" {
			return fmt.Errorf("groups[%d]: name is required", i)
		}
		if g.Provider == "" {
			return fmt.Errorf("groups[%d] (%q): provider is required", i, g.Name)
		}
		if seen[g.Name] {
			return fmt.Errorf("duplicate group name %q", g.Name)
		}
		seen[g.Name] = true
		if len(g.Dependents) == 0 && len(g.LabelSelector) == 0 {
			return fmt.Errorf("groups[%d] (%q): at least one of dependents or label_selector must be set", i, g.Name)
		}
	}
	return nil
}
