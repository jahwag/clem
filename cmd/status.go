package cmd

import (
	"fmt"
	"sort"
	"time"

	"github.com/jahwag/clem/internal/agent"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show agent health: systemd state, tmux, token expiry, last log",
	RunE:  runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	// Collect and sort agent keys for consistent output
	keys := make([]string, 0, len(cfg.Agents))
	for k := range cfg.Agents {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Header
	fmt.Printf("%-10s %-20s %-20s %-8s %-8s %-8s %-20s %s\n",
		"AGENT", "NAME", "USER", "SYSTEMD", "TMUX", "TTYD", "TOKEN EXPIRES", "LAST LOG")
	fmt.Println(repeatStr("-", 120))

	for _, agentKey := range keys {
		ac := cfg.Agents[agentKey]
		osUser := cfg.OSUsername(agentKey)
		svcName := cfg.ServiceName(agentKey)

		systemdState := agent.SystemdState(svcName)
		tmuxAlive := "no"
		if agent.TmuxAlive(osUser, agentKey) {
			tmuxAlive = "yes"
		}

		ttydStr := "-"
		if ac.WebTerminalPort > 0 {
			ttydState := agent.SystemdState(cfg.TtydServiceName(agentKey))
			if ttydState == "active" {
				ttydStr = fmt.Sprintf(":%d", ac.WebTerminalPort)
			} else {
				ttydStr = "off"
			}
		}

		expiry := agent.TokenExpiry(fmt.Sprintf("/home/%s", osUser))
		expiryStr := "missing"
		if !expiry.IsZero() {
			daysLeft := int(time.Until(expiry).Hours() / 24)
			if daysLeft < 0 {
				expiryStr = fmt.Sprintf("EXPIRED (%d days)", -daysLeft)
			} else {
				expiryStr = fmt.Sprintf("%s (%dd)", expiry.Format("2006-01-02"), daysLeft)
			}
		}

		logPath := fmt.Sprintf("/home/%s/.claude/%s-runner.log", osUser, agentKey)
		lastLog := agent.LastLogLine(logPath)

		fmt.Printf("%-10s %-20s %-20s %-8s %-8s %-8s %-20s %s\n",
			agentKey, ac.Name, osUser, systemdState, tmuxAlive, ttydStr, expiryStr, lastLog)
	}
	return nil
}

func repeatStr(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}
