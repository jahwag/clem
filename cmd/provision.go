package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jahwag/clem/internal/agent"
	"github.com/jahwag/clem/internal/agentdoc"
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

	for agentKey, ac := range cfg.Agents {
		osUser := cfg.OSUsername(agentKey)
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
		providerEnv, pErr := ac.ProviderEnv()
		if pErr != nil {
			return fmt.Errorf("agent %s: %w", agentKey, pErr)
		}
		if ac.Provider != "" && ac.Provider != "anthropic" {
			fmt.Printf("  provider: %s\n", ac.Provider)
		}

		secrets, err := vault.DecryptForAgent(agentKey, ac.Vaults)
		if err != nil {
			fmt.Printf("  warning: could not decrypt secrets for %s: %v\n", agentKey, err)
			if len(providerEnv) > 0 {
				// still write provider env so agents can run without vault
				if err := agent.WriteEnvFile(osUser, providerEnv); err != nil {
					return fmt.Errorf("writing .env for %s: %w", agentKey, err)
				}
				fmt.Printf("  wrote /home/%s/.env (provider only, no vault)\n", osUser)
			} else {
				fmt.Println("  skipping .env — run clem vault init and set secrets first")
			}
		} else {
			merged := make(map[string]string, len(secrets)+len(providerEnv))
			for k, v := range secrets {
				merged[k] = v
			}
			for k, v := range providerEnv {
				merged[k] = v
			}
			if err := agent.WriteEnvFile(osUser, merged); err != nil {
				return fmt.Errorf("writing .env for %s: %w", agentKey, err)
			}
			fmt.Printf("  wrote /home/%s/.env (%d secrets + %d provider)\n", osUser, len(secrets), len(providerEnv))

			// If wrangler credentials are present, write the wrangler config file
			if err := agent.WriteWranglerConfig(osUser, secrets); err != nil {
				fmt.Printf("  warning: writing wrangler config: %v\n", err)
			} else if secrets["WRANGLER_OAUTH_TOKEN"] != "" {
				fmt.Printf("  wrote wrangler config for %s\n", osUser)
			}
		}

		// 3. Write Claude Code settings (skip MCP trust dialog, onboarding)
		if err := agent.WriteSettings(osUser); err != nil {
			return fmt.Errorf("writing settings for %s: %w", agentKey, err)
		}
		fmt.Printf("  wrote /home/%s/.claude/settings.json\n", osUser)

		// 3aa. Install caveman plugin if enabled (reduces output tokens ~75%)
		if ac.Caveman {
			if err := agent.InstallCaveman(osUser); err != nil {
				fmt.Printf("  warning: caveman install for %s: %v\n", osUser, err)
			} else {
				fmt.Printf("  installed caveman plugin for %s\n", osUser)
			}
		}

		// 3a. Generate SSH keypair (idempotent)
		pubKey, err := agent.EnsureSSHKey(osUser)
		if err != nil {
			fmt.Printf("  warning: ssh key for %s: %v\n", osUser, err)
		} else {
			fmt.Printf("  ssh pubkey: %s\n", pubKey)
		}

		// 4. Ensure agent-owned directories (workdir, ~/.local/bin, ~/.claude).
		// MkdirAll as root would leave intermediate parents (.local, .claude)
		// root-owned, which breaks the runner's log writes and claude's
		// credential reads. EnsureOwnedDir chowns the full tree.
		homeDir := fmt.Sprintf("/home/%s", osUser)
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

		// 4. Write runner.sh
		runnerContent := runner.Generate(cfg, agentKey)
		runnerPath := filepath.Join(binDir, "clem-runner.sh")
		if err := os.WriteFile(runnerPath, []byte(runnerContent), 0755); err != nil {
			return fmt.Errorf("writing runner.sh for %s: %w", agentKey, err)
		}
		chownDir(runnerPath, osUser)
		fmt.Printf("  wrote %s\n", runnerPath)

		// 5. Install systemd service
		svcContent := runner.GenerateService(cfg, agentKey)
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
	}

	// 6. Install watchdog
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

	fmt.Printf("\nProvisioning complete. Run 'clem login' then 'clem up'.\n")
	return nil
}

func chownDir(path, username string) {
	// best effort
	agent.ChownPath(path, username)
}
