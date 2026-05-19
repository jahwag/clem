package cmd

import (
	"fmt"

	"github.com/jahwag/clem/internal/agent"
	"github.com/spf13/cobra"
)

// syncSkillsCmd is invoked by clem-runner.sh as the agent user before each TUI
// launch. It pulls the team skills repo and refreshes ~/.claude/skills/<name>
// symlinks so any skill merged since the last iteration becomes available on
// the next claude-code startup. No sudo wrapping — caller is already the
// target user (runner runs under systemd User=).
var syncSkillsCmd = &cobra.Command{
	Use:    "sync-skills",
	Short:  "Pull skills repo and refresh ~/.claude/skills/ symlinks (runner-side)",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		home, _ := cmd.Flags().GetString("home")
		agentKey, _ := cmd.Flags().GetString("agent-key")
		repo, _ := cmd.Flags().GetString("repo")
		if home == "" || agentKey == "" || repo == "" {
			return fmt.Errorf("sync-skills requires --home, --agent-key, and --repo")
		}
		return agent.SyncSkillsRepoAsSelf(home, agentKey, repo)
	},
}

func init() {
	syncSkillsCmd.Flags().String("home", "", "agent home directory (e.g. /home/myteam-lead)")
	syncSkillsCmd.Flags().String("agent-key", "", "agent key from clem.yaml (e.g. lead, worker)")
	syncSkillsCmd.Flags().String("repo", "", "skills repo URL")
	rootCmd.AddCommand(syncSkillsCmd)
}
