// Egress containment configuration (agent-vault TLS-MITM + per-agent nftables
// UID firewall): schema, defaults, and per-agent resolution.

package config

// EgressConfig configures hard egress containment. Containment is implemented
// via agent-vault: an egress-contained agent is a brokered agent whose outbound
// HTTP(S) is forced through agent-vault's TLS-MITM proxy on loopback, and whose
// UID is otherwise rejected by a per-agent nftables kernel firewall. agent-vault
// allowlists the configured Domains as passthrough services and denies every
// unmatched host, so a compromised agent cannot reach the network except through
// the allowlisted set. Egress therefore REQUIRES vault_broker (see config
// validation): agent-vault is the only containment proxy.
//
// This supersedes the per-agent EgressRestrictionExperimental flag, which used
// systemd IPAddressAllow with hardcoded CIDRs. The CIDR approach is replaced by
// domain allowlisting in agent-vault + a loopback-only systemd block as a second
// kernel layer.
type EgressConfig struct {
	// Enabled turns on egress containment for every agent that does not
	// individually override via AgentConfig.Egress. Contained agents must also
	// enable vault_broker (validated at load).
	Enabled bool `yaml:"enabled"`
	// Domains is the outbound allowlist. Each entry becomes an agent-vault
	// passthrough service; every unmatched host is denied. Hostnames, no scheme.
	// Empty falls back to DefaultEgressDomains.
	Domains []string `yaml:"domains"`
	// AllowLocalhostPorts are loopback ports the agent UID may reach besides
	// agent-vault's MITM port (e.g. 11434 for Ollama).
	//
	// WARNING: any daemon listening on an allowed loopback port runs as a
	// non-contained UID and egresses freely, so it is an SSRF pivot — only
	// list services that cannot be coerced into making outbound requests on
	// the agent's behalf.
	AllowLocalhostPorts []int `yaml:"allow_localhost_ports"`
}

// DefaultEgressDomains is the allowlist applied when egress is enabled but no
// domains are configured: the minimum an agent needs to reach Anthropic and
// GitHub. api.github.com is appended by EgressDomainsOrDefault when GitHub
// coordination is active.
var DefaultEgressDomains = []string{"*.anthropic.com", "github.com", "*.githubusercontent.com"}

// domainsOrDefault returns the configured allowlist or DefaultEgressDomains.
// Unexported: EgressDomainsOrDefault on Config is the public entry point.
func (e EgressConfig) domainsOrDefault() []string {
	if len(e.Domains) == 0 {
		return append([]string(nil), DefaultEgressDomains...)
	}
	return e.Domains
}

// EgressDomainsOrDefault returns the effective egress allowlist for the project,
// appending api.github.com when GitHub coordination is active (so brokered git/
// API polling is not denied by the unmatched-host policy).
func (c *Config) EgressDomainsOrDefault() []string {
	domains := c.Egress.domainsOrDefault()
	if !c.UsesGitHubCoordination() {
		return domains
	}
	for _, d := range domains {
		if d == "api.github.com" {
			return domains
		}
	}
	return append(append([]string(nil), domains...), "api.github.com")
}

// EgressEnabledFor reports whether egress containment applies to an agent.
// Resolution order: explicit per-agent override, then the deprecated per-agent
// experimental flag, then the top-level egress.enabled default.
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
