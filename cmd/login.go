package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/jahwag/clem/internal/agent"
	"github.com/jahwag/clem/internal/config"
	"github.com/jahwag/clem/internal/remote"
	"github.com/spf13/cobra"
)

var loginRemote string

var loginCmd = &cobra.Command{
	Use:   "login [agent...]",
	Short: "Authenticate each agent with Claude (su - <user> -c 'claude /login')",
	RunE:  runLogin,
}

func init() {
	rootCmd.AddCommand(loginCmd)
	loginCmd.Flags().StringVar(&loginRemote, "remote", "", "run login on a remote host via SSH (e.g. root@1.2.3.4)")
}

func runLogin(cmd *cobra.Command, args []string) error {
	if loginRemote != "" {
		if len(args) > 0 {
			return fmt.Errorf("agent args are not supported with --remote; run clem login %s on the remote host instead", strings.Join(args, " "))
		}
		return remote.Login(loginRemote)
	}

	agents, err := selectAgents(cfg.Agents, args)
	if err != nil {
		return err
	}

	// Sort agent keys for consistent output (matches clem status).
	keys := make([]string, 0, len(agents))
	for key := range agents {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, agentKey := range keys {
		ac := agents[agentKey]
		osUser := cfg.OSUsername(agentKey)
		fmt.Printf("[%s] %s (%s)\n", agentKey, ac.Name, osUser)

		homeDir := fmt.Sprintf("/home/%s", osUser)

		if ac.RuntimeKind() == "codex" {
			if !agent.CodexNeedsLogin(homeDir) {
				fmt.Printf("  codex already authenticated — skipping\n")
				continue
			}
			// Device-auth prints a URL + code rather than opening a browser,
			// which is what a headless agent host needs. The binary lives at
			// the per-user npm prefix, off the default PATH, so call it directly.
			fmt.Printf("  running codex login as %s\n", osUser)
			loginCmd := exec.Command("su", "-", osUser, "-c", `"$HOME/.npm-global/bin/codex" login --device-auth`)
			loginCmd.Stdin = os.Stdin
			loginCmd.Stdout = os.Stdout
			loginCmd.Stderr = os.Stderr
			if err := loginCmd.Run(); err != nil {
				return fmt.Errorf("codex login for %s: %w", osUser, err)
			}
			continue
		}

		if !agent.NeedsLogin(homeDir) {
			expiry := agent.TokenExpiry(homeDir)
			fmt.Printf("  token valid until %s — skipping\n", expiry.Format("2006-01-02"))
			continue
		}

		fmt.Printf("  running claude /login as %s\n", osUser)
		loginCmd := exec.Command("su", "-", osUser, "-c", "claude /login")
		loginCmd.Stdin = os.Stdin
		loginCmd.Stdout = os.Stdout
		loginCmd.Stderr = os.Stderr
		if err := loginCmd.Run(); err != nil {
			return fmt.Errorf("claude /login for %s: %w", osUser, err)
		}
	}
	return nil
}

// selectAgents narrows the configured agents to the keys given on the
// command line. No keys means all agents.
func selectAgents(all map[string]config.AgentConfig, keys []string) (map[string]config.AgentConfig, error) {
	if len(keys) == 0 {
		return all, nil
	}
	selected := make(map[string]config.AgentConfig, len(keys))
	for _, key := range keys {
		ac, ok := all[key]
		if !ok {
			return nil, fmt.Errorf("unknown agent: %s", key)
		}
		selected[key] = ac
	}
	return selected, nil
}
