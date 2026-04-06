package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/jahwag/clem/internal/agent"
	"github.com/jahwag/clem/internal/remote"
	"github.com/jahwag/clem/internal/runner"
	"github.com/jahwag/clem/internal/vault"
	"github.com/jahwag/clem/internal/watchdog"
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

		// 2. Decrypt and write .env
		secrets, err := vault.DecryptForAgent(agentKey, ac.Vaults)
		if err != nil {
			fmt.Printf("  warning: could not decrypt secrets for %s: %v\n", agentKey, err)
			fmt.Println("  skipping .env — run clem vault init and set secrets first")
		} else {
			if err := agent.WriteEnvFile(osUser, secrets); err != nil {
				return fmt.Errorf("writing .env for %s: %w", agentKey, err)
			}
			fmt.Printf("  wrote /home/%s/.env (%d secrets)\n", osUser, len(secrets))
		}

		// 3. Write Claude Code settings (skip MCP trust dialog, onboarding)
		if err := agent.WriteSettings(osUser); err != nil {
			return fmt.Errorf("writing settings for %s: %w", agentKey, err)
		}
		fmt.Printf("  wrote /home/%s/.claude/settings.json\n", osUser)

		// 4. Create working directory and copy CLAUDE.local.md
		homeDir := fmt.Sprintf("/home/%s", osUser)
		workDir := filepath.Join(homeDir, cfg.Project)
		if err := os.MkdirAll(workDir, 0755); err != nil {
			return fmt.Errorf("creating workdir %s: %w", workDir, err)
		}
		if src, err := os.ReadFile("CLAUDE.local.md"); err == nil {
			dst := filepath.Join(workDir, "CLAUDE.local.md")
			os.WriteFile(dst, src, 0644)
			fmt.Printf("  copied CLAUDE.local.md to %s\n", workDir)
		}
		chownDir(workDir, osUser)

		// 4. Write runner.sh
		runnerContent := runner.Generate(cfg, agentKey)
		runnerPath := filepath.Join(homeDir, ".local", "bin", "clem-runner.sh")
		if err := os.MkdirAll(filepath.Dir(runnerPath), 0755); err != nil {
			return fmt.Errorf("creating bin dir: %w", err)
		}
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
