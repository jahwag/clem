package cmd

import (
	"fmt"
	"sort"
	"strings"
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
	fmt.Println(strings.Repeat("-", 120))

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

		homeDir := fmt.Sprintf("/home/%s", osUser)
		expiry := agent.TokenExpiry(homeDir)
		hasRefresh := agent.HasRefreshToken(homeDir)
		expiryStr := tokenExpiryDisplay(expiry, hasRefresh)

		logPath := fmt.Sprintf("/home/%s/.claude/%s-runner.log", osUser, agentKey)
		lastLog := agent.LastLogLine(logPath)

		fmt.Printf("%-10s %-20s %-20s %-8s %-8s %-8s %-20s %s\n",
			agentKey, ac.Name, osUser, systemdState, tmuxAlive, ttydStr, expiryStr, lastLog)
	}
	return nil
}

// tokenExpiryDisplay formats the access-token expiry for `clem status`.
// Claude Max access tokens last ~8h and are refreshed transparently, so we
// distinguish "no credentials at all" (login required) from "access token
// short/expired but refresh token present" (auto-refresh handles it).
func tokenExpiryDisplay(expiry time.Time, hasRefresh bool) string {
	if expiry.IsZero() && !hasRefresh {
		return "missing"
	}
	if expiry.IsZero() {
		return "auto-refresh"
	}
	remaining := time.Until(expiry)
	if remaining < 0 {
		if hasRefresh {
			return "auto-refresh"
		}
		return fmt.Sprintf("EXPIRED (%dd)", -int(remaining.Hours()/24))
	}
	if remaining < 24*time.Hour {
		return fmt.Sprintf("%s (%dh)", expiry.Format("2006-01-02"), int(remaining.Hours()))
	}
	return fmt.Sprintf("%s (%dd)", expiry.Format("2006-01-02"), int(remaining.Hours()/24))
}
