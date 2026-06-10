// Egress containment configuration (pipelock + per-agent nftables UID
// firewall): schema, defaults, and per-agent resolution.

package config

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
