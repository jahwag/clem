package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const clemYAMLTemplate = `# clem.yaml — Clementine team configuration
# Secrets go in secrets.sops.yaml (encrypted). Run: clem vault init

project: myteam   # prefix for OS usernames (myteam-lead) and systemd services

coordination:
  backend: discord
  server_id: "YOUR_DISCORD_SERVER_ID"   # right-click server in Discord > Copy Server ID
  channels:
    general: "CHANNEL_ID"   # text channel for status updates
    tasks:   "CHANNEL_ID"   # forum channel for task board
    alerts:  "CHANNEL_ID"   # text channel for critical alerts
    lessons: "CHANNEL_ID"   # forum channel for post-mortems

agents:
  lead:
    name: "Amara"                      # display name in Claude Code and Discord
    role: "Lead Software Engineer"     # human-readable, for your reference
    model: "claude-sonnet-4-6"
    iteration_minutes: 10              # sleep between sessions during active hours (07-22); 2x at night
    vaults: [github, discord-lead]     # vault names from secrets.sops.yaml to merge into .env
    prompt: >-
      Act as Amara per CLAUDE.local.md.
      Check Discord #tasks for tasks assigned to you.
      Work on ONE task. When done post results and run: kill $PPID.
      If no tasks: kill $PPID

  worker:
    name: "Athena"
    role: "Software Engineer"
    model: "claude-sonnet-4-6"
    iteration_minutes: 5
    reports_to: lead                   # key of supervising agent
    vaults: [github, discord-worker]
    prompt: >-
      Act as Athena per CLAUDE.local.md.
      Check Discord #tasks for tasks assigned to you.
      Work on ONE task. When done post results and run: kill $PPID.
      If no tasks: kill $PPID
`

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Write a commented clem.yaml template to the current directory",
	RunE:  runInit,
}

func init() {
	rootCmd.AddCommand(initCmd)
}

func runInit(cmd *cobra.Command, args []string) error {
	const target = "clem.yaml"
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("%s already exists — delete it first if you want to regenerate", target)
	}

	if err := os.WriteFile(target, []byte(clemYAMLTemplate), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}

	fmt.Printf("Wrote %s\n", target)
	fmt.Println("Edit clem.yaml, then run: clem vault init")
	return nil
}
