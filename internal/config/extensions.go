// Agent extension configuration: marketplaces, plugins, skills, MCP
// servers, and managed-settings deny rules, plus their validation.

package config

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// vaultRefRe matches ${vault:BUCKET.KEY} in MCP server env values.
var vaultRefRe = regexp.MustCompile(`\$\{vault:([^.}]+)\.([^}]+)\}`)

// extensionNameRe allows alphanumeric names with dots, hyphens, and underscores.
// Rejects semicolons, spaces, backticks, and other shell-special characters.
var extensionNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// extensionRepoRe requires owner/repo format with safe characters only.
var extensionRepoRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+/[a-zA-Z0-9._-]+$`)

// commitHashRe accepts hex SHA strings (full or prefix).
var commitHashRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

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
//
// KNOWN GAP: yaml.Node.Decode cannot inherit the parent decoder's
// KnownFields strictness, so an unknown key inside the struct form (e.g. a
// misspelled `marketplce:`) is silently dropped here even though Load
// rejects unknown keys everywhere else. Tolerable because no plugin field
// is security-bearing; revisit if one ever becomes so.
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
