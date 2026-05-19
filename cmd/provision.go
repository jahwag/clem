package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jahwag/clem/internal/agent"
	"github.com/jahwag/clem/internal/agentdoc"
	"github.com/jahwag/clem/internal/config"
	"github.com/jahwag/clem/internal/proxy"
	"github.com/jahwag/clem/internal/remote"
	"github.com/jahwag/clem/internal/runner"
	"github.com/jahwag/clem/internal/vault"
	"github.com/jahwag/clem/internal/watchdog"
	"github.com/spf13/cobra"
)

var (
	provisionRemote  string
	provisionGHToken string
)

var provisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "Create OS users, write runner.sh, install systemd services and watchdog",
	RunE:  runProvision,
}

func init() {
	rootCmd.AddCommand(provisionCmd)
	provisionCmd.Flags().StringVar(&provisionRemote, "remote", "", "provision on a remote host via SSH (e.g. root@1.2.3.4)")
	provisionCmd.Flags().StringVar(&provisionGHToken, "gh-token", "", "GitHub token for cloning the repo on the remote (falls back to GH_TOKEN env)")
}

// decryptForAgent is the vault decryption entry point used by provisioning.
// A package-level seam (same pattern as agent.sys / runner.userHomeLookup) so
// tests can exercise the env-construction logic without sops on PATH.
var decryptForAgent = vault.DecryptForAgent

func runProvision(cmd *cobra.Command, args []string) error {
	if provisionRemote != "" {
		token := provisionGHToken
		if token == "" {
			token = os.Getenv("GH_TOKEN")
		}
		return remote.Provision(provisionRemote, token)
	}

	if err := requireRoot(); err != nil {
		return err
	}

	fmt.Printf("Provisioning project: %s\n", cfg.Project)

	// Phase 2: stand up the agent-vault credential proxy before the agent loop
	// so per-agent tokens can be minted inside it. No-op unless backend active.
	if cfg.Vault.IsAgentVault() {
		if err := provisionAgentVaultHost(); err != nil {
			return err
		}
	}

	for agentKey, ac := range cfg.Agents {
		if err := provisionAgent(agentKey, ac); err != nil {
			return err
		}
	}

	// Egress containment (pipelock proxy + nftables UID firewall), host-level.
	// Runs after the agent loop so all agent OS users exist for UID resolution.
	if err := provisionEgress(); err != nil {
		return err
	}

	// Privileged MCP sidecars: stand up mcp-proxy listeners under a dedicated
	// system user so secrets that can't be HTTP-brokered stay out of agent .envs.
	// Also after the agent loop — the loopback firewall keys on subscriber UIDs.
	if err := provisionMCPSidecars(); err != nil {
		return err
	}

	if err := provisionWatchdog(); err != nil {
		return err
	}

	if err := provisionManagedSettings(); err != nil {
		return err
	}

	fmt.Printf("\nProvisioning complete. Run 'clem login' then 'clem up'.\n")
	return nil
}

// provisionAgent provisions one agent end-to-end: OS user, runtime CLI, .env
// (plain or brokered), Claude settings, extensions, SSH/git identity, secret
// hooks, workdir + runner script, and systemd services.
func provisionAgent(agentKey string, ac config.AgentConfig) error {
	osUser := cfg.OSUsername(agentKey)
	homeDir := fmt.Sprintf("/home/%s", osUser)
	fmt.Printf("\n[%s] %s (%s)\n", agentKey, ac.Name, osUser)

	// 1. Create OS user
	if err := agent.EnsureUser(osUser); err != nil {
		return fmt.Errorf("agent %s: %w", agentKey, err)
	}

	// 1a. Install the agent's runtime (claude-code or opencode) into the
	// user's home so self-update works and the runner always invokes a
	// binary owned by the agent user.
	runtimeKind := ac.RuntimeKind()
	fmt.Printf("  installing runtime %s for %s\n", runtimeKind, osUser)
	if err := agent.InstallRuntime(osUser, runtimeKind); err != nil {
		return fmt.Errorf("installing %s for %s: %w", runtimeKind, osUser, err)
	}

	// 2. Decrypt and write .env (merged with provider env vars)
	secrets, ghToken, err := writeAgentEnv(agentKey, ac, osUser, homeDir)
	if err != nil {
		return err
	}

	// 3. Write Claude Code settings (skip MCP trust dialog, onboarding)
	if err := agent.WriteSettings(osUser, homeDir, cfg.Project, ac.Effort); err != nil {
		return fmt.Errorf("writing settings for %s: %w", agentKey, err)
	}
	fmt.Printf("  wrote %s/.claude/settings.json\n", homeDir)

	// 3aa. Install extensions (marketplaces, plugins, skills, MCP servers).
	// caveman: true is handled as a shorthand inside InstallExtensions.
	ext := ac.Extensions
	if ac.Caveman.Enabled() || len(ext.Marketplaces)+len(ext.Plugins)+len(ext.Skills)+len(ext.MCPServers) > 0 {
		if err := agent.InstallExtensions(osUser, homeDir, ext, ac.Caveman, secrets); err != nil {
			fmt.Printf("  warning: extensions for %s: %v\n", osUser, err)
		} else {
			fmt.Printf("  installed extensions for %s\n", osUser)
		}
	}

	// 3ab. Sync team skills repo. Symlinks shared/* and <agentKey>/*
	// from the cloned repo into ~/.claude/skills/. Lets agents PR new
	// skills without operator-mediated config edits.
	if cfg.SkillsRepo != "" {
		if err := agent.SyncSkillsRepo(osUser, homeDir, agentKey, cfg.SkillsRepo); err != nil {
			fmt.Printf("  warning: skills repo sync for %s: %v\n", osUser, err)
		} else {
			fmt.Printf("  synced skills from %s for %s\n", cfg.SkillsRepo, osUser)
		}
	}

	// 3a. Generate SSH keypair (idempotent)
	pubKey, err := agent.EnsureSSHKey(osUser, homeDir)
	if err != nil {
		fmt.Printf("  warning: ssh key for %s: %v\n", osUser, err)
	} else {
		fmt.Printf("  ssh pubkey: %s\n", pubKey)
	}

	// 3b. Configure git commit signing and user identity via the agent's SSH key.
	if pubKey != "" {
		if err := agent.ConfigureGit(osUser, homeDir, pubKey, ac.GitName, ac.GitEmail); err != nil {
			fmt.Printf("  warning: git config for %s: %v\n", osUser, err)
		} else {
			fmt.Printf("  configured git signing + identity for %s\n", osUser)
		}

		// 3b1. Register the signing key on GitHub so commits show as verified.
		// Requires write:ssh_signing_key scope on the agent's GH_TOKEN.
		// Title includes the OS user so multiple agents sharing a GitHub
		// account are distinguishable in https://github.com/settings/ssh/signing.
		signingTitle := fmt.Sprintf("clem-%s", osUser)
		if err := agent.RegisterSSHSigningKey(pubKey, ghToken, signingTitle); err != nil {
			fmt.Printf("  warning: register SSH signing key for %s: %v\n", osUser, err)
		} else if ghToken != "" {
			fmt.Printf("  registered SSH signing key on GitHub for %s\n", osUser)
		}
	}

	// 3c. Install client-side pre-push hook that scans for secret patterns.
	// Defense-in-depth alongside the existing .gitignore_global + GitHub
	// Push Protection. Refuses any push whose diff contains credentials.
	if err := agent.InstallGitHooks(osUser, homeDir); err != nil {
		return fmt.Errorf("installing git hooks for %s: %w", osUser, err)
	}
	fmt.Printf("  installed pre-push secret-scan hook\n")

	// 4. Ensure agent-owned directories (workdir, ~/.local/bin, ~/.claude).
	// MkdirAll as root would leave intermediate parents (.local, .claude)
	// root-owned, which breaks the runner's log writes and claude's
	// credential reads. EnsureOwnedDir chowns the full tree.
	workDir := filepath.Join(homeDir, cfg.Project)
	binDir := filepath.Join(homeDir, ".local", "bin")
	claudeDir := filepath.Join(homeDir, ".claude")
	for _, d := range []string{workDir, binDir, claudeDir} {
		if err := agent.EnsureOwnedDir(d, osUser); err != nil {
			return fmt.Errorf("ensuring %s: %w", d, err)
		}
	}
	content, mode, err := agentdoc.Render(cfg, agentKey, ".")
	if err != nil {
		return fmt.Errorf("rendering CLAUDE.local.md for %s: %w", agentKey, err)
	}
	if content != nil {
		dst := filepath.Join(workDir, "CLAUDE.local.md")
		if err := os.WriteFile(dst, content, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", dst, err)
		}
		fmt.Printf("  wrote %s (%s, %d bytes)\n", dst, mode, len(content))
	}
	chownDir(workDir, osUser)

	// 4a. Write runner.sh
	runnerContent := runner.Generate(cfg, agentKey)
	runnerPath := filepath.Join(binDir, "clem-runner.sh")
	if err := os.WriteFile(runnerPath, []byte(runnerContent), 0755); err != nil {
		return fmt.Errorf("writing runner.sh for %s: %w", agentKey, err)
	}
	chownDir(runnerPath, osUser)
	fmt.Printf("  wrote %s\n", runnerPath)

	// 5. Install systemd service
	svcContent, err := runner.GenerateService(cfg, agentKey)
	if err != nil {
		return fmt.Errorf("generating service for %s: %w", agentKey, err)
	}
	if err := agent.InstallService(cfg, agentKey, svcContent); err != nil {
		return fmt.Errorf("installing service for %s: %w", agentKey, err)
	}
	fmt.Printf("  installed %s\n", cfg.ServiceName(agentKey))

	// 6. Install ttyd web terminal service (if configured)
	if ac.WebTerminalPort > 0 {
		ttydContent := runner.GenerateTtydService(cfg, agentKey)
		ttydSvcName := cfg.TtydServiceName(agentKey)
		if err := agent.InstallServiceByName(ttydSvcName, ttydContent); err != nil {
			return fmt.Errorf("installing ttyd service for %s: %w", agentKey, err)
		}
		fmt.Printf("  installed %s (port %d)\n", ttydSvcName, ac.WebTerminalPort)
	}
	return nil
}

// writeAgentEnv decrypts the agent's vaults and writes /home/<user>/.env —
// plain values, or placeholders plus a freshly-minted inject-only token when
// the agent is brokered. Returns the qualified secrets map (for extensions'
// vault refs) and the agent's GH_TOKEN (for signing-key registration). A
// decrypt failure is a warning, not an error: provider-only env still lets an
// agent run without a vault.
func writeAgentEnv(agentKey string, ac config.AgentConfig, osUser, homeDir string) (map[string]string, string, error) {
	providerEnv, pErr := ac.ProviderEnv()
	if pErr != nil {
		return nil, "", fmt.Errorf("agent %s: %w", agentKey, pErr)
	}
	if ac.Provider != "" && ac.Provider != "anthropic" {
		fmt.Printf("  provider: %s\n", ac.Provider)
	}

	secrets, err := decryptForAgent(agentKey, ac.Vaults)
	if err != nil {
		fmt.Printf("  warning: could not decrypt secrets for %s: %v\n", agentKey, err)
		if len(providerEnv) > 0 {
			// still write provider env so agents can run without vault
			if err := agent.WriteEnvFile(osUser, homeDir, providerEnv); err != nil {
				return nil, "", fmt.Errorf("writing .env for %s: %w", agentKey, err)
			}
			fmt.Printf("  wrote %s/.env (provider only, no vault)\n", homeDir)
		} else {
			fmt.Println("  skipping .env — run clem vault init and set secrets first")
		}
		return nil, "", nil
	}

	flatSecrets := vault.FlatSecrets(secrets)
	merged := make(map[string]string, len(flatSecrets)+len(providerEnv)+12)
	if ac.VaultBroker {
		// agent-vault brokered: consolidate this agent's brokered secrets +
		// their service rules into a single per-agent agent-vault vault, mint
		// a scoped inject-only token bound to it, and write placeholders. The
		// real upstream credentials live only inside agent-vault, never in
		// this agent's .env. One vault per agent works around agent-vault's
		// single-vault-context proxy (injection only resolves the URL vault).
		addr := cfg.Vault.AddrOrDefault()
		consolidated, brokeredKV, svcs, bErr := brokeredSeedInputs(osUser, ac, cfg.Vault.Services, flatSecrets)
		if bErr != nil {
			return nil, "", fmt.Errorf("agent %s: %w", agentKey, bErr)
		}
		if err := vault.SeedVault(addr, consolidated, brokeredKV); err != nil {
			return nil, "", fmt.Errorf("agent %s: seeding consolidated vault: %w", agentKey, err)
		}
		if err := vault.ApplyServices(addr, consolidated, svcs); err != nil {
			return nil, "", fmt.Errorf("agent %s: applying service rules: %w", agentKey, err)
		}
		token, terr := vault.EnsureAgentIdentity(addr, osUser, []string{consolidated})
		if terr != nil {
			return nil, "", fmt.Errorf("agent %s: minting agent-vault token: %w", agentKey, terr)
		}
		merged = agent.BrokeredEnv(cfg.Vault, ac, token, consolidated, flatSecrets)
		for k, v := range providerEnv {
			merged[k] = v
		}
		fmt.Printf("  wrote %s/.env (agent-vault brokered: %d placeholder(s) in vault %s, %d service rule(s))\n",
			homeDir, len(ac.BrokeredSecrets), consolidated, len(svcs))
	} else {
		for k, v := range flatSecrets {
			merged[k] = v
		}
		for k, v := range providerEnv {
			merged[k] = v
		}
		fmt.Printf("  wrote %s/.env (%d secrets + %d provider)\n", homeDir, len(secrets), len(providerEnv))
	}
	if err := agent.WriteEnvFile(osUser, homeDir, merged); err != nil {
		return nil, "", fmt.Errorf("writing .env for %s: %w", agentKey, err)
	}

	// If wrangler credentials are present, write the wrangler config file
	if err := agent.WriteWranglerConfig(osUser, homeDir, secrets); err != nil {
		fmt.Printf("  warning: writing wrangler config: %v\n", err)
	} else if flatSecrets["WRANGLER_OAUTH_TOKEN"] != "" {
		fmt.Printf("  wrote wrangler config for %s\n", osUser)
	}

	ghToken := flatSecrets["GH_TOKEN"]
	if ghToken != "" && ac.GitEmail == "" {
		fmt.Printf("  warning: agent %s has GH_TOKEN but no git_email in clem.yaml — commits may leak operator identity\n", agentKey)
	}
	return secrets, ghToken, nil
}

// brokeredSeedInputs resolves everything the brokering flow needs for one
// agent before any side effect: the consolidated per-agent vault name, the
// key/values to seed into it (the brokered secrets plus any extra credential
// keys referenced by applicable service rules, e.g. a basic-auth username),
// and the applicable service rules themselves. Pure — seeding, rule
// application, and token mint stay in the caller, so a missing brokered
// secret aborts before agent-vault is touched.
//
// The consolidated vault name must differ from the agent name — agent-vault
// conflates a token's vault scope when an agent and a vault share a name, and
// the proxy then 403s even serviced hosts.
func brokeredSeedInputs(osUser string, ac config.AgentConfig, services []config.Service, flat map[string]string) (string, map[string]string, []config.Service, error) {
	consolidated := config.AgentVaultName(osUser + "-brokered")
	brokered := map[string]bool{}
	kv := make(map[string]string, len(ac.BrokeredSecrets))
	for _, k := range ac.BrokeredSecrets {
		v, ok := flat[k]
		if !ok {
			return "", nil, nil, fmt.Errorf("brokered secret %q not found in its vaults", k)
		}
		brokered[k] = true
		kv[k] = v
	}
	svcs := brokeredServicesFor(services, brokered, flat)
	// seed any extra service credential keys (e.g. a basic-auth username)
	for _, s := range svcs {
		for _, ck := range s.CredentialKeys() {
			if _, have := kv[ck]; !have {
				kv[ck] = flat[ck]
			}
		}
	}
	return consolidated, kv, svcs, nil
}

// provisionWatchdog writes the watchdog script and installs its service+timer.
func provisionWatchdog() error {
	fmt.Printf("\n[watchdog]\n")
	wdScript := watchdog.GenerateScript(cfg)
	wdPath := fmt.Sprintf("/usr/local/bin/clem-watchdog-%s.sh", cfg.Project)
	if err := os.WriteFile(wdPath, []byte(wdScript), 0755); err != nil {
		return fmt.Errorf("writing watchdog script: %w", err)
	}
	fmt.Printf("  wrote %s\n", wdPath)

	wdSvc := watchdog.GenerateService(cfg)
	wdTimer := watchdog.GenerateTimer(cfg)
	if err := agent.InstallWatchdogTimer(cfg, wdSvc, wdTimer); err != nil {
		return fmt.Errorf("installing watchdog timer: %w", err)
	}
	fmt.Printf("  installed %s\n", cfg.WatchdogTimerName())
	return nil
}

// provisionManagedSettings writes the host-level managed-settings.json
// (root-owned; agent users cannot override).
func provisionManagedSettings() error {
	managedPath := "/etc/claude-code/managed-settings.json"
	if err := agent.WriteHostManagedSettings(cfg, managedPath); err != nil {
		return fmt.Errorf("writing managed-settings: %w", err)
	}
	fmt.Printf("\nwrote %s\n", managedPath)
	return nil
}

func chownDir(path, username string) {
	// best effort
	agent.ChownPath(path, username)
}

// provisionAgentVaultHost stands up the agent-vault credential proxy: creates
// the vault system user, installs the pinned binary, supplies the master
// password from sops via a root-owned EnvironmentFile, starts the service,
// waits for health, logs in (or registers) the instance owner from sops, seeds
// all sops vaults, applies the injection (service) rules, and fetches the CA
// cert. All admin operations ride the owner session (no admin token exists in
// agent-vault). Only called when the agent-vault backend is active.
func provisionAgentVaultHost() error {
	fmt.Printf("\n[agent-vault]\n")
	vaultUser := cfg.Vault.SystemUserOrDefault()
	if err := agent.EnsureSystemUser(vaultUser); err != nil {
		return fmt.Errorf("agent-vault: %w", err)
	}
	if err := agent.InstallAgentVault(); err != nil {
		return fmt.Errorf("agent-vault: %w", err)
	}
	if err := os.MkdirAll("/etc/clem", 0755); err != nil {
		return fmt.Errorf("agent-vault: creating /etc/clem: %w", err)
	}
	if err := os.MkdirAll(proxy.AgentVaultDataDir, 0700); err != nil {
		return fmt.Errorf("agent-vault: creating data dir: %w", err)
	}
	agent.ChownPath(proxy.AgentVaultDataDir, vaultUser)

	// Master password + owner credentials from sops (vault "clem-vault"). The
	// master password goes into a root-owned EnvironmentFile (0600) — agent-vault
	// has no systemd-credential support; the owner email/password are used
	// transiently to log in and never touch any agent .env.
	allVaults, err := vault.AllVaults()
	if err != nil {
		return fmt.Errorf("agent-vault: reading sops: %w", err)
	}
	clemVault := allVaults["clem-vault"]
	master := clemVault["AGENT_VAULT_MASTER_PASSWORD"]
	if master == "" {
		return fmt.Errorf("agent-vault: set the master password: clem vault set clem-vault AGENT_VAULT_MASTER_PASSWORD=...")
	}
	ownerEmail := clemVault["AGENT_VAULT_OWNER_EMAIL"]
	ownerPassword := clemVault["AGENT_VAULT_OWNER_PASSWORD"]
	if ownerEmail == "" || ownerPassword == "" {
		return fmt.Errorf("agent-vault: set the owner account: clem vault set clem-vault AGENT_VAULT_OWNER_EMAIL=... AGENT_VAULT_OWNER_PASSWORD=...")
	}
	if err := os.WriteFile(proxy.AgentVaultEnvFile,
		[]byte("AGENT_VAULT_MASTER_PASSWORD="+master+"\n"), 0600); err != nil {
		return fmt.Errorf("agent-vault: writing master env file: %w", err)
	}

	if err := agent.InstallServiceByName(cfg.AgentVaultServiceName(), proxy.GenerateAgentVaultService(cfg)); err != nil {
		return fmt.Errorf("agent-vault: installing service: %w", err)
	}
	if err := agent.StartService(cfg.AgentVaultServiceName()); err != nil {
		return fmt.Errorf("agent-vault: starting service: %w", err)
	}
	addr := cfg.Vault.AddrOrDefault()
	if err := waitHealthy(addr, 30); err != nil {
		return fmt.Errorf("agent-vault: %w", err)
	}
	if err := vault.EnsureOwner(addr, ownerEmail, ownerPassword); err != nil {
		return fmt.Errorf("agent-vault: owner auth: %w", err)
	}
	if err := vault.FetchCA(addr, cfg.Vault.CACertPathOrDefault()); err != nil {
		return fmt.Errorf("agent-vault: fetching CA: %w", err)
	}
	// Per-agent consolidated vaults (secrets + service rules) are seeded inside
	// the agent loop, since each brokers a different set; see the VaultBroker
	// branch in runProvision.
	fmt.Printf("  agent-vault up; CA at %s\n", cfg.Vault.CACertPathOrDefault())
	return nil
}

// brokeredServicesFor returns the subset of services applicable to an agent: a
// service applies when it injects at least one of the agent's brokered secrets
// and all of its credential keys are available in the agent's decrypted secrets.
func brokeredServicesFor(services []config.Service, brokered map[string]bool, flat map[string]string) []config.Service {
	var out []config.Service
	for _, s := range services {
		injectsBrokered, allAvailable := false, true
		for _, k := range s.CredentialKeys() {
			if brokered[k] {
				injectsBrokered = true
			}
			if _, ok := flat[k]; !ok {
				allAvailable = false
			}
		}
		if injectsBrokered && allAvailable {
			out = append(out, s)
		}
	}
	return out
}

// waitHealthy polls agent-vault's health endpoint up to attempts times (1s apart).
func waitHealthy(addr string, attempts int) error {
	for i := 0; i < attempts; i++ {
		if err := vault.Health(addr); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("agent-vault did not become healthy at %s after %ds", addr, attempts)
}

// provisionEgress installs the host-level egress containment stack — the
// pipelock forward proxy and the per-agent nftables UID firewall — when any
// agent has egress containment enabled. No-op otherwise. Must run after agent
// OS users exist, since the firewall ruleset is keyed on their UIDs.
func provisionEgress() error {
	anyEgress := false
	for key := range cfg.Agents {
		if cfg.EgressEnabledFor(key) {
			anyEgress = true
			break
		}
	}
	if !anyEgress {
		return nil
	}

	fmt.Printf("\n[egress]\n")
	proxyUser := cfg.Egress.ProxyUserOrDefault()
	if err := agent.EnsureSystemUser(proxyUser); err != nil {
		return fmt.Errorf("egress: %w", err)
	}
	if err := agent.InstallPipelock(); err != nil {
		return fmt.Errorf("egress: %w", err)
	}

	// /etc/clem holds the generated proxy config + firewall ruleset (root-owned,
	// no secrets). /var/log/clem holds the signed audit log, written by the
	// proxy user.
	if err := os.MkdirAll("/etc/clem", 0755); err != nil {
		return fmt.Errorf("egress: creating /etc/clem: %w", err)
	}
	if err := os.MkdirAll(proxy.AuditLogPath, 0750); err != nil {
		return fmt.Errorf("egress: creating %s: %w", proxy.AuditLogPath, err)
	}
	agent.ChownPath(proxy.AuditLogPath, proxyUser)

	cfgPath := proxy.PipelockConfigPath(cfg.Project)
	if err := os.WriteFile(cfgPath, []byte(proxy.GeneratePipelockConfig(cfg)), 0644); err != nil {
		return fmt.Errorf("egress: writing %s: %w", cfgPath, err)
	}
	fmt.Printf("  wrote %s\n", cfgPath)

	nft, err := proxy.GenerateNftables(cfg)
	if err != nil {
		return fmt.Errorf("egress: %w", err)
	}
	nftPath := proxy.NftablesPath(cfg.Project)
	if err := os.WriteFile(nftPath, []byte(nft), 0644); err != nil {
		return fmt.Errorf("egress: writing %s: %w", nftPath, err)
	}
	fmt.Printf("  wrote %s\n", nftPath)

	// Firewall first (so the proxy comes up behind a closed boundary), then the
	// proxy. The agent units order After= both via runner.proxyUnitDeps.
	if err := agent.InstallServiceByName(cfg.NftablesServiceName(), proxy.GenerateNftablesService(cfg)); err != nil {
		return fmt.Errorf("egress: installing firewall service: %w", err)
	}
	if err := agent.StartService(cfg.NftablesServiceName()); err != nil {
		return fmt.Errorf("egress: starting firewall: %w", err)
	}
	fmt.Printf("  installed + started %s\n", cfg.NftablesServiceName())

	if err := agent.InstallServiceByName(cfg.PipelockServiceName(), proxy.GeneratePipelockService(cfg)); err != nil {
		return fmt.Errorf("egress: installing pipelock service: %w", err)
	}
	if err := agent.StartService(cfg.PipelockServiceName()); err != nil {
		return fmt.Errorf("egress: starting pipelock: %w", err)
	}
	fmt.Printf("  installed + started %s (port %d)\n", cfg.PipelockServiceName(), cfg.Egress.ProxyPortOrDefault())
	return nil
}

// provisionMCPSidecars stands up the privileged MCP sidecar stack: the dedicated
// mcp system user, the pinned mcp-proxy bridge, one loopback listener per
// subscribed sidecar (mcp-proxy fronting the wrapped stdio MCP server, with the
// upstream secret supplied root-side via EnvironmentFile), and a loopback
// nftables firewall that lets only each listener's subscribing agent UID(s)
// reach its port. No-op when no agent subscribes to a sidecar. Must run after
// the agent loop so subscriber UIDs resolve.
func provisionMCPSidecars() error {
	listeners := cfg.SidecarListeners()
	if len(listeners) == 0 {
		return nil
	}
	fmt.Printf("\n[mcp-sidecars]\n")
	mcpUser := cfg.MCPSidecars.SystemUserOrDefault()
	if err := agent.EnsureSystemUser(mcpUser); err != nil {
		return fmt.Errorf("mcp-sidecars: %w", err)
	}
	if err := agent.InstallMCPProxy(); err != nil {
		return fmt.Errorf("mcp-sidecars: %w", err)
	}
	if err := os.MkdirAll("/etc/clem", 0755); err != nil {
		return fmt.Errorf("mcp-sidecars: creating /etc/clem: %w", err)
	}
	if err := agent.EnsureOwnedDir(proxy.SidecarStateDir, mcpUser); err != nil {
		return fmt.Errorf("mcp-sidecars: state dir: %w", err)
	}

	allVaults, err := vault.AllVaults()
	if err != nil {
		return fmt.Errorf("mcp-sidecars: reading sops: %w", err)
	}

	// Pre-flight: resolve EVERY listener's secrets before touching any unit. A
	// missing secret must abort while the system is still untouched — never
	// half-installed with a credential-holding listener left behind a stale or
	// absent firewall (the firewall is the only cross-UID boundary on hosts
	// without egress containment).
	type resolved struct {
		l   config.SidecarListener
		env map[string]string
	}
	pending := make([]resolved, 0, len(listeners))
	for _, l := range listeners {
		secretEnv, err := sidecarSecretEnv(cfg, l, allVaults)
		if err != nil {
			return fmt.Errorf("mcp-sidecars: sidecar %s: %w", l.Server.Name, err)
		}
		pending = append(pending, resolved{l, secretEnv})
	}

	// Write each listener's root-owned secret env + (re)install its unit.
	for _, r := range pending {
		envPath := proxy.SidecarEnvFile(cfg.Project, r.l.Server.Name, r.l.AgentKey)
		if err := agent.WriteSystemdEnvFile(envPath, r.env); err != nil {
			return fmt.Errorf("mcp-sidecars: sidecar %s: %w", r.l.Server.Name, err)
		}
		svcName := cfg.SidecarServiceName(r.l.Server.Name, r.l.AgentKey)
		if err := agent.InstallServiceByName(svcName, proxy.GenerateSidecarService(cfg, r.l)); err != nil {
			return fmt.Errorf("mcp-sidecars: installing %s: %w", svcName, err)
		}
		fmt.Printf("  wrote %s + installed %s (port %d, %d secret(s))\n", envPath, svcName, r.l.Port, len(r.env))
	}

	// Apply the loopback firewall (and REAPPLY on re-provision) BEFORE starting
	// listeners, so a listener is never reachable on a port the firewall has not
	// yet locked to its subscribers.
	nft, err := proxy.GenerateSidecarNftables(cfg)
	if err != nil {
		return fmt.Errorf("mcp-sidecars: %w", err)
	}
	nftPath := proxy.SidecarNftablesPath(cfg.Project)
	if err := os.WriteFile(nftPath, []byte(nft), 0644); err != nil {
		return fmt.Errorf("mcp-sidecars: writing %s: %w", nftPath, err)
	}
	if err := agent.InstallServiceByName(cfg.SidecarNftablesServiceName(), proxy.GenerateSidecarNftablesService(cfg)); err != nil {
		return fmt.Errorf("mcp-sidecars: installing firewall service: %w", err)
	}
	// restart (not start) so a re-provision re-runs `nft -f` and the updated
	// ruleset takes effect; only after this succeeds do we (re)start listeners.
	if err := agent.RestartService(cfg.SidecarNftablesServiceName()); err != nil {
		return fmt.Errorf("mcp-sidecars: applying firewall: %w", err)
	}
	fmt.Printf("  installed + applied %s\n", cfg.SidecarNftablesServiceName())

	// restart (not start) so a changed ExecStart (port/command) takes effect and
	// the listener comes up behind the freshly-applied firewall.
	for _, r := range pending {
		svcName := cfg.SidecarServiceName(r.l.Server.Name, r.l.AgentKey)
		if err := agent.RestartService(svcName); err != nil {
			return fmt.Errorf("mcp-sidecars: starting %s: %w", svcName, err)
		}
	}
	fmt.Printf("  started %d sidecar listener(s)\n", len(pending))
	return nil
}

// sidecarSecretEnv resolves the upstream secret values for a listener. A shared
// listener reads the named sops vault (secrets_vault); a per-agent listener
// reads the subscribing agent's own vaults (the value differs per agent).
func sidecarSecretEnv(c *config.Config, l config.SidecarListener, allVaults map[string]map[string]string) (map[string]string, error) {
	var source map[string]string
	if l.AgentKey != "" {
		ac := c.Agents[l.AgentKey]
		sec, err := decryptForAgent(l.AgentKey, ac.Vaults)
		if err != nil {
			return nil, fmt.Errorf("decrypting secrets for %s: %w", l.AgentKey, err)
		}
		source = vault.FlatSecrets(sec)
	} else {
		if l.Server.SecretsVault == "" {
			return nil, fmt.Errorf("shared sidecar requires secrets_vault")
		}
		source = allVaults[l.Server.SecretsVault]
		if source == nil {
			return nil, fmt.Errorf("secrets_vault %q not found in sops", l.Server.SecretsVault)
		}
	}
	out := make(map[string]string, len(l.Server.Secrets))
	for _, k := range l.Server.Secrets {
		v, ok := source[k]
		if !ok || v == "" {
			return nil, fmt.Errorf("secret %q not found in source vault", k)
		}
		out[k] = v
	}
	return out, nil
}
