package cmd

import (
	"fmt"
	"os"

	"github.com/jahwag/clem/internal/config"
	"github.com/spf13/cobra"
)

var configPath string
var cfg *config.Config

// Version is set at build time via -ldflags "-X github.com/jahwag/clem/cmd.Version=v0.1.0".
// Defaults to "dev" for unversioned local builds.
var Version = "dev"

var rootCmd = &cobra.Command{
	Use:     "clem",
	Short:   "Manage persistent 24/7 multi-agent Claude Code teams",
	Long:    `clem — docker-compose for Claude agents. Manages OS users, tmux sessions, and systemd services.`,
	Version: Version,
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "clem.yaml", "path to clem.yaml")
	rootCmd.PersistentPreRunE = loadConfig
}

func loadConfig(cmd *cobra.Command, args []string) error {
	// vault subcommands operate on secrets.sops.yaml, not clem.yaml
	if cmd.Parent() != nil && cmd.Parent().Name() == "vault" {
		return nil
	}
	if cmd.Name() == "vault" {
		return nil
	}
	// init writes clem.yaml — skip loading it
	if cmd.Name() == "init" {
		return nil
	}

	var err error
	cfg, err = config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	return nil
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("this command requires root privileges — run with sudo")
	}
	return nil
}
