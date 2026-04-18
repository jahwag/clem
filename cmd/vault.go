package cmd

import (
	"fmt"
	"strings"

	"github.com/jahwag/clem/internal/vault"
	"github.com/spf13/cobra"
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
	Use:   "set <vault> KEY=value",
	Short: "Set a secret in a vault in secrets.sops.yaml",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		vaultName := args[0]
		keyval := args[1]
		if !strings.Contains(keyval, "=") {
			return fmt.Errorf("invalid format: expected KEY=value, got %q", keyval)
		}
		return vault.Set(vaultName, keyval)
	},
}

var vaultGetCmd = &cobra.Command{
	Use:   "get <vault> KEY",
	Short: "Get a secret from a vault in secrets.sops.yaml",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return vault.Get(args[0], args[1])
	},
}

var vaultListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all vaults and their keys (values hidden) in secrets.sops.yaml",
	RunE: func(cmd *cobra.Command, args []string) error {
		return vault.List()
	},
}

var vaultDeleteCmd = &cobra.Command{
	Use:   "delete <vault> [KEY]",
	Short: "Delete a secret key (or entire vault if no key) from secrets.sops.yaml",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key := ""
		if len(args) == 2 {
			key = args[1]
		}
		return vault.Delete(args[0], key)
	},
}

func init() {
	vaultCmd.AddCommand(vaultInitCmd, vaultSetCmd, vaultGetCmd, vaultListCmd, vaultDeleteCmd)
	rootCmd.AddCommand(vaultCmd)
}
