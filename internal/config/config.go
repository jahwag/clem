package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jahwag/clem/internal/coordination"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// snowflakeRe matches a Discord snowflake ID: 17–19 decimal digits.
var snowflakeRe = regexp.MustCompile(`^[0-9]{17,19}$`)

// githubLoginRe matches a valid GitHub username per GitHub's own rules.
var githubLoginRe = regexp.MustCompile(`^[a-zA-Z0-9-]{1,39}$`)

// vaultRefRe matches ${vault:BUCKET.KEY} in MCP server env values.
var vaultRefRe = regexp.MustCompile(`\$\{vault:([^.}]+)\.([^}]+)\}`)

// OperatorConfig identifies the humans who are trusted to issue instructions
// to agents via Discord or GitHub. Provisioned agents use these IDs in the
// generated prompt so no operator ID is hardcoded in clem source.
//
// discord_ids must be 17–19-digit decimal Discord snowflakes.
// github_logins must match ^[a-zA-Z0-9-]{1,39}$ (GitHub username rules).
//
// The block is optional. When omitted, {{operator.discord_ids}} and
// {{operator.github_logins}} in CLAUDE.shared.md render as empty strings.
type OperatorConfig struct {
	DiscordIDs   []string `yaml:"discord_ids"`
	GitHubLogins []string `yaml:"github_logins"`
}

// MarketplaceConfig declares a Claude Code plugin marketplace to install.
// source must be "github". commit is optional; when set, provision verifies
// the cloned HEAD matches before proceeding (supply-chain pin).
type MarketplaceConfig struct {
	Name   string `yaml:"name"`
	Source string `yaml:"source"`
	Repo   string `yaml:"repo"`
	Commit string `yaml:"commit"`
}

// PluginConfig declares a plugin to install from a named marketplace.
// Accepts the shorthand string form "pluginName@marketplaceName".
type PluginConfig struct {
	Name        string `yaml:"name"`
	Marketplace string `yaml:"marketplace"`
}

// UnmarshalYAML accepts "name@marketplace" shorthand and the struct form.
func (p *PluginConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Tag == "!!str" {
		parts := strings.SplitN(value.Value, "@", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("plugin shorthand must be name@marketplace, got %q", value.Value)
		}
		p.Name, p.Marketplace = parts[0], parts[1]
		return nil
	}
	type plain PluginConfig
	return value.Decode((*plain)(p))
}

// SkillConfig declares a skill to clone into ~/.claude/skills/<name>/.
// When path is set, the skill entrypoint is at ~/.claude/skills/<name>/<path>.
type SkillConfig struct {
	Name   string `yaml:"name"`
	Source string `yaml:"source"`
	Repo   string `yaml:"repo"`
	Path   string `yaml:"path"`
}

// MCPServerConfig declares an MCP server to register in ~/.claude/settings.json.
// Env values may contain ${vault:BUCKET.KEY} refs resolved at provision time.
// command/args run as the agent OS user; clem.yaml is operator-controlled.
type MCPServerConfig struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	URL     string            `yaml:"url"`
	Env     map[string]string `yaml:"env"`
}

// ExtensionsConfig declares all extensions to install for an agent at provision
// time. Provision is idempotent: already-installed extensions are no-ops.
// Removing an entry does not auto-uninstall; re-provision logs a reminder.
type ExtensionsConfig struct {
	Marketplaces []MarketplaceConfig `yaml:"marketplaces"`
	Plugins      []PluginConfig      `yaml:"plugins"`
	Skills       []SkillConfig       `yaml:"skills"`
	MCPServers   []MCPServerConfig   `yaml:"mcp_servers"`
}

// ExpandVaultRefs replaces ${vault:BUCKET.KEY} refs in s using the flat
// secrets map from vault.DecryptForAgent. Unresolvable refs are left as-is.
func ExpandVaultRefs(s string, secrets map[string]string) string {
	return vaultRefRe.ReplaceAllStringFunc(s, func(match string) string {
		m := vaultRefRe.FindStringSubmatch(match)
		if v, ok := secrets[m[2]]; ok {
			return v
		}
		return match
	})
}

// CavemanLevel controls whether and at what intensity the caveman plugin is
// injected. Accepts "off"/""  (disabled), "lite", "full", "ultra", or the
// legacy boolean true (→ "ultra") / false (→ "off").
type CavemanLevel string

const (
	CavemanOff   CavemanLevel = ""
	CavemanLite  CavemanLevel = "lite"
	CavemanFull  CavemanLevel = "full"
	CavemanUltra CavemanLevel = "ultra"
)

// Enabled reports whether the caveman plugin should be installed and injected.
func (cl CavemanLevel) Enabled() bool { return cl != CavemanOff }

// Level returns the intensity string ("lite", "full", "ultra").
// Callers should check Enabled() first.
func (cl CavemanLevel) Level() string { return string(cl) }

// UnmarshalYAML accepts string levels, "off", and legacy booleans.
func (cl *CavemanLevel) UnmarshalYAML(value *yaml.Node) error {
	switch value.Tag {
	case "!!bool":
		if value.Value == "true" {
			*cl = CavemanUltra
		} else {
			*cl = CavemanOff
		}
		return nil
	case "!!str", "!!null":
		switch value.Value {
		case "", "off", "null":
			*cl = CavemanOff
		case "lite":
			*cl = CavemanLite
		case "full":
			*cl = CavemanFull
		case "ultra":
			*cl = CavemanUltra
		default:
			return fmt.Errorf("invalid caveman value %q (valid: off, lite, full, ultra)", value.Value)
		}
		return nil
	default:
		return fmt.Errorf("invalid caveman value %q (valid: off, lite, full, ultra)", value.Value)
	}
}

type Config struct {
	Project          string                 `yaml:"project"`
	PrimaryMilestone string                 `yaml:"primary_milestone"`
	Coordination     Coordination           `yaml:"coordination"`
	Operator         OperatorConfig         `yaml:"operator"`
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
	// Parsed via time.ParseDuration. Sleep between agent sessions; same
	// value applies day and night. Default 5m.
	Iteration       string   `yaml:"iteration"`
	Vaults          []string `yaml:"vaults"`
	Prompt          string   `yaml:"prompt"`
	WebTerminalPort int      `yaml:"web_terminal_port"`
	// WebTerminalBind controls which interface ttyd listens on. Default
	// 127.0.0.1 for safety (expects SSH tunnel or reverse proxy). Use
	// 0.0.0.0 when running inside a container with host port-forward.
	WebTerminalBind string `yaml:"web_terminal_bind"`
	Caveman         CavemanLevel `yaml:"caveman"`
	// Runtime selects which CLI drives the agent's session. Default is
	// claude-code. opencode talks to 75+ providers (including Ollama) via
	// models.dev without the Anthropic-format translator in the middle.
	Runtime string `yaml:"runtime"`
	// Provider selects the model backend: anthropic (default), bedrock, vertex,
	// ollama, openai-compat. ollama and openai-compat require ProviderURL.
	Provider    string `yaml:"provider"`
	ProviderURL string `yaml:"provider_url"`
	// SubagentModel sets CLAUDE_CODE_SUBAGENT_MODEL in the runner env so
	// subagents (Task tool, Explore, general-purpose) use a cheaper model
	// than the main session. Accepts model aliases (sonnet, haiku, opus) or
	// full IDs (claude-sonnet-4-6). Empty = inherit main model.
	SubagentModel string `yaml:"subagent_model"`
	// Effort caps extended-thinking budget per session via Claude Code's
	// effortLevel setting. Accepts "low", "medium", "high". Empty = use
	// Claude Code's own default (currently medium). Lowering trims output
	// tokens — the dominant cost driver in agent loops.
	Effort string `yaml:"effort"`
	// GitName and GitEmail set the agent's git user identity during provision.
	// Without these, commits are authored with whatever identity the OAuth login
	// stored, which may leak the operator's personal email into public history.
	// If GitEmail is unset and the agent's vault contains GH_TOKEN, provision
	// logs a warning at runtime.
	GitName  string `yaml:"git_name"`
	GitEmail string `yaml:"git_email"`
	// Extensions declares marketplaces, plugins, skills, and MCP servers to
	// install for this agent at provision time. caveman: true is handled as a
	// shorthand that prepends the caveman marketplace and plugin entries.
	Extensions ExtensionsConfig `yaml:"extensions"`
	// EgressRestrictionExperimental adds systemd IPAddressDeny=any + IPAddressAllow
	// rules to the agent service unit. EXPERIMENTAL — known limitations:
	//
	//   DNS: only works if the host uses systemd-resolved (127.0.0.53). Hosts
	//   using an external resolver (1.1.1.1, 8.8.8.8, corporate DNS) will fail
	//   DNS resolution entirely because those IPs are not in the allowlist.
	//
	//   Cloudflare width: the Cloudflare CIDRs (104.16.0.0/13 etc.) cover
	//   millions of CF-fronted sites beyond Anthropic and Discord. A compromised
	//   agent can still reach arbitrary CF-hosted attacker infrastructure.
	//
	//   CIDR drift: allowlist is hardcoded. GitHub, Cloudflare, and Discord
	//   rotate ranges; the list will go stale silently and block legitimate
	//   traffic. Refresh manually before each release or automate via
	//   `curl https://api.github.com/meta`.
	//
	//   DNS exfil: kernel IP filter does not inspect DNS query payloads. An
	//   agent can exfil data by encoding secrets in subdomain labels sent to
	//   an attacker-controlled nameserver (e.g. base64(secret).evil.example.com).
	//   This restriction does NOT close that channel.
	//
	// Despite these limitations, this provides meaningful containment against
	// naive outbound connections. Use with understanding of the gaps above.
	EgressRestrictionExperimental bool `yaml:"egress_restriction_experimental"`
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
	if err := cfg.Operator.validate(); err != nil {
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
		ac.normalizeSubagentModel()
		if err := ac.validateExtensions(key); err != nil {
			return nil, err
		}
		cfg.Agents[key] = ac
	}
	return &cfg, nil
}

// validate checks that all discord_ids and github_logins are well-formed.
func (op *OperatorConfig) validate() error {
	for _, id := range op.DiscordIDs {
		if !snowflakeRe.MatchString(id) {
			return fmt.Errorf("operator.discord_ids: %q is not a valid Discord snowflake (17–19 decimal digits)", id)
		}
	}
	for _, login := range op.GitHubLogins {
		if !githubLoginRe.MatchString(login) {
			return fmt.Errorf("operator.github_logins: %q is not a valid GitHub login (^[a-zA-Z0-9-]{1,39}$)", login)
		}
	}
	return nil
}

// validateExtensions checks extension config for an agent identified by key.
func (ac *AgentConfig) validateExtensions(key string) error {
	for _, mp := range ac.Extensions.Marketplaces {
		if mp.Name == "" || mp.Repo == "" {
			return fmt.Errorf("agent %s: marketplace entry missing name or repo", key)
		}
	}
	for _, sk := range ac.Extensions.Skills {
		if sk.Name == "" || sk.Repo == "" {
			return fmt.Errorf("agent %s: skill entry missing name or repo", key)
		}
	}
	for _, mcp := range ac.Extensions.MCPServers {
		if mcp.Name == "" {
			return fmt.Errorf("agent %s: mcp_server entry missing name", key)
		}
		if mcp.URL == "" && mcp.Command == "" {
			return fmt.Errorf("agent %s: mcp_server %s requires command or url", key, mcp.Name)
		}
		vaultSet := make(map[string]bool, len(ac.Vaults))
		for _, v := range ac.Vaults {
			vaultSet[v] = true
		}
		for envKey, envVal := range mcp.Env {
			for _, m := range vaultRefRe.FindAllStringSubmatch(envVal, -1) {
				if !vaultSet[m[1]] {
					return fmt.Errorf("agent %s: mcp_server %s: env %s references vault %q not in agent vaults list", key, mcp.Name, envKey, m[1])
				}
			}
		}
	}
	return nil
}

// DefaultSubagentModel is applied when subagent_model is unset in clem.yaml.
// Subagents (Task tool, Explore, general-purpose) handle most work well with
// Sonnet; defaulting avoids silent Opus-on-Opus cost when running an Opus main
// session. Opt out with subagent_model: "off" in clem.yaml.
const DefaultSubagentModel = "claude-sonnet-4-6"

// normalizeSubagentModel applies the default and maps the "off" sentinel to
// empty string. Called from Load after YAML parse so runner.go stays simple.
// Default only applies to Anthropic-backed providers (anthropic, bedrock,
// vertex); ollama and openai-compat cannot use CLAUDE_CODE_SUBAGENT_MODEL.
func (ac *AgentConfig) normalizeSubagentModel() {
	switch ac.SubagentModel {
	case "off":
		ac.SubagentModel = ""
	case "":
		switch ac.Provider {
		case "", "anthropic", "bedrock", "vertex":
			ac.SubagentModel = DefaultSubagentModel
		}
	}
}
