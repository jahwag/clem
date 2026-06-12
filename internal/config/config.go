package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jahwag/clem/internal/coordination"
)

var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// isPlausibleGitURL accepts the URL shapes git clone understands: https://,
// http://, git://, ssh://, and the scp-style git@host:owner/repo. Operator-only
// config so this is a shape check, not a security boundary.
func isPlausibleGitURL(s string) bool {
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "git://") || strings.HasPrefix(s, "ssh://") {
		return true
	}
	if i := strings.Index(s, "@"); i > 0 {
		if j := strings.Index(s[i+1:], ":"); j > 0 {
			return true
		}
	}
	return false
}

// snowflakeRe matches a Discord snowflake ID: 17–19 decimal digits.
var snowflakeRe = regexp.MustCompile(`^[0-9]{17,19}$`)

// gitEmailInvalid matches ASCII whitespace and control characters — the
// characters that corrupt the line-delimited files git_email is written
// into: ~/.ssh/allowed_signers uses a space-delimited principal field (a
// space or newline injects a second principal mapped to the agent's signing
// key), and ~/.gitconfig is newline-delimited. Unicode separators (U+2028,
// NEL, NBSP) pass, but neither git config nor OpenSSH's allowed_signers
// parser treats them as line or field breaks.
var gitEmailInvalid = regexp.MustCompile(`[\s\x00-\x1f\x7f]`)

// agentNameInvalid matches ASCII control characters — the characters that
// corrupt the line-delimited sinks name and role are written into. The worst
// sink is systemd unit Description= lines (a newline terminates the directive
// and injects arbitrary subsequent directives, including a second [Service]
// section with a crafted ExecStart); name also reaches the generated agent
// doc and several bash strings in the runner templates, where a newline adds
// log/script lines. Spaces stay legal — display names like "Lead Software
// Engineer" are the common case. systemd splits unit files only on ASCII
// newline, so unicode separators (U+2028, NEL) are not line breaks in that
// sink. Shell metacharacters (quotes, $, backticks) are out of scope here:
// escaping them belongs at the template render sites.
var agentNameInvalid = regexp.MustCompile(`[\x00-\x1f\x7f]`)

// githubLoginRe matches a valid GitHub username per GitHub's own rules.
var githubLoginRe = regexp.MustCompile(`^[a-zA-Z0-9-]{1,39}$`)

// modelRe constrains model IDs to the characters real providers use:
// claude-sonnet-4-6, qwen2.5:7b-instruct, library/model:tag, Vertex's
// claude-sonnet-4-6@20250514, Bedrock inference-profile ARNs
// (arn:aws:bedrock:...:inference-profile/...). The value is rendered into a
// single-quoted --model argument in the generated runner.sh, so quotes,
// whitespace, and shell metacharacters must not appear.
var modelRe = regexp.MustCompile(`^[A-Za-z0-9._:/@-]+$`)

// validBindRe matches a safe web_terminal_bind value: an interface name,
// IPv4/IPv6 address, hostname, or unix socket path (everything ttyd -i
// accepts). The value lands verbatim on the ExecStart= line of the ttyd
// systemd unit, where a newline starts a new directive and whitespace
// splits arguments — so only this conservative character set is allowed.
var validBindRe = regexp.MustCompile(`^[0-9A-Za-z./:_-]+$`)

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
	// SkillsRepo names a git repo whose `shared/<skill>/` and
	// `<agentKey>/<skill>/` subdirs are symlinked into each agent's
	// ~/.claude/skills/. Agents PR new skills there; clem provision pulls and
	// re-syncs. Empty = no skills repo wiring.
	//
	// Accepts any URL git clone understands:
	//   https://github.com/owner/repo
	//   https://github.com/owner/repo.git
	//   git@gitlab.com:owner/repo.git
	//   ssh://git@self-hosted/owner/repo.git
	// The cache directory name is derived from the URL's last path segment
	// (with .git stripped).
	SkillsRepo string `yaml:"skills_repo"`
	// Extra collects top-level keys not matched by any field above (the
	// decoder is otherwise strict — see Load). Only "x-"-prefixed extension
	// keys are accepted, as holders for shared YAML anchors (the
	// docker-compose convention); anything else is treated as a typo and
	// rejected, because a silently-dropped key like `egresss:` would leave a
	// security control off while the operator believes it is on.
	Extra map[string]yaml.Node `yaml:",inline"`
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
	// active hours (07-22 host time). Default 5m.
	Iteration string `yaml:"iteration"`
	// IterationNight is the sleep between sessions during night hours
	// (22:00-07:00 host time). Same format as Iteration. Empty = match
	// Iteration. On a Claude subscription the prompt-cache TTL is 1h,
	// refreshed on access, so values up to ~45m keep session starts warm;
	// longer values trade one cold start per gap for fewer idle wakeups.
	IterationNight  string   `yaml:"iteration_night"`
	Vaults          []string `yaml:"vaults"`
	Prompt          string   `yaml:"prompt"`
	WebTerminalPort int      `yaml:"web_terminal_port"`
	// WebTerminalBind controls which interface ttyd listens on. Default
	// 127.0.0.1 for safety (expects SSH tunnel or reverse proxy). Use
	// 0.0.0.0 when running inside a container with host port-forward.
	WebTerminalBind string       `yaml:"web_terminal_bind"`
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
	// gives the agent a loopback HTTP MCP endpoint locked to its UID by the
	// sidecar nftables rule, with the upstream credential held by the clem-mcp
	// user (never this agent's .env).
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
// String fields are checked against resourceLimitValue at Load() time so a
// value cannot break out of its directive line in the generated [Service]
// section; semantic validation is delegated to systemd. Standard formats:
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

// resourceLimitValue permits the documented value formats for CPUQuota,
// MemoryHigh, and MemoryMax ("150%", "200ms/1s", "8G", "infinity") while
// excluding newlines and every other character that would let a value span
// directive lines in the rendered unit. Directives() concatenates these
// strings into a newline-delimited [Service] section, so an embedded newline
// would inject arbitrary directives (e.g. a second ExecStart=).
var resourceLimitValue = regexp.MustCompile(`^[0-9A-Za-z%./]*$`)

// validate rejects string field values that could escape their directive
// line in the systemd unit rendered by Directives(). key names the agent
// in error messages.
func (r ResourceLimits) validate(key string) error {
	fields := []struct {
		name string
		val  string
	}{
		{"cpu_quota", r.CPUQuota},
		{"memory_high", r.MemoryHigh},
		{"memory_max", r.MemoryMax},
	}
	for _, f := range fields {
		if !resourceLimitValue.MatchString(f.val) {
			return fmt.Errorf("agent %s: resource_limits.%s %q must match %s", key, f.name, f.val, resourceLimitValue.String())
		}
	}
	return nil
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

// IterationNightDuration returns the parsed night iteration period, falling
// back to IterationDuration when iteration_night is unset.
func (ac AgentConfig) IterationNightDuration() (time.Duration, error) {
	if ac.IterationNight == "" {
		return ac.IterationDuration()
	}
	d, err := time.ParseDuration(ac.IterationNight)
	if err != nil {
		return 0, fmt.Errorf("invalid iteration_night %q: %w (expected Go duration like 30s, 1m30s, 2h)", ac.IterationNight, err)
	}
	if d < time.Second {
		return 0, fmt.Errorf("iteration_night %q is too small (minimum 1s)", ac.IterationNight)
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
//
// Unknown keys are a hard error (KnownFields). clem.yaml carries security
// dispositions — egress, vault_broker, brokered_secrets, permissions — and a
// silently-ignored typo in any of them would leave a control off while the
// operator believes it is on. Fail loud at load instead. Top-level "x-"
// extension keys are the one exception (collected into Config.Extra), so
// shared YAML anchors still have a place to live:
//
//	x-defaults: &defaults
//	  model: claude-sonnet-4-6
//	agents:
//	  lead:
//	    <<: *defaults
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	data = expandEnv(data)
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config: %w (unknown keys are rejected — check for typos against the clem.yaml reference)", err)
	}
	// Top-level keys land in Extra instead of erroring (the inline map takes
	// precedence over KnownFields there); enforce the x- convention manually.
	extraKeys := make([]string, 0, len(cfg.Extra))
	for k := range cfg.Extra {
		if !strings.HasPrefix(k, "x-") {
			extraKeys = append(extraKeys, k)
		}
	}
	if len(extraKeys) > 0 {
		sort.Strings(extraKeys)
		return nil, fmt.Errorf("unknown top-level key(s) in config: %s (misspelled? prefix with \"x-\" if intended as an extension/anchor key)", strings.Join(extraKeys, ", "))
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
	if cfg.SkillsRepo != "" && !isPlausibleGitURL(cfg.SkillsRepo) {
		return nil, fmt.Errorf("skills_repo %q is not a recognized git URL (expected https://, git://, ssh://, or git@host:path)", cfg.SkillsRepo)
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
		if ac.Model != "" && !modelRe.MatchString(ac.Model) {
			return nil, fmt.Errorf("agent %s: model %q must match %s (rendered into a quoted shell argument in runner.sh)", key, ac.Model, modelRe.String())
		}
		if _, err := ac.IterationDuration(); err != nil {
			return nil, fmt.Errorf("agent %s: %w", key, err)
		}
		if _, err := ac.IterationNightDuration(); err != nil {
			return nil, fmt.Errorf("agent %s: %w", key, err)
		}
		if _, err := ac.ProviderEnv(); err != nil {
			return nil, fmt.Errorf("agent %s: %w", key, err)
		}
		if ac.GitEmail != "" && gitEmailInvalid.MatchString(ac.GitEmail) {
			return nil, fmt.Errorf("agent %s: git_email must not contain whitespace or control characters, got %q", key, ac.GitEmail)
		}
		if agentNameInvalid.MatchString(ac.Name) {
			return nil, fmt.Errorf("agent %s: name must not contain control characters, got %q", key, ac.Name)
		}
		if agentNameInvalid.MatchString(ac.Role) {
			return nil, fmt.Errorf("agent %s: role must not contain control characters, got %q", key, ac.Role)
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
		if ac.WebTerminalBind != "" && !validBindRe.MatchString(ac.WebTerminalBind) {
			return nil, fmt.Errorf("agent %s: web_terminal_bind must be an interface, IP address, hostname, or socket path (characters [0-9A-Za-z./:_-]), got %q", key, ac.WebTerminalBind)
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
		if err := ac.ResourceLimits.validate(key); err != nil {
			return nil, err
		}
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
