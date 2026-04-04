package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Project       string                 `yaml:"project"`
	Coordination  Coordination           `yaml:"coordination"`
	Agents        map[string]AgentConfig `yaml:"agents"`
}

type Coordination struct {
	Backend  string            `yaml:"backend"`
	ServerID string            `yaml:"server_id"`
	Channels map[string]string `yaml:"channels"`
}

type AgentConfig struct {
	Name             string   `yaml:"name"`
	Role             string   `yaml:"role"`
	Model            string   `yaml:"model"`
	IterationMinutes int      `yaml:"iteration_minutes"`
	Vaults           []string `yaml:"vaults"`
	Prompt           string   `yaml:"prompt"`
}

// OSUsername returns the OS username for an agent: <project>-<agentkey>
func (c *Config) OSUsername(agentKey string) string {
	return fmt.Sprintf("%s-%s", c.Project, agentKey)
}

// ServiceName returns the systemd service name for an agent.
func (c *Config) ServiceName(agentKey string) string {
	return fmt.Sprintf("clem-%s-%s.service", c.Project, agentKey)
}

// WatchdogServiceName returns the systemd service name for the watchdog.
func (c *Config) WatchdogServiceName() string {
	return fmt.Sprintf("clem-watchdog-%s.service", c.Project)
}

// WatchdogTimerName returns the systemd timer name for the watchdog.
func (c *Config) WatchdogTimerName() string {
	return fmt.Sprintf("clem-watchdog-%s.timer", c.Project)
}

// Load reads and parses clem.yaml from the given path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if cfg.Project == "" {
		return nil, fmt.Errorf("config missing required field: project")
	}
	if len(cfg.Agents) == 0 {
		return nil, fmt.Errorf("config has no agents defined")
	}
	return &cfg, nil
}
