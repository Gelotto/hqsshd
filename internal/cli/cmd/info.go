package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var infoJSON bool

var infoCmd = &cobra.Command{
	Use:   "info",
	Short: "Show system and daemon information",
	Long: `Display information about the remote system and hqsshd daemon.

Shows hostname, OS, daemon version, available AI tools, uptime,
and current session count.

Examples:
  hqssh info -H server.example.com
  hqssh info --json    # Machine-readable output`,
	RunE: runInfo,
}

func init() {
	infoCmd.Flags().BoolVar(&infoJSON, "json", false, "Output as JSON")
	rootCmd.AddCommand(infoCmd)
}

func runInfo(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, hostLabel, err := connectDaemon(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	// Get system info
	info, err := c.SystemService.GetInfo(ctx, &pb.Empty{})
	if err != nil {
		return fmt.Errorf("get info: %w", err)
	}

	// Get system status
	status, err := c.SystemService.GetStatus(ctx, &pb.Empty{})
	if err != nil {
		return fmt.Errorf("get status: %w", err)
	}

	// JSON output
	if infoJSON {
		host := hostLabel
		if host == "" {
			host = "localhost"
		}
		output := struct {
			Host           string   `json:"host"`
			Hostname       string   `json:"hostname"`
			OS             string   `json:"os"`
			Arch           string   `json:"arch"`
			DaemonVersion  string   `json:"daemon_version"`
			InstalledTools []string `json:"installed_tools"`
			UptimeSeconds  int64    `json:"uptime_seconds"`
			ActiveSessions int32    `json:"active_sessions"`
			ActiveProjects int32    `json:"active_projects"`
		}{
			Host:           host,
			Hostname:       info.Hostname,
			OS:             info.Os,
			Arch:           info.Arch,
			DaemonVersion:  info.DaemonVersion,
			InstalledTools: info.InstalledTools,
			UptimeSeconds:  status.UptimeSeconds,
			ActiveSessions: status.ActiveSessions,
			ActiveProjects: status.ActiveProjects,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	// Human-readable output
	infoHost := hostLabel
	if infoHost == "" {
		infoHost = "localhost"
	}
	fmt.Printf("\nHost:       %s\n", infoHost)
	fmt.Printf("Hostname:   %s\n", info.Hostname)
	fmt.Printf("OS:         %s (%s)\n", info.Os, info.Arch)
	fmt.Printf("Daemon:     hqsshd %s\n", info.DaemonVersion)
	fmt.Printf("Uptime:     %s\n", formatUptime(status.UptimeSeconds))
	fmt.Printf("Sessions:   %d active\n", status.ActiveSessions)
	fmt.Printf("Projects:   %d registered\n", status.ActiveProjects)

	if len(info.InstalledTools) > 0 {
		fmt.Printf("Tools:      %s\n", strings.Join(info.InstalledTools, ", "))
	} else {
		fmt.Printf("Tools:      none detected\n")
	}

	fmt.Println()
	return nil
}

func formatUptime(seconds int64) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	}
	if seconds < 86400 {
		hours := seconds / 3600
		mins := (seconds % 3600) / 60
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	return fmt.Sprintf("%dd %dh", days, hours)
}
