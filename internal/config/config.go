package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

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
	WebTerminalPort  int      `yaml:"web_terminal_port"`
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

// TtydServiceName returns the systemd service name for the agent's web terminal.
func (c *Config) TtydServiceName(agentKey string) string {
	return fmt.Sprintf("clem-ttyd-%s-%s.service", c.Project, agentKey)
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
	if !validName.MatchString(cfg.Project) {
		return nil, fmt.Errorf("project name must match ^[a-z][a-z0-9-]{0,30}$, got: %q", cfg.Project)
	}
	if len(cfg.Agents) == 0 {
		return nil, fmt.Errorf("config has no agents defined")
	}
	usedPorts := make(map[int]string)
	for key, ac := range cfg.Agents {
		if !validName.MatchString(key) {
			return nil, fmt.Errorf("agent key must match ^[a-z][a-z0-9-]{0,30}$, got: %q", key)
		}
		if ac.WebTerminalPort != 0 {
			if ac.WebTerminalPort < 1024 || ac.WebTerminalPort > 65535 {
				return nil, fmt.Errorf("agent %s: web_terminal_port must be 1024-65535, got %d", key, ac.WebTerminalPort)
			}
			if other, exists := usedPorts[ac.WebTerminalPort]; exists {
				return nil, fmt.Errorf("agents %s and %s have the same web_terminal_port %d", other, key, ac.WebTerminalPort)
			}
			usedPorts[ac.WebTerminalPort] = key
		}
	}
	return &cfg, nil
}
