package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/jahwag/clem/internal/vault"
)

var vaultCmd = &cobra.Command{
	Use:   "vault",
	Short: "Manage secrets in secrets.sops.yaml",
}

var vaultInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate age keypair and print .sops.yaml instructions",
	RunE: func(cmd *cobra.Command, args []string) error {
		return vault.Init()
	},
}

var vaultSetCmd = &cobra.Command{
	Use:   "set <agent> KEY=value",
	Short: "Set a secret for an agent in secrets.sops.yaml",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		agentKey := args[0]
		keyval := args[1]
		if !strings.Contains(keyval, "=") {
			return fmt.Errorf("invalid format: expected KEY=value, got %q", keyval)
		}
		return vault.Set(agentKey, keyval)
	},
}

var vaultGetCmd = &cobra.Command{
	Use:   "get <agent> KEY",
	Short: "Get a secret for an agent from secrets.sops.yaml",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return vault.Get(args[0], args[1])
	},
}

var vaultListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all secrets (keys only) in secrets.sops.yaml",
	RunE: func(cmd *cobra.Command, args []string) error {
		return vault.List()
	},
}

func init() {
	vaultCmd.AddCommand(vaultInitCmd, vaultSetCmd, vaultGetCmd, vaultListCmd)
	rootCmd.AddCommand(vaultCmd)
}
