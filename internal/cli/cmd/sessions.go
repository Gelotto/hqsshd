package cmd

import (
	"context"
	"encoding/json"
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
	jsonOutput   bool
	statusFilter string
	toolFilter   string
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
	sessionsCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	sessionsCmd.Flags().StringVar(&statusFilter, "status", "", "Filter by status: RUNNING, IDLE, ENDED")
	sessionsCmd.Flags().StringVar(&toolFilter, "tool", "", "Filter by tool: claude, codex, aider, shell")
}

func runSessions(cmd *cobra.Command, args []string) error {
	cfg := resolveConfig()
	if cfg.Host == "" {
		return fmt.Errorf("no host specified\n\nProvide a host using one of:\n  --host/-H flag:    hqssh sessions -H server.example.com\n  Environment var:   export HQSSH_HOST=server.example.com\n  Config file:       ~/.hqssh/config.yaml with default_host set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Connect to daemon with retry on transient failures
	fmt.Fprintf(os.Stderr, "Connecting to %s...\n", cfg.Host)
	c, err := client.ConnectWithRetry(ctx, client.Config{
		Host:            cfg.Host,
		Port:            cfg.Port,
		User:            cfg.User,
		KeyPath:         cfg.Key,
		Password:        cfg.Password,
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

	// Apply local filters
	sessions := filterSessions(resp.Sessions)

	// JSON output
	if jsonOutput {
		type jsonSession struct {
			ID           string `json:"id"`
			Tool         string `json:"tool"`
			Status       string `json:"status"`
			ProjectID    string `json:"project_id,omitempty"`
			WorkingDir   string `json:"working_directory,omitempty"`
			ClientCount  int32  `json:"client_count"`
			CreatedAt    int64  `json:"created_at"`
			LastActivity int64  `json:"last_activity"`
		}
		out := make([]jsonSession, len(sessions))
		for i, s := range sessions {
			out[i] = jsonSession{
				ID:           s.Id,
				Tool:         s.Tool,
				Status:       statusString(s.Status),
				ProjectID:    s.ProjectId,
				WorkingDir:   s.WorkingDirectory,
				ClientCount:  s.ClientCount,
				CreatedAt:    s.CreatedAt,
				LastActivity: s.LastActivity,
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(sessions) == 0 {
		fmt.Println("No active sessions.")
		fmt.Println("\nTo create a new session:")
		fmt.Printf("  hqssh new --tool claude -H %s\n", cfg.Host)
		fmt.Println("\nOr start from the HQSSH mobile app, then attach with:")
		fmt.Printf("  hqssh attach <session-id> -H %s\n", cfg.Host)
		return nil
	}

	// Print sessions table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTOOL\tSTATUS\tPROJECT\tCLIENTS\tLAST ACTIVITY")
	fmt.Fprintln(w, "--\t----\t------\t-------\t-------\t-------------")

	for _, s := range sessions {
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
	fmt.Printf("  hqssh attach <ID> -H %s\n", cfg.Host)

	return nil
}

func filterSessions(sessions []*pb.Session) []*pb.Session {
	if statusFilter == "" && toolFilter == "" {
		return sessions
	}

	var filtered []*pb.Session
	for _, s := range sessions {
		// Filter by status
		if statusFilter != "" {
			status := statusString(s.Status)
			if !strings.EqualFold(status, statusFilter) {
				continue
			}
		}

		// Filter by tool
		if toolFilter != "" {
			if !strings.EqualFold(s.Tool, toolFilter) {
				continue
			}
		}

		filtered = append(filtered, s)
	}
	return filtered
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
