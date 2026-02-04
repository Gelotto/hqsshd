package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gelotto/hqsshd/internal/cli/client"
	pb "github.com/gelotto/hqsshd/proto"
	"github.com/spf13/cobra"
)

var (
	includeEnded bool
	projectID    string
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "List active sessions on a remote system",
	Long: `List all active AI sessions on the remote system.

Sessions that were started from your mobile device will appear here.
Use 'hqssh attach <session-id>' to connect to a session.`,
	RunE: runSessions,
}

func init() {
	sessionsCmd.Flags().BoolVar(&includeEnded, "all", false, "Include ended sessions")
	sessionsCmd.Flags().StringVar(&projectID, "project", "", "Filter by project ID")
}

func runSessions(cmd *cobra.Command, args []string) error {
	if host == "" {
		return fmt.Errorf("--host is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Connect to daemon with retry on transient failures
	fmt.Fprintf(os.Stderr, "Connecting to %s...\n", host)
	c, err := client.ConnectWithRetry(ctx, client.Config{
		Host:            host,
		Port:            port,
		User:            user,
		KeyPath:         keyPath,
		Password:        password,
		InsecureHostKey: insecureKey,
	})
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Close()

	// List sessions
	resp, err := c.SessionService.List(ctx, &pb.ListSessionsRequest{
		ProjectId:    projectID,
		IncludeEnded: includeEnded,
	})
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	if len(resp.Sessions) == 0 {
		fmt.Println("No active sessions.")
		fmt.Println("\nStart a session from the HQSSH mobile app, then attach with:")
		fmt.Println("  hqssh attach <session-id> -H", host)
		return nil
	}

	// Print sessions table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTOOL\tSTATUS\tPROJECT\tCLIENTS\tLAST ACTIVITY")
	fmt.Fprintln(w, "--\t----\t------\t-------\t-------\t-------------")

	for _, s := range resp.Sessions {
		status := statusString(s.Status)
		project := s.ProjectId
		if project == "" {
			project = "(shell)"
		} else if len(project) > 20 {
			project = project[:17] + "..."
		}

		lastActivity := formatTime(s.LastActivity)

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			shortID(s.Id),
			strings.ToUpper(s.Tool),
			status,
			project,
			s.ClientCount,
			lastActivity,
		)
	}
	w.Flush()

	fmt.Println("\nTo attach to a session:")
	fmt.Println("  hqssh attach <ID> -H", host)

	return nil
}

func statusString(s pb.SessionStatus) string {
	switch s {
	case pb.SessionStatus_SESSION_STATUS_RUNNING:
		return "RUNNING"
	case pb.SessionStatus_SESSION_STATUS_IDLE:
		return "IDLE"
	case pb.SessionStatus_SESSION_STATUS_ENDED:
		return "ENDED"
	default:
		return "UNKNOWN"
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func formatTime(ts int64) string {
	if ts == 0 {
		return "Never"
	}
	t := time.Unix(ts, 0)
	diff := time.Since(t)

	if diff < time.Minute {
		return "Just now"
	}
	if diff < time.Hour {
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	}
	if diff < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(diff.Hours()/24))
}
