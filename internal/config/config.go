package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jahwag/clem/internal/coordination"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// snowflakeRe matches a Discord snowflake ID: 17–19 decimal digits.
var snowflakeRe = regexp.MustCompile(`^[0-9]{17,19}$`)

// serviceNameRe matches an agent-vault service slug: 3–64 lowercase
// alphanumeric/hyphen characters (agent-vault's own constraint).
var serviceNameRe = regexp.MustCompile(`^[a-z0-9-]{3,64}$`)

// avNameInvalid matches characters not allowed in an agent-vault vault name.
var avNameInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

// AgentVaultName maps a clem/sops vault name to an agent-vault-compatible vault
// name: lowercased, with any run of characters outside [a-z0-9-] collapsed to a
// single hyphen. sops vault names may contain '_' (e.g. dev_to) or uppercase,
// which agent-vault rejects (names are lowercase alphanumeric/hyphen). Used
// everywhere clem hands a vault name to agent-vault so e.g. dev_to -> dev-to
// consistently across seed, token mint, service rules, and the agent proxy env.
func AgentVaultName(sopsVault string) string {
	return avNameInvalid.ReplaceAllString(strings.ToLower(sopsVault), "-")
}

// githubLoginRe matches a valid GitHub username per GitHub's own rules.
var githubLoginRe = regexp.MustCompile(`^[a-zA-Z0-9-]{1,39}$`)

// vaultRefRe matches ${vault:BUCKET.KEY} in MCP server env values.
var vaultRefRe = regexp.MustCompile(`\$\{vault:([^.}]+)\.([^}]+)\}`)

// reservedMCPNames are MCP server names clem's runner hardcodes (runner.go); a
// sidecar's tool name must not collide with them or it would shadow/duplicate a
// builtin in the agent's .mcp.json.
var reservedMCPNames = map[string]bool{
	"discord-bot": true, "slack-mcp": true, "social": true, "browser-render": true,
}

// secretKeyRe matches an env-var-style credential key (what a sidecar's secrets
// and the vault keys look like).
var secretKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// extensionNameRe allows alphanumeric names with dots, hyphens, and underscores.
// Rejects semicolons, spaces, backticks, and other shell-special characters.
var extensionNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// extensionRepoRe requires owner/repo format with safe characters only.
var extensionRepoRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+$`)

// commitHashRe accepts hex SHA strings (full or prefix).
var commitHashRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

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

// PermissionsConfig declares opt-in deny rules written to the root-owned
// /etc/claude-code/managed-settings.json at provision time. Deny lists from
// all agents on the host are merged into one file (managed-settings is
// host-level, not per-user). Empty or absent block = no clem-injected denies.
type PermissionsConfig struct {
	Deny []string `yaml:"deny"`
}

// ExpandVaultRefs replaces ${vault:BUCKET.KEY} refs in s using the qualified
// secrets map from vault.DecryptForAgent (keys are "vaultName.keyName").
// Unresolvable refs are left as-is.
func ExpandVaultRefs(s string, secrets map[string]string) string {
	return vaultRefRe.ReplaceAllStringFunc(s, func(match string) string {
		m := vaultRefRe.FindStringSubmatch(match)
		if v, ok := secrets[m[1]+"."+m[2]]; ok {
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
	Egress           EgressConfig           `yaml:"egress"`
	Vault            VaultBackend           `yaml:"vault"`
	MCPSidecars      MCPSidecarsConfig      `yaml:"mcp_sidecars"`
	Agents           map[string]AgentConfig `yaml:"agents"`
}

// MCPSidecarsConfig configures privileged MCP "sidecars": MCP servers that hold
// a secret the agent must USE but not READ, run as a dedicated non-agent system
// user, and are reached by the agent over a loopback HTTP MCP transport. This is
// the "sidecar" disposition in clem's secret-protection model — for credentials
// the broker can't rewrite (gateway WebSocket tokens) or that warrant scoped
// access (e.g. read-only Elasticsearch). A stdio MCP server cannot provide this:
// it runs as the agent's own UID, so its secret is in the agent's reach.
//
// FOUNDATION ONLY (this commit): schema + validation. Provision wiring (the
// clem-mcp system user, the stdio→HTTP bridge install, the systemd loopback
// service, per-agent token mint, nftables allow-rule, and runner .mcp.json http
// entry) is the follow-up build. See docs/threat-model.md and the design notes.
type MCPSidecarsConfig struct {
	// SystemUser is the dedicated non-login user that runs all sidecar servers.
	// Default "clem-mcp".
	SystemUser string `yaml:"system_user"`
	// BasePort is the first loopback port allocated to sidecar listeners;
	// subsequent listeners take successive ports deterministically. Default 14500.
	BasePort int `yaml:"base_port"`
	// Servers declares the available sidecar MCP servers; agents subscribe by
	// name via AgentConfig.Sidecars.
	Servers []SidecarServer `yaml:"servers"`
}

// SidecarServer is one privileged MCP server definition. The agent sees only a
// loopback HTTP MCP endpoint + a scoped bearer token; the upstream credential
// lives only in the sidecar process (sourced from Secrets, written to a
// root-owned env file, never any agent .env).
type SidecarServer struct {
	// Name is the sidecar slug (matches validName) and the MCP server name the
	// agent sees in .mcp.json unless Tool overrides it.
	Name string `yaml:"name"`
	// Identity is "shared" (one upstream credential for all subscribers, e.g.
	// read-only ES) or "per-agent" (one listener per subscribing agent with that
	// agent's own credential, e.g. each agent's Discord bot token). Default shared.
	Identity string `yaml:"identity"`
	// Command + Args are the underlying stdio MCP server the bridge wraps.
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// Transport is the agent-facing transport. "http" (streamable-HTTP, default;
	// recommended) or "sse" (deprecated).
	Transport string `yaml:"transport"`
	// Secrets are the credential keys this sidecar holds (written to its
	// root-owned env file); for identity per-agent they are resolved per agent.
	Secrets []string `yaml:"secrets"`
	// SecretsVault is the sops/agent-vault vault to pull Secrets from.
	SecretsVault string `yaml:"secrets_vault"`
	// Tool overrides the MCP server name the agent sees (default Name).
	Tool string `yaml:"tool"`
}

// SystemUserOrDefault returns the sidecar system user, default clem-mcp.
func (m MCPSidecarsConfig) SystemUserOrDefault() string {
	if m.SystemUser == "" {
		return "clem-mcp"
	}
	return m.SystemUser
}

// BasePortOrDefault returns the first sidecar loopback port, default 14500.
func (m MCPSidecarsConfig) BasePortOrDefault() int {
	if m.BasePort == 0 {
		return 14500
	}
	return m.BasePort
}

// IdentityKind normalizes the identity to "shared" (default) or "per-agent".
func (s SidecarServer) IdentityKind() string {
	if s.Identity == "per-agent" {
		return "per-agent"
	}
	return "shared"
}

// TransportKind normalizes the transport to "http" (default) or "sse".
func (s SidecarServer) TransportKind() string {
	if s.Transport == "sse" {
		return "sse"
	}
	return "http"
}

// ToolName returns the MCP server name the agent sees (Tool or Name).
func (s SidecarServer) ToolName() string {
	if s.Tool != "" {
		return s.Tool
	}
	return s.Name
}

// SidecarPort returns the deterministic loopback port for the i-th sidecar
// listener (allocated upward from base_port). Stable across provisions so
// re-provision yields minimal nftables/.mcp.json diffs.
func (m MCPSidecarsConfig) SidecarPort(i int) int { return m.BasePortOrDefault() + i }

// VaultBackend selects how agent secrets are materialized (Phase 2). Default
// "env" preserves the legacy flow: secrets decrypted from sops are written
// verbatim into each agent's .env. "agent-vault" routes HTTP-brokerable
// credentials through an Infisical agent-vault credential proxy so the real
// secret never reaches the agent — the agent holds only a placeholder and a
// scoped, inject-only token. sops remains the git-committable source of truth
// and seeds agent-vault at provision time.
type VaultBackend struct {
	// Backend is "env" (default) or "agent-vault".
	Backend string `yaml:"backend"`
	// SystemUser is the dedicated non-login user that owns the vault store and
	// runs agent-vault. Default "clem-vault".
	SystemUser string `yaml:"system_user"`
	// Addr is the agent-vault management API the provisioner seeds/mints against.
	// Default "http://127.0.0.1:14321".
	Addr string `yaml:"addr"`
	// ProxyHost is the host:port of agent-vault's TLS-MITM proxy that agents
	// point HTTPS_PROXY at. Default "127.0.0.1:14322".
	ProxyHost string `yaml:"proxy_host"`
	// CACertPath is where agent-vault's CA cert is written for agents to trust
	// the intercepted TLS. Default "/etc/clem/agent-vault-ca.pem".
	CACertPath string `yaml:"ca_cert_path"`
	// Services are the global injection rules. agent-vault matches outbound
	// requests by host and swaps in the real credential, so a brokered agent
	// egresses with the real value while its .env holds only a placeholder. Each
	// service names credential KEYS (token_key etc.); at provision clem applies
	// every service whose keys an agent brokers into that agent's consolidated
	// vault. A brokered secret with no matching service egresses as a placeholder.
	Services []Service `yaml:"services"`
}

// Service is one agent-vault injection rule: for requests to Host, attach the
// credential(s) named below using the AuthType scheme. Services are not bound to
// a vault — clem applies each one into the consolidated vault of every agent
// that brokers the referenced credential keys. Mirrors `agent-vault vault
// service add` flags 1:1.
type Service struct {
	// Name is the service slug (3-64 lowercase alphanumeric/hyphen chars).
	Name string `yaml:"name"`
	// Host is the target: a bare host (api.x.com), one-level wildcard
	// (*.x.com), or inline path form (slack.com/api/*).
	Host string `yaml:"host"`
	// AuthType is bearer | basic | api-key | custom | passthrough.
	AuthType string `yaml:"auth_type"`
	// TokenKey is the credential key for bearer auth.
	TokenKey string `yaml:"token_key"`
	// UsernameKey / PasswordKey are the credential keys for basic auth.
	UsernameKey string `yaml:"username_key"`
	PasswordKey string `yaml:"password_key"`
	// APIKeyKey is the credential key for api-key auth; APIKeyHeader (default
	// Authorization) and APIKeyPrefix (e.g. "Bot ") shape the injected header.
	APIKeyKey    string `yaml:"api_key_key"`
	APIKeyHeader string `yaml:"api_key_header"`
	APIKeyPrefix string `yaml:"api_key_prefix"`
}

// CredentialKeys returns the credential keys this service injects, per auth type.
func (s Service) CredentialKeys() []string {
	switch s.AuthType {
	case "bearer":
		return []string{s.TokenKey}
	case "basic":
		return []string{s.UsernameKey, s.PasswordKey}
	case "api-key":
		return []string{s.APIKeyKey}
	}
	return nil
}

// ValidAuthTypes are the agent-vault service auth schemes clem accepts.
var ValidAuthTypes = map[string]bool{
	"bearer": true, "basic": true, "api-key": true, "custom": true, "passthrough": true,
}

// UnbrokerableSecrets are credential keys that agent-vault cannot broker over
// HTTP and must therefore stay as real values in .env: the Discord gateway
// token (sent in a post-upgrade WebSocket frame, not an HTTP header) and the
// SSH/Elasticsearch creds (not HTTP at all). Listing any of these in
// brokered_secrets is a hard config error.
var UnbrokerableSecrets = map[string]bool{
	"DISCORD_TOKEN": true,
	"SSH_HOST":      true,
	"SSH_USER":      true,
	"SSH_KEY_PATH":  true,
	"ES_USER":       true,
	"ES_PASSWORD":   true,
}

// IsAgentVault reports whether the agent-vault backend is selected.
func (v VaultBackend) IsAgentVault() bool { return v.Backend == "agent-vault" }

// SystemUserOrDefault returns the vault system user, default clem-vault.
func (v VaultBackend) SystemUserOrDefault() string {
	if v.SystemUser == "" {
		return "clem-vault"
	}
	return v.SystemUser
}

// AddrOrDefault returns the management API address, default localhost:14321.
func (v VaultBackend) AddrOrDefault() string {
	if v.Addr == "" {
		return "http://127.0.0.1:14321"
	}
	return v.Addr
}

// ProxyHostOrDefault returns the MITM proxy host:port, default 127.0.0.1:14322.
func (v VaultBackend) ProxyHostOrDefault() string {
	if v.ProxyHost == "" {
		return "127.0.0.1:14322"
	}
	return v.ProxyHost
}

// CACertPathOrDefault returns the CA cert path, default under /etc/clem.
func (v VaultBackend) CACertPathOrDefault() string {
	if v.CACertPath == "" {
		return "/etc/clem/agent-vault-ca.pem"
	}
	return v.CACertPath
}

// EgressConfig configures hard egress containment via pipelock + a per-agent
// nftables UID firewall. When enabled, each agent's outbound traffic is forced
// through a single pipelock forward proxy (run as a dedicated non-login system
// user) on loopback; everything else is rejected by the kernel firewall, so a
// compromised agent cannot reach the network except via the proxy.
//
// This supersedes the per-agent EgressRestrictionExperimental flag, which used
// systemd IPAddressAllow with hardcoded CIDRs. The CIDR approach is replaced by
// domain allowlisting in pipelock + a loopback-only systemd block as a second
// kernel layer.
type EgressConfig struct {
	// Enabled turns on pipelock + the per-agent UID firewall for every agent
	// that does not individually override via AgentConfig.Egress.
	Enabled bool `yaml:"enabled"`
	// Posture maps to pipelock's mode: "strict" (allowlist-only), "balanced"
	// (block known-bad, default), or "audit" (log-only, block nothing).
	Posture string `yaml:"posture"`
	// Domains is the outbound allowlist written into pipelock's api_allowlist.
	// Hostnames, no scheme. pipelock wildcard "*.example.com" also matches the
	// apex. Empty falls back to DefaultEgressDomains.
	Domains []string `yaml:"domains"`
	// ProxyPort is the loopback port pipelock's forward proxy listens on.
	// Default 8888.
	ProxyPort int `yaml:"proxy_port"`
	// ProxyUser is the dedicated non-login system user that runs pipelock.
	// Default "clem-proxy".
	ProxyUser string `yaml:"proxy_user"`
	// TLSIntercept enables pipelock body/response scanning, which requires
	// distributing the pipelock CA into each agent's trust stores. Default
	// false — CONNECT-only (SNI/host allowlist + audit) needs no CA.
	TLSIntercept bool `yaml:"tls_intercept"`
	// AllowLocalhostPorts are loopback ports the agent UID may reach besides
	// the proxy (e.g. 11434 for Ollama). The proxy port is always allowed.
	//
	// WARNING: any daemon listening on an allowed loopback port runs as a
	// non-contained UID and egresses freely, so it is an SSRF pivot — only
	// list services that cannot be coerced into making outbound requests on
	// the agent's behalf.
	AllowLocalhostPorts []int `yaml:"allow_localhost_ports"`
}

// DefaultEgressDomains is the allowlist applied when egress is enabled but no
// domains are configured: the minimum an agent needs to reach Anthropic and
// GitHub. pipelock wildcards match the apex too.
var DefaultEgressDomains = []string{"*.anthropic.com", "github.com", "*.githubusercontent.com"}

// PostureOrDefault returns the configured pipelock mode, defaulting to balanced.
func (e EgressConfig) PostureOrDefault() string {
	if e.Posture == "" {
		return "balanced"
	}
	return e.Posture
}

// ProxyPortOrDefault returns the configured proxy port, defaulting to 8888.
func (e EgressConfig) ProxyPortOrDefault() int {
	if e.ProxyPort == 0 {
		return 8888
	}
	return e.ProxyPort
}

// ProxyUserOrDefault returns the configured proxy system user, default clem-proxy.
func (e EgressConfig) ProxyUserOrDefault() string {
	if e.ProxyUser == "" {
		return "clem-proxy"
	}
	return e.ProxyUser
}

// DomainsOrDefault returns the configured allowlist or DefaultEgressDomains.
func (e EgressConfig) DomainsOrDefault() []string {
	if len(e.Domains) == 0 {
		return DefaultEgressDomains
	}
	return e.Domains
}

// EgressEnabledFor reports whether egress containment applies to an agent.
// Resolution order: explicit per-agent override, then the deprecated per-agent
// egress_restriction_experimental flag, then the top-level egress.enabled.
func (c *Config) EgressEnabledFor(agentKey string) bool {
	ac := c.Agents[agentKey]
	if ac.Egress != nil {
		return *ac.Egress
	}
	if ac.EgressRestrictionExperimental {
		return true
	}
	return c.Egress.Enabled
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
	// effortLevel setting. Accepts "low", "medium", "high", "xhigh", "max".
	// Empty = use Claude Code's own default (currently medium). Lowering trims
	// output tokens — the dominant cost driver in agent loops.
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
	// Sidecars lists names of mcp_sidecars.servers this agent subscribes to. Each
	// gives the agent a loopback HTTP MCP endpoint + a scoped bearer token, with
	// the upstream credential held by the clem-mcp user (never this agent's .env).
	Sidecars []string `yaml:"sidecars"`
	// Permissions configures root-owned /etc/claude-code/managed-settings.json
	// deny rules for this agent. managed-settings.json has higher precedence
	// than the agent's own ~/.claude/settings.json and cannot be overridden by
	// the agent user. Deny patterns are merged from all agents at provision time.
	// Empty or absent block means no clem-injected denies for this agent.
	//
	// WARNING: Bash(...) arg-pattern denies are fragile — they do not catch
	// equivalent invocations (env-var URLs, renamed remotes, aliased flags).
	// For high-stakes constraints, add a PreToolUse hook in addition to or
	// instead of deny patterns.
	Permissions PermissionsConfig `yaml:"permissions"`
	// Egress overrides the top-level egress.enabled for this agent. nil =
	// inherit. Set false to exclude one agent from containment, or true to
	// opt a single agent in while the top-level block is off.
	Egress *bool `yaml:"egress"`
	// VaultBroker opts this agent into agent-vault credential brokering
	// (Phase 2). Requires vault.backend: agent-vault. When false (default) the
	// agent uses the legacy .env flow unchanged — making the whole feature
	// per-agent opt-in so a research-preview proxy outage can't take down
	// agents that never enrolled.
	VaultBroker bool `yaml:"vault_broker"`
	// BrokeredSecrets lists which vault keys are HTTP-brokered (placeholder in
	// .env, real value only inside agent-vault). Keys NOT listed fall back to
	// .env materialization — this is how DISCORD_TOKEN (gateway, unbrokerable)
	// stays real while ANTHROPIC_API_KEY/Slack/Typefully/GH_TOKEN are brokered.
	BrokeredSecrets []string `yaml:"brokered_secrets"`
	// EgressRestrictionExperimental is DEPRECATED — superseded by the top-level
	// egress block (pipelock + nftables). When set true it still opts the agent
	// into egress containment (treated like egress: true) but Load logs a
	// deprecation warning. The old hardcoded-CIDR systemd allowlist is gone.
	//
	// Historical note — the original approach added systemd IPAddressDeny=any +
	// IPAddressAllow rules with the following known limitations:
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
	// ResourceLimits caps CPU/memory/IO for the agent's systemd service via
	// cgroup directives. Use when co-locating agents with other workloads
	// (e.g. CI runners on the same VPS) so one cannot starve the other.
	// Empty fields are omitted from the generated unit — systemd defaults apply.
	ResourceLimits ResourceLimits `yaml:"resource_limits"`
}

// ResourceLimits maps directly to systemd cgroup directives. All fields
// optional; empty values are omitted from the rendered service unit.
//
// Field strings are written verbatim into the [Service] section; validation
// is delegated to systemd. Standard formats:
//   - CPUQuota:   "150%" (1.5 cores), "50%" (half a core), "200ms/1s"
//   - MemoryHigh: "8G", "512M" (soft throttle — process slowed but not killed)
//   - MemoryMax:  "10G" (hard kill — fires before global OOM-killer)
//   - CPUWeight:  1..10000 (default 100; higher = more shares under contention)
//   - IOWeight:   1..10000 (default 100; higher = more disk bandwidth)
type ResourceLimits struct {
	CPUQuota   string `yaml:"cpu_quota"`
	MemoryHigh string `yaml:"memory_high"`
	MemoryMax  string `yaml:"memory_max"`
	CPUWeight  int    `yaml:"cpu_weight"`
	IOWeight   int    `yaml:"io_weight"`
}

// Directives renders the resource-limit block for injection into a systemd
// service unit's [Service] section. Returns an empty string when no fields
// are set, so callers can safely concatenate without conditional logic.
func (r ResourceLimits) Directives() string {
	var lines []string
	if r.CPUQuota != "" {
		lines = append(lines, "CPUQuota="+r.CPUQuota)
	}
	if r.MemoryHigh != "" {
		lines = append(lines, "MemoryHigh="+r.MemoryHigh)
	}
	if r.MemoryMax != "" {
		lines = append(lines, "MemoryMax="+r.MemoryMax)
	}
	if r.CPUWeight != 0 {
		lines = append(lines, fmt.Sprintf("CPUWeight=%d", r.CPUWeight))
	}
	if r.IOWeight != 0 {
		lines = append(lines, fmt.Sprintf("IOWeight=%d", r.IOWeight))
	}
	if len(lines) == 0 {
		return ""
	}
	return "# Resource limits (resource_limits in clem.yaml)\n" +
		strings.Join(lines, "\n") + "\n"
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

// PipelockServiceName returns the systemd service name for the egress proxy.
func (c *Config) PipelockServiceName() string {
	return fmt.Sprintf("clem-pipelock-%s.service", c.Project)
}

// NftablesServiceName returns the systemd service name for the egress firewall.
func (c *Config) NftablesServiceName() string {
	return fmt.Sprintf("clem-nftables-%s.service", c.Project)
}

// AgentVaultServiceName returns the systemd service name for the agent-vault
// credential proxy.
func (c *Config) AgentVaultServiceName() string {
	return fmt.Sprintf("clem-agent-vault-%s.service", c.Project)
}

// SidecarServiceName returns the systemd service name for a sidecar listener.
// For a per-agent sidecar there is one listener per subscriber, so the agent
// key disambiguates; shared sidecars pass an empty agentKey.
func (c *Config) SidecarServiceName(name, agentKey string) string {
	if agentKey != "" {
		return fmt.Sprintf("clem-mcp-%s-%s-%s.service", c.Project, name, agentKey)
	}
	return fmt.Sprintf("clem-mcp-%s-%s.service", c.Project, name)
}

// SidecarNftablesServiceName returns the systemd service name for the sidecar
// loopback firewall (distinct from the egress firewall; sidecar isolation must
// hold even on hosts with no egress containment).
func (c *Config) SidecarNftablesServiceName() string {
	return fmt.Sprintf("clem-sidecar-nft-%s.service", c.Project)
}

// SidecarListener is one concrete loopback listener clem provisions for a
// privileged MCP sidecar. A shared sidecar yields one listener (Subscribers =
// every subscribing agent, AgentKey = ""); a per-agent sidecar yields one
// listener per subscriber (Subscribers = [that agent], AgentKey = that agent).
type SidecarListener struct {
	Server      SidecarServer
	Port        int
	Subscribers []string // sorted agent keys allowed to reach this listener
	AgentKey    string   // subscriber key for per-agent identity; "" when shared
}

// SidecarListeners returns the deterministic listener layout for all subscribed
// sidecars: the single source of truth for port allocation shared by config
// validation, the systemd/nftables generators, provisioning, and the runner.
// Servers are taken in config order; per-agent subscribers in sorted order;
// ports allocated upward from base_port. Unsubscribed servers yield no listener.
func (c *Config) SidecarListeners() []SidecarListener {
	var out []SidecarListener
	idx := 0
	for _, s := range c.MCPSidecars.Servers {
		subs := c.sidecarSubscribers(s.Name)
		if len(subs) == 0 {
			continue
		}
		if s.IdentityKind() == "per-agent" {
			for _, ak := range subs {
				out = append(out, SidecarListener{
					Server:      s,
					Port:        c.MCPSidecars.SidecarPort(idx),
					Subscribers: []string{ak},
					AgentKey:    ak,
				})
				idx++
			}
			continue
		}
		out = append(out, SidecarListener{
			Server:      s,
			Port:        c.MCPSidecars.SidecarPort(idx),
			Subscribers: subs,
		})
		idx++
	}
	return out
}

// sidecarSubscribers returns the sorted agent keys subscribing to a sidecar.
func (c *Config) sidecarSubscribers(name string) []string {
	var subs []string
	for ak, ac := range c.Agents {
		for _, n := range ac.Sidecars {
			if n == name {
				subs = append(subs, ak)
				break
			}
		}
	}
	sort.Strings(subs)
	return subs
}

// IsBrokered reports whether a secret key is HTTP-brokered for this agent
// (placeholder in .env, real value only inside agent-vault).
func (ac AgentConfig) IsBrokered(key string) bool {
	if !ac.VaultBroker {
		return false
	}
	for _, k := range ac.BrokeredSecrets {
		if k == key {
			return true
		}
	}
	return false
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
	switch cfg.Egress.Posture {
	case "", "strict", "balanced", "audit":
		// valid
	default:
		return nil, fmt.Errorf("egress.posture must be strict, balanced, or audit, got %q", cfg.Egress.Posture)
	}
	if cfg.Egress.ProxyPort != 0 && (cfg.Egress.ProxyPort < 1024 || cfg.Egress.ProxyPort > 65535) {
		return nil, fmt.Errorf("egress.proxy_port must be 1024-65535, got %d", cfg.Egress.ProxyPort)
	}
	// Out-of-range entries would land verbatim in the nftables `tcp dport` set
	// and make `nft -f` reject the whole ruleset (→ agents fail to start, since
	// the firewall unit is a hard Requires=).
	for _, p := range cfg.Egress.AllowLocalhostPorts {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("egress.allow_localhost_ports: %d out of range 1-65535", p)
		}
	}
	switch cfg.Vault.Backend {
	case "", "env", "agent-vault":
		// valid
	default:
		return nil, fmt.Errorf("vault.backend must be env or agent-vault, got %q", cfg.Vault.Backend)
	}
	usedPorts := make(map[int]string)
	// Reserve the egress proxy port so no agent's web terminal collides with it.
	if cfg.Egress.Enabled {
		usedPorts[cfg.Egress.ProxyPortOrDefault()] = "egress.proxy_port"
	}
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
		if _, err := ac.ProviderEnv(); err != nil {
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
		switch ac.Effort {
		case "", "low", "medium", "high", "xhigh", "max":
			// valid
		default:
			return nil, fmt.Errorf("agent %s: effort must be low, medium, high, xhigh, or max, got %q", key, ac.Effort)
		}
		if ac.EgressRestrictionExperimental {
			fmt.Fprintf(os.Stderr, "warning: agent %s: egress_restriction_experimental is deprecated — use the top-level egress: block (pipelock + nftables containment)\n", key)
		}
		if ac.VaultBroker {
			if !cfg.Vault.IsAgentVault() {
				return nil, fmt.Errorf("agent %s: vault_broker requires vault.backend: agent-vault", key)
			}
			// Brokering and pipelock egress containment are NOT composable in
			// v1: a brokered agent's HTTPS_PROXY points at agent-vault (which
			// agent-vault cannot chain through pipelock), so enabling both would
			// silently route the agent's traffic around pipelock's allowlist and
			// audit log. Reject the combination rather than weaken containment.
			if cfg.EgressEnabledFor(key) {
				return nil, fmt.Errorf("agent %s: vault_broker and egress containment cannot both be enabled (agent-vault cannot chain through pipelock); pick one per agent", key)
			}
			for _, s := range ac.BrokeredSecrets {
				if UnbrokerableSecrets[s] {
					return nil, fmt.Errorf("agent %s: %q cannot be brokered by agent-vault (gateway/SSH/non-HTTP); keep it in .env (remove from brokered_secrets)", key, s)
				}
			}
			// Brokered secrets are consolidated into a single per-agent agent-vault
			// vault at provision (see vault.SeedVault/cmd provision), so they may
			// span multiple sops vaults — no first-vault constraint.
		}
		ac.normalizeSubagentModel()
		if err := ac.validateExtensions(key); err != nil {
			return nil, err
		}
		cfg.Agents[key] = ac
	}
	if err := cfg.validateVaultServices(); err != nil {
		return nil, err
	}
	if err := cfg.validateMCPSidecars(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// validateMCPSidecars checks the privileged-sidecar declarations and that every
// agent subscription references a defined server.
func (cfg *Config) validateMCPSidecars() error {
	subscribed := false
	for _, ac := range cfg.Agents {
		if len(ac.Sidecars) > 0 {
			subscribed = true
		}
	}
	if len(cfg.MCPSidecars.Servers) == 0 && !subscribed {
		return nil
	}
	if p := cfg.MCPSidecars.BasePort; p != 0 && (p < 1 || p > 65535) {
		return fmt.Errorf("mcp_sidecars.base_port %d out of range 1-65535", p)
	}
	defined := map[string]bool{}
	for i, s := range cfg.MCPSidecars.Servers {
		where := fmt.Sprintf("mcp_sidecars.servers[%d]", i)
		if s.Name != "" {
			where = "mcp_sidecars " + s.Name
		}
		if !validName.MatchString(s.Name) {
			return fmt.Errorf("%s: name must match %s", where, validName.String())
		}
		if defined[s.Name] {
			return fmt.Errorf("%s: duplicate sidecar name %q", where, s.Name)
		}
		defined[s.Name] = true
		if s.Command == "" {
			return fmt.Errorf("%s: command is required", where)
		}
		if len(s.Secrets) == 0 {
			return fmt.Errorf("%s: at least one secret is required (a sidecar with no secret to hide should be a normal MCP server)", where)
		}
		if !strings.HasPrefix(s.Command, "/") {
			return fmt.Errorf("%s: command must be an absolute path, got %q", where, s.Command)
		}
		tn := s.ToolName()
		if !validName.MatchString(tn) {
			return fmt.Errorf("%s: tool name %q must match %s (set `tool:` to override)", where, tn, validName.String())
		}
		if reservedMCPNames[tn] {
			return fmt.Errorf("%s: tool name %q collides with a builtin MCP server (%v); set a distinct `tool:`", where, tn, []string{"discord-bot", "slack-mcp", "social", "browser-render"})
		}
		for _, k := range s.Secrets {
			if !secretKeyRe.MatchString(k) {
				return fmt.Errorf("%s: secret key %q must match %s", where, k, secretKeyRe.String())
			}
		}
		switch s.Identity {
		case "", "shared", "per-agent":
		default:
			return fmt.Errorf("%s: identity must be shared or per-agent, got %q", where, s.Identity)
		}
		switch s.Transport {
		case "", "http":
		case "sse":
			fmt.Fprintf(os.Stderr, "warning: %s: transport sse is deprecated; prefer http (streamable-HTTP)\n", where)
		default:
			return fmt.Errorf("%s: transport must be http or sse, got %q", where, s.Transport)
		}
		// A shared sidecar's one credential set comes from a named sops vault;
		// per-agent reads each subscriber's own vaults at provision, so it needs none.
		if s.IdentityKind() == "shared" && s.SecretsVault == "" {
			return fmt.Errorf("%s: a shared sidecar requires secrets_vault (the sops vault to read its secrets from)", where)
		}
	}
	for key, ac := range cfg.Agents {
		for _, name := range ac.Sidecars {
			if !defined[name] {
				return fmt.Errorf("agent %s: sidecar %q is not defined in mcp_sidecars.servers", key, name)
			}
		}
		// The runner only emits the http MCP entry into the claude-code .mcp.json;
		// an opencode agent would get the listener + firewall but never the config,
		// a silent no-op. Reject rather than mislead.
		if len(ac.Sidecars) > 0 && ac.RuntimeKind() == "opencode" {
			return fmt.Errorf("agent %s: sidecars are not supported with runtime opencode", key)
		}
	}
	// Deterministic loopback-port allocation (SidecarListeners is the single
	// source of truth). A shared sidecar listens once; a per-agent sidecar once
	// per subscribing agent. Check the range fits below 65535 and never collides
	// with another loopback port clem binds on the same host.
	listeners := cfg.SidecarListeners()
	base := cfg.MCPSidecars.BasePortOrDefault()
	if n := len(listeners); n > 0 && base+n-1 > 65535 {
		return fmt.Errorf("mcp_sidecars: %d listeners from base_port %d overflow past 65535", n, base)
	}
	reserved := map[int]string{}
	for key, ac := range cfg.Agents {
		if ac.WebTerminalPort != 0 {
			reserved[ac.WebTerminalPort] = "agent " + key + " web_terminal_port"
		}
	}
	egressInUse := cfg.Egress.Enabled
	for key := range cfg.Agents {
		if cfg.EgressEnabledFor(key) {
			egressInUse = true
		}
	}
	if egressInUse {
		reserved[cfg.Egress.ProxyPortOrDefault()] = "egress proxy_port"
		for _, p := range cfg.Egress.AllowLocalhostPorts {
			reserved[p] = "egress allow_localhost_ports"
		}
	}
	if cfg.Vault.IsAgentVault() {
		// Default mgmt/MITM ports; a custom vault address near base_port is not parsed here.
		reserved[14321] = "agent-vault management port"
		reserved[14322] = "agent-vault MITM port"
	}
	for _, l := range listeners {
		if owner, clash := reserved[l.Port]; clash {
			return fmt.Errorf("mcp_sidecars: sidecar %q port %d collides with %s", l.Server.Name, l.Port, owner)
		}
	}
	return nil
}

// validateVaultServices checks the agent-vault injection rules. Services are
// global (not vault-bound): clem applies each into the consolidated vault of any
// agent that brokers the referenced credential keys. Warns on services no agent
// can use and on brokered secrets with no service to inject them.
func (cfg *Config) validateVaultServices() error {
	if len(cfg.Vault.Services) == 0 {
		return nil
	}
	if !cfg.Vault.IsAgentVault() {
		return fmt.Errorf("vault.services requires vault.backend: agent-vault")
	}
	// All credential keys brokered by some agent.
	allBrokered := map[string]bool{}
	for _, ac := range cfg.Agents {
		if !ac.VaultBroker {
			continue
		}
		for _, s := range ac.BrokeredSecrets {
			allBrokered[s] = true
		}
	}
	serviceKeys := map[string]bool{}
	for i, s := range cfg.Vault.Services {
		where := fmt.Sprintf("vault.services[%d]", i)
		if s.Name != "" {
			where = "vault.services " + s.Name
		}
		if !serviceNameRe.MatchString(s.Name) {
			return fmt.Errorf("%s: name must be 3-64 lowercase alphanumeric/hyphen chars", where)
		}
		if s.Host == "" {
			return fmt.Errorf("%s: host is required", where)
		}
		if !ValidAuthTypes[s.AuthType] {
			return fmt.Errorf("%s: auth_type must be bearer, basic, api-key, custom, or passthrough, got %q", where, s.AuthType)
		}
		switch s.AuthType {
		case "bearer":
			if s.TokenKey == "" {
				return fmt.Errorf("%s: auth_type bearer requires token_key", where)
			}
		case "basic":
			if s.UsernameKey == "" || s.PasswordKey == "" {
				return fmt.Errorf("%s: auth_type basic requires username_key and password_key", where)
			}
		case "api-key":
			if s.APIKeyKey == "" {
				return fmt.Errorf("%s: auth_type api-key requires api_key_key", where)
			}
		}
		anyBrokered := false
		for _, k := range s.CredentialKeys() {
			serviceKeys[k] = true
			if allBrokered[k] {
				anyBrokered = true
			}
		}
		if len(allBrokered) > 0 && !anyBrokered {
			fmt.Fprintf(os.Stderr, "warning: %s references credential keys no agent brokers — it will not be applied\n", where)
		}
	}
	// Warn on any brokered secret with no service to inject it.
	for key, ac := range cfg.Agents {
		if !ac.VaultBroker {
			continue
		}
		for _, s := range ac.BrokeredSecrets {
			if !serviceKeys[s] {
				fmt.Fprintf(os.Stderr, "warning: agent %s: brokered secret %q has no matching vault.service — it would egress as a placeholder\n", key, s)
			}
		}
	}
	return nil
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
		if !extensionNameRe.MatchString(mp.Name) {
			return fmt.Errorf("agent %s: marketplace name %q contains invalid characters", key, mp.Name)
		}
		if !extensionRepoRe.MatchString(mp.Repo) {
			return fmt.Errorf("agent %s: marketplace repo %q is not a valid owner/repo", key, mp.Repo)
		}
		if mp.Commit != "" && !commitHashRe.MatchString(mp.Commit) {
			return fmt.Errorf("agent %s: marketplace commit %q is not a valid hex hash", key, mp.Commit)
		}
	}
	for _, pl := range ac.Extensions.Plugins {
		if pl.Name == "" || pl.Marketplace == "" {
			return fmt.Errorf("agent %s: plugin entry missing name or marketplace", key)
		}
		if !extensionNameRe.MatchString(pl.Name) {
			return fmt.Errorf("agent %s: plugin name %q contains invalid characters", key, pl.Name)
		}
		if !extensionNameRe.MatchString(pl.Marketplace) {
			return fmt.Errorf("agent %s: plugin marketplace %q contains invalid characters", key, pl.Marketplace)
		}
	}
	for _, sk := range ac.Extensions.Skills {
		if sk.Name == "" || sk.Repo == "" {
			return fmt.Errorf("agent %s: skill entry missing name or repo", key)
		}
		if !extensionNameRe.MatchString(sk.Name) {
			return fmt.Errorf("agent %s: skill name %q contains invalid characters", key, sk.Name)
		}
		if !extensionRepoRe.MatchString(sk.Repo) {
			return fmt.Errorf("agent %s: skill repo %q is not a valid owner/repo", key, sk.Repo)
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
