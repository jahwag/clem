package cmd

import (
	"fmt"

	"github.com/jahwag/clem/internal/agent"
	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:     "stop",
	Aliases: []string{"down"},
	Short:   "Stop all agents and keep them stopped",
	Long: `Stop all agents for the project in the current directory.

Stopped means stopped: every unit is disabled ('systemctl disable --now'), so
neither the watchdog nor a reboot brings the agents back. 'clem start' undoes
it. The watchdog exists to recover crashes — a deliberate stop is not a crash,
so operator intent always wins.

'clem down' is a deprecated alias.`,
	// Fleet-wide kill switch: reject args so 'clem stop <agent>' errors
	// loudly instead of silently stopping everything.
	Args: cobra.NoArgs,
	RunE: runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
}

func runStop(cmd *cobra.Command, args []string) error {
	if err := requireRoot(); err != nil {
		return err
	}

	// Disable the watchdog before the agents: the timer fires every 5 minutes
	// and a mid-flight run could otherwise restart a service stopped below
	// before its unit is disabled. Disabling the oneshot service too covers a
	// run that is already executing.
	for _, wd := range []string{cfg.WatchdogTimerName(), cfg.WatchdogServiceName()} {
		fmt.Printf("disabling %s... ", wd)
		if err := agent.DisableNowService(wd); err != nil {
			fmt.Println("FAILED")
			return err
		}
		fmt.Println("ok")
	}

	for agentKey, ac := range cfg.Agents {
		if ac.WebTerminalPort > 0 {
			ttydSvc := cfg.TtydServiceName(agentKey)
			fmt.Printf("disabling %s... ", ttydSvc)
			if err := agent.DisableNowService(ttydSvc); err != nil {
				fmt.Println("FAILED")
				return err
			}
			fmt.Println("ok")
		}

		if cfg.UsesGitHubCoordination() {
			// A stop that leaves the GitHub issue watcher spawning work is not
			// a stop.
			watchSvc := cfg.GitHubWatchServiceName(agentKey)
			fmt.Printf("disabling %s... ", watchSvc)
			if err := agent.DisableNowService(watchSvc); err != nil {
				fmt.Println("FAILED")
				return err
			}
			fmt.Println("ok")
		}

		svcName := cfg.ServiceName(agentKey)
		fmt.Printf("disabling %s (%s)... ", ac.Name, svcName)
		if err := agent.DisableNowService(svcName); err != nil {
			fmt.Println("FAILED")
			return err
		}
		fmt.Println("ok")
	}
	return nil
}
