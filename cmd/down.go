package cmd

import (
	"fmt"

	"github.com/jahwag/clem/internal/agent"
	"github.com/spf13/cobra"
)

var downCmd = &cobra.Command{
	Use:   "down",
	Short: "Stop all agent systemd services",
	RunE:  runDown,
}

func init() {
	rootCmd.AddCommand(downCmd)
}

func runDown(cmd *cobra.Command, args []string) error {
	if err := requireRoot(); err != nil {
		return err
	}

	for agentKey, ac := range cfg.Agents {
		if ac.WebTerminalPort > 0 {
			ttydSvc := cfg.TtydServiceName(agentKey)
			fmt.Printf("stopping %s... ", ttydSvc)
			if err := agent.StopService(ttydSvc); err != nil {
				fmt.Println("FAILED")
				return err
			}
			fmt.Println("ok")
		}

		svcName := cfg.ServiceName(agentKey)
		fmt.Printf("stopping %s (%s)... ", ac.Name, svcName)
		if err := agent.StopService(svcName); err != nil {
			fmt.Println("FAILED")
			return err
		}
		fmt.Println("ok")
	}
	return nil
}
