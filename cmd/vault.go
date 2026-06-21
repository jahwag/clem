package cmd

import (
	"fmt"
	"os"
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

var vaultRenameCmd = &cobra.Command{
	Use:   "rename <vault> <new-vault> | <vault> <old-key> <new-key>",
	Short: "Rename a vault, or a secret key within a vault, in secrets.sops.yaml",
	Long: "Two args renames a vault; three args renames a key within a vault.\n" +
		"Each secret is re-encrypted under the new name (sops binds the YAML\n" +
		"key path into every value's AAD, so a raw key edit breaks decryption).",
	Args: cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 2 {
			return vault.RenameVault(args[0], args[1])
		}
		return vault.RenameKey(args[0], args[1], args[2])
	},
}

var vaultMigrateAddr string

var vaultMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Seed all sops vaults into a running agent-vault instance",
	Long: "Decrypts secrets.sops.yaml and pushes every vault into agent-vault,\n" +
		"then applies the injection (service) rules from clem.yaml.\n" +
		"sops remains the source of truth; agent-vault is derived, reproducible state.\n" +
		"Authenticates as the instance owner using AGENT_VAULT_OWNER_EMAIL/PASSWORD\n" +
		"from the sops clem-vault (logs in, or registers the owner on a fresh instance).\n" +
		"Address precedence: --addr, then AGENT_VAULT_ADDR, then http://127.0.0.1:14321.",
	RunE: func(cmd *cobra.Command, args []string) error {
		addr := vaultMigrateAddr
		if addr == "" {
			addr = os.Getenv("AGENT_VAULT_ADDR")
		}
		if addr == "" {
			addr = "http://127.0.0.1:14321"
		}
		allVaults, err := vault.AllVaults()
		if err != nil {
			return fmt.Errorf("reading sops: %w", err)
		}
		clemVault := allVaults["clem-vault"]
		email := clemVault["AGENT_VAULT_OWNER_EMAIL"]
		password := clemVault["AGENT_VAULT_OWNER_PASSWORD"]
		if email == "" || password == "" {
			return fmt.Errorf("set the owner account: clem vault set clem-vault AGENT_VAULT_OWNER_EMAIL=... AGENT_VAULT_OWNER_PASSWORD=...")
		}
		if err := vault.Health(addr); err != nil {
			return fmt.Errorf("agent-vault not reachable at %s: %w", addr, err)
		}
		if err := vault.EnsureOwner(addr, email, password); err != nil {
			return fmt.Errorf("owner auth: %w", err)
		}
		// Mirror every sops vault into agent-vault. Per-agent brokering vaults +
		// service rules are set up by `clem provision`, not here.
		return vault.Migrate(addr)
	},
}

func init() {
	vaultMigrateCmd.Flags().StringVar(&vaultMigrateAddr, "addr", "", "agent-vault management API address")
	vaultCmd.AddCommand(vaultInitCmd, vaultSetCmd, vaultGetCmd, vaultListCmd, vaultDeleteCmd, vaultRenameCmd, vaultMigrateCmd)
	rootCmd.AddCommand(vaultCmd)
}
