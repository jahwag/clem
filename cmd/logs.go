package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs <agent>",
	Short: "Tail the runner log for an agent",
	Args:  cobra.ExactArgs(1),
	RunE:  runLogs,
}

func init() {
	rootCmd.AddCommand(logsCmd)
}

func runLogs(cmd *cobra.Command, args []string) error {
	agentKey := args[0]

	ac, ok := cfg.Agents[agentKey]
	if !ok {
		return fmt.Errorf("unknown agent: %s", agentKey)
	}

	osUser := cfg.OSUsername(agentKey)
	logPath := fmt.Sprintf("/home/%s/.claude/%s-runner.log", osUser, agentKey)

	fmt.Printf("tailing log for %s (%s): %s\n", ac.Name, osUser, logPath)

	tailCmd := exec.Command("tail", "-f", logPath)
	tailCmd.Stdout = os.Stdout
	tailCmd.Stderr = os.Stderr
	return tailCmd.Run()
}
