// Privileged MCP sidecar configuration: schema, per-listener port
// allocation, and validation. Provision wiring lives in cmd/provision.go
// and internal/proxy.

package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// reservedMCPNames are MCP server names clem's runner hardcodes (runner.go); a
// sidecar's tool name must not collide with them or it would shadow/duplicate a
// builtin in the agent's .mcp.json.
var reservedMCPNames = map[string]bool{
	"discord-bot": true, "slack-mcp": true, "social": true, "browser-render": true,
}

// secretKeyRe matches an env-var-style credential key (what a sidecar's secrets
// and the vault keys look like).
var secretKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// MCPSidecarsConfig configures privileged MCP "sidecars": MCP servers that hold
// a secret the agent must USE but not READ, run as a dedicated non-agent system
// user, and are reached by the agent over a loopback HTTP MCP transport. This is
// the "sidecar" disposition in clem's secret-protection model — for credentials
// the broker can't rewrite (gateway WebSocket tokens) or that warrant scoped
// access (e.g. read-only Elasticsearch). A stdio MCP server cannot provide this:
// it runs as the agent's own UID, so its secret is in the agent's reach.
//
// Provision wiring lives in cmd/provision.go (provisionMCPSidecars) and
// internal/proxy: the clem-mcp system user, the pinned mcp-proxy stdio→HTTP
// bridge, one systemd loopback listener per subscribed sidecar (upstream
// secret supplied root-side via EnvironmentFile), a per-port nftables rule
// restricting each listener to its subscriber UID(s) — the listener
// Requires= that firewall unit, fail-closed — and the http entry in the
// runner-generated .mcp.json. See docs/threat-model.md.
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
// loopback HTTP MCP endpoint, reachable solely from its own UID (enforced by
// the sidecar nftables rule); the upstream credential lives only in the
// sidecar process (sourced from Secrets, written to a root-owned env file,
// never any agent .env).
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
