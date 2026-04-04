package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/jahwag/clem/internal/agent"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate each agent with Claude (sudo -u <user> claude /login)",
	RunE:  runLogin,
}

func init() {
	rootCmd.AddCommand(loginCmd)
}

func runLogin(cmd *cobra.Command, args []string) error {
	for agentKey, ac := range cfg.Agents {
		osUser := cfg.OSUsername(agentKey)
		fmt.Printf("[%s] %s (%s)\n", agentKey, ac.Name, osUser)

		if !agent.NeedsLogin(osUser) {
			expiry := agent.TokenExpiry(osUser)
			fmt.Printf("  token valid until %s — skipping\n", expiry.Format("2006-01-02"))
			continue
		}

		fmt.Printf("  running claude /login as %s\n", osUser)
		loginCmd := exec.Command("sudo", "-u", osUser, "claude", "/login")
		loginCmd.Stdin = os.Stdin
		loginCmd.Stdout = os.Stdout
		loginCmd.Stderr = os.Stderr
		if err := loginCmd.Run(); err != nil {
			return fmt.Errorf("claude /login for %s: %w", osUser, err)
		}
	}
	return nil
}
