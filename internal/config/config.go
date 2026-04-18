package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jahwag/clem/internal/coordination"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

type Config struct {
	Project          string                 `yaml:"project"`
	PrimaryMilestone string                 `yaml:"primary_milestone"`
	Coordination     Coordination           `yaml:"coordination"`
	Agents           map[string]AgentConfig `yaml:"agents"`
}

type Coordination struct {
	Backend  string            `yaml:"backend"`
	ServerID string            `yaml:"server_id"`
	Channels map[string]string `yaml:"channels"`
}

type AgentConfig struct {
	Name  string `yaml:"name"`
	Role  string `yaml:"role"`
	Model string `yaml:"model"`
	// Iteration is a Go-style duration string (e.g. "30s", "1m30s", "2h").
	// Parsed via time.ParseDuration. Sleep between agent sessions during
	// active hours (07-22); doubled overnight. Default 5m.
	Iteration       string   `yaml:"iteration"`
	Vaults          []string `yaml:"vaults"`
	Prompt          string   `yaml:"prompt"`
	WebTerminalPort int      `yaml:"web_terminal_port"`
	// WebTerminalBind controls which interface ttyd listens on. Default
	// 127.0.0.1 for safety (expects SSH tunnel or reverse proxy). Use
	// 0.0.0.0 when running inside a container with host port-forward.
	WebTerminalBind string `yaml:"web_terminal_bind"`
	Caveman         bool   `yaml:"caveman"`
	// Runtime selects which CLI drives the agent's session. Default is
	// claude-code. opencode talks to 75+ providers (including Ollama) via
	// models.dev without the Anthropic-format translator in the middle.
	Runtime string `yaml:"runtime"`
	// Provider selects the model backend: anthropic (default), bedrock, vertex,
	// ollama, openai-compat. ollama and openai-compat require ProviderURL.
	Provider    string `yaml:"provider"`
	ProviderURL string `yaml:"provider_url"`
}

// RuntimeKind returns the normalized runtime name for this agent.
func (ac AgentConfig) RuntimeKind() string {
	switch ac.Runtime {
	case "", "claude-code", "claude":
		return "claude-code"
	case "opencode":
		return "opencode"
	default:
		return ac.Runtime
	}
}

// IterationDuration returns the parsed iteration period, or 5m default when
// unset. Errors are surfaced at config load time via Load's validation pass.
func (ac AgentConfig) IterationDuration() (time.Duration, error) {
	if ac.Iteration == "" {
		return 5 * time.Minute, nil
	}
	d, err := time.ParseDuration(ac.Iteration)
	if err != nil {
		return 0, fmt.Errorf("invalid iteration %q: %w (expected Go duration like 30s, 1m30s, 2h)", ac.Iteration, err)
	}
	if d < time.Second {
		return 0, fmt.Errorf("iteration %q is too small (minimum 1s)", ac.Iteration)
	}
	return d, nil
}

// ProviderEnv returns env vars that should be exported for this agent based on
// its provider selection. These are merged into /home/<user>/.env alongside
// vault secrets at provision time.
func (ac AgentConfig) ProviderEnv() (map[string]string, error) {
	switch ac.Provider {
	case "", "anthropic":
		return nil, nil
	case "bedrock":
		return map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, nil
	case "vertex":
		return map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}, nil
	case "ollama", "openai-compat":
		if ac.ProviderURL == "" {
			return nil, fmt.Errorf("provider %q requires provider_url", ac.Provider)
		}
		env := map[string]string{
			"ANTHROPIC_BASE_URL":   ac.ProviderURL,
			"ANTHROPIC_AUTH_TOKEN": "none",
		}
		if ac.Model != "" {
			env["ANTHROPIC_MODEL"] = ac.Model
		}
		return env, nil
	default:
		return nil, fmt.Errorf("unknown provider %q (valid: anthropic, bedrock, vertex, ollama, openai-compat)", ac.Provider)
	}
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

// envVarRe matches ${VAR} or ${VAR:-default} for env interpolation in clem.yaml.
// Names are conservative on purpose: [A-Z_][A-Z0-9_]* — no silent expansion of
// arbitrary shell constructs.
var envVarRe = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv substitutes ${VAR} and ${VAR:-default} in raw YAML before parsing.
// Leaves unknown variables as-is so config errors still surface at load time
// rather than producing silently-empty strings.
func expandEnv(raw []byte) []byte {
	return envVarRe.ReplaceAllFunc(raw, func(match []byte) []byte {
		m := envVarRe.FindSubmatch(match)
		name := string(m[1])
		if v, ok := os.LookupEnv(name); ok {
			return []byte(v)
		}
		if len(m) > 2 && len(m[2]) > 0 {
			return m[2]
		}
		return match
	})
}

// Load reads and parses clem.yaml from the given path.
// ${ENV_VAR} and ${ENV_VAR:-default} references in the YAML are expanded from
// the process environment at load time.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	data = expandEnv(data)
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
	if _, err := coordination.Known(cfg.Coordination.Backend); err != nil {
		return nil, err
	}
	usedPorts := make(map[int]string)
	for key, ac := range cfg.Agents {
		if !validName.MatchString(key) {
			return nil, fmt.Errorf("agent key must match ^[a-z][a-z0-9-]{0,30}$, got: %q", key)
		}
		switch ac.RuntimeKind() {
		case "claude-code", "opencode":
			// supported
		default:
			return nil, fmt.Errorf("agent %s: unknown runtime %q (valid: claude-code, opencode)", key, ac.Runtime)
		}
		if _, err := ac.IterationDuration(); err != nil {
			return nil, fmt.Errorf("agent %s: %w", key, err)
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
