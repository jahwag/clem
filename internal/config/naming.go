// Derived names: every OS user, systemd unit, and firewall table clem
// provisions is named here, so generators and CLI commands agree.

package config

import (
	"fmt"
)

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
