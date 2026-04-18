package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/jahwag/clem/internal/agent"
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
		return remote.Login(loginRemote)
	}

	for agentKey, ac := range cfg.Agents {
		osUser := cfg.OSUsername(agentKey)
		fmt.Printf("[%s] %s (%s)\n", agentKey, ac.Name, osUser)

		if !agent.NeedsLogin(osUser) {
			expiry := agent.TokenExpiry(osUser)
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
